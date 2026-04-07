package main

import (
	"crypto/rand"
	"database/sql"
	"fmt"
	"os"
	"strings"

	igit "force-orchestrator/internal/git"
	"force-orchestrator/internal/agents"
	"force-orchestrator/internal/claude"
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
	const usageMsg = "Usage: force add [--priority N] [--plan-only] [--type Feature|CodeEdit|Investigate|Audit] [--repo <name>] [--idempotency-key KEY] <task description>"
	if len(args) == 0 {
		fmt.Println(usageMsg)
		os.Exit(1)
	}
	priority := 0
	planOnly := false
	taskType := ""
	repo := ""
	idempotencyKey := ""
	validTypes := map[string]bool{"Feature": true, "CodeEdit": true, "Investigate": true, "Audit": true, "WriteMemory": true, "MedicReview": true}
	addArgs := args
	for i := 0; i < len(addArgs); i++ {
		switch {
		case addArgs[i] == "--priority" && i+1 < len(addArgs):
			priority = mustParseID(addArgs[i+1])
			addArgs = append(addArgs[:i], addArgs[i+2:]...)
			i--
		case addArgs[i] == "--plan-only":
			planOnly = true
			addArgs = append(addArgs[:i], addArgs[i+1:]...)
			i--
		case addArgs[i] == "--type" && i+1 < len(addArgs):
			taskType = addArgs[i+1]
			if !validTypes[taskType] {
				fmt.Printf("Invalid type '%s'. Valid values: Feature, CodeEdit, Investigate, Audit\n", taskType)
				os.Exit(1)
			}
			addArgs = append(addArgs[:i], addArgs[i+2:]...)
			i--
		case addArgs[i] == "--repo" && i+1 < len(addArgs):
			repo = addArgs[i+1]
			addArgs = append(addArgs[:i], addArgs[i+2:]...)
			i--
		case addArgs[i] == "--idempotency-key" && i+1 < len(addArgs):
			idempotencyKey = addArgs[i+1]
			addArgs = append(addArgs[:i], addArgs[i+2:]...)
			i--
		}
	}
	if len(addArgs) == 0 {
		fmt.Println(usageMsg)
		os.Exit(1)
	}
	// CodeEdit tasks go directly to an astromech and require a known repo.
	if taskType == "CodeEdit" && repo == "" {
		fmt.Fprintf(os.Stderr, "error: --type CodeEdit requires --repo <name>\n  Usage: force add --type CodeEdit --repo <name> <description>\n  Or omit --type to let the classifier route it automatically.\n")
		os.Exit(1)
	}
	if repo != "" && store.GetRepoPath(db, repo) == "" {
		fmt.Fprintf(os.Stderr, "error: unknown repo '%s'. Register it first with: force add-repo\n", repo)
		os.Exit(1)
	}
	taskPayload := strings.Join(addArgs, " ")
	if planOnly {
		taskPayload = "[PLAN_ONLY]\n" + taskPayload
	}
	if idempotencyKey == "" {
		idempotencyKey = newUUID()
	}
	// When no type is specified, submit as Auto/Classifying so the UI is not blocked.
	// The Inquisitor will classify it asynchronously and transition it to Pending.
	if taskType == "" {
		id, err := store.AddBountyClassifying(db, repo, taskPayload, priority, idempotencyKey)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: failed to add task: %v\n", err)
			os.Exit(1)
		}
		planSuffix := ""
		if planOnly {
			planSuffix = " — Commander will plan only; approve with: force convoy approve <convoy-id>"
		}
		fmt.Printf("Queued as task #%d (classifying): '%s'%s\n", id, strings.Join(addArgs, " "), planSuffix)
		return
	}
	id := store.AddBounty(db, 0, taskType, taskPayload)
	if repo != "" {
		db.Exec(`UPDATE BountyBoard SET target_repo = ? WHERE id = ?`, repo, id)
	}
	if priority != 0 {
		store.SetBountyPriority(db, id, priority)
	}
	planSuffix := ""
	if planOnly {
		planSuffix = " — Commander will plan only; approve with: force convoy approve <convoy-id>"
	}
	fmt.Printf("Queued as task #%d: '%s'%s\n", id, strings.Join(addArgs, " "), planSuffix)
}

