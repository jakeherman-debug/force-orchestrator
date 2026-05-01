package agents

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"

	"force-orchestrator/internal/agents/capabilities"
	"force-orchestrator/internal/claude"
	"force-orchestrator/internal/clients/librarian"
	igit "force-orchestrator/internal/git"
	"force-orchestrator/internal/store"
	"force-orchestrator/internal/telemetry"
)

// InquisitorConfig is the constructor-injection bundle for SpawnInquisitor.
// The Inquisitor delegates to RunDogs each tick, which needs the Librarian
// client to drive the post-merge / post-shipped WriteMemory enqueues.
type InquisitorConfig struct {
	Librarian librarian.Client
}

const staleLockTimeout = 45 * time.Minute // locked this long (regardless of commits) → hard reset
const stallEscTimeout  = 30 * time.Minute // locked this long with no commits → Boot triage + escalate
const inquisitorInterval = 5 * time.Minute

// bootCallCooldown prevents the Boot triage LLM from being called repeatedly on
// the same stalled task every inquisitor cycle.
const bootCallCooldown = 30 * time.Minute

// bootLastCalled tracks the most recent Boot triage invocation per task ID.
var bootLastCalled = map[int]time.Time{}

// SpawnInquisitor periodically hunts the fleet for problems.
//
// AUDIT-047 (Fix #8d): each iteration runs inside a derived context.WithTimeout
// (15 min hard budget) so a wedged subcomponent (e.g., a stuck gh call inside
// RunDogs, a hung pragma WAL_CHECKPOINT) cannot freeze the Inquisitor
// indefinitely. Dog-level timeouts (see RunDogs) provide a finer-grained
// 5-min budget per dog, and DogMarkHeartbeat writes heartbeat_at on each
// dog-start so /healthz can surface a wedged dog to the operator.
func SpawnInquisitor(ctx context.Context, db *sql.DB, cfg InquisitorConfig) {
	logger := NewLogger("Inquisitor")

	// D1 T0-1: load the two profiles Inquisitor handlers use. Boot triage
	// runs under the boot profile; the task classifier runs under the
	// inquisitor profile (both currently empty — pure-reasoning calls).
	bootProfile, err := capabilities.LoadProfile("boot")
	if err != nil {
		logger.Printf("Inquisitor cannot start: %v", err)
		return
	}
	inquisitorProfile, err := capabilities.LoadProfile("inquisitor")
	if err != nil {
		logger.Printf("Inquisitor cannot start: %v", err)
		return
	}
	logger.Printf("Inquisitor deployed — hunting every %v, stale threshold %v", inquisitorInterval, staleLockTimeout)
	validateWorktrees(db, logger)

	for {
		if ctx.Err() != nil {
			logger.Printf("Inquisitor exiting: %v", ctx.Err())
			return
		}
		time.Sleep(inquisitorInterval)
		// AUDIT-047: bound each Inquisitor tick with context.WithTimeout.
		// When the tick returns normally the cancel frees the context.
		// When the tick runs long, the context fires and downstream calls
		// that honour it (gh, future claude calls) abort rather than hang.
		tickCtx, tickCancel := context.WithTimeout(ctx, 15*time.Minute)
		runInquisitorTick(tickCtx, db, cfg.Librarian, bootProfile, inquisitorProfile, logger)
		tickCancel()
		continue
	}
}

