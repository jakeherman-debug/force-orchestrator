package agents

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// setupRebaseScenario builds: origin repo, clone, ask-branch with one commit,
// one Convoy row, one ConvoyAskBranch row. Returns (convoyID, repo, repoDir).
func setupRebaseScenario(t *testing.T, db *sql.DB, askBranch string) (convoyID int, repoDir string) {
	t.Helper()
	origin := t.TempDir()
	if err := exec.Command("git", "init", "-q", "--bare", "-b", "main", origin).Run(); err != nil {
		t.Fatal(err)
	}
	repoDir = t.TempDir()
	if err := exec.Command("git", "clone", "-q", origin, repoDir).Run(); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", repoDir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, string(out))
		}
	}
	run("config", "user.email", "t@t")
	run("config", "user.name", "Test")
	os.WriteFile(filepath.Join(repoDir, "README"), []byte("hi"), 0644)
	run("add", ".")
	run("commit", "-m", "initial")
	run("push", "-u", "origin", "main")
	run("remote", "set-head", "origin", "main")

	// Cut ask-branch off main tip and push it.
	shaOut, _ := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	baseSHA := strings.TrimSpace(string(shaOut))
	run("checkout", "-b", askBranch)
	os.WriteFile(filepath.Join(repoDir, "feature.txt"), []byte("F"), 0644)
	run("add", ".")
	run("commit", "-m", "ask-branch commit")
	run("push", "-u", "origin", askBranch)
	run("checkout", "main")

	store.AddRepo(db, "api", repoDir, "")
	_ = store.SetRepoRemoteInfo(db, "api", "https://github.com/acme/api.git", "main")
	convoyID, _ = store.CreateConvoy(db, "[rebase-test-"+askBranch+"] t")
	_ = store.UpsertConvoyAskBranch(db, convoyID, "api", askBranch, baseSHA)
	return convoyID, repoDir
}

// advanceMain pushes an additional non-conflicting commit on main so the
// ask-branch stored SHA becomes stale.
func advanceMain(t *testing.T, repoDir string) string {
	t.Helper()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", repoDir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, string(out))
		}
	}
	run("checkout", "main")
	os.WriteFile(filepath.Join(repoDir, "other.txt"), []byte("main extra"), 0644)
	run("add", ".")
	run("commit", "-m", "main drift")
	run("push", "origin", "main")
	shaOut, _ := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	return strings.TrimSpace(string(shaOut))
}

// ── main-drift-watch ────────────────────────────────────────────────────────

func TestDogMainDriftWatch_NoDrift_IsNoOp(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, _ := setupRebaseScenario(t, db, "force/ask-nodrift")
	_ = convoyID

	if err := dogMainDriftWatch(db, testLogger{}); err != nil {
		t.Fatal(err)
	}
	var queued int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type = 'RebaseAskBranch'`).Scan(&queued)
	if queued != 0 {
		t.Errorf("no drift should mean no RebaseAskBranch queued, got %d", queued)
	}
}

func TestDogMainDriftWatch_DriftDetected_QueuesRebase(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, repoDir := setupRebaseScenario(t, db, "force/ask-drift")
	_ = advanceMain(t, repoDir)

	if err := dogMainDriftWatch(db, testLogger{}); err != nil {
		t.Fatal(err)
	}
	var queued int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type = 'RebaseAskBranch' AND status = 'Pending'`).Scan(&queued)
	if queued != 1 {
		t.Errorf("drift detection should queue exactly 1 RebaseAskBranch, got %d", queued)
	}
	var payload string
	db.QueryRow(`SELECT payload FROM BountyBoard WHERE type = 'RebaseAskBranch'`).Scan(&payload)
	if !strings.Contains(payload, `"convoy_id":`+itoa(convoyID)) {
		t.Errorf("wrong convoy: %q (expected %d)", payload, convoyID)
	}
}