func cmdAddTask(db *sql.DB, args []string) {
	// Direct CodeEdit task, skips Commander decomposition
	// Usage: force add-task [--blocked-by <id>] [--convoy <id>] [--priority N] [--timeout <secs>] <repo> <description>
	blockedBy := 0
	convoyID := 0
	priority := 0
	taskTimeout := 0
	taskArgs := args
	for i := 0; i < len(taskArgs)-1; i++ {
		switch taskArgs[i] {
		case "--blocked-by":
			blockedBy = mustParseID(taskArgs[i+1])
			taskArgs = append(taskArgs[:i], taskArgs[i+2:]...)
			i--
		case "--convoy":
			convoyID = mustParseID(taskArgs[i+1])
			taskArgs = append(taskArgs[:i], taskArgs[i+2:]...)
			i--
		case "--priority":
			priority = mustParseID(taskArgs[i+1])
			taskArgs = append(taskArgs[:i], taskArgs[i+2:]...)
			i--
		case "--timeout":
			taskTimeout = mustParseID(taskArgs[i+1])
			taskArgs = append(taskArgs[:i], taskArgs[i+2:]...)
			i--
		}
	}
	if len(taskArgs) < 2 {
		fmt.Println("Usage: force add-task [--blocked-by <id>] [--convoy <id>] [--priority N] [--timeout <secs>] <repo> <description>")
		os.Exit(1)
	}
	repo := taskArgs[0]
	taskPayload := strings.Join(taskArgs[1:], " ")
	repoPath := store.GetRepoPath(db, repo)
	if repoPath == "" {
		fmt.Printf("Unknown repo '%s'. Register it first with: force add-repo\n", repo)
		os.Exit(1)
	}
	newID := store.AddCodeEditTask(db, repo, taskPayload, convoyID, priority, taskTimeout)
	if blockedBy > 0 {
		store.AddDependency(db, newID, blockedBy)
	}
	var suffix string
	if blockedBy > 0 {
		suffix += fmt.Sprintf(" (blocked by #%d)", blockedBy)
	}
	if convoyID > 0 {
		suffix += fmt.Sprintf(" (convoy %d)", convoyID)
	}
	if priority != 0 {
		suffix += fmt.Sprintf(" (priority %d)", priority)
	}
	if taskTimeout > 0 {
		suffix += fmt.Sprintf(" (timeout %ds)", taskTimeout)
	}
	fmt.Printf("CodeEdit task #%d queued for '%s'%s: %s\n", newID, repo, suffix, taskPayload)
}

func cmdAddInvestigate(db *sql.DB, args []string) {
	// Usage: force investigate [--priority N] [--repo <name>] <question>
	priority := 0
	repo := ""
	taskArgs := args
	for i := 0; i < len(taskArgs); i++ {
		switch taskArgs[i] {
		case "--priority":
			if i+1 < len(taskArgs) {
				priority = mustParseID(taskArgs[i+1])
				taskArgs = append(taskArgs[:i], taskArgs[i+2:]...)
				i--
			}
		case "--repo":
			if i+1 < len(taskArgs) {
				repo = taskArgs[i+1]
				taskArgs = append(taskArgs[:i], taskArgs[i+2:]...)
				i--
			}
		}
	}
	if len(taskArgs) == 0 {
		fmt.Println("Usage: force investigate [--priority N] [--repo <name>] <question>")
		os.Exit(1)
	}
	if repo != "" && store.GetRepoPath(db, repo) == "" {
		fmt.Printf("Unknown repo '%s'. Register it first with: force add-repo\n", repo)
		os.Exit(1)
	}
	payload := strings.Join(taskArgs, " ")
	res, _ := db.Exec(
		`INSERT INTO BountyBoard (target_repo, type, status, payload, priority, created_at)
		 VALUES (?, 'Investigate', 'Pending', ?, ?, datetime('now'))`,
		repo, payload, priority)
	id, _ := res.LastInsertId()
	repoSuffix := ""
	if repo != "" {
		repoSuffix = fmt.Sprintf(" (scoped to %s)", repo)
	}
	fmt.Printf("Investigation #%d queued%s: %s\n", id, repoSuffix, payload)
}

