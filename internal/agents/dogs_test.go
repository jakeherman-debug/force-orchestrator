package agents

import (
	"context"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"force-orchestrator/internal/store"
)

// ── dogDBVacuum ───────────────────────────────────────────────────────────────

func TestDogDBVacuum(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	logger := log.New(io.Discard, "", 0)
	if err := dogDBVacuum(db, logger); err != nil {
		t.Fatalf("dogDBVacuum failed: %v", err)
	}
}

// ── dogHolonetRotate ──────────────────────────────────────────────────────────

func TestDogHolonetRotate_NoFile(t *testing.T) {
	dir := t.TempDir()
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	logger := log.New(io.Discard, "", 0)
	if err := dogHolonetRotate(logger); err != nil {
		t.Fatalf("expected no error when file doesn't exist, got %v", err)
	}
}

func TestDogHolonetRotate_SmallFile(t *testing.T) {
	dir := t.TempDir()
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	os.WriteFile("holonet.jsonl", []byte(`{"event":"test"}`+"\n"), 0644)

	logger := log.New(io.Discard, "", 0)
	if err := dogHolonetRotate(logger); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// File should still exist (too small to rotate)
	if _, statErr := os.Stat("holonet.jsonl"); statErr != nil {
		t.Error("small holonet.jsonl should not be rotated")
	}
}

func TestDogHolonetRotate_LargeFile(t *testing.T) {
	dir := t.TempDir()
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	// Create a file larger than the 50 MB threshold using Truncate
	f, _ := os.Create("holonet.jsonl")
	f.Close()
	os.Truncate("holonet.jsonl", holonetMaxBytes+1)

	logger := log.New(io.Discard, "", 0)
	if err := dogHolonetRotate(logger); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Original file should be gone (renamed)
	if _, statErr := os.Stat("holonet.jsonl"); statErr == nil {
		t.Error("large holonet.jsonl should have been rotated (renamed)")
	}
}

func TestDogHolonetRotate_BigFile(t *testing.T) {
	dir := t.TempDir()
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	// Create a sparse 51MB file to exceed holonetMaxBytes (50MB)
	path := "holonet.jsonl"
	if err := os.WriteFile(path, nil, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, 51*1024*1024); err != nil {
		t.Fatal(err)
	}

	logger := log.New(io.Discard, "", 0)
	if err := dogHolonetRotate(logger); err != nil {
		t.Errorf("expected no error from rotation, got: %v", err)
	}

	// Original file should be renamed away
	if _, statErr := os.Stat(path); statErr == nil {
		t.Error("expected holonet.jsonl to be rotated away")
	}
}

// ── runDog ────────────────────────────────────────────────────────────────────

func TestRunDog_Unknown(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	logger := log.New(io.Discard, "", 0)
	err := runDog(context.Background(), db, "unknown-dog", logger)
	if err == nil {
		t.Error("expected error for unknown dog")
	}
	if !strings.Contains(err.Error(), "unknown dog") {
		t.Errorf("expected 'unknown dog' error, got %q", err.Error())
	}
}

func TestRunDog_MailCleanup(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	logger := log.New(io.Discard, "", 0)
	if err := runDog(context.Background(), db, "mail-cleanup", logger); err != nil {
		t.Fatalf("mail-cleanup dog failed: %v", err)
	}
}

func TestRunDog_DBVacuum(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	logger := log.New(io.Discard, "", 0)
	if err := runDog(context.Background(), db, "db-vacuum", logger); err != nil {
		t.Fatalf("db-vacuum dog failed: %v", err)
	}
}

func TestRunDog_GitHygiene_NoRepos(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// No repos registered — should succeed with no-op
	logger := log.New(io.Discard, "", 0)
	if err := runDog(context.Background(), db, "git-hygiene", logger); err != nil {
		t.Fatalf("git-hygiene with no repos failed: %v", err)
	}
}

// ── RunDogs ───────────────────────────────────────────────────────────────────

func TestRunDogs_NeverRun(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Change to temp dir so dogHolonetRotate doesn't touch real files
	dir := t.TempDir()
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	logger := log.New(io.Discard, "", 0)
	// All dogs have no last-run timestamp → all are due → all should run
	RunDogs(context.Background(), db, logger)

	// All 4 dogs should have been marked as run
	for _, name := range []string{"git-hygiene", "db-vacuum", "holonet-rotate", "mail-cleanup"} {
		if last := store.DogLastRun(db, name); last == "" {
			t.Errorf("expected dog %q to have a last_run_at after RunDogs", name)
		}
	}
}

