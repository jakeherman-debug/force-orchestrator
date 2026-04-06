package agents

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"force-orchestrator/internal/claude"
	igit "force-orchestrator/internal/git"
	"force-orchestrator/internal/store"
	"force-orchestrator/internal/telemetry"
)

const staleLockTimeout = 45 * time.Minute // locked this long (regardless of commits) → hard reset
const stallEscTimeout  = 30 * time.Minute // locked this long with no commits → Boot triage + escalate
const inquisitorInterval = 5 * time.Minute

// bootCallCooldown prevents the Boot triage LLM from being called repeatedly on
// the same stalled task every inquisitor cycle.
const bootCallCooldown = 30 * time.Minute

// bootLastCalled tracks the most recent Boot triage invocation per task ID.
var bootLastCalled = map[int]time.Time{}

// SpawnInquisitor periodically hunts the fleet for problems.
func SpawnInquisitor(db *sql.DB) {
	logger := NewLogger("Inquisitor")
	logger.Printf("Inquisitor deployed — hunting every %v, stale threshold %v", inquisitorInterval, staleLockTimeout)
	validateWorktrees(db, logger)

	for {
		time.Sleep(inquisitorInterval)

		rows, err := db.Query(`
			SELECT id FROM BountyBoard
			WHERE status IN ('Locked', 'UnderReview', 'UnderCaptainReview')
			  AND locked_at != ''
			  AND locked_at < datetime('now', ?)
		`, "-"+staleLockTimeout.String())

		var staleIDs []int
		if err == nil {
			for rows.Next() {
				var id int
				rows.Scan(&id)
				staleIDs = append(staleIDs, id)
			}
			rows.Close()
		}

		if len(staleIDs) > 0 {
			// Also clear infra_failures: a stale-lock reset almost always means the host
			// went to sleep and killed the Claude process, not a genuine infra issue.
			// Leaving infra_failures elevated would push the task toward permanent failure
			// for what is a laptop availability event, not a code or environment problem.
			_, resetErr := db.Exec(`
				UPDATE BountyBoard
				SET status = 'Pending', owner = '', locked_at = '',
				    infra_failures = 0,
				    error_log = 'Inquisitor: reset after stale lock timeout (infra_failures cleared)'
				WHERE status IN ('Locked', 'UnderReview', 'UnderCaptainReview')
				  AND locked_at != ''
				  AND locked_at < datetime('now', ?)
			`, "-"+staleLockTimeout.String())

			if resetErr != nil {
				logger.Printf("ERROR resetting stale tasks: %v", resetErr)
			} else {
				logger.Printf("Hunted down %d stale task(s) — returned to Pending", len(staleIDs))
				telemetry.EmitEvent(telemetry.EventInquisitorReset(staleIDs))
				for _, id := range staleIDs {
					delete(bootLastCalled, id)
				}
			}
		}

		classifyPendingTasks(db, logger)
		CheckStaleEscalations(db)
		CheckConvoyCompletions(db, logger)
		cleanOrphanedBranches(db, logger)
		detectStalledTasks(db, logger)

		// Prune bootLastCalled entries for tasks that are no longer in a locked state
		// (completed, failed, reset, or escalated since the last cycle). This prevents
		// unbounded growth of the map across long-running daemons.
		for taskID := range bootLastCalled {
			var status string
			db.QueryRow(`SELECT IFNULL(status,'') FROM BountyBoard WHERE id = ?`, taskID).Scan(&status)
			if status != "Locked" && status != "UnderCaptainReview" && status != "UnderReview" {
				delete(bootLastCalled, taskID)
			}
		}

		RunDogs(db, logger)

		db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	}
}