func cmdAddAudit(db *sql.DB, args []string) {
	// Usage: force audit [--priority N] [--repo <name>] <scope/question>
	priority := 0
	repo := ""
	taskArgs := args
	for i := 0; i < len(taskArgs); i++ {
		switch taskArgs[i] {
		case "--priority":
			if i+1 < len(taskArgs) {
				priority = mustParseID(taskArgs[i+1])
				taskArgs = append(taskArgs[:i], taskArgs[i+2:]...)
				i--
			}
		case "--repo":
			if i+1 < len(taskArgs) {
				repo = taskArgs[i+1]
				taskArgs = append(taskArgs[:i], taskArgs[i+2:]...)
				i--
			}
		}
	}
	if len(taskArgs) == 0 {
		fmt.Println("Usage: force audit [--priority N] [--repo <name>] <scope/question>")
		os.Exit(1)
	}
	if repo != "" && store.GetRepoPath(db, repo) == "" {
		fmt.Printf("Unknown repo '%s'. Register it first with: force add-repo\n", repo)
		os.Exit(1)
	}
	payload := strings.Join(taskArgs, " ")
	res, _ := db.Exec(
		`INSERT INTO BountyBoard (target_repo, type, status, payload, priority, created_at)
		 VALUES (?, 'Audit', 'Pending', ?, ?, datetime('now'))`,
		repo, payload, priority)
	id, _ := res.LastInsertId()
	repoSuffix := ""
	if repo != "" {
		repoSuffix = fmt.Sprintf(" (scoped to %s)", repo)
	}
	fmt.Printf("Audit #%d queued%s — findings will be Planned tasks awaiting your approval: %s\n", id, repoSuffix, payload)
}

func cmdAddJira(db *sql.DB, args []string) {
	// Usage: force add-jira [--priority N] [--plan-only] <TICKET-ID>
	priority := 0
	planOnly := false
	jiraArgs := args
	for i := 0; i < len(jiraArgs); i++ {
		if jiraArgs[i] == "--priority" && i+1 < len(jiraArgs) {
			priority = mustParseID(jiraArgs[i+1])
			jiraArgs = append(jiraArgs[:i], jiraArgs[i+2:]...)
			i--
		} else if jiraArgs[i] == "--plan-only" {
			planOnly = true
			jiraArgs = append(jiraArgs[:i], jiraArgs[i+1:]...)
			i--
		}
	}
	if len(jiraArgs) < 1 {
		fmt.Println("Usage: force add-jira [--priority N] [--plan-only] <TICKET-ID>")
		os.Exit(1)
	}
	ticketID := jiraArgs[0]
	fmt.Printf("Fetching Jira ticket %s...\n", ticketID)

	jiraSystemPrompt := `You are a product manager assistant. Use your Atlassian Jira MCP tools to fetch the requested ticket.
Return a comprehensive feature description as plain text including: ticket title, description, acceptance criteria, and any relevant context from linked tickets.
Do not use markdown formatting. Write it as a clear feature request that a software architect can decompose into coding tasks.`

	description, err := claude.AskClaudeCLI(jiraSystemPrompt,
		fmt.Sprintf("Fetch Jira ticket %s and return its full context as a feature description.", ticketID),
		claude.AtlassianReadTools, 5)
	if err != nil {
		fmt.Printf("Failed to fetch Jira ticket: %v\n", err)
		os.Exit(1)
	}

	payload := fmt.Sprintf("[JIRA: %s]\n%s", ticketID, strings.TrimSpace(description))
	if planOnly {
		payload = "[PLAN_ONLY]\n" + payload
	}
	id := store.AddBounty(db, 0, "Feature", payload)
	if priority != 0 {
		store.SetBountyPriority(db, id, priority)
	}
	planSuffix := ""
	if planOnly {
		planSuffix = " — Commander will plan only; approve with: force convoy approve <convoy-id>"
	}
	fmt.Printf("Jira ticket %s added to the Fleet%s.\n", ticketID, planSuffix)
}

// cmdReset handles both "reset" and "retry" (identical behavior).
func cmdReset(db *sql.DB, id int, via string) {
	store.ResetTask(db, id)
	store.LogAudit(db, "operator", "reset", id, via)
	fmt.Printf("Task %d reset to Pending.\n", id)
}