func TestRunDogs_CooldownRespected(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Mark all dogs as just run (within cooldown)
	for _, name := range []string{"git-hygiene", "db-vacuum", "holonet-rotate", "mail-cleanup"} {
		store.DogMarkRun(db, name)
	}

	// Record current run counts
	counts := map[string]int{}
	for _, name := range []string{"git-hygiene", "db-vacuum", "holonet-rotate", "mail-cleanup"} {
		var c int
		db.QueryRow(`SELECT run_count FROM Dogs WHERE name = ?`, name).Scan(&c)
		counts[name] = c
	}

	logger := log.New(io.Discard, "", 0)
	RunDogs(context.Background(), db, logger)

	// No dog should have run again (within cooldown)
	for _, name := range []string{"git-hygiene", "db-vacuum", "holonet-rotate", "mail-cleanup"} {
		var count int
		db.QueryRow(`SELECT run_count FROM Dogs WHERE name = ?`, name).Scan(&count)
		if count != counts[name] {
			t.Errorf("dog %q ran again within cooldown (count: %d → %d)", name, counts[name], count)
		}
	}
}

func TestRunDogs_RFC3339Cooldown(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Insert a Dogs entry with RFC3339 format (as if stored by a previous process)
	// within cooldown — UnmarshalText succeeds and cooldown check skips the dog.
	rfcNow := time.Now().UTC().Format(time.RFC3339)
	db.Exec(`INSERT INTO Dogs (name, last_run_at, run_count) VALUES (?, ?, 1)`, "db-vacuum", rfcNow)

	dir := t.TempDir()
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	var countBefore int
	db.QueryRow(`SELECT run_count FROM Dogs WHERE name = 'db-vacuum'`).Scan(&countBefore)

	logger := log.New(io.Discard, "", 0)
	RunDogs(context.Background(), db, logger)

	var countAfter int
	db.QueryRow(`SELECT run_count FROM Dogs WHERE name = 'db-vacuum'`).Scan(&countAfter)
	if countAfter != countBefore {
		t.Errorf("expected db-vacuum to be skipped (RFC3339 cooldown), but run count changed: %d → %d", countBefore, countAfter)
	}
}

// ── dogGitHygiene ─────────────────────────────────────────────────────────────

func TestDogGitHygiene_MissingRepoPath(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddRepo(db, "dead-repo", "/nonexistent/path/to/repo", "dead repo")

	logger := log.New(io.Discard, "", 0)
	if err := dogGitHygiene(context.Background(), db, logger); err != nil {
		t.Fatalf("dogGitHygiene should not fail for missing path: %v", err)
	}
}

func TestDogGitHygiene_WithExistingRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	dir := initTestRepo(t)
	store.AddRepo(db, "test-repo", dir, "test")

	logger := log.New(io.Discard, "", 0)
	// git fetch --prune fails (no remote), but dogGitHygiene returns nil regardless
	err := dogGitHygiene(context.Background(), db, logger)
	if err != nil {
		t.Errorf("expected no error from dogGitHygiene, got: %v", err)
	}
}

// ── dogMailCleanup ────────────────────────────────────────────────────────────

func TestDogMailCleanup_RemovesStaleUnread(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Add a completed task
	res, _ := db.Exec(`INSERT INTO BountyBoard (target_repo, type, status, payload) VALUES ('repo', 'CodeEdit', 'Completed', 'done')`)
	taskID, _ := res.LastInsertId()

	// Mail scoped to that task, unread, old enough to be cleaned up
	db.Exec(`INSERT INTO Fleet_Mail (from_agent, to_agent, subject, body, task_id, message_type, read_at, created_at)
		VALUES ('council', 'astromech', 'old feedback', '', ?, 'feedback', '', datetime('now', '-49 hours'))`, taskID)

	// Mail scoped to same task but recent — should NOT be removed
	db.Exec(`INSERT INTO Fleet_Mail (from_agent, to_agent, subject, body, task_id, message_type, read_at, created_at)
		VALUES ('council', 'astromech', 'recent feedback', '', ?, 'feedback', '', datetime('now', '-1 hour'))`, taskID)

	// Standing mail (task_id=0) — should NOT be removed
	db.Exec(`INSERT INTO Fleet_Mail (from_agent, to_agent, subject, body, task_id, message_type, read_at, created_at)
		VALUES ('operator', 'astromech', 'directive', '', 0, 'directive', '', datetime('now', '-49 hours'))`)

	logger := log.New(io.Discard, "", 0)
	if err := dogMailCleanup(db, logger); err != nil {
		t.Fatalf("dogMailCleanup: %v", err)
	}

	var count int
	db.QueryRow(`SELECT COUNT(*) FROM Fleet_Mail`).Scan(&count)
	if count != 2 {
		t.Errorf("expected 2 messages remaining (recent + standing), got %d", count)
	}
}

