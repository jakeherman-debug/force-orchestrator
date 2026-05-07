package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strings"

	"force-orchestrator/internal/agents"
	igit "force-orchestrator/internal/git"
	"force-orchestrator/internal/store"
	"force-orchestrator/internal/telemetry"
)

// newUUID returns a random UUID v4 string.
func newUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant bits
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func cmdAdd(db *sql.DB, args []string) {
	const usageMsg = "Usage: force add [--priority N] [--plan-only] [--type Feature|Investigate|Audit] [--repo <name>] [--idempotency-key KEY] <task description>"
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	priority := fs.Int("priority", 0, "task priority (higher claims first)")
	planOnly := fs.Bool("plan-only", false, "Commander plans only; operator approves the convoy before agents run")
	taskType := fs.String("type", "", "Feature|Investigate|Audit|WriteMemory|MedicReview (default: auto-classify)")
	repo := fs.String("repo", "", "scope task to a registered repo")
	idempotencyKey := fs.String("idempotency-key", "", "client-provided uniqueness token")
	helped, perr := parseSubcommandFlags(fs, args, "add",
		"Queue a new task. With no --type the Inquisitor classifies asynchronously.",
		[]flagDoc{
			{Name: "--priority N", Desc: "task priority (higher claims first)"},
			{Name: "--plan-only", Desc: "Commander plans only; approve convoy to run"},
			{Name: "--type T", Desc: "Feature|Investigate|Audit|WriteMemory|MedicReview"},
			{Name: "--repo R", Desc: "scope task to a registered repo"},
			{Name: "--idempotency-key K", Desc: "client-provided uniqueness token"},
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{"force add Implement /api/foo", "force add --type Feature --repo backend Add /api/bar"})
	if helped {
		return
	}
	if perr != nil {
		os.Exit(2)
	}
	addArgs := fs.Args()
	if len(addArgs) == 0 {
		fmt.Println(usageMsg)
		os.Exit(1)
	}
	validTypes := map[string]bool{"Feature": true, "Investigate": true, "Audit": true, "WriteMemory": true, "MedicReview": true}
	if *taskType != "" {
		if *taskType == "CodeEdit" {
			fmt.Fprintf(os.Stderr, "error: CodeEdit is no longer a valid direct task type.\nAll code changes flow through Commander → Chancellor for conflict review.\nUse: force add --type Feature <description>\n  Or omit --type to auto-classify.\n")
			os.Exit(1)
		}
		if !validTypes[*taskType] {
			fmt.Printf("Invalid type '%s'. Valid values: Feature, Investigate, Audit\n", *taskType)
			os.Exit(1)
		}
	}
	if *repo != "" && store.GetRepoPath(db, *repo) == "" {
		fmt.Fprintf(os.Stderr, "error: unknown repo '%s'. Register it first with: force add-repo\n", *repo)
		os.Exit(1)
	}
	taskPayload := strings.Join(addArgs, " ")
	if *planOnly {
		taskPayload = "[PLAN_ONLY]\n" + taskPayload
	}
	idemKey := *idempotencyKey
	if idemKey == "" {
		idemKey = newUUID()
	}
	// When no type is specified, submit as Auto/Classifying so the UI is not blocked.
	// The Inquisitor will classify it asynchronously and transition it to Pending.
	if *taskType == "" {
		id, err := store.AddBountyClassifying(db, *repo, taskPayload, *priority, idemKey)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: failed to add task: %v\n", err)
			os.Exit(1)
		}
		planSuffix := ""
		if *planOnly {
			planSuffix = " — Commander will plan only; approve with: force convoy approve <convoy-id>"
		}
		fmt.Printf("Queued as task #%d (classifying): '%s'%s\n", id, strings.Join(addArgs, " "), planSuffix)
		return
	}
	id := store.AddBounty(db, 0, *taskType, taskPayload)
	if *repo != "" {
		db.Exec(`UPDATE BountyBoard SET target_repo = ? WHERE id = ?`, *repo, id)
	}
	if *priority != 0 {
		store.SetBountyPriority(db, id, *priority)
	}
	planSuffix := ""
	if *planOnly {
		planSuffix = " — Commander will plan only; approve with: force convoy approve <convoy-id>"
	}
	fmt.Printf("Queued as task #%d: '%s'%s\n", id, strings.Join(addArgs, " "), planSuffix)
}

