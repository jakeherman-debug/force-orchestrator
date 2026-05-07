package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"force-orchestrator/internal/store"
)

func printConvoys(db *sql.DB) {
	convoys := store.ListConvoys(db)
	if len(convoys) == 0 {
		fmt.Println("No convoys found.")
		return
	}
	fmt.Printf("%-4s %-30s %-12s %-10s %-20s\n", "ID", "NAME", "STATUS", "PROGRESS", "CREATED")
	fmt.Println(strings.Repeat("-", 100))
	for _, c := range convoys {
		completed, total := store.ConvoyProgress(db, c.ID)
		fmt.Printf("%-4d %-30s %-12s %-10s %-20s\n", c.ID, c.Name, c.Status, fmt.Sprintf("%d/%d", completed, total), c.CreatedAt)
	}
}

// cmdConvoy handles the `force convoy` subcommands.
//
// Read-only verbs (list, show, pr, pr-review) keep their inline switch-case
// bodies — they print to stdout and have no destructive side-effect that
// could fire if a stray --bogus-flag slipped through.
//
// Destructive verbs (create, approve, reset, reject, ship) are extracted into
// per-verb cmdConvoy<Verb> handlers that route through parseSubcommandFlags
// so --help short-circuits BEFORE any DB mutation and unknown flags are
// rejected with a non-zero exit BEFORE any side effect. See
// fix(cli)/cli-destructive-verbs and Pattern P_CLIFlagParsing for the
// regression contract.
func cmdConvoy(db *sql.DB, args []string) {
	subCmd := ""
	if len(args) >= 1 {
		subCmd = args[0]
	}
	switch subCmd {
	case "--help", "-h", "help":
		fmt.Println("Usage: force convoy [list|create <name>|show <name>|approve <id>|reset <id>|reject <id> <feedback>|review <id>|pr <id>|ship <id>]")
		return
	case "list", "":
		printConvoys(db)
	case "create":
		cmdConvoyCreate(db, args[1:])
	case "show":
		if len(args) < 2 {
			fmt.Println("Usage: force convoy show <name>")
			os.Exit(1)
		}
		convoyName := strings.Join(args[1:], " ")
		var convoy store.Convoy
		err := db.QueryRow(`SELECT id, name, status, created_at FROM Convoys WHERE name = ?`, convoyName).
			Scan(&convoy.ID, &convoy.Name, &convoy.Status, &convoy.CreatedAt)
		if err != nil {
			fmt.Printf("No convoy found with name: %s\n", convoyName)
			os.Exit(1)
		}
		completed, total := store.ConvoyProgress(db, convoy.ID)
		printConvoyShow(db, convoy.ID, convoy.Name, convoy.Status, completed, total)
	case "approve":
		cmdConvoyApprove(db, args[1:])
	case "reset":
		cmdConvoyReset(db, args[1:])
	case "reject":
		cmdConvoyReject(db, args[1:])
	case "pr":
		// force convoy pr <id> — read-only PR-state print.
		if len(args) < 2 {
			fmt.Println("Usage: force convoy pr <id>")
			os.Exit(1)
		}
		cmdConvoyPR(db, mustParseID(args[1]))
	case "ship":
		cmdConvoyShipCLI(db, args[1:])
	case "pr-review":
		// force convoy pr-review <id> — read-only PR review-comment table.
		if len(args) < 2 {
			fmt.Println("Usage: force convoy pr-review <id>")
			os.Exit(1)
		}
		cmdConvoyPRReview(db, mustParseID(args[1]))
	default:
		fmt.Printf("Unknown convoy subcommand: %s\n", subCmd)
		fmt.Println("Usage: force convoy [list|create <name>|show <name>|approve <id>|reset <id>|reject <id> <feedback>|pr <id>|ship <id>|pr-review <id>]")
		os.Exit(1)
	}
}