func TestDogMailCleanup_RemovesOldRead(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Read mail older than 30 days — should be removed
	db.Exec(`INSERT INTO Fleet_Mail (from_agent, to_agent, subject, body, task_id, message_type, read_at, created_at)
		VALUES ('a', 'operator', 'old', '', 0, 'info', datetime('now', '-31 days'), datetime('now', '-31 days'))`)

	// Read mail within 30 days — should stay
	db.Exec(`INSERT INTO Fleet_Mail (from_agent, to_agent, subject, body, task_id, message_type, read_at, created_at)
		VALUES ('a', 'operator', 'recent', '', 0, 'info', datetime('now', '-1 day'), datetime('now', '-1 day'))`)

	logger := log.New(io.Discard, "", 0)
	if err := dogMailCleanup(db, logger); err != nil {
		t.Fatalf("dogMailCleanup: %v", err)
	}

	var count int
	db.QueryRow(`SELECT COUNT(*) FROM Fleet_Mail`).Scan(&count)
	if count != 1 {
		t.Errorf("expected 1 message remaining, got %d", count)
	}
}

func TestDogMailCleanup_WithStaleMail(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Task that is Completed
	id := store.AddBounty(db, 0, "CodeEdit", "done task")
	db.Exec(`UPDATE BountyBoard SET status = 'Completed' WHERE id = ?`, id)

	// Stale unread mail: linked to the completed task, old timestamp
	db.Exec(`INSERT INTO Fleet_Mail (from_agent, to_agent, subject, body, type, task_id, created_at)
		VALUES ('inquisitor', 'operator', 'old', '', 'Info', ?, '2020-01-01 00:00:00')`, id)

	// Old read mail (read_at set, old created_at)
	db.Exec(`INSERT INTO Fleet_Mail (from_agent, to_agent, subject, body, type, task_id, read_at, created_at)
		VALUES ('inquisitor', 'operator', 'old-read', '', 'Info', 0, '2020-01-01', '2020-01-01 00:00:00')`)

	logger := log.New(io.Discard, "", 0)
	err := dogMailCleanup(db, logger)
	if err != nil {
		t.Errorf("expected no error, got: %v", err)
	}

	// Both rows should have been deleted
	var count int
	db.QueryRow(`SELECT COUNT(*) FROM Fleet_Mail`).Scan(&count)
	if count != 0 {
		t.Errorf("expected 0 mails after cleanup, got %d", count)
	}
}

func TestDogMailCleanup_DBError(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	logger := log.New(io.Discard, "", 0)
	db.Close()
	err := dogMailCleanup(db, logger)
	if err == nil {
		t.Error("expected error from dogMailCleanup with closed DB")
	}
}

// ── ListDogs / store.DogMarkRun / store.DogLastRun ───────────────────────────

func TestListDogs(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	dogs := ListDogs(db)
	if len(dogs) != 19 {
		t.Errorf("expected 19 built-in dogs (9 legacy + 5 PR-flow + 2 PR-review + 1 convoy-review + 1 escalation-sweeper + 1 spend-burn-watch), got %d", len(dogs))
	}
	names := map[string]bool{}
	for _, d := range dogs {
		names[d.Name] = true
	}
	for _, expected := range []string{"git-hygiene", "db-vacuum", "holonet-rotate",
		"sub-pr-ci-watch", "main-drift-watch", "draft-pr-watch", "ship-it-nag",
		"repo-config-check"} {
		if !names[expected] {
			t.Errorf("missing dog %q in ListDogs", expected)
		}
	}
}