// runInquisitorTick executes one Inquisitor sweep cycle. Split out from
// SpawnInquisitor so each tick runs under its own timeout-bounded
// context (AUDIT-047) without the enclosing for-loop needing deferred
// cancellation book-keeping.
func runInquisitorTick(ctx context.Context, db *sql.DB, lib librarian.Client, bootProfile, inquisitorProfile *capabilities.Profile, logger *log.Logger) {
	// Fix #8e: ctx now threads into cleanOrphanedBranches (igit.RunCmd) and
	// RunDogs (per-dog ctx); the prior _=ctx discard reflected the pre-Fix-#8e
	// state where the only consumers were gh/claude calls that took ctx
	// directly.

		rows, err := db.Query(`
			SELECT id FROM BountyBoard
			WHERE status IN ('Locked', 'UnderReview', 'UnderCaptainReview')
			  AND locked_at != ''
			  AND locked_at < datetime('now', ?)
		`, fmt.Sprintf("-%d seconds", int(staleLockTimeout.Seconds())))

		var staleIDs []int
		if err == nil {
			for rows.Next() {
				var id int
				if err := rows.Scan(&id); err != nil {
					logger.Printf("inquisitor: scan failed on stale-lock query: %v", err)
					continue
				}
				staleIDs = append(staleIDs, id)
			}
			if rErr := rows.Err(); rErr != nil {
				log.Printf("inquisitor.go:runInquisitorTick: rows iter error: %v", rErr)
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
			`, fmt.Sprintf("-%d seconds", int(staleLockTimeout.Seconds())))

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

		classifyPendingTasks(db, inquisitorProfile, logger)
		CheckStaleEscalations(db)
		CheckConvoyCompletions(db, logger)
		store.RecoverStaleConvoys(db)
		cleanOrphanedBranches(ctx, db, logger)
		detectStalledTasks(ctx, db, bootProfile, logger)
		backfillMissingAskBranches(db, logger)

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

		// AUDIT-096 (Fix #8d): prune rateLimitRetries entries for agent
		// names that are no longer present in the Agents table. Without a
		// prune the map grows unbounded across fleet scale-ups/downs as
		// retired agent names leave dangling counter entries. Helper
		// lives in astromech.go next to the map declaration.
		pruneRateLimitRetries(db)

		RunDogs(ctx, db, lib, logger)

		db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
}

// staleClassifyingTimeout is how long a task may remain in status='Classifying'
// before the Inquisitor gives up and fails it. Two classification failures in a
// row within a single poll cycle leaves the task untouched; repeated failures
// across multiple cycles (totalling this duration) indicate a persistent problem.
const staleClassifyingTimeout = 30 * time.Minute

// classifyPendingTasks finds tasks with status='Classifying', calls ClassifyTaskType
// for each, updates the task type to the result, and transitions the task to Pending.
// It also fails tasks that have been stuck in Classifying beyond staleClassifyingTimeout.
func classifyPendingTasks(db *sql.DB, profile *capabilities.Profile, logger interface{ Printf(string, ...any) }) {
	// Fail tasks that have been stuck in Classifying too long — repeated Claude errors.
	staleRes, staleErr := db.Exec(`
		UPDATE BountyBoard
		SET status    = 'Failed',
		    error_log = CASE WHEN IFNULL(error_log, '') != ''
		                THEN 'Inquisitor: classification timed out after 30 minutes — last error: ' || error_log
		                ELSE 'Inquisitor: classification timed out after 30 minutes'
		                END
		WHERE status = 'Classifying'
		  AND created_at < datetime('now', ?)
	`, fmt.Sprintf("-%d seconds", int(staleClassifyingTimeout.Seconds())))
	if staleErr != nil {
		logger.Printf("classifyPendingTasks: stale-reset error — %v", staleErr)
	} else if n, _ := staleRes.RowsAffected(); n > 0 {
		logger.Printf("classifyPendingTasks: failed %d task(s) stuck in Classifying beyond timeout", n)
	}

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
		if err := rows.Scan(&t.id, &t.payload); err != nil {
			logger.Printf("classifyPendingTasks: scan failed: %v", err)
			continue
		}
		tasks = append(tasks, t)
	}
	if rErr := rows.Err(); rErr != nil {
		log.Printf("inquisitor.go:classifyPendingTasks: rows iter error: %v", rErr)
	}
	rows.Close()

	for _, t := range tasks {
		mcpConfig, _ := profile.MCPConfigArg()
		taskType, reason, err := claude.ClassifyTaskType(t.payload,
			profile.AllowedToolsArg(), profile.DisallowedToolsArg(), mcpConfig)
		if err != nil {
			logger.Printf("Task #%d: classification error — %v", t.id, err)
			db.Exec(`UPDATE BountyBoard SET error_log = ? WHERE id = ?`,
				fmt.Sprintf("classification error: %v", err), t.id)
			continue
		}
		if _, err = db.Exec(`UPDATE BountyBoard SET type = ?, status = 'Pending' WHERE id = ?`, taskType, t.id); err != nil {
			logger.Printf("Task #%d: failed to persist classification — %v", t.id, err)
			continue
		}
		logger.Printf("Task #%d classified as %s — %s", t.id, taskType, reason)
	}
}

// detectStalledTasks finds tasks that are Locked/UnderReview for too long with
// no new commits in their worktree.
func detectStalledTasks(ctx context.Context, db *sql.DB, bootProfile *capabilities.Profile, logger interface{ Printf(string, ...any) }) {
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
		if err := rows.Scan(&t.id, &t.owner, &t.repo, &t.branchName, &t.lockedAt, &t.lockedMinutes); err != nil {
			logger.Printf("detectStalledTasks: scan failed: %v", err)
			continue
		}
		tasks = append(tasks, t)
	}
	if rErr := rows.Err(); rErr != nil {
		log.Printf("inquisitor.go:detectStalledTasks: rows iter error: %v", rErr)
	}
	rows.Close()

	for _, t := range tasks {
		repoPath := store.GetRepoPath(db, t.repo)
		if repoPath == "" {
			continue
		}
		worktreePath := igit.ResolveWorktreeDir(db, t.branchName, repoPath, t.id, BranchAgentName)

		// Fix #8c (AUDIT-147): route SQLite-shaped parsing through
		// store.ParseSQLiteTime so the UTC assumption is spelled out at the
		// call site (the prior raw time.Parse returned UTC by stdlib default
		// but coupled every caller to that implicit contract). Any future
		// caller that wants a comparable "now" should use store.NowSQLite().
		lockedAtTime, parseErr := store.ParseSQLiteTime(strings.TrimSpace(t.lockedAt))
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
			verdict := BootTriage(ctx, db, t.id, t.owner, t.repo, t.lockedMinutes, errorLog, bootProfile)
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
				if _, escErr := CreateEscalation(db, t.id, store.SeverityLow,
					fmt.Sprintf("Boot agent: %s (locked %.0f min, no commits)", verdict.Reason, t.lockedMinutes)); escErr != nil {
					// Escalation insert failed; the MailTypeAlert mail below
					// is the unconditional fallback surfacing path.
					logger.Printf("Inquisitor #%d: CreateEscalation (BootEscalate) failed: %v — operator mail below is the fallback", t.id, escErr)
				}
				store.LogAudit(db, "boot-agent", "escalate", t.id, verdict.Reason)
				// P27 burn-down: budget-gate the operator emit before SendMail.
				// On allowed=false the helper has already drop/digested per the
				// configured budget. Fail-open on err so a transient SQLite
				// glitch never silences a high-stakes alert.
				if allowed, _ := store.RespectNotificationBudget(
					context.Background(), db, "operator", "inquisitor", "email", "{}",
					store.StakesHigh,
				); !allowed {
					// budget exhausted (StakesHigh always punches through, so
					// this branch only fires on a real config-set 0-cap row).
				} else {
					_ = allowed
				}
				store.SendMail(db, "inquisitor", "operator",
					fmt.Sprintf("[STALL ESCALATED] Task #%d — %s", t.id, t.repo),
					fmt.Sprintf("Boot agent escalated task #%d.\n\nAgent: %s\nRepo: %s\nLocked: %.0f minutes with no commits\nReason: %s\n\nTask requires human review.",
						t.id, t.owner, t.repo, t.lockedMinutes, verdict.Reason),
					t.id, store.MailTypeAlert)
			case BootIgnore:
				logger.Printf("Task %d: Boot agent says IGNORE — agent may still be working", t.id)
			default: // BootWarn
				logger.Printf("Task %d: Boot agent says WARN — stall noted but not acting", t.id)
				if _, escErr := CreateEscalation(db, t.id, store.SeverityLow,
					fmt.Sprintf("Agent %s locked %.0f min with no commits — possible stall", t.owner, t.lockedMinutes)); escErr != nil {
					// Warn-level escalation insert failed; next inquisitor
					// tick (5 min) will re-evaluate the stalled task and
					// retry the escalation if it's still stuck.
					logger.Printf("Inquisitor #%d: CreateEscalation (BootWarn) failed: %v — next inquisitor tick will re-evaluate", t.id, escErr)
				}
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
		if err := rows.Scan(&e.agent, &e.repo, &e.path); err != nil {
			logger.Printf("validateWorktrees: scan failed: %v", err)
			continue
		}
		if _, statErr := os.Stat(e.path); statErr != nil {
			stale = append(stale, e)
		}
	}
	if rErr := rows.Err(); rErr != nil {
		log.Printf("inquisitor.go:validateWorktrees: rows iter error: %v", rErr)
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
// Fix #8e: ctx threads from runInquisitorTick (the per-tick ctx) so the
// branch-delete subprocess cancels on daemon shutdown.
func cleanOrphanedBranches(ctx context.Context, db *sql.DB, logger interface{ Printf(string, ...any) }) {
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
		if err := rows.Scan(&b.id, &b.repo, &b.branchName); err != nil {
			logger.Printf("cleanOrphanedBranches: scan failed: %v", err)
			continue
		}
		branches = append(branches, b)
	}
	if rErr := rows.Err(); rErr != nil {
		log.Printf("inquisitor.go:cleanOrphanedBranches: rows iter error: %v", rErr)
	}
	rows.Close()

	for _, b := range branches {
		repoPath := store.GetRepoPath(db, b.repo)
		if repoPath == "" {
			continue
		}

		out, delErr := igit.RunCmd(ctx, repoPath, "branch", "-D", b.branchName)
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