// cmdConvoyCreate — `force convoy create <name>`. DESTRUCTIVE: writes a new
// row to the Convoys table.
func cmdConvoyCreate(db *sql.DB, args []string) {
	fs := flag.NewFlagSet("convoy create", flag.ContinueOnError)
	helped, perr := parseSubcommandFlags(fs, args, "convoy create",
		"Create a new convoy with the given name.",
		[]flagDoc{{Name: "--help, -h", Desc: "show this help and exit"}},
		[]string{"force convoy create my-feature"})
	if helped {
		return
	}
	if perr != nil {
		os.Exit(2)
	}
	rest := fs.Args()
	if len(rest) < 1 {
		fmt.Println("Usage: force convoy create <name>")
		os.Exit(1)
	}
	name := strings.Join(rest, " ")
	id, err := store.CreateConvoy(db, name)
	if err != nil {
		fmt.Printf("Failed to create convoy: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Convoy '%s' created (id: %d).\n", name, id)
}

// cmdConvoyApprove — `force convoy approve <id>`. DESTRUCTIVE: transitions
// every Planned task in the convoy to Pending and writes an AuditLog row.
func cmdConvoyApprove(db *sql.DB, args []string) {
	fs := flag.NewFlagSet("convoy approve", flag.ContinueOnError)
	helped, perr := parseSubcommandFlags(fs, args, "convoy approve",
		"Transition all Planned tasks in a convoy to Pending so agents can claim them.",
		[]flagDoc{{Name: "--help, -h", Desc: "show this help and exit"}},
		[]string{"force convoy approve 17"})
	if helped {
		return
	}
	if perr != nil {
		os.Exit(2)
	}
	rest := fs.Args()
	if len(rest) < 1 {
		fmt.Println("Usage: force convoy approve <id>")
		os.Exit(1)
	}
	convoyApproveID := mustParseID(rest[0])
	// Preview which tasks will be activated before committing
	previewRows, prevErr := db.Query(`SELECT id, target_repo, payload FROM BountyBoard WHERE convoy_id = ? AND status = 'Planned' ORDER BY id ASC`, convoyApproveID)
	if prevErr == nil {
		var previewTasks []string
		for previewRows.Next() {
			var pid int
			var previewRepo, previewPayload string
			if err := previewRows.Scan(&pid, &previewRepo, &previewPayload); err != nil {
				fmt.Fprintf(os.Stderr, "warn: scan failed: %v\n", err)
				continue
			}
			firstLine := previewPayload
			if nl := strings.Index(previewPayload, "\n"); nl != -1 {
				firstLine = previewPayload[:nl]
			}
			previewTasks = append(previewTasks, fmt.Sprintf("  #%d [%s] %s", pid, previewRepo, truncate(firstLine, 70)))
		}
		if rErr := previewRows.Err(); rErr != nil {
			log.Printf("convoy.go:cmdConvoyApprove: rows iter error: %v", rErr)
		}
		previewRows.Close()
		if len(previewTasks) == 0 {
			fmt.Printf("No Planned tasks in convoy %d (already approved or wrong convoy).\n", convoyApproveID)
			os.Exit(0)
		}
		fmt.Printf("Approving %d task(s) in convoy %d:\n%s\n",
			len(previewTasks), convoyApproveID, strings.Join(previewTasks, "\n"))
	}
	n := store.ApproveConvoyTasks(db, convoyApproveID)
	store.LogAudit(db, "operator", "convoy-approve", convoyApproveID, fmt.Sprintf("approved %d planned tasks", n))
	fmt.Printf("Convoy %d approved: %d task(s) moved from Planned to Pending.\n", convoyApproveID, n)
}

// cmdConvoyReset — `force convoy reset <id>`. DESTRUCTIVE: returns every
// Failed/Escalated task in the convoy back to Pending and writes an
// AuditLog row.
func cmdConvoyReset(db *sql.DB, args []string) {
	fs := flag.NewFlagSet("convoy reset", flag.ContinueOnError)
	helped, perr := parseSubcommandFlags(fs, args, "convoy reset",
		"Reset all Failed/Escalated tasks in a convoy back to Pending.",
		[]flagDoc{{Name: "--help, -h", Desc: "show this help and exit"}},
		[]string{"force convoy reset 17"})
	if helped {
		return
	}
	if perr != nil {
		os.Exit(2)
	}
	rest := fs.Args()
	if len(rest) < 1 {
		fmt.Println("Usage: force convoy reset <id>")
		os.Exit(1)
	}
	convoyID := mustParseID(rest[0])
	n := store.ResetConvoyTasks(db, convoyID)
	if n == 0 {
		fmt.Printf("No failed/escalated tasks in convoy %d.\n", convoyID)
	} else {
		store.LogAudit(db, "operator", "convoy-reset", convoyID, fmt.Sprintf("reset %d tasks in convoy", n))
		fmt.Printf("Reset %d task(s) in convoy %d to Pending.\n", n, convoyID)
	}
}

// cmdConvoyReject — `force convoy reject <id> <feedback>`. DESTRUCTIVE:
// cancels the convoy's un-started tasks, sends Commander a feedback mail,
// and either re-queues or permanently fails the parent Feature task. After
// maxConvoyRejections, the Feature is permanently failed instead.
func cmdConvoyReject(db *sql.DB, args []string) {
	fs := flag.NewFlagSet("convoy reject", flag.ContinueOnError)
	helped, perr := parseSubcommandFlags(fs, args, "convoy reject",
		"Reject the plan for a convoy: cancel its un-started tasks, send feedback to Commander, and re-queue (or, after 3 rejections, permanently fail) the parent Feature.",
		[]flagDoc{{Name: "--help, -h", Desc: "show this help and exit"}},
		[]string{"force convoy reject 17 'plan misses the rate-limit edge case'"})
	if helped {
		return
	}
	if perr != nil {
		os.Exit(2)
	}
	rest := fs.Args()
	if len(rest) < 2 {
		fmt.Println("Usage: force convoy reject <id> <feedback>")
		os.Exit(1)
	}
	rejectConvoyID := mustParseID(rest[0])
	rejectFeedback := strings.Join(rest[1:], " ")

	// Find the parent Feature task — Commander set parent_id on every subtask
	var rejectParentID int
	db.QueryRow(`SELECT DISTINCT parent_id FROM BountyBoard WHERE convoy_id = ? AND parent_id > 0 LIMIT 1`,
		rejectConvoyID).Scan(&rejectParentID)
	if rejectParentID == 0 {
		fmt.Printf("No parent Feature task found for convoy %d — cannot reject.\n", rejectConvoyID)
		os.Exit(1)
	}

	// Warn about any tasks currently locked/under-review — they will continue running
	// and may still try to merge after the plan is rejected.
	lockedRows, _ := db.Query(`
		SELECT id, owner, target_repo FROM BountyBoard
		WHERE convoy_id = ? AND status IN ('Locked', 'UnderReview', 'UnderCaptainReview', 'AwaitingCaptainReview', 'AwaitingCouncilReview')
		ORDER BY id ASC`, rejectConvoyID)
	if lockedRows != nil {
		var locked []string
		for lockedRows.Next() {
			var lid int
			var lowner, lrepo string
			if err := lockedRows.Scan(&lid, &lowner, &lrepo); err != nil {
				fmt.Fprintf(os.Stderr, "warn: scan failed: %v\n", err)
				continue
			}
			locked = append(locked, fmt.Sprintf("  #%d [%s] owned by %s", lid, lrepo, lowner))
		}
		if rErr := lockedRows.Err(); rErr != nil {
			log.Printf("convoy.go:cmdConvoyReject: rows iter error: %v", rErr)
		}
		lockedRows.Close()
		if len(locked) > 0 {
			fmt.Printf("Warning: %d task(s) are currently in-flight and will NOT be cancelled:\n%s\n"+
				"These may still complete or merge independently.\n\n",
				len(locked), strings.Join(locked, "\n"))
		}
	}

	// Cancel only Planned/Pending tasks — don't touch tasks already running or done
	cancelled := store.CancelConvoyPendingTasks(db, rejectConvoyID)

	store.SendMail(db, "operator", "commander",
		fmt.Sprintf("[PLAN REJECTED] Convoy #%d — please re-plan", rejectConvoyID),
		fmt.Sprintf("The operator has rejected the plan for convoy #%d.\n\nFeedback:\n%s\n\nPlease decompose the original feature request again, incorporating this feedback.",
			rejectConvoyID, rejectFeedback),
		rejectParentID, store.MailTypeDirective)

	// Guard against infinite re-planning: count how many convoys have been
	// created for this Feature task (each rejection spawns a new one).
	const maxConvoyRejections = 3
	var priorConvoys int
	db.QueryRow(`SELECT COUNT(*) FROM Convoys WHERE id IN (
		SELECT DISTINCT convoy_id FROM BountyBoard WHERE parent_id = ? AND convoy_id > 0
	)`, rejectParentID).Scan(&priorConvoys)

	if priorConvoys >= maxConvoyRejections {
		db.Exec(`UPDATE BountyBoard SET status = 'Failed', owner = '', locked_at = '',
			error_log = ? WHERE id = ?`,
			fmt.Sprintf("Feature permanently failed after %d plan rejections. Final feedback: %s",
				priorConvoys, rejectFeedback),
			rejectParentID)
		store.LogAudit(db, "operator", "convoy-reject-final", rejectConvoyID,
			fmt.Sprintf("feature #%d permanently failed after %d rejections: %s",
				rejectParentID, priorConvoys, rejectFeedback))
		fmt.Printf("Feature #%d has been rejected %d time(s) and is now permanently failed.\n",
			rejectParentID, priorConvoys)
		return
	}

	// Reset the parent Feature task to Pending so Commander re-plans.
	// Preserve retry_count so infra-failure circuit-breaker state is not wiped.
	db.Exec(`UPDATE BountyBoard SET status = 'Pending', owner = '', locked_at = '', infra_failures = 0, error_log = '' WHERE id = ?`,
		rejectParentID)

	store.LogAudit(db, "operator", "convoy-reject", rejectConvoyID, rejectFeedback)
	fmt.Printf("Convoy %d rejected: %d task(s) cancelled, Feature #%d re-queued for Commander.\n",
		rejectConvoyID, cancelled, rejectParentID)
	fmt.Printf("Feedback sent to Commander: %s\n", rejectFeedback)
}

// cmdConvoyShipCLI — `force convoy ship <id> [--merge squash|merge|rebase]`.
// DESTRUCTIVE: flips draft PRs to ready-for-review and optionally merges
// them. Wraps the lower-level cmdConvoyShip helper in convoy_pr.go.
//
// The wrapper is named with the CLI suffix to avoid shadowing the existing
// cmdConvoyShip(db, convoyID, mergeStrategy) helper that several tests
// invoke directly.
func cmdConvoyShipCLI(db *sql.DB, args []string) {
	fs := flag.NewFlagSet("convoy ship", flag.ContinueOnError)
	mergeFlag := fs.String("merge", "", "merge strategy: squash | merge | rebase")
	helped, perr := parseSubcommandFlags(fs, args, "convoy ship",
		"Promote each of the convoy's draft PRs to ready-for-review, and optionally merge them.",
		[]flagDoc{
			{Name: "--merge S", Desc: "merge strategy: squash | merge | rebase (omit to only un-draft)"},
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{"force convoy ship 17", "force convoy ship 17 --merge squash"})
	if helped {
		return
	}
	if perr != nil {
		os.Exit(2)
	}
	rest := fs.Args()
	if len(rest) < 1 {
		fmt.Println("Usage: force convoy ship <id> [--merge squash|merge|rebase]")
		os.Exit(1)
	}
	convoyShipID := mustParseID(rest[0])
	// Legacy positional shape: support --merge as a positional pair too
	// (for back-compat with any operator muscle-memory). The flag form
	// takes precedence.
	mergeStrategy := *mergeFlag
	if mergeStrategy == "" {
		for i := 1; i < len(rest); i++ {
			if rest[i] == "--merge" && i+1 < len(rest) {
				mergeStrategy = rest[i+1]
				break
			}
		}
	}
	cmdConvoyShip(db, convoyShipID, mergeStrategy)
}