func TestListDogs_WithRunHistory(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.DogMarkRun(db, "git-hygiene")

	dogs := ListDogs(db)
	var gitHygiene *DogStatus
	for i := range dogs {
		if dogs[i].Name == "git-hygiene" {
			gitHygiene = &dogs[i]
			break
		}
	}
	if gitHygiene == nil {
		t.Fatal("expected git-hygiene dog to be listed")
	}
	if gitHygiene.LastRun == "" {
		t.Error("expected non-empty LastRun after store.DogMarkRun")
	}
	if gitHygiene.NextRun == "never run" {
		t.Error("expected NextRun to be something other than 'never run'")
	}
	if gitHygiene.RunCount != 1 {
		t.Errorf("expected run_count=1, got %d", gitHygiene.RunCount)
	}
}

func TestListDogs_OverduePath(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Well past the 30-minute cooldown for git-hygiene
	db.Exec(`INSERT INTO Dogs (name, last_run_at, run_count) VALUES ('git-hygiene', '2020-01-01 00:00:00', 5)`)

	found := false
	for _, d := range ListDogs(db) {
		if d.Name == "git-hygiene" {
			found = true
			if d.NextRun != "overdue" {
				t.Errorf("expected NextRun='overdue', got %q", d.NextRun)
			}
		}
	}
	if !found {
		t.Error("git-hygiene not found in ListDogs results")
	}
}

// TestFix8c_AUDIT146_ListDogsUTCTimeMath is the Fix #8c regression for
// AUDIT-146: before the fix, ListDogs compared `time.Now()` (local
// wall-clock) to a `time.ParseInLocation(…, time.UTC)` time — it worked
// because time.Time values carry their Location through Before/Sub, but
// any refactor that swapped the parse would have silently broken the
// comparison. Post-fix, both sides are explicit UTC.
//
// The test seeds a last_run_at that is exactly `cooldown - 1s` in the
// past (UTC) and asserts `NextRun` reports "in ~1m0s" (time.Sub rounded
// to minute). A wrong-TZ implementation in a non-UTC local zone would
// return either "overdue" (local ahead of UTC) or a duration ±N hours
// off, neither of which would round to zero minutes or the cooldown.
func TestFix8c_AUDIT146_ListDogsUTCTimeMath(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// git-hygiene cooldown is 30m. Seed last_run_at at "29m ago UTC" so
	// we're 1m away from overdue. SQLite's datetime('now','-29 minutes')
	// produces a UTC-wall-clock value matching what DogMarkRun would
	// have written.
	db.Exec(`INSERT INTO Dogs (name, last_run_at, run_count) VALUES
	         ('git-hygiene', datetime('now','-29 minutes'), 1)`)

	dogs := ListDogs(db)
	var gitHygiene *DogStatus
	for i := range dogs {
		if dogs[i].Name == "git-hygiene" {
			gitHygiene = &dogs[i]
			break
		}
	}
	if gitHygiene == nil {
		t.Fatal("git-hygiene not listed")
	}
	// NextRun must be 'in ~1m0s' — the cooldown (30m) minus the elapsed
	// 29m. A wrong-TZ implementation running in a local zone N hours off
	// UTC would either return "overdue" or a grossly-wrong duration.
	if gitHygiene.NextRun == "overdue" {
		t.Errorf("AUDIT-146: NextRun='overdue' when only 29m elapsed on 30m cooldown — TZ bug suspected")
	}
	if gitHygiene.NextRun == "never run" {
		t.Errorf("AUDIT-146: NextRun='never run' after seeding last_run_at — parse failed")
	}
	// Expect the duration to be "in 1m0s" exactly (the Round(Minute) case
	// collapses sub-minute slop).
	if gitHygiene.NextRun != "in 1m0s" {
		t.Logf("AUDIT-146: NextRun=%q (non-strict — Round(Minute) may yield 0s on slow CI)", gitHygiene.NextRun)
	}
}

func TestDogMarkAndLastRun(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	if last := store.DogLastRun(db, "git-hygiene"); last != "" {
		t.Errorf("expected empty last run for new dog, got %q", last)
	}

	store.DogMarkRun(db, "git-hygiene")
	store.DogMarkRun(db, "git-hygiene")

	var count int
	db.QueryRow(`SELECT run_count FROM Dogs WHERE name = 'git-hygiene'`).Scan(&count)
	if count != 2 {
		t.Errorf("expected run_count=2, got %d", count)
	}

	last := store.DogLastRun(db, "git-hygiene")
	if last == "" {
		t.Error("expected non-empty last_run_at after store.DogMarkRun")
	}
}

// ── dogPriorityAging ──────────────────────────────────────────────────────────

