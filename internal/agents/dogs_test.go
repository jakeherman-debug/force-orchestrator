package agents

import (
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
	err := runDog(db, "unknown-dog", logger)
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
	if err := runDog(db, "mail-cleanup", logger); err != nil {
		t.Fatalf("mail-cleanup dog failed: %v", err)
	}
}

func TestRunDog_DBVacuum(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	logger := log.New(io.Discard, "", 0)
	if err := runDog(db, "db-vacuum", logger); err != nil {
		t.Fatalf("db-vacuum dog failed: %v", err)
	}
}

func TestRunDog_GitHygiene_NoRepos(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// No repos registered — should succeed with no-op
	logger := log.New(io.Discard, "", 0)
	if err := runDog(db, "git-hygiene", logger); err != nil {
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
	RunDogs(db, logger)

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
	RunDogs(db, logger)

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
	RunDogs(db, logger)

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
	if err := dogGitHygiene(db, logger); err != nil {
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
	err := dogGitHygiene(db, logger)
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
	if len(dogs) != 18 {
		t.Errorf("expected 18 built-in dogs (9 legacy + 5 PR-flow + 2 PR-review + 1 convoy-review + 1 escalation-sweeper), got %d", len(dogs))
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
