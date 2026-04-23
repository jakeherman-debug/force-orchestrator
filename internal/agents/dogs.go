package agents

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	igit "force-orchestrator/internal/git"
	"force-orchestrator/internal/store"
)

// dogCooldowns defines how often each built-in dog may run.
var dogCooldowns = map[string]time.Duration{
	"git-hygiene":           30 * time.Minute,
	"db-vacuum":             6 * time.Hour,
	"holonet-rotate":        24 * time.Hour,
	"mail-cleanup":          12 * time.Hour,
	"memory-hygiene":        24 * time.Hour,
	"stalled-reviews":       6 * time.Hour,
	"priority-aging":        6 * time.Hour,
	"daily-digest":          24 * time.Hour,
	"stale-convoys-report":  12 * time.Hour,
	// sub-pr-ci-watch polls open sub-PRs for CI state + external closure.
	// Cooldown is 0 so it runs every inquisitor tick (5 min) — the underlying
	// `gh pr view` calls are cheap enough that this is the right cadence to
	// avoid blocking tasks waiting on auto-merge for more than 5 minutes.
	"sub-pr-ci-watch":       0,
	// main-drift-watch polls `git ls-remote` per ask-branch. The detection is
	// cheap (~50ms per repo), so 15 min is a generous cadence — rebases fire
	// only when main has actually moved.
	"main-drift-watch":      15 * time.Minute,
	// draft-pr-watch polls `gh pr view` per DraftPROpen convoy. Merged/Closed
	// transitions are cheap to detect; 5 min cadence is a reasonable balance
	// between timeliness for the operator and API rate-limit hygiene.
	"draft-pr-watch":        0, // every inquisitor tick
	// ship-it-nag sends operator reminders for aged draft PRs. Once per 6h is
	// plenty — the internal per-threshold dedupe prevents spam.
	"ship-it-nag":           6 * time.Hour,
	// repo-config-check revalidates remote URL, default branch, and PR template
	// path for every registered repo once a day.
	"repo-config-check":     24 * time.Hour,
	// pr-review-poll fetches bot/human review comments on each DraftPROpen
	// convoy's draft PR and queues PRReviewTriage tasks when new comments land.
	// 5-min cadence mirrors draft-pr-watch — bot comments typically arrive in
	// bursts minutes after the draft PR opens, so this matches their arrival pattern.
	"pr-review-poll":        5 * time.Minute,
	"convoy-review-watch":   5 * time.Minute,
	// pr-review-resolve sweeps in_scope_fix comments whose spawned CodeEdit has
	// Completed, calling the GraphQL resolveReviewThread mutation. Runs on every
	// inquisitor tick — batches are small and cost is two gh calls per resolve.
	"pr-review-resolve":     0,
}

// dogOrder determines the execution order of dogs within each inquisitor cycle.
var dogOrder = []string{
	"git-hygiene", "db-vacuum", "holonet-rotate", "mail-cleanup", "memory-hygiene",
	"stalled-reviews", "priority-aging", "daily-digest", "stale-convoys-report",
	"sub-pr-ci-watch", "main-drift-watch", "draft-pr-watch", "ship-it-nag",
	"repo-config-check", "pr-review-poll", "pr-review-resolve", "convoy-review-watch",
}