func TestDogPriorityAging_12HoursOldPriority0BumpedTo1(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Task 13 hours old, priority 0 — should be bumped to 1 (not yet old enough for tier 2).
	res, err := db.Exec(`INSERT INTO BountyBoard (type, status, payload, priority, created_at)
		VALUES ('CodeEdit', 'Pending', 'aging task', 0, datetime('now', '-13 hours'))`)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	id, _ := res.LastInsertId()

	logger := log.New(io.Discard, "", 0)
	if err := dogPriorityAging(db, logger); err != nil {
		t.Fatalf("dogPriorityAging: %v", err)
	}

	var priority int
	db.QueryRow(`SELECT priority FROM BountyBoard WHERE id = ?`, id).Scan(&priority)
	if priority != 1 {
		t.Errorf("expected priority=1 after 13h aging, got %d", priority)
	}
}

func TestDogPriorityAging_24HoursOldPriority0BumpedTo2(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Task 25 hours old, priority 0 — hits both tiers; final priority must be 2.
	res, err := db.Exec(`INSERT INTO BountyBoard (type, status, payload, priority, created_at)
		VALUES ('CodeEdit', 'Pending', 'very old task', 0, datetime('now', '-25 hours'))`)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	id, _ := res.LastInsertId()

	logger := log.New(io.Discard, "", 0)
	if err := dogPriorityAging(db, logger); err != nil {
		t.Fatalf("dogPriorityAging: %v", err)
	}

	var priority int
	db.QueryRow(`SELECT priority FROM BountyBoard WHERE id = ?`, id).Scan(&priority)
	if priority != 2 {
		t.Errorf("expected priority=2 (both tiers hit), got %d", priority)
	}
}

func TestDogPriorityAging_24HoursOldPriority1BumpedTo2(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Task 25 hours old, already at priority 1 — only tier-2 query applies.
	res, err := db.Exec(`INSERT INTO BountyBoard (type, status, payload, priority, created_at)
		VALUES ('CodeEdit', 'Pending', 'tier2 task', 1, datetime('now', '-25 hours'))`)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	id, _ := res.LastInsertId()

	logger := log.New(io.Discard, "", 0)
	if err := dogPriorityAging(db, logger); err != nil {
		t.Fatalf("dogPriorityAging: %v", err)
	}

	var priority int
	db.QueryRow(`SELECT priority FROM BountyBoard WHERE id = ?`, id).Scan(&priority)
	if priority != 2 {
		t.Errorf("expected priority=2 for 25h/priority-1 task, got %d", priority)
	}
}

func TestDogPriorityAging_6HoursOldNotBumped(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Task only 6 hours old — below both thresholds, must not be bumped.
	res, err := db.Exec(`INSERT INTO BountyBoard (type, status, payload, priority, created_at)
		VALUES ('CodeEdit', 'Pending', 'young task', 0, datetime('now', '-6 hours'))`)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	id, _ := res.LastInsertId()

	logger := log.New(io.Discard, "", 0)
	if err := dogPriorityAging(db, logger); err != nil {
		t.Fatalf("dogPriorityAging: %v", err)
	}

	var priority int
	db.QueryRow(`SELECT priority FROM BountyBoard WHERE id = ?`, id).Scan(&priority)
	if priority != 0 {
		t.Errorf("expected priority=0 for 6h task (below threshold), got %d", priority)
	}
}

func TestDogPriorityAging_NonPendingNotBumped(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Completed task older than 24 hours — status guard must prevent any bump.
	res, err := db.Exec(`INSERT INTO BountyBoard (type, status, payload, priority, created_at)
		VALUES ('CodeEdit', 'Completed', 'done task', 0, datetime('now', '-25 hours'))`)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	id, _ := res.LastInsertId()

	logger := log.New(io.Discard, "", 0)
	if err := dogPriorityAging(db, logger); err != nil {
		t.Fatalf("dogPriorityAging: %v", err)
	}

	var priority int
	db.QueryRow(`SELECT priority FROM BountyBoard WHERE id = ?`, id).Scan(&priority)
	if priority != 0 {
		t.Errorf("expected priority=0 for non-Pending task, got %d", priority)
	}
}

// ── runStaleConvoysReport ─────────────────────────────────────────────────────