func cmdAddInvestigate(db *sql.DB, args []string) {
	fs := flag.NewFlagSet("investigate", flag.ContinueOnError)
	priority := fs.Int("priority", 0, "task priority")
	repo := fs.String("repo", "", "scope investigation to a registered repo")
	helped, perr := parseSubcommandFlags(fs, args, "investigate",
		"Queue an Investigate task — agent reads code/docs and writes findings, no edits.",
		[]flagDoc{
			{Name: "--priority N", Desc: "task priority"},
			{Name: "--repo R", Desc: "scope investigation to a repo"},
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{"force investigate --repo backend why is /api/foo flaky"})
	if helped {
		return
	}
	if perr != nil {
		os.Exit(2)
	}
	taskArgs := fs.Args()
	if len(taskArgs) == 0 {
		fmt.Println("Usage: force investigate [--priority N] [--repo <name>] <question>")
		os.Exit(1)
	}
	if *repo != "" && store.GetRepoPath(db, *repo) == "" {
		fmt.Printf("Unknown repo '%s'. Register it first with: force add-repo\n", *repo)
		os.Exit(1)
	}
	payload := strings.Join(taskArgs, " ")
	res, _ := db.Exec(
		`INSERT INTO BountyBoard (target_repo, type, status, payload, priority, created_at)
		 VALUES (?, 'Investigate', 'Pending', ?, ?, datetime('now'))`,
		*repo, payload, *priority)
	id, _ := res.LastInsertId()
	repoSuffix := ""
	if *repo != "" {
		repoSuffix = fmt.Sprintf(" (scoped to %s)", *repo)
	}
	fmt.Printf("Investigation #%d queued%s: %s\n", id, repoSuffix, payload)
}

func cmdAddAudit(db *sql.DB, args []string) {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	priority := fs.Int("priority", 0, "task priority")
	repo := fs.String("repo", "", "scope scan to a registered repo")
	helped, perr := parseSubcommandFlags(fs, args, "scan",
		"Queue an Audit task — agent surveys code/security and emits Planned tasks.",
		[]flagDoc{
			{Name: "--priority N", Desc: "task priority"},
			{Name: "--repo R", Desc: "scope scan to a repo"},
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{"force scan --repo backend audit auth flow"})
	if helped {
		return
	}
	if perr != nil {
		os.Exit(2)
	}
	taskArgs := fs.Args()
	if len(taskArgs) == 0 {
		fmt.Println("Usage: force scan [--priority N] [--repo <name>] <scope/question>")
		os.Exit(1)
	}
	if *repo != "" && store.GetRepoPath(db, *repo) == "" {
		fmt.Printf("Unknown repo '%s'. Register it first with: force add-repo\n", *repo)
		os.Exit(1)
	}
	payload := strings.Join(taskArgs, " ")
	res, _ := db.Exec(
		`INSERT INTO BountyBoard (target_repo, type, status, payload, priority, created_at)
		 VALUES (?, 'Audit', 'Pending', ?, ?, datetime('now'))`,
		*repo, payload, *priority)
	id, _ := res.LastInsertId()
	repoSuffix := ""
	if *repo != "" {
		repoSuffix = fmt.Sprintf(" (scoped to %s)", *repo)
	}
	fmt.Printf("Audit #%d queued%s — findings will be Planned tasks awaiting your approval: %s\n", id, repoSuffix, payload)
}

func cmdAddJira(db *sql.DB, args []string) {
	// JIRA-from-UI: the fetch + payload-formatting body has moved into
	// agents.QueueFeatureFromJira so the dashboard's
	// `POST /api/feature/from-jira` handler can call the same core.
	fs := flag.NewFlagSet("add-jira", flag.ContinueOnError)
	priority := fs.Int("priority", 0, "task priority")
	planOnly := fs.Bool("plan-only", false, "Commander plans only; operator approves convoy to run")
	helped, perr := parseSubcommandFlags(fs, args, "add-jira",
		"Fetch a Jira ticket and queue it as a Feature task.",
		[]flagDoc{
			{Name: "--priority N", Desc: "task priority"},
			{Name: "--plan-only", Desc: "Commander plans only; approve convoy to run"},
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{"force add-jira PROJ-123"})
	if helped {
		return
	}
	if perr != nil {
		os.Exit(2)
	}
	jiraArgs := fs.Args()
	if len(jiraArgs) < 1 {
		fmt.Println("Usage: force add-jira [--priority N] [--plan-only] <TICKET-ID>")
		os.Exit(1)
	}
	ticketID := jiraArgs[0]
	fmt.Printf("Fetching Jira ticket %s...\n", ticketID)

	res, err := agents.QueueFeatureFromJira(context.Background(), db, ticketID, *priority, *planOnly)
	if err != nil {
		fmt.Printf("Failed to fetch Jira ticket: %v\n", err)
		os.Exit(1)
	}
	planSuffix := ""
	if *planOnly {
		planSuffix = " — Commander will plan only; approve with: force convoy approve <convoy-id>"
	}
	fmt.Printf("Jira ticket %s added to the Fleet as task #%d%s.\n", ticketID, res.TaskID, planSuffix)
}

// cmdReset handles both "reset" and "retry" (identical behavior).
// `name` distinguishes the help-text label between the two aliases.
func cmdReset(db *sql.DB, name, via string, args []string) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	helped, perr := parseSubcommandFlags(fs, args, name,
		"Reset a task to Pending so an agent can re-claim it. Records an AuditLog entry.",
		[]flagDoc{{Name: "--help, -h", Desc: "show this help and exit"}},
		[]string{"force " + name + " 42"})
	if helped {
		return
	}
	if perr != nil {
		os.Exit(2)
	}
	rest := fs.Args()
	if len(rest) < 1 {
		fmt.Printf("Usage: force %s <task-id>\n", name)
		os.Exit(1)
	}
	id := mustParseID(rest[0])
	store.ResetTask(db, id)
	store.LogAudit(db, "operator", "reset", id, via)
	var status string
	db.QueryRow(`SELECT status FROM BountyBoard WHERE id = ?`, id).Scan(&status)
	fmt.Printf("Task %d reset to %s.\n", id, status)
}

func cmdCancel(db *sql.DB, args []string) {
	fs := flag.NewFlagSet("cancel", flag.ContinueOnError)
	requeueType := fs.String("requeue", "", "if set, requeue the cancelled task as a new task of this type")
	helped, perr := parseSubcommandFlags(fs, args, "cancel",
		"Cancel a task. Optionally requeue it as a new task of a given type.",
		[]flagDoc{
			{Name: "--requeue T", Desc: "Feature|CodeEdit|Investigate|Audit|WriteMemory"},
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{"force cancel 42", "force cancel 42 --requeue Feature"})
	if helped {
		return
	}
	if perr != nil {
		os.Exit(2)
	}
	cancelArgs := fs.Args()
	if len(cancelArgs) == 0 {
		fmt.Println("Usage: force cancel <task-id> [--requeue <type>]")
		os.Exit(1)
	}

	id := mustParseID(cancelArgs[0])

	if *requeueType != "" {
		validTypes := map[string]bool{"Feature": true, "CodeEdit": true, "Investigate": true, "Audit": true, "WriteMemory": true, "MedicReview": true}
		if !validTypes[*requeueType] {
			fmt.Printf("Invalid requeue type %q — must be one of: Feature, CodeEdit, Investigate, Audit, WriteMemory\n", *requeueType)
			os.Exit(1)
		}
	}

	var currentStatus string
	db.QueryRow(`SELECT status FROM BountyBoard WHERE id = ?`, id).Scan(&currentStatus)
	if currentStatus == "" {
		fmt.Printf("Task %d not found.\n", id)
		os.Exit(1)
	}
	if currentStatus == "Completed" {
		fmt.Printf("Task %d is already Completed and cannot be cancelled.\n", id)
		os.Exit(1)
	}
	store.CancelTask(db, id, "Cancelled by operator")
	store.LogAudit(db, "operator", "cancel", id, "cancelled via CLI")

	if *requeueType != "" {
		var payload string
		db.QueryRow(`SELECT payload FROM BountyBoard WHERE id = ?`, id).Scan(&payload)
		newID := store.AddBounty(db, 0, *requeueType, payload)
		fmt.Printf("Task #%d cancelled — re-queued as %s #%d\n", id, *requeueType, newID)
	} else {
		fmt.Printf("Task %d cancelled.\n", id)
	}
}

func cmdBlock(db *sql.DB, args []string) {
	fs := flag.NewFlagSet("block", flag.ContinueOnError)
	helped, perr := parseSubcommandFlags(fs, args, "block",
		"Mark a task as blocked-by another task (adds a TaskDependencies edge).",
		[]flagDoc{{Name: "--help, -h", Desc: "show this help and exit"}},
		[]string{"force block 42 41"})
	if helped {
		return
	}
	if perr != nil {
		os.Exit(2)
	}
	rest := fs.Args()
	if len(rest) < 2 {
		fmt.Println("Usage: force block <task-id> <blocker-id>")
		os.Exit(1)
	}
	taskID := mustParseID(rest[0])
	blockerID := mustParseID(rest[1])
	var count int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE id IN (?, ?)`, taskID, blockerID).Scan(&count)
	if count != 2 {
		fmt.Printf("One or both tasks not found (task %d, blocker %d).\n", taskID, blockerID)
		os.Exit(1)
	}
	store.AddDependency(db, taskID, blockerID)
	fmt.Printf("Task %d is now blocked by task %d.\n", taskID, blockerID)
}

func cmdUnblock(db *sql.DB, args []string) {
	fs := flag.NewFlagSet("unblock", flag.ContinueOnError)
	helped, perr := parseSubcommandFlags(fs, args, "unblock",
		"Remove all TaskDependencies edges from this task.",
		[]flagDoc{{Name: "--help, -h", Desc: "show this help and exit"}},
		[]string{"force unblock 42"})
	if helped {
		return
	}
	if perr != nil {
		os.Exit(2)
	}
	rest := fs.Args()
	if len(rest) < 1 {
		fmt.Println("Usage: force unblock <task-id>")
		os.Exit(1)
	}
	id := mustParseID(rest[0])
	var taskExists int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE id = ?`, id).Scan(&taskExists)
	if taskExists == 0 {
		fmt.Printf("Task %d not found.\n", id)
	} else {
		store.RemoveDependenciesOf(db, id)
		fmt.Printf("Task %d unblocked (all dependencies removed).\n", id)
	}
}

func cmdUnblockDependents(db *sql.DB, args []string) {
	fs := flag.NewFlagSet("unblock-dependents", flag.ContinueOnError)
	helped, perr := parseSubcommandFlags(fs, args, "unblock-dependents",
		"Remove all TaskDependencies edges that point to this task.",
		[]flagDoc{{Name: "--help, -h", Desc: "show this help and exit"}},
		[]string{"force unblock-dependents 42"})
	if helped {
		return
	}
	if perr != nil {
		os.Exit(2)
	}
	rest := fs.Args()
	if len(rest) < 1 {
		fmt.Println("Usage: force unblock-dependents <task-id>")
		os.Exit(1)
	}
	id := mustParseID(rest[0])
	count := store.UnblockDependentsOf(db, id)
	if count == 0 {
		fmt.Printf("No tasks were depending on #%d.\n", id)
	} else {
		fmt.Printf("Removed %d dependency edge(s) pointing to #%d.\n", count, id)
	}
}

func cmdTree(db *sql.DB, args []string) {
	fs := flag.NewFlagSet("tree", flag.ContinueOnError)
	helped, perr := parseSubcommandFlags(fs, args, "tree",
		"Print the task tree rooted at this task.",
		[]flagDoc{{Name: "--help, -h", Desc: "show this help and exit"}},
		[]string{"force tree 42"})
	if helped {
		return
	}
	if perr != nil {
		os.Exit(2)
	}
	rest := fs.Args()
	if len(rest) < 1 {
		fmt.Println("Usage: force tree <task-id>")
		os.Exit(1)
	}
	id := mustParseID(rest[0])
	printTree(db, id, 0)
}

// Fix #8e: ctx threads from main's signal-cancellation ctx so the diff
// subprocess cancels on Ctrl-C.
func cmdDiff(ctx context.Context, db *sql.DB, args []string) {
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	helped, perr := parseSubcommandFlags(fs, args, "diff",
		"Print the git diff for the task's branch (vs the repo's default branch).",
		[]flagDoc{{Name: "--help, -h", Desc: "show this help and exit"}},
		[]string{"force diff 42"})
	if helped {
		return
	}
	if perr != nil {
		os.Exit(2)
	}
	rest := fs.Args()
	if len(rest) < 1 {
		fmt.Println("Usage: force diff <task-id>")
		os.Exit(1)
	}
	id := mustParseID(rest[0])
	b, err := store.GetBounty(db, id)
	if err != nil {
		fmt.Printf("Task %d not found\n", id)
		os.Exit(1)
	}
	if b.BranchName == "" {
		fmt.Printf("Task %d has no branch yet (status: %s)\n", id, b.Status)
		os.Exit(1)
	}
	repoPath := store.GetRepoPath(db, b.TargetRepo)
	if repoPath == "" {
		fmt.Printf("Unknown repo '%s'\n", b.TargetRepo)
		os.Exit(1)
	}
	diff := igit.GetDiff(ctx, repoPath, b.BranchName)
	if diff == "" {
		fmt.Printf("No diff found for branch %s — branch may not have any commits yet\n", b.BranchName)
	} else {
		fmt.Printf("Branch: %s\n\n", b.BranchName)
		fmt.Println(diff)
	}
}

// cmdApproveTask handles operator manual task approval (NOT convoy approve).
// Fix #8e: ctx threads from main's signal-cancellation ctx.
func cmdApproveTask(ctx context.Context, db *sql.DB, args []string) {
	fs := flag.NewFlagSet("approve", flag.ContinueOnError)
	helped, perr := parseSubcommandFlags(fs, args, "approve",
		"Operator-approve a task awaiting review and merge it. Records an AuditLog entry.",
		[]flagDoc{{Name: "--help, -h", Desc: "show this help and exit"}},
		[]string{"force approve 42"})
	if helped {
		return
	}
	if perr != nil {
		os.Exit(2)
	}
	rest := fs.Args()
	if len(rest) < 1 {
		fmt.Println("Usage: force approve <task-id>")
		os.Exit(1)
	}
	id := mustParseID(rest[0])
	b, err := store.GetBounty(db, id)
	if err != nil {
		fmt.Printf("Task %d not found\n", id)
		os.Exit(1)
	}
	if b.Status != "AwaitingCouncilReview" && b.Status != "UnderReview" &&
		b.Status != "AwaitingCaptainReview" && b.Status != "UnderCaptainReview" {
		fmt.Printf("Task %d is not awaiting review (status: %s)\n", id, b.Status)
		os.Exit(1)
	}
	repoPath := store.GetRepoPath(db, b.TargetRepo)
	if repoPath == "" {
		fmt.Printf("Unknown repo '%s'\n", b.TargetRepo)
		os.Exit(1)
	}
	branchName := b.BranchName
	if branchName == "" {
		branchName = fmt.Sprintf("agent/task-%d", id)
	}
	worktreeDir := igit.ResolveWorktreeDir(db, branchName, repoPath, id, agents.BranchAgentName)
	// Get diff before merge — branch is deleted by MergeAndCleanup.
	diff := igit.GetDiff(ctx, repoPath, branchName)
	if mergeErr := igit.MergeAndCleanup(ctx, db, b.TargetRepo, repoPath, branchName, worktreeDir); mergeErr != nil {
		fmt.Printf("Merge failed: %v\n", mergeErr)
		os.Exit(1)
	}
	if err := store.UpdateBountyStatus(db, id, "Completed"); err != nil {
		fmt.Fprintf(os.Stderr, "warning: task %d merged but status update to Completed failed: %v\n", id, err)
		fmt.Fprintf(os.Stderr, "  the merge itself succeeded; re-run the approval or manually set status to Completed.\n")
		os.Exit(1)
	}
	store.UnblockDependentsOf(db, id)
	if diff != "" {
		changedFiles := igit.ExtractDiffFiles(diff)
		filesStr := strings.Join(changedFiles, ", ")
		store.StoreFleetMemory(db, b.TargetRepo, b.ID, "success",
			fmt.Sprintf("Task: %s", truncate(b.Payload, 400)), filesStr, "operator-approved")
	}
	telemetry.EmitEvent(telemetry.TelemetryEvent{
		EventType: "operator_approved",
		Payload:   map[string]any{"task_id": id},
	})
	store.LogAudit(db, "operator", "approve", id, "manually approved and merged")
	fmt.Printf("Task %d approved and merged by operator.\n", id)
}

// cmdRejectTask handles operator reject.
func cmdRejectTask(db *sql.DB, args []string) {
	fs := flag.NewFlagSet("reject", flag.ContinueOnError)
	helped, perr := parseSubcommandFlags(fs, args, "reject",
		"Operator-reject a task. With retries left it returns for rework; otherwise FailBounty.",
		[]flagDoc{{Name: "--help, -h", Desc: "show this help and exit"}},
		[]string{"force reject 42 needs better tests"})
	if helped {
		return
	}
	if perr != nil {
		os.Exit(2)
	}
	rest := fs.Args()
	if len(rest) < 2 {
		fmt.Println("Usage: force reject <task-id> <reason>")
		os.Exit(1)
	}
	id := mustParseID(rest[0])
	reason := strings.Join(rest[1:], " ")
	b, err := store.GetBounty(db, id)
	if err != nil {
		fmt.Printf("Task %d not found\n", id)
		os.Exit(1)
	}
	retryCount := store.IncrementRetryCount(db, id)
	if retryCount >= agents.MaxRetries {
		if err := store.FailBounty(db, id, fmt.Sprintf("Operator rejected (final): %s", reason)); err != nil {
			fmt.Fprintf(os.Stderr, "error: task %d rejected but FailBounty write failed: %v\n", id, err)
			os.Exit(1)
		}
		fmt.Printf("Task %d permanently failed (max retries reached).\n", id)
	} else {
		newPayload := fmt.Sprintf("%s\n\nOPERATOR FEEDBACK (attempt %d/%d): %s", b.Payload, retryCount, agents.MaxRetries, reason)
		store.ReturnTaskForRework(db, id, newPayload)
		fmt.Printf("Task %d returned for rework (attempt %d/%d): %s\n", id, retryCount, agents.MaxRetries, reason)
	}
	telemetry.EmitEvent(telemetry.TelemetryEvent{
		EventType: "operator_rejected",
		Payload:   map[string]any{"task_id": id, "reason": reason},
	})
	store.LogAudit(db, "operator", "reject", id, reason)
}

func cmdPrioritize(db *sql.DB, args []string) {
	fs := flag.NewFlagSet("prioritize", flag.ContinueOnError)
	helped, perr := parseSubcommandFlags(fs, args, "prioritize",
		"Set a task's priority (higher claims first; default 0).",
		[]flagDoc{{Name: "--help, -h", Desc: "show this help and exit"}},
		[]string{"force prioritize 42 100"})
	if helped {
		return
	}
	if perr != nil {
		os.Exit(2)
	}
	rest := fs.Args()
	if len(rest) < 2 {
		fmt.Println("Usage: force prioritize <task-id> <priority>")
		fmt.Println("  priority is an integer — higher values claim first (default 0)")
		os.Exit(1)
	}
	taskID := mustParseID(rest[0])
	prio := mustParseID(rest[1])
	var exists int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE id = ?`, taskID).Scan(&exists)
	if exists == 0 {
		fmt.Printf("Task %d not found.\n", taskID)
		os.Exit(1)
	}
	store.SetBountyPriority(db, taskID, prio)
	store.LogAudit(db, "operator", "prioritize", taskID, fmt.Sprintf("set priority=%d", prio))
	fmt.Printf("Task %d priority set to %d.\n", taskID, prio)
}

func cmdRetryAllFailed(db *sql.DB, args []string) {
	fs := flag.NewFlagSet("retry-all-failed", flag.ContinueOnError)
	helped, perr := parseSubcommandFlags(fs, args, "retry-all-failed",
		"Reset every Failed task back to Pending. Records an AuditLog entry.",
		[]flagDoc{{Name: "--help, -h", Desc: "show this help and exit"}},
		[]string{"force retry-all-failed"})
	if helped {
		return
	}
	if perr != nil {
		os.Exit(2)
	}
	n := store.ResetAllFailed(db)
	store.LogAudit(db, "operator", "retry-all-failed", 0, fmt.Sprintf("reset %d failed tasks", n))
	fmt.Printf("Reset %d failed task(s) to Pending.\n", n)
}

func cmdTaskNote(db *sql.DB, args []string) {
	fs := flag.NewFlagSet("task note", flag.ContinueOnError)
	helped, perr := parseSubcommandFlags(fs, args, "task note",
		"Append an operator note to a task's annotation log.",
		[]flagDoc{{Name: "--help, -h", Desc: "show this help and exit"}},
		[]string{"force task note 42 useful context here"})
	if helped {
		return
	}
	if perr != nil {
		os.Exit(2)
	}
	rest := fs.Args()
	if len(rest) < 2 {
		fmt.Fprintln(os.Stderr, "Usage: force task note <id> <text>")
		os.Exit(1)
	}
	id := mustParseID(rest[0])
	note := strings.Join(rest[1:], " ")
	if err := store.AppendTaskNote(db, id, note); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Note added to task #%d\n", id)
}