func cmdCancel(db *sql.DB, args []string) {
	if len(args) == 0 {
		fmt.Println("Usage: force cancel <task-id> [--requeue <type>]")
		os.Exit(1)
	}

	requeueType := ""
	cancelArgs := args
	for i := 0; i < len(cancelArgs); i++ {
		if cancelArgs[i] == "--requeue" && i+1 < len(cancelArgs) {
			requeueType = cancelArgs[i+1]
			cancelArgs = append(cancelArgs[:i], cancelArgs[i+2:]...)
			i--
		}
	}

	if len(cancelArgs) == 0 {
		fmt.Println("Usage: force cancel <task-id> [--requeue <type>]")
		os.Exit(1)
	}

	id := mustParseID(cancelArgs[0])

	if requeueType != "" {
		validTypes := map[string]bool{"Feature": true, "CodeEdit": true, "Investigate": true, "Audit": true, "WriteMemory": true, "MedicReview": true}
		if !validTypes[requeueType] {
			fmt.Printf("Invalid requeue type %q — must be one of: Feature, CodeEdit, Investigate, Audit, WriteMemory\n", requeueType)
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

	if requeueType != "" {
		var payload string
		db.QueryRow(`SELECT payload FROM BountyBoard WHERE id = ?`, id).Scan(&payload)
		newID := store.AddBounty(db, 0, requeueType, payload)
		fmt.Printf("Task #%d cancelled — re-queued as %s #%d\n", id, requeueType, newID)
	} else {
		fmt.Printf("Task %d cancelled.\n", id)
	}
}

func cmdBlock(db *sql.DB, taskID, blockerID int) {
	var count int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE id IN (?, ?)`, taskID, blockerID).Scan(&count)
	if count != 2 {
		fmt.Printf("One or both tasks not found (task %d, blocker %d).\n", taskID, blockerID)
		os.Exit(1)
	}
	store.AddDependency(db, taskID, blockerID)
	fmt.Printf("Task %d is now blocked by task %d.\n", taskID, blockerID)
}

func cmdUnblock(db *sql.DB, id int) {
	var taskExists int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE id = ?`, id).Scan(&taskExists)
	if taskExists == 0 {
		fmt.Printf("Task %d not found.\n", id)
	} else {
		store.RemoveDependenciesOf(db, id)
		fmt.Printf("Task %d unblocked (all dependencies removed).\n", id)
	}
}

func cmdUnblockDependents(db *sql.DB, id int) {
	count := store.UnblockDependentsOf(db, id)
	if count == 0 {
		fmt.Printf("No tasks were depending on #%d.\n", id)
	} else {
		fmt.Printf("Removed %d dependency edge(s) pointing to #%d.\n", count, id)
	}
}

func cmdTree(db *sql.DB, id int) {
	printTree(db, id, 0)
}

func cmdDiff(db *sql.DB, id int) {
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
	diff := igit.GetDiff(repoPath, b.BranchName)
	if diff == "" {
		fmt.Printf("No diff found for branch %s — branch may not have any commits yet\n", b.BranchName)
	} else {
		fmt.Printf("Branch: %s\n\n", b.BranchName)
		fmt.Println(diff)
	}
}

// cmdApproveTask handles operator manual task approval (NOT convoy approve).
func cmdApproveTask(db *sql.DB, id int) {
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
	diff := igit.GetDiff(repoPath, branchName)
	if mergeErr := igit.MergeAndCleanup(repoPath, branchName, worktreeDir); mergeErr != nil {
		fmt.Printf("Merge failed: %v\n", mergeErr)
		os.Exit(1)
	}
	store.UpdateBountyStatus(db, id, "Completed")
	store.UnblockDependentsOf(db, id)
	if diff != "" {
		changedFiles := igit.ExtractDiffFiles(diff)
		filesStr := strings.Join(changedFiles, ", ")
		store.StoreFleetMemory(db, b.TargetRepo, b.ID, "success",
			fmt.Sprintf("Task: %s", truncate(b.Payload, 400)), filesStr)
	}
	telemetry.EmitEvent(telemetry.TelemetryEvent{
		EventType: "operator_approved",
		Payload:   map[string]any{"task_id": id},
	})
	store.LogAudit(db, "operator", "approve", id, "manually approved and merged")
	fmt.Printf("Task %d approved and merged by operator.\n", id)
}

// cmdRejectTask handles operator reject.
func cmdRejectTask(db *sql.DB, id int, reason string) {
	b, err := store.GetBounty(db, id)
	if err != nil {
		fmt.Printf("Task %d not found\n", id)
		os.Exit(1)
	}
	retryCount := store.IncrementRetryCount(db, id)
	if retryCount >= agents.MaxRetries {
		store.FailBounty(db, id, fmt.Sprintf("Operator rejected (final): %s", reason))
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

func cmdPrioritize(db *sql.DB, taskID, prio int) {
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

func cmdRetryAllFailed(db *sql.DB) {
	n := store.ResetAllFailed(db)
	store.LogAudit(db, "operator", "retry-all-failed", 0, fmt.Sprintf("reset %d failed tasks", n))
	fmt.Printf("Reset %d failed task(s) to Pending.\n", n)
}

func cmdTaskNote(db *sql.DB, id int, note string) {
	if err := store.AppendTaskNote(db, id, note); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Note added to task #%d\n", id)
}