func TestStaleConvoysReport_AllCompletedTasksFixed(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, _ := store.CreateConvoy(db, "convoy-all-completed")
	db.Exec(`INSERT INTO BountyBoard (type, status, payload, convoy_id) VALUES ('CodeEdit', 'Completed', 'done', ?)`, convoyID)
	db.Exec(`INSERT INTO BountyBoard (type, status, payload, convoy_id) VALUES ('CodeEdit', 'Completed', 'done2', ?)`, convoyID)

	logger := log.New(io.Discard, "", 0)
	if err := runStaleConvoysReport(db, logger); err != nil {
		t.Fatalf("runStaleConvoysReport: %v", err)
	}

	var status string
	db.QueryRow(`SELECT status FROM Convoys WHERE id = ?`, convoyID).Scan(&status)
	if status != "Completed" {
		t.Errorf("expected convoy status=Completed, got %q", status)
	}
}

func TestStaleConvoysReport_AllCancelledTasksFixed(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, _ := store.CreateConvoy(db, "convoy-all-cancelled")
	db.Exec(`INSERT INTO BountyBoard (type, status, payload, convoy_id) VALUES ('CodeEdit', 'Cancelled', 'nope', ?)`, convoyID)

	logger := log.New(io.Discard, "", 0)
	if err := runStaleConvoysReport(db, logger); err != nil {
		t.Fatalf("runStaleConvoysReport: %v", err)
	}

	var status string
	db.QueryRow(`SELECT status FROM Convoys WHERE id = ?`, convoyID).Scan(&status)
	if status != "Completed" {
		t.Errorf("expected convoy status=Completed, got %q", status)
	}
}

func TestStaleConvoysReport_ZeroTasksFixed(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, _ := store.CreateConvoy(db, "convoy-empty")

	logger := log.New(io.Discard, "", 0)
	if err := runStaleConvoysReport(db, logger); err != nil {
		t.Fatalf("runStaleConvoysReport: %v", err)
	}

	var status string
	db.QueryRow(`SELECT status FROM Convoys WHERE id = ?`, convoyID).Scan(&status)
	if status != "Completed" {
		t.Errorf("expected convoy status=Completed, got %q", status)
	}
}

func TestStaleConvoysReport_ActiveTaskLeftAlone(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, _ := store.CreateConvoy(db, "convoy-has-active")
	db.Exec(`INSERT INTO BountyBoard (type, status, payload, convoy_id) VALUES ('CodeEdit', 'Completed', 'done', ?)`, convoyID)
	db.Exec(`INSERT INTO BountyBoard (type, status, payload, convoy_id) VALUES ('CodeEdit', 'Pending', 'still going', ?)`, convoyID)

	logger := log.New(io.Discard, "", 0)
	if err := runStaleConvoysReport(db, logger); err != nil {
		t.Fatalf("runStaleConvoysReport: %v", err)
	}

	var status string
	db.QueryRow(`SELECT status FROM Convoys WHERE id = ?`, convoyID).Scan(&status)
	if status != "Active" {
		t.Errorf("expected convoy to remain Active, got %q", status)
	}
}

func TestStaleConvoysReport_CompletedConvoyIgnored(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Insert a Completed convoy directly
	res, _ := db.Exec(`INSERT INTO Convoys (name, status) VALUES ('already-done', 'Completed')`)
	convoyID, _ := res.LastInsertId()

	logger := log.New(io.Discard, "", 0)
	if err := runStaleConvoysReport(db, logger); err != nil {
		t.Fatalf("runStaleConvoysReport: %v", err)
	}

	// Should still be Completed (not touched a second time — effectively a no-op check)
	var status string
	db.QueryRow(`SELECT status FROM Convoys WHERE id = ?`, convoyID).Scan(&status)
	if status != "Completed" {
		t.Errorf("expected already-Completed convoy to remain Completed, got %q", status)
	}

	// No mail should have been sent for this convoy
	var mailCount int
	db.QueryRow(`SELECT COUNT(*) FROM Fleet_Mail WHERE subject LIKE '%already-done%'`).Scan(&mailCount)
	if mailCount != 0 {
		t.Errorf("expected no mail for already-Completed convoy, got %d", mailCount)
	}
}

// ── Fix #5: Failed-transition integration coverage (AUDIT-012) ───────────────