// classifyPendingTasks finds tasks with status='Classifying', calls ClassifyTaskType
// for each, updates the task type to the result, and transitions the task to Pending.
func classifyPendingTasks(db *sql.DB, logger interface{ Printf(string, ...any) }) {
	rows, err := db.Query(`SELECT id, payload FROM BountyBoard WHERE status = 'Classifying'`)
	if err != nil {
		return
	}
	type classTask struct {
		id      int
		payload string
	}
	var tasks []classTask
	for rows.Next() {
		var t classTask
		rows.Scan(&t.id, &t.payload)
		tasks = append(tasks, t)
	}
	rows.Close()

	for _, t := range tasks {
		taskType, reason, err := claude.ClassifyTaskType(t.payload)
		if err != nil {
			logger.Printf("Task #%d: classification error — %v", t.id, err)
			continue
		}
		db.Exec(`UPDATE BountyBoard SET type = ?, status = 'Pending' WHERE id = ?`, taskType, t.id)
		logger.Printf("Task #%d classified as %s — %s", t.id, taskType, reason)
	}
}

// detectStalledTasks finds tasks that are Locked/UnderReview for too long with
// no new commits in their worktree.
func detectStalledTasks(db *sql.DB, logger interface{ Printf(string, ...any) }) {
	rows, err := db.Query(`
		SELECT id, owner, target_repo, branch_name, locked_at,
		       (julianday('now') - julianday(locked_at)) * 1440 AS locked_minutes
		FROM BountyBoard
		WHERE status IN ('Locked', 'UnderReview', 'UnderCaptainReview')
		  AND locked_at != ''
		  AND locked_at < datetime('now', ?)
	`, fmt.Sprintf("-%d seconds", int(StallWarnTimeout.Seconds())))
	if err != nil {
		return
	}

	type stalledTask struct {
		id            int
		owner         string
		repo          string
		branchName    string
		lockedAt      string
		lockedMinutes float64
	}
	var tasks []stalledTask
	for rows.Next() {
		var t stalledTask
		rows.Scan(&t.id, &t.owner, &t.repo, &t.branchName, &t.lockedAt, &t.lockedMinutes)
		tasks = append(tasks, t)
	}
	rows.Close()

	for _, t := range tasks {
		repoPath := store.GetRepoPath(db, t.repo)
		if repoPath == "" {
			continue
		}
		worktreePath := igit.ResolveWorktreeDir(db, t.branchName, repoPath, t.id, BranchAgentName)

		lockedAtTime, parseErr := time.Parse("2006-01-02 15:04:05", strings.TrimSpace(t.lockedAt))
		if parseErr != nil {
			continue
		}
		commitsSince, gitErr := exec.Command(
			"git", "-C", worktreePath, "log", "--oneline",
			"--since="+lockedAtTime.UTC().Format(time.RFC3339), "HEAD",
		).Output()
		if gitErr != nil || len(strings.TrimSpace(string(commitsSince))) > 0 {
			continue
		}

		logger.Printf("Task %d: STALL detected — %s locked %.0f min with no commits in %s",
			t.id, t.owner, t.lockedMinutes, worktreePath)
		telemetry.EmitEvent(telemetry.EventStallDetected(t.id, t.owner, t.repo, t.lockedMinutes))

		if t.lockedMinutes >= stallEscTimeout.Minutes() {
			if lastBoot, seen := bootLastCalled[t.id]; seen && time.Since(lastBoot) < bootCallCooldown {
				logger.Printf("Task %d: Boot triage throttled (last called %v ago)", t.id, time.Since(lastBoot).Round(time.Minute))
				continue
			}
			bootLastCalled[t.id] = time.Now()
			var errorLog string
			db.QueryRow(`SELECT IFNULL(error_log,'') FROM BountyBoard WHERE id = ?`, t.id).Scan(&errorLog)
			verdict := BootTriage(db, t.id, t.owner, t.repo, t.lockedMinutes, errorLog)
			logger.Printf("Task %d: Boot triage → %s (%s)", t.id, verdict.Decision, verdict.Reason)

			switch verdict.Decision {
			case BootReset:
				db.Exec(`UPDATE BountyBoard SET status = 'Pending', owner = '', locked_at = '',
					error_log = ? WHERE id = ?`,
					fmt.Sprintf("Boot agent reset: %s", verdict.Reason), t.id)
				logger.Printf("Task %d: Boot agent ordered RESET — returned to Pending", t.id)
				store.LogAudit(db, "boot-agent", "reset", t.id, verdict.Reason)
			case BootEscalate:
				logger.Printf("Task %d: Boot agent ordered ESCALATE — escalating at LOW severity", t.id)
				CreateEscalation(db, t.id, store.SeverityLow,
					fmt.Sprintf("Boot agent: %s (locked %.0f min, no commits)", verdict.Reason, t.lockedMinutes))
				store.LogAudit(db, "boot-agent", "escalate", t.id, verdict.Reason)
				store.SendMail(db, "inquisitor", "operator",
					fmt.Sprintf("[STALL ESCALATED] Task #%d — %s", t.id, t.repo),
					fmt.Sprintf("Boot agent escalated task #%d.\n\nAgent: %s\nRepo: %s\nLocked: %.0f minutes with no commits\nReason: %s\n\nTask requires human review.",
						t.id, t.owner, t.repo, t.lockedMinutes, verdict.Reason),
					t.id, store.MailTypeAlert)
			case BootIgnore:
				logger.Printf("Task %d: Boot agent says IGNORE — agent may still be working", t.id)
			default: // BootWarn
				logger.Printf("Task %d: Boot agent says WARN — stall noted but not acting", t.id)
				CreateEscalation(db, t.id, store.SeverityLow,
					fmt.Sprintf("Agent %s locked %.0f min with no commits — possible stall", t.owner, t.lockedMinutes))
			}
		}
	}
}