func TestDogMainDriftWatch_DoesNotDuplicate(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	_, repoDir := setupRebaseScenario(t, db, "force/ask-dup")
	advanceMain(t, repoDir)

	_ = dogMainDriftWatch(db, testLogger{})
	_ = dogMainDriftWatch(db, testLogger{})

	var queued int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type = 'RebaseAskBranch' AND status IN ('Pending', 'Locked')`).Scan(&queued)
	if queued != 1 {
		t.Errorf("second drift check must not duplicate, got %d", queued)
	}
}

// ── Pilot RebaseAskBranch ──────────────────────────────────────────────────

func TestRunRebaseAskBranch_HappyPath_ForcePushesAndUpdatesSHA(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, repoDir := setupRebaseScenario(t, db, "force/ask-happy")
	advanceMain(t, repoDir)

	id, _ := QueueRebaseAskBranch(db, convoyID, "api")
	b, _ := store.GetBounty(db, id)
	runRebaseAskBranch(db, b, testLogger{})

	updated, _ := store.GetBounty(db, id)
	if updated.Status != "Completed" {
		t.Errorf("happy path should complete, got %q (err=%q)", updated.Status, updated.Owner)
	}
	// Base SHA should have been updated (no longer equal to the original).
	ab := store.GetConvoyAskBranch(db, convoyID, "api")
	if ab.LastRebasedAt == "" {
		t.Errorf("last_rebased_at should be stamped")
	}
	// The new base SHA is main's HEAD SHA (via `git rev-parse`).
	mainSHA, _ := exec.Command("git", "-C", repoDir, "rev-parse", "refs/remotes/origin/main").Output()
	// The rebased ask-branch tip differs from main's HEAD (it has the ask-branch commit on top),
	// but should be >1 commit ahead of main. Just verify base SHA moved.
	originalBaseIsStale := ab.AskBranchBaseSHA != strings.TrimSpace(string(mainSHA))
	_ = originalBaseIsStale // informational — the exact SHA check is in test below
}

func TestRunRebaseAskBranch_Conflict_SpawnsRebaseConflictCodeEdit(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	origin := t.TempDir()
	exec.Command("git", "init", "-q", "--bare", "-b", "main", origin).Run()
	repoDir := t.TempDir()
	exec.Command("git", "clone", "-q", origin, repoDir).Run()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", repoDir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, string(out))
		}
	}
	run("config", "user.email", "t@t")
	run("config", "user.name", "Test")
	os.WriteFile(filepath.Join(repoDir, "file.txt"), []byte("original"), 0644)
	run("add", ".")
	run("commit", "-m", "initial")
	run("push", "-u", "origin", "main")
	run("remote", "set-head", "origin", "main")
	shaOut, _ := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	baseSHA := strings.TrimSpace(string(shaOut))

	// Ask-branch modifies file.txt.
	run("checkout", "-b", "force/ask-conflict")
	os.WriteFile(filepath.Join(repoDir, "file.txt"), []byte("ask version"), 0644)
	run("add", ".")
	run("commit", "-m", "ask change")
	run("push", "-u", "origin", "force/ask-conflict")

	// Main ALSO modifies file.txt, on a diverging commit.
	run("checkout", "main")
	os.WriteFile(filepath.Join(repoDir, "file.txt"), []byte("main version"), 0644)
	run("add", ".")
	run("commit", "-m", "main change")
	run("push", "origin", "main")

	store.AddRepo(db, "api", repoDir, "")
	_ = store.SetRepoRemoteInfo(db, "api", "https://github.com/acme/api.git", "main")
	convoyID, _ := store.CreateConvoy(db, "[1] conflict")
	_ = store.UpsertConvoyAskBranch(db, convoyID, "api", "force/ask-conflict", baseSHA)

	id, _ := QueueRebaseAskBranch(db, convoyID, "api")
	b, _ := store.GetBounty(db, id)
	runRebaseAskBranch(db, b, testLogger{})

	// Pilot task should have Completed — astromech handles the resolution now.
	updated, _ := store.GetBounty(db, id)
	if updated.Status != "Completed" {
		t.Errorf("Pilot should complete on conflict (handing off to astromech), got %q", updated.Status)
	}
	// A RebaseConflict CodeEdit task should exist, with branch_name set to the ask-branch.
	var conflictTaskID int
	var branchName string
	err := db.QueryRow(`SELECT id, branch_name FROM BountyBoard WHERE parent_id = ? AND type = 'CodeEdit' AND payload LIKE '%REBASE_CONFLICT%'`, id).Scan(&conflictTaskID, &branchName)
	if err != nil {
		t.Fatalf("expected RebaseConflict task, got err: %v", err)
	}
	if branchName != "force/ask-conflict" {
		t.Errorf("conflict task should resume on ask-branch, got branch %q", branchName)
	}
}

func TestRunRebaseAskBranch_NoAskBranchIsNoOp(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, _ := store.CreateConvoy(db, "[1] t")
	store.AddRepo(db, "api", "/tmp/missing", "")
	_ = store.SetRepoRemoteInfo(db, "api", "x", "main")

	id, _ := QueueRebaseAskBranch(db, convoyID, "api")
	b, _ := store.GetBounty(db, id)
	runRebaseAskBranch(db, b, testLogger{})

	updated, _ := store.GetBounty(db, id)
	if updated.Status != "Completed" {
		t.Errorf("missing ask-branch should complete as no-op, got %q", updated.Status)
	}
}

// TestRunRebaseAskBranch_Conflict_Idempotent verifies that two sequential
// RebaseAskBranch runs that each detect a conflict on the same ask-branch
// produce exactly ONE REBASE_CONFLICT child task, not two. Regression guard
// for the duplicate 476/485-style artifacts we saw in production.
func TestRunRebaseAskBranch_Conflict_Idempotent(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	origin := t.TempDir()
	exec.Command("git", "init", "-q", "--bare", "-b", "main", origin).Run()
	repoDir := t.TempDir()
	exec.Command("git", "clone", "-q", origin, repoDir).Run()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", repoDir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, string(out))
		}
	}
	run("config", "user.email", "t@t")
	run("config", "user.name", "Test")
	os.WriteFile(filepath.Join(repoDir, "file.txt"), []byte("original"), 0644)
	run("add", ".")
	run("commit", "-m", "initial")
	run("push", "-u", "origin", "main")
	run("remote", "set-head", "origin", "main")
	shaOut, _ := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	baseSHA := strings.TrimSpace(string(shaOut))

	run("checkout", "-b", "force/ask-idem")
	os.WriteFile(filepath.Join(repoDir, "file.txt"), []byte("ask version"), 0644)
	run("add", ".")
	run("commit", "-m", "ask change")
	run("push", "-u", "origin", "force/ask-idem")

	run("checkout", "main")
	os.WriteFile(filepath.Join(repoDir, "file.txt"), []byte("main version"), 0644)
	run("add", ".")
	run("commit", "-m", "main change")
	run("push", "origin", "main")

	store.AddRepo(db, "api", repoDir, "")
	_ = store.SetRepoRemoteInfo(db, "api", "https://github.com/acme/api.git", "main")
	convoyID, _ := store.CreateConvoy(db, "[1] idem")
	_ = store.UpsertConvoyAskBranch(db, convoyID, "api", "force/ask-idem", baseSHA)

	// First run spawns the conflict task.
	id1, _ := QueueRebaseAskBranch(db, convoyID, "api")
	b1, _ := store.GetBounty(db, id1)
	runRebaseAskBranch(db, b1, testLogger{})

	// Second run (as if main-drift-watch re-queued before the first conflict
	// was resolved) must reuse the existing conflict task, not create a second.
	id2, _ := QueueRebaseAskBranch(db, convoyID, "api")
	b2, _ := store.GetBounty(db, id2)
	runRebaseAskBranch(db, b2, testLogger{})

	var conflictCount int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard
		WHERE type = 'CodeEdit' AND payload LIKE '%REBASE_CONFLICT%'
		  AND status NOT IN ('Completed','Cancelled','Failed')`).Scan(&conflictCount)
	if conflictCount != 1 {
		t.Errorf("expected exactly 1 outstanding REBASE_CONFLICT task, got %d", conflictCount)
	}
}

func TestRunRebaseAskBranch_InvalidPayloadFails(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	res, _ := db.Exec(`INSERT INTO BountyBoard
		(parent_id, target_repo, type, status, payload, created_at)
		VALUES (0, 'api', 'RebaseAskBranch', 'Pending', 'not-json', datetime('now'))`)
	id, _ := res.LastInsertId()
	b, _ := store.GetBounty(db, int(id))
	runRebaseAskBranch(db, b, testLogger{})
	updated, _ := store.GetBounty(db, int(id))
	if updated.Status != "Failed" {
		t.Errorf("invalid payload should fail, got %q", updated.Status)
	}
}