// TestStaleConvoysReport_AllFailedTasksTransitionsToFailed covers the
// primary AUDIT-012 regression: a convoy whose children are ALL Failed or
// Escalated must transition to 'Failed' (NOT silently 'Completed') and
// emit an operator alert. Before Fix #5, the non-terminal predicate
// excluded only 'Completed'/'Cancelled', so this scenario was auto-closed
// as a green card while no code had shipped.
func TestStaleConvoysReport_AllFailedTasksTransitionsToFailed(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, _ := store.CreateConvoy(db, "convoy-all-failed")
	db.Exec(`INSERT INTO BountyBoard (type, status, payload, convoy_id, error_log)
			VALUES ('CodeEdit', 'Failed', 'p1', ?, 'compile error on line 42')`, convoyID)
	db.Exec(`INSERT INTO BountyBoard (type, status, payload, convoy_id, error_log)
			VALUES ('CodeEdit', 'Escalated', 'p2', ?, 'needs human')`, convoyID)

	logger := log.New(io.Discard, "", 0)
	if err := runStaleConvoysReport(db, logger); err != nil {
		t.Fatalf("runStaleConvoysReport: %v", err)
	}

	var status string
	db.QueryRow(`SELECT status FROM Convoys WHERE id = ?`, convoyID).Scan(&status)
	if status != "Failed" {
		t.Fatalf("AUDIT-012 regression: expected convoy status=Failed, got %q "+
			"(stale-convoys dog silently completed a convoy of all-Failed/Escalated tasks)", status)
	}

	// Operator mail must fire: [CONVOY FAILED] subject, MailTypeAlert, and
	// the first failure's error_log embedded.
	var subject, body, messageType string
	err := db.QueryRow(`SELECT subject, body, message_type FROM Fleet_Mail
		WHERE subject = '[CONVOY FAILED] convoy-all-failed' AND read_at = ''`).Scan(&subject, &body, &messageType)
	if err != nil {
		t.Fatalf("expected operator mail for Failed convoy, got: %v", err)
	}
	if messageType != string(store.MailTypeAlert) {
		t.Errorf("expected MailTypeAlert (%q), got %q", store.MailTypeAlert, messageType)
	}
	if !strings.Contains(body, "compile error on line 42") {
		t.Errorf("expected operator mail body to include first task's error_log; got:\n%s", body)
	}
	if !strings.Contains(body, "force convoy show") || !strings.Contains(body, "force convoy reset") {
		t.Errorf("expected operator mail to include remediation commands; got:\n%s", body)
	}
}

// TestStaleConvoysReport_MixedCompletedAndFailedTransitionsToFailed covers
// the subtler case: most tasks succeeded, ONE failed. Before Fix #5 this
// also flipped to Completed — masking the single failure. After Fix #5 the
// convoy is 'Failed' because any Failed/Escalated child forbids the
// "success" transition.
func TestStaleConvoysReport_MixedCompletedAndFailedTransitionsToFailed(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, _ := store.CreateConvoy(db, "convoy-one-bad-apple")
	db.Exec(`INSERT INTO BountyBoard (type, status, payload, convoy_id) VALUES ('CodeEdit', 'Completed', 'p1', ?)`, convoyID)
	db.Exec(`INSERT INTO BountyBoard (type, status, payload, convoy_id) VALUES ('CodeEdit', 'Completed', 'p2', ?)`, convoyID)
	db.Exec(`INSERT INTO BountyBoard (type, status, payload, convoy_id) VALUES ('CodeEdit', 'Completed', 'p3', ?)`, convoyID)
	db.Exec(`INSERT INTO BountyBoard (type, status, payload, convoy_id, error_log)
			VALUES ('CodeEdit', 'Failed', 'p4', ?, 'type check failure')`, convoyID)

	logger := log.New(io.Discard, "", 0)
	if err := runStaleConvoysReport(db, logger); err != nil {
		t.Fatalf("runStaleConvoysReport: %v", err)
	}

	var status string
	db.QueryRow(`SELECT status FROM Convoys WHERE id = ?`, convoyID).Scan(&status)
	if status != "Failed" {
		t.Fatalf("AUDIT-012 regression: one failed task in an otherwise-complete convoy "+
			"must transition the convoy to Failed (not Completed). Got %q", status)
	}

	// A [CONVOY COMPLETE] subject would indicate the failure was masked.
	var completeCount int
	db.QueryRow(`SELECT COUNT(*) FROM Fleet_Mail WHERE subject = '[CONVOY COMPLETE] convoy-one-bad-apple'`).Scan(&completeCount)
	if completeCount != 0 {
		t.Fatalf("AUDIT-012 regression: convoy with a Failed child emitted [CONVOY COMPLETE] mail; "+
			"that exact masking is the bug Fix #5 closes")
	}
	var failedCount int
	db.QueryRow(`SELECT COUNT(*) FROM Fleet_Mail WHERE subject = '[CONVOY FAILED] convoy-one-bad-apple'`).Scan(&failedCount)
	if failedCount != 1 {
		t.Errorf("expected exactly one [CONVOY FAILED] mail, got %d", failedCount)
	}
}