// validateWorktrees checks every persisted Agents entry at startup.
func validateWorktrees(db *sql.DB, logger interface{ Printf(string, ...any) }) {
	rows, err := db.Query(`SELECT agent_name, repo, worktree_path FROM Agents`)
	if err != nil {
		return
	}

	type entry struct{ agent, repo, path string }
	var stale []entry
	total := 0
	for rows.Next() {
		total++
		var e entry
		rows.Scan(&e.agent, &e.repo, &e.path)
		if _, statErr := os.Stat(e.path); statErr != nil {
			stale = append(stale, e)
		}
	}
	rows.Close()

	for _, e := range stale {
		logger.Printf("Worktree validation: %s/%s path missing (%s) — removing record", e.agent, e.repo, e.path)
		db.Exec(`DELETE FROM Agents WHERE agent_name = ? AND repo = ?`, e.agent, e.repo)
		res, _ := db.Exec(`
			UPDATE BountyBoard SET status = 'Pending', owner = '', locked_at = '',
			  error_log = 'Inquisitor: worktree missing at startup, reset to Pending'
			WHERE owner = ? AND status IN ('Locked', 'UnderReview', 'UnderCaptainReview')`, e.agent)
		if n, _ := res.RowsAffected(); n > 0 {
			logger.Printf("Worktree validation: reset %d task(s) owned by %s", n, e.agent)
		}
	}
	if len(stale) == 0 {
		logger.Printf("Worktree validation: all %d worktree(s) present", total)
	}
}

// cleanOrphanedBranches deletes git branches for tasks that are permanently done.
func cleanOrphanedBranches(db *sql.DB, logger interface{ Printf(string, ...any) }) {
	rows, err := db.Query(`
		SELECT id, target_repo, branch_name FROM BountyBoard
		WHERE status IN ('Failed', 'Escalated')
		  AND branch_name != ''`)
	if err != nil {
		return
	}

	type orphanedBranch struct {
		id         int
		repo       string
		branchName string
	}
	var branches []orphanedBranch
	for rows.Next() {
		var b orphanedBranch
		rows.Scan(&b.id, &b.repo, &b.branchName)
		branches = append(branches, b)
	}
	rows.Close()

	for _, b := range branches {
		repoPath := store.GetRepoPath(db, b.repo)
		if repoPath == "" {
			continue
		}

		out, delErr := igit.RunCmd(repoPath, "branch", "-D", b.branchName)
		if delErr == nil {
			logger.Printf("Cleaned up orphaned branch %s for task %d", b.branchName, b.id)
			db.Exec(`UPDATE BountyBoard SET branch_name = '' WHERE id = ?`, b.id)
		} else if !strings.Contains(out, "not found") {
			logger.Printf("Could not delete branch %s for task %d: %s", b.branchName, b.id, strings.TrimSpace(out))
		} else {
			db.Exec(`UPDATE BountyBoard SET branch_name = '' WHERE id = ?`, b.id)
		}
	}
}