// RunDogs checks each built-in dog against its cooldown and runs any that are due.
// Called by SpawnInquisitor on every inquisitor cycle.
func RunDogs(db *sql.DB, logger interface{ Printf(string, ...any) }) {
	for _, dogName := range dogOrder {
		cooldown := dogCooldowns[dogName]
		last := store.DogLastRun(db, dogName)
		if last != "" {
			var lastT time.Time
			if err := (&lastT).UnmarshalText([]byte(last)); err == nil {
				if time.Since(lastT) < cooldown {
					continue
				}
			} else {
				t, err := time.ParseInLocation("2006-01-02 15:04:05", last, time.UTC)
				if err == nil && time.Since(t) < cooldown {
					continue
				}
			}
		}
		logger.Printf("Dog %s: running (cooldown %v)", dogName, cooldown)
		store.DogMarkRun(db, dogName)
		if err := runDog(db, dogName, logger); err != nil {
			logger.Printf("Dog %s: error — %v", dogName, err)
			store.SendMail(db, "inquisitor", "operator",
				fmt.Sprintf("[DOG FAILURE] %s", dogName),
				fmt.Sprintf("Watchdog '%s' failed during its scheduled run.\n\nError: %v\n\nThis may indicate a system health issue requiring attention.", dogName, err),
				0, store.MailTypeAlert)
		} else {
			logger.Printf("Dog %s: done", dogName)
		}
	}
}

// RunDogByName force-runs the named dog exactly once, bypassing the cooldown.
// Used by CLI ("force dogs run <name>") and dashboard ("Run now") buttons.
// Returns an error with the list of valid dog names if the name is unknown.
func RunDogByName(db *sql.DB, name string, logger interface{ Printf(string, ...any) }) error {
	// Mark the run before executing so a crashed dog still shows up as attempted.
	store.DogMarkRun(db, name)
	return runDog(db, name, logger)
}

// DogNames returns the canonical order of registered dogs. Used for CLI
// completion and validation.
func DogNames() []string {
	out := make([]string, len(dogOrder))
	copy(out, dogOrder)
	return out
}

func runDog(db *sql.DB, name string, logger interface{ Printf(string, ...any) }) error {
	switch name {
	case "git-hygiene":
		return dogGitHygiene(db, logger)
	case "db-vacuum":
		return dogDBVacuum(db, logger)
	case "holonet-rotate":
		return dogHolonetRotate(logger)
	case "mail-cleanup":
		return dogMailCleanup(db, logger)
	case "memory-hygiene":
		return dogMemoryHygiene(db, logger)
	case "stalled-reviews":
		return dogStalledReviews(db, logger)
	case "priority-aging":
		return dogPriorityAging(db, logger)
	case "daily-digest":
		return runDailyDigest(db, logger)
	case "stale-convoys-report":
		return runStaleConvoysReport(db, logger)
	case "sub-pr-ci-watch":
		return dogSubPRCIWatch(db, logger)
	case "main-drift-watch":
		return dogMainDriftWatch(db, logger)
	case "draft-pr-watch":
		return dogDraftPRWatch(db, logger)
	case "ship-it-nag":
		return dogShipItNag(db, logger)
	case "repo-config-check":
		return dogRepoConfigCheck(db, logger)
	case "pr-review-poll":
		return dogPRReviewPoll(db, logger)
	case "pr-review-resolve":
		return dogPRReviewResolve(db, logger)
	case "convoy-review-watch":
		return dogConvoyReviewWatch(db, logger)
	default:
		return fmt.Errorf("unknown dog: %s", name)
	}
}