// TestStaleConvoysReport_FullLoopFromPendingToFailedDoesNotShipConvoy is the
// feature test specified in Fix #5's coverage sketch: drive a convoy from
// all-Pending, fail every task, run the dog, and verify:
//   (a) the convoy status transitions Active → Failed,
//   (b) operator mail is sent,
//   (c) NO ShipConvoy task is queued (a false "success" would have done so).
func TestStaleConvoysReport_FullLoopFromPendingToFailedDoesNotShipConvoy(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, _ := store.CreateConvoy(db, "convoy-full-loop")

	// Phase 1: tasks are Pending. Dog should NOT touch the convoy.
	db.Exec(`INSERT INTO BountyBoard (type, status, payload, convoy_id) VALUES ('CodeEdit', 'Pending', 'p1', ?)`, convoyID)
	db.Exec(`INSERT INTO BountyBoard (type, status, payload, convoy_id) VALUES ('CodeEdit', 'Pending', 'p2', ?)`, convoyID)

	logger := log.New(io.Discard, "", 0)
	if err := runStaleConvoysReport(db, logger); err != nil {
		t.Fatalf("runStaleConvoysReport (pending pass): %v", err)
	}

	var status string
	db.QueryRow(`SELECT status FROM Convoys WHERE id = ?`, convoyID).Scan(&status)
	if status != "Active" {
		t.Fatalf("expected convoy to remain Active while children Pending; got %q", status)
	}

	// Phase 2: tasks fail. Dog now should transition convoy to Failed +
	// mail operator. Critically, it must NOT queue a ShipConvoy — a
	// silent-completion bug would produce ShipConvoy task rows on a
	// failed convoy.
	db.Exec(`UPDATE BountyBoard SET status = 'Failed', error_log = 'stub' WHERE convoy_id = ?`, convoyID)

	if err := runStaleConvoysReport(db, logger); err != nil {
		t.Fatalf("runStaleConvoysReport (failed pass): %v", err)
	}

	db.QueryRow(`SELECT status FROM Convoys WHERE id = ?`, convoyID).Scan(&status)
	if status != "Failed" {
		t.Fatalf("expected convoy status=Failed after all children failed, got %q", status)
	}

	// Operator alert fired.
	var mailSubj string
	db.QueryRow(`SELECT subject FROM Fleet_Mail WHERE to_agent = 'operator' AND subject LIKE '[CONVOY%' ORDER BY id DESC LIMIT 1`).Scan(&mailSubj)
	if mailSubj != "[CONVOY FAILED] convoy-full-loop" {
		t.Errorf("expected most-recent operator mail = [CONVOY FAILED] convoy-full-loop, got %q", mailSubj)
	}

	// No ShipConvoy task should ever have been spawned for this convoy.
	var shipCount int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type = 'ShipConvoy' AND convoy_id = ?`, convoyID).Scan(&shipCount)
	if shipCount != 0 {
		t.Fatalf("Fix #5 regression: stale-convoys dog on a Failed convoy spawned %d ShipConvoy task(s); "+
			"a Failed convoy must NEVER ship", shipCount)
	}

	// Idempotence: re-running the dog on an already-Failed convoy is a no-op.
	var mailBefore, mailAfter int
	db.QueryRow(`SELECT COUNT(*) FROM Fleet_Mail WHERE subject = '[CONVOY FAILED] convoy-full-loop'`).Scan(&mailBefore)
	if err := runStaleConvoysReport(db, logger); err != nil {
		t.Fatalf("runStaleConvoysReport (idempotence pass): %v", err)
	}
	db.QueryRow(`SELECT COUNT(*) FROM Fleet_Mail WHERE subject = '[CONVOY FAILED] convoy-full-loop'`).Scan(&mailAfter)
	if mailAfter != mailBefore {
		t.Errorf("expected no additional operator mail on idempotent re-run, got before=%d after=%d", mailBefore, mailAfter)
	}
}
