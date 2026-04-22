package main

import (
	"database/sql"
	"fmt"
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
func cmdConvoy(db *sql.DB, args []string) {
	subCmd := ""
	if len(args) >= 1 {
		subCmd = args[0]
	}
	switch subCmd {
	case "list", "":
		printConvoys(db)
	case "create":
		if len(args) < 2 {
			fmt.Println("Usage: force convoy create <name>")
			os.Exit(1)
		}
		name := strings.Join(args[1:], " ")
		id, err := store.CreateConvoy(db, name)
		if err != nil {
			fmt.Printf("Failed to create convoy: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Convoy '%s' created (id: %d).\n", name, id)
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
		// Transition all Planned tasks in a convoy to Pending so agents can claim them.
		if len(args) < 2 {
			fmt.Println("Usage: force convoy approve <id>")
			os.Exit(1)
		}
		convoyApproveID := mustParseID(args[1])
		// Preview which tasks will be activated before committing
		previewRows, prevErr := db.Query(`SELECT id, target_repo, payload FROM BountyBoard WHERE convoy_id = ? AND status = 'Planned' ORDER BY id ASC`, convoyApproveID)
		if prevErr == nil {
			var previewTasks []string
			for previewRows.Next() {
				var pid int
				var previewRepo, previewPayload string
				previewRows.Scan(&pid, &previewRepo, &previewPayload)
				firstLine := previewPayload
				if nl := strings.Index(previewPayload, "\n"); nl != -1 {
					firstLine = previewPayload[:nl]
				}
				previewTasks = append(previewTasks, fmt.Sprintf("  #%d [%s] %s", pid, previewRepo, truncate(firstLine, 70)))
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
	case "reset":
		// Reset all Failed/Escalated tasks in a convoy back to Pending
		if len(args) < 2 {
			fmt.Println("Usage: force convoy reset <id>")
			os.Exit(1)
		}
		convoyID := mustParseID(args[1])
		n := store.ResetConvoyTasks(db, convoyID)
		if n == 0 {
			fmt.Printf("No failed/escalated tasks in convoy %d.\n", convoyID)
		} else {
			store.LogAudit(db, "operator", "convoy-reset", convoyID, fmt.Sprintf("reset %d tasks in convoy", n))
			fmt.Printf("Reset %d task(s) in convoy %d to Pending.\n", n, convoyID)
		}
	case "reject":
		// Reject the plan for a convoy: cancel its un-started tasks, send feedback
		// mail to Commander, and requeue the parent Feature task so Commander re-plans.
		// After maxConvoyRejections, the Feature task is permanently failed instead.
		if len(args) < 3 {
			fmt.Println("Usage: force convoy reject <id> <feedback>")
			os.Exit(1)
		}
		rejectConvoyID := mustParseID(args[1])
		rejectFeedback := strings.Join(args[2:], " ")

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
				lockedRows.Scan(&lid, &lowner, &lrepo)
				locked = append(locked, fmt.Sprintf("  #%d [%s] owned by %s", lid, lrepo, lowner))
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
	case "pr":
		// force convoy pr <id> — show draft PR URL + state per repo
		if len(args) < 2 {
			fmt.Println("Usage: force convoy pr <id>")
			os.Exit(1)
		}
		cmdConvoyPR(db, mustParseID(args[1]))
	case "ship":
		// force convoy ship <id> [--merge <strategy>] — remove draft, optionally merge
		if len(args) < 2 {
			fmt.Println("Usage: force convoy ship <id> [--merge squash|merge|rebase]")
			os.Exit(1)
		}
		convoyShipID := mustParseID(args[1])
		mergeStrategy := ""
		for i := 2; i < len(args); i++ {
			if args[i] == "--merge" && i+1 < len(args) {
				mergeStrategy = args[i+1]
				i++
			}
		}
		cmdConvoyShip(db, convoyShipID, mergeStrategy)
	case "pr-review":
		// force convoy pr-review <id> — print the PR review-comment table
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