func dogGitHygiene(db *sql.DB, logger interface{ Printf(string, ...any) }) error {
	// Collect repos first, then close rows before doing any further DB work.
	rows, err := db.Query(`SELECT name, local_path FROM Repositories`)
	if err != nil {
		return err
	}
	type repo struct{ name, path string }
	var repos []repo
	for rows.Next() {
		var r repo
		rows.Scan(&r.name, &r.path)
		repos = append(repos, r)
	}
	rows.Close()

	for _, r := range repos {
		if _, statErr := os.Stat(r.path); statErr != nil {
			logger.Printf("ERROR: Dog git-hygiene: repo '%s' path not accessible (%s) — check registration with: force repos", r.name, r.path)
			continue
		}
		if out, gitErr := igit.RunCmd(r.path, "fetch", "--prune", "--quiet"); gitErr != nil {
			logger.Printf("Dog git-hygiene: fetch failed for %s: %s", r.name, out)
		} else {
			logger.Printf("Dog git-hygiene: fetched %s", r.name)
		}
		igit.RunCmd(r.path, "gc", "--auto", "--quiet")
		igit.RunCmd(r.path, "worktree", "prune")
	}

	// Detach agent worktrees that are on branches no longer referenced by any live task,
	// and delete those orphaned branches. This happens when a task retries with a different
	// agent — the new agent creates a fresh branch, updating branch_name in the DB, but
	// the old agent's worktree stays on the old branch until we clean it up here.
	agentRows, agentErr := db.Query(`SELECT agent_name, repo, worktree_path FROM Agents`)
	if agentErr != nil {
		return nil // non-fatal — skip orphan cleanup this cycle
	}
	type agentEntry struct{ name, repo, path string }
	var agents []agentEntry
	for agentRows.Next() {
		var a agentEntry
		agentRows.Scan(&a.name, &a.repo, &a.path)
		agents = append(agents, a)
	}
	agentRows.Close()

	var detached int
	for _, a := range agents {
		if _, statErr := os.Stat(a.path); statErr != nil {
			continue
		}
		out, gitErr := exec.Command("git", "-C", a.path, "rev-parse", "--abbrev-ref", "HEAD").Output()
		if gitErr != nil {
			continue
		}
		branch := strings.TrimSpace(string(out))
		if branch == "HEAD" {
			continue // already detached
		}
		// Keep the branch if any non-terminal task still references it.
		// Failed/Escalated are included as "live" so operators can inspect the branch.
		var count int
		db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE branch_name = ? AND status NOT IN ('Completed','Cancelled')`, branch).Scan(&count)
		if count > 0 {
			continue
		}
		exec.Command("git", "-C", a.path, "checkout", "--detach", "HEAD").Run()
		exec.Command("git", "-C", a.repo, "branch", "-D", branch).Run()
		detached++
		logger.Printf("Dog git-hygiene: detached worktree %s from orphaned branch %s", a.name, branch)
	}
	if detached > 0 {
		logger.Printf("Dog git-hygiene: cleaned up %d orphaned worktree branch(es)", detached)
	}
	return nil
}

const vacuumThresholdBytes = 100 * 1024 * 1024 // 100 MB

func dogDBVacuum(db *sql.DB, logger interface{ Printf(string, ...any) }) error {
	// WAL checkpointing is handled by SpawnInquisitor every 5 minutes — no need to repeat here.
	db.Exec(`ANALYZE`)

	// Only VACUUM when the database file exceeds the threshold; VACUUM holds an
	// exclusive lock for its entire duration, so we avoid it during normal operation.
	var seq int
	var dbName, dbFile string
	db.QueryRow(`PRAGMA database_list`).Scan(&seq, &dbName, &dbFile)
	if dbFile != "" {
		info, err := os.Stat(dbFile)
		if err == nil && info.Size() < vacuumThresholdBytes {
			logger.Printf("Dog db-vacuum: skipping VACUUM (%.1f MB < threshold)", float64(info.Size())/1024/1024)
			return nil
		}
	}

	_, err := db.Exec(`VACUUM`)
	return err
}

const holonetMaxBytes = 50 * 1024 * 1024 // 50 MB

func dogHolonetRotate(logger interface{ Printf(string, ...any) }) error {
	const path = "holonet.jsonl"
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	if info.Size() < holonetMaxBytes {
		return nil
	}
	// Use millisecond precision to prevent same-second filename collisions.
	stamp := time.Now().UTC().Format("20060102-150405.000")
	archivePath := filepath.Join("holonet-" + stamp + ".jsonl")
	if renErr := os.Rename(path, archivePath); renErr != nil {
		return renErr
	}
	logger.Printf("Dog holonet-rotate: rotated %s → %s (%.1f MB)", path, archivePath, float64(info.Size())/1024/1024)
	return nil
}

func dogMailCleanup(db *sql.DB, logger interface{ Printf(string, ...any) }) error {
	res, err := db.Exec(`
		DELETE FROM Fleet_Mail
		WHERE consumed_at = ''
		  AND task_id != 0
		  AND created_at < datetime('now', '-48 hours')
		  AND task_id IN (
		      SELECT id FROM BountyBoard WHERE status IN ('Completed', 'Failed', 'Escalated')
		  )`)
	if err != nil {
		return err
	}
	stale, _ := res.RowsAffected()

	res2, err := db.Exec(`DELETE FROM Fleet_Mail WHERE read_at != '' AND created_at < datetime('now', '-30 days')`)
	if err != nil {
		return err
	}
	old, _ := res2.RowsAffected()

	if stale > 0 || old > 0 {
		logger.Printf("Dog mail-cleanup: removed %d stale unread + %d old read messages", stale, old)
	}
	return nil
}

func dogMemoryHygiene(db *sql.DB, logger interface{ Printf(string, ...any) }) error {
	// Pass 1: delete failure memories where a success memory exists for the same task_id.
	res1, err := db.Exec(`
		DELETE FROM FleetMemory
		WHERE outcome = 'failure'
		  AND task_id IN (
		      SELECT task_id FROM FleetMemory WHERE outcome = 'success' AND task_id != 0
		  )`)
	if err != nil {
		return fmt.Errorf("memory-hygiene pass 1: %w", err)
	}
	removed1, _ := res1.RowsAffected()
	if removed1 > 0 {
		if _, err := db.Exec(`DELETE FROM FleetMemory_fts WHERE rowid NOT IN (SELECT id FROM FleetMemory)`); err != nil {
			return fmt.Errorf("memory-hygiene pass 1 fts sync: %w", err)
		}
		logger.Printf("Dog memory-hygiene: pass 1 removed %d stale failure memories", removed1)
	}

	// Pass 2: delete stale audit-finding memories whose underlying task is Completed.
	res2, err := db.Exec(`
		DELETE FROM FleetMemory
		WHERE summary LIKE '[AUDIT FINDING%'
		  AND task_id IN (
		      SELECT id FROM BountyBoard WHERE status = 'Completed'
		  )`)
	if err != nil {
		return fmt.Errorf("memory-hygiene pass 2: %w", err)
	}
	removed2, _ := res2.RowsAffected()
	if removed2 > 0 {
		if _, err := db.Exec(`DELETE FROM FleetMemory_fts WHERE rowid NOT IN (SELECT id FROM FleetMemory)`); err != nil {
			return fmt.Errorf("memory-hygiene pass 2 fts sync: %w", err)
		}
		logger.Printf("Dog memory-hygiene: pass 2 removed %d stale audit-finding memories", removed2)
	}

	if removed1 == 0 && removed2 == 0 {
		logger.Printf("Dog memory-hygiene: nothing to clean up")
	}
	return nil
}

func dogStalledReviews(db *sql.DB, logger interface{ Printf(string, ...any) }) error {
	// Traditional review-stall: tasks stuck in Captain/Council queues. locked_at
	// is set when they enter review so we can measure age directly.
	rows, err := db.Query(`
		SELECT id, status,
		       ROUND((julianday('now') - julianday(locked_at)) * 24, 1) AS hours_waiting
		FROM BountyBoard
		WHERE status IN ('AwaitingCaptainReview', 'AwaitingCouncilReview')
		  AND locked_at < datetime('now', '-4 hours')
		ORDER BY locked_at ASC`)
	if err != nil {
		return err
	}
	type stalledTask struct {
		id           int64
		status       string
		hoursWaiting float64
	}
	var tasks []stalledTask
	for rows.Next() {
		var t stalledTask
		if err := rows.Scan(&t.id, &t.status, &t.hoursWaiting); err != nil {
			rows.Close()
			return err
		}
		tasks = append(tasks, t)
	}
	rows.Close()

	// Sub-PR-CI stall: AwaitingSubPRCI tasks have owner='' (Jedi clears it when
	// handing off to sub-pr-ci-watch). Use the AskBranchPR's created_at as the
	// age yardstick. Threshold is longer (12h) because CI legitimately takes
	// minutes-to-hours on some repos and Medic has up to 3 attempts to fix.
	subPRRows, sErr := db.Query(`
		SELECT b.id,
		       ROUND((julianday('now') - julianday(abp.created_at)) * 24, 1) AS hours_open
		FROM BountyBoard b
		JOIN AskBranchPRs abp ON abp.task_id = b.id
		WHERE b.status = 'AwaitingSubPRCI'
		  AND abp.state = 'Open'
		  AND abp.created_at < datetime('now', '-12 hours')
		ORDER BY abp.created_at ASC`)
	if sErr == nil {
		for subPRRows.Next() {
			var id int64
			var hours float64
			if err := subPRRows.Scan(&id, &hours); err == nil {
				tasks = append(tasks, stalledTask{id: id, status: "AwaitingSubPRCI", hoursWaiting: hours})
			}
		}
		subPRRows.Close()
	}

	if len(tasks) == 0 {
		return nil
	}

	logger.Printf("Dog stalled-reviews: %d task(s) stuck in review queue", len(tasks))

	var body strings.Builder
	fmt.Fprintf(&body, "%d task(s) have been waiting in the review queue for extended periods:\n\n", len(tasks))
	for _, t := range tasks {
		fmt.Fprintf(&body, "  Task #%d  status=%-24s  waiting=%.1fh\n", t.id, t.status, t.hoursWaiting)
	}

	store.SendMail(db, "inquisitor", "operator",
		fmt.Sprintf("[STALLED REVIEWS] %d tasks stuck in review", len(tasks)),
		body.String(), 0, store.MailTypeAlert)
	return nil
}

func dogPriorityAging(db *sql.DB, logger interface{ Printf(string, ...any) }) error {
	res1, err := db.Exec(`
		UPDATE BountyBoard
		SET priority = 1
		WHERE status = 'Pending'
		  AND priority = 0
		  AND created_at < datetime('now', '-12 hours')`)
	if err != nil {
		return err
	}
	bumped1, _ := res1.RowsAffected()

	res2, err := db.Exec(`
		UPDATE BountyBoard
		SET priority = 2
		WHERE status = 'Pending'
		  AND priority < 2
		  AND created_at < datetime('now', '-24 hours')`)
	if err != nil {
		return err
	}
	bumped2, _ := res2.RowsAffected()

	logger.Printf("Dog priority-aging: bumped %d task(s) to priority 1, %d task(s) to priority 2", bumped1, bumped2)
	return nil
}

func runDailyDigest(db *sql.DB, logger interface{ Printf(string, ...any) }) error {
	stats, err := store.FetchDigestStats(db)
	if err != nil {
		return fmt.Errorf("daily-digest: fetch stats: %w", err)
	}

	var body strings.Builder

	fmt.Fprintf(&body, "## Tasks (Last 24h)\n\n")
	fmt.Fprintf(&body, "- Completed: %d\n", stats.Completed)
	fmt.Fprintf(&body, "- Failed: %d\n", stats.Failed)
	fmt.Fprintf(&body, "- Escalated: %d\n\n", stats.Escalated)

	fmt.Fprintf(&body, "## Current Queue\n\n")
	fmt.Fprintf(&body, "- Pending: %d\n", stats.Pending)
	fmt.Fprintf(&body, "- Locked: %d\n\n", stats.Locked)

	fmt.Fprintf(&body, "## Top Agents\n\n")
	if len(stats.TopAgents) == 0 {
		fmt.Fprintf(&body, "None\n\n")
	} else {
		for _, a := range stats.TopAgents {
			fmt.Fprintf(&body, "- %s: %d\n", a.Agent, a.Count)
		}
		fmt.Fprintf(&body, "\n")
	}

	fmt.Fprintf(&body, "## Stale Convoys\n\n")
	if len(stats.StaleConvoys) == 0 {
		fmt.Fprintf(&body, "None\n")
	} else {
		for _, c := range stats.StaleConvoys {
			fmt.Fprintf(&body, "- #%d %s\n", c.ID, c.Name)
		}
	}

	store.SendMail(db, "inquisitor", "operator", "[DAILY DIGEST] Fleet Health Summary", body.String(), 0, store.MailTypeInfo)
	logger.Printf("Dog daily-digest: digest sent (completed=%d failed=%d escalated=%d pending=%d locked=%d stale_convoys=%d)",
		stats.Completed, stats.Failed, stats.Escalated, stats.Pending, stats.Locked, len(stats.StaleConvoys))
	return nil
}

func runStaleConvoysReport(db *sql.DB, logger interface{ Printf(string, ...any) }) error {
	rows, err := db.Query(`SELECT id, name FROM Convoys WHERE status = 'Active'`)
	if err != nil {
		return err
	}
	type convoy struct {
		id   int64
		name string
	}
	var convoys []convoy
	for rows.Next() {
		var c convoy
		rows.Scan(&c.id, &c.name)
		convoys = append(convoys, c)
	}
	rows.Close()

	var fixedEmpty, fixedStale int
	for _, c := range convoys {
		var total int
		db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE convoy_id = ?`, c.id).Scan(&total)

		var shouldComplete bool
		var reason string
		if total == 0 {
			shouldComplete = true
			reason = "no tasks"
		} else {
			var nonTerminal int
			db.QueryRow(`
				SELECT COUNT(*) FROM BountyBoard
				WHERE convoy_id = ?
				  AND status NOT IN ('Completed', 'Cancelled')`, c.id).Scan(&nonTerminal)
			if nonTerminal == 0 {
				shouldComplete = true
				reason = "all tasks terminal"
			}
		}

		if !shouldComplete {
			continue
		}

		db.Exec(`UPDATE Convoys SET status = 'Completed' WHERE id = ?`, c.id)
		store.SendMail(db, "inquisitor", "operator",
			fmt.Sprintf("[CONVOY COMPLETE] %s", c.name),
			fmt.Sprintf("Convoy '%s' was auto-completed by the stale-convoys-report dog (%s).", c.name, reason),
			0, store.MailTypeInfo)

		if total == 0 {
			fixedEmpty++
		} else {
			fixedStale++
		}
	}

	if fixedEmpty == 0 && fixedStale == 0 {
		logger.Printf("Dog stale-convoys-report: no stale convoys found")
	} else {
		logger.Printf("Dog stale-convoys-report: completed %d empty convoy(s) and %d stale convoy(s) where all tasks were terminal", fixedEmpty, fixedStale)
	}
	return nil
}

// DogStatus holds the status of a single watchdog.
type DogStatus struct {
	Name     string
	Cooldown time.Duration
	LastRun  string
	NextRun  string
	RunCount int
}

// ListDogs returns the name, last-run time, and run count for all known dogs.
func ListDogs(db *sql.DB) []DogStatus {
	var result []DogStatus
	for _, name := range dogOrder {
		cooldown := dogCooldowns[name]
		var lastRun string
		var count int
		db.QueryRow(`SELECT last_run_at, run_count FROM Dogs WHERE name = ?`, name).Scan(&lastRun, &count)
		var nextRun string
		if lastRun != "" {
			t, err := time.ParseInLocation("2006-01-02 15:04:05", lastRun, time.UTC)
			if err == nil {
				next := t.Add(cooldown)
				if time.Now().Before(next) {
					nextRun = fmt.Sprintf("in %v", next.Sub(time.Now()).Round(time.Minute))
				} else {
					nextRun = "overdue"
				}
			}
		} else {
			nextRun = "never run"
		}
		result = append(result, DogStatus{
			Name:     name,
			Cooldown: cooldown,
			LastRun:  lastRun,
			NextRun:  nextRun,
			RunCount: count,
		})
	}
	return result
}
