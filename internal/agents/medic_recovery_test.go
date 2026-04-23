package agents

import (
	"database/sql"
	"encoding/json"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// makeOriginAndCloneWithBranch is a lean helper that mirrors the test git
// fixtures elsewhere: bare origin, working clone, one initial commit on main.
// Tests that need a branch with/without diffs create it themselves on top.
func makeOriginAndCloneWithBranch(t *testing.T) (repoDir, origin string) {
	t.Helper()
	origin = t.TempDir()
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
	run("config", "user.name", "T")
	os.WriteFile(filepath.Join(repoDir, "README"), []byte("hi"), 0644)
	run("add", ".")
	run("commit", "-q", "-m", "initial")
	run("push", "-u", "origin", "main")
	run("remote", "set-head", "origin", "main")
	return repoDir, origin
}

// TestAutoCompletedMedicTask_BranchHasNoDiff is the direct regression for
// task 470: Medic was invoked on a task whose branch had no net diff vs main
// (the work had already landed via a sibling task), but Medic still called
// Claude which escalated to the operator. Now we short-circuit before any
// LLM call, mark the task Completed, and resolve its Open escalations.
func TestAutoCompletedMedicTask_BranchHasNoDiff(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	repoDir, _ := makeOriginAndCloneWithBranch(t)
	store.AddRepo(db, "api", repoDir, "")

	// Create an agent branch that points to main — no diff, no commits ahead.
	run := func(args ...string) {
		exec.Command("git", append([]string{"-C", repoDir}, args...)...).Run()
	}
	run("checkout", "-b", "agent/task-1")
	run("push", "-u", "origin", "agent/task-1")
	run("checkout", "main")

	taskID, _ := store.AddConvoyTask(db, 0, "api", "already-delivered work", 1, 5, "Pending")
	store.SetBranchName(db, taskID, "agent/task-1")
	db.Exec(`UPDATE BountyBoard SET status = 'Failed' WHERE id = ?`, taskID)

	// Seed an Open escalation the Medic call should sweep along with the auto-complete.
	db.Exec(`INSERT INTO Escalations (task_id, severity, message, status)
		VALUES (?, 'LOW', 'already-completed', 'Open')`, taskID)

	// Queue MedicReview for this task.
	medicID := store.QueueMedicReview(db,
		&store.Bounty{ID: taskID, TargetRepo: "api", ConvoyID: 1, Priority: 5},
		"test", "test error")
	mb, _ := store.GetBounty(db, medicID)

	logger := log.New(io.Discard, "", 0)
	runMedicTask(db, "Medic-1", mb, logger)

	// Parent must be Completed.
	parent, _ := store.GetBounty(db, taskID)
	if parent.Status != "Completed" {
		t.Errorf("expected parent Completed on empty-diff auto-complete, got %q", parent.Status)
	}
	// Medic review task must be Completed (not left Locked).
	medic, _ := store.GetBounty(db, medicID)
	if medic.Status != "Completed" {
		t.Errorf("expected Medic review Completed, got %q", medic.Status)
	}
	// The Open escalation should have been resolved by the same path.
	var escStatus string
	db.QueryRow(`SELECT status FROM Escalations WHERE task_id = ?`, taskID).Scan(&escStatus)
	if escStatus != "Resolved" {
		t.Errorf("escalation for auto-completed task should be Resolved, got %q", escStatus)
	}
}

// TestAutoCompletedMedicTask_BranchHasDiff confirms we do NOT short-circuit
// when the branch has real changes — normal Medic LLM flow runs.
func TestAutoCompletedMedicTask_BranchHasDiff(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	repoDir, _ := makeOriginAndCloneWithBranch(t)
	store.AddRepo(db, "api", repoDir, "")

	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", repoDir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@t")
		cmd.Run()
	}
	run("checkout", "-b", "agent/task-has-diff")
	os.WriteFile(filepath.Join(repoDir, "newfile.txt"), []byte("x"), 0644)
	run("add", ".")
	run("commit", "-m", "real change")
	run("push", "-u", "origin", "agent/task-has-diff")
	run("checkout", "main")

	taskID, _ := store.AddConvoyTask(db, 0, "api", "real work", 1, 5, "Pending")
	store.SetBranchName(db, taskID, "agent/task-has-diff")
	parent, _ := store.GetBounty(db, taskID)

	medicBounty := &store.Bounty{ID: 9999, ParentID: parent.ID}

	if autoCompletedMedicTask(db, "Medic-1", medicBounty, parent, log.New(io.Discard, "", 0)) {
		t.Error("must NOT auto-complete when branch has real diff — normal Medic flow should run")
	}
}

// TestApplyMedicCleanup_QueuesWorktreeResetWithInferredTarget covers the
// path the user specifically asked about: Medic decided "cleanup" but didn't
// specify a target branch. The convoy's ask-branch is the right default; we
// should use it instead of escalating. Regression for task 492.
func TestApplyMedicCleanup_QueuesWorktreeResetWithInferredTarget(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddRepo(db, "api", "/tmp/api", "")
	_ = store.SetRepoRemoteInfo(db, "api", "git@github.com:acme/api.git", "main")
	cid, _ := store.CreateConvoy(db, "[1] contaminated")
	_ = store.UpsertConvoyAskBranch(db, cid, "api", "force/ask-1-contamination", "sha")
	taskID, _ := store.AddConvoyTask(db, 0, "api", "two-line claude.go change", cid, 5, "Pending")
	db.Exec(`UPDATE BountyBoard SET status = 'Failed' WHERE id = ?`, taskID)
	parent, _ := store.GetBounty(db, taskID)

	medicID := store.QueueMedicReview(db, parent, "repeated_identical_diff", "three agents produced identical out-of-scope diff")
	mb, _ := store.GetBounty(db, medicID)

	applyMedicCleanup(db, "Medic-1", mb, parent,
		medicDecision{
			Decision: "cleanup",
			Reason:   "three agents produced identical out-of-scope dashboard changes",
			// Deliberately omit CleanupTargetBranch — exercise the inference path.
		},
		log.New(io.Discard, "", 0))

	// A WorktreeReset task should exist, targeting the convoy's ask-branch.
	var resetID int
	var status, payload string
	err := db.QueryRow(`SELECT id, status, payload FROM BountyBoard
		WHERE type = 'WorktreeReset' AND parent_id = ? LIMIT 1`, taskID).
		Scan(&resetID, &status, &payload)
	if err != nil {
		t.Fatalf("no WorktreeReset task spawned: %v", err)
	}
	if status != "Pending" {
		t.Errorf("WorktreeReset should be Pending, got %q", status)
	}
	if !strings.Contains(payload, `"target_branch":"force/ask-1-contamination"`) {
		t.Errorf("cleanup target should be inferred from ask-branch; payload=%s", payload)
	}
}

// TestApplyMedicCleanup_NoTargetAvailableFallsToEscalate verifies the escape
// hatch: if we truly can't infer a reset target (no convoy, no default
// branch), don't silently drop the cleanup — escalate instead.
func TestApplyMedicCleanup_NoTargetAvailableFallsToEscalate(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Repo WITHOUT default_branch and WITHOUT convoy.
	store.AddRepo(db, "api", "/tmp/api", "")
	// Explicitly do NOT call SetRepoRemoteInfo.

	res, _ := db.Exec(`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, created_at)
		VALUES (0, 'api', 'CodeEdit', 'Failed', 'x', datetime('now'))`)
	taskID, _ := res.LastInsertId()
	parent, _ := store.GetBounty(db, int(taskID))
	medicID := store.QueueMedicReview(db, parent, "contamination", "err")
	mb, _ := store.GetBounty(db, medicID)

	applyMedicCleanup(db, "Medic-1", mb, parent,
		medicDecision{Decision: "cleanup", Reason: "can't infer target"},
		log.New(io.Discard, "", 0))

	// No WorktreeReset should have been queued.
	var count int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type = 'WorktreeReset'`).Scan(&count)
	if count != 0 {
		t.Errorf("no-target-available path must not queue WorktreeReset; got %d", count)
	}
	// An escalation should have been created instead.
	var escCount int
	db.QueryRow(`SELECT COUNT(*) FROM Escalations WHERE task_id = ?`, taskID).Scan(&escCount)
	if escCount == 0 {
		t.Error("must fall back to escalation when no reset target can be inferred")
	}
}

// TestQueueWorktreeReset_Idempotent protects the dedup guarantee — if a
// second MedicReview fires on the same task before the first WorktreeReset
// completes (rare but possible across two Medic agents), we must not queue
// duplicate wipes against the same worktrees.
func TestQueueWorktreeReset_Idempotent(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	p := worktreeResetPayload{
		ParentTaskID: 42,
		Repo:         "api",
		TargetBranch: "main",
	}
	first, err := QueueWorktreeReset(db, p)
	if err != nil || first == 0 {
		t.Fatalf("first queue: id=%d err=%v", first, err)
	}
	second, err := QueueWorktreeReset(db, p)
	if err != nil {
		t.Fatalf("second queue: %v", err)
	}
	if second != first {
		t.Errorf("second queue should return existing id %d, got %d", first, second)
	}
}

// TestRunWorktreeReset_CleansAndRequeuesParent is the end-to-end happy path:
// seed a worktree with stale uncommitted + untracked files, run the reset,
// assert worktree is clean AND parent task is re-Pending.
func TestRunWorktreeReset_CleansAndRequeuesParent(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	repoDir, _ := makeOriginAndCloneWithBranch(t)
	store.AddRepo(db, "api", repoDir, "")

	// Fake an astromech worktree at the expected path and dirty it up.
	worktreeRoot := filepath.Join(filepath.Dir(repoDir), ".force-worktrees", filepath.Base(repoDir), "R2-D2")
	if err := os.MkdirAll(worktreeRoot, 0755); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("git", "-C", repoDir, "worktree", "add", "-B", "agent/R2-D2/stale", worktreeRoot, "main").Run(); err != nil {
		t.Fatalf("worktree add: %v", err)
	}
	// Plant both an uncommitted modification and an untracked file.
	os.WriteFile(filepath.Join(worktreeRoot, "README"), []byte("contaminated"), 0644)
	os.WriteFile(filepath.Join(worktreeRoot, "untracked.txt"), []byte("leftover"), 0644)

	// Parent task to re-queue.
	taskID, _ := store.AddConvoyTask(db, 0, "api", "original work", 1, 5, "Pending")
	db.Exec(`UPDATE BountyBoard SET status = 'Failed', branch_name = 'agent/R2-D2/stale',
		error_log = 'contaminated' WHERE id = ?`, taskID)
	// And an Open escalation that should be swept as part of the fix.
	db.Exec(`INSERT INTO Escalations (task_id, severity, message, status)
		VALUES (?, 'MEDIUM', 'contamination', 'Open')`, taskID)

	resetID, _ := QueueWorktreeReset(db, worktreeResetPayload{
		ParentTaskID: taskID, Repo: "api", TargetBranch: "main", Reason: "test contamination",
	})
	rb, _ := store.GetBounty(db, resetID)
	runWorktreeReset(db, rb, testLogger{})

	// Reset task must have completed.
	rbAfter, _ := store.GetBounty(db, resetID)
	if rbAfter.Status != "Completed" {
		t.Errorf("WorktreeReset should complete, got %q", rbAfter.Status)
	}
	// Worktree should be clean — no README modification, no untracked file.
	out, _ := exec.Command("git", "-C", worktreeRoot, "status", "--porcelain").Output()
	if strings.TrimSpace(string(out)) != "" {
		t.Errorf("worktree should be clean after reset; got %q", string(out))
	}
	if _, err := os.Stat(filepath.Join(worktreeRoot, "untracked.txt")); !os.IsNotExist(err) {
		t.Error("untracked.txt should have been wiped by clean -fdx")
	}
	// Parent should be requeued with empty branch_name.
	parent, _ := store.GetBounty(db, taskID)
	if parent.Status != "Pending" {
		t.Errorf("parent should be re-Pending, got %q", parent.Status)
	}
	if parent.BranchName != "" {
		t.Errorf("branch_name should be cleared for fresh start, got %q", parent.BranchName)
	}
	// Escalation should be resolved.
	var escStatus string
	db.QueryRow(`SELECT status FROM Escalations WHERE task_id = ?`, taskID).Scan(&escStatus)
	if escStatus != "Resolved" {
		t.Errorf("escalation should auto-resolve after cleanup; got %q", escStatus)
	}
}

// TestMedicDecisionJSON_ParsesCleanupFields regression-guards the JSON schema
// — a hand-crafted cleanup decision must round-trip correctly through the
// struct tags so Medic's LLM output is consumable.
func TestMedicDecisionJSON_ParsesCleanupFields(t *testing.T) {
	raw := `{
		"decision": "cleanup",
		"reason": "three agents produced identical out-of-scope diff",
		"cleanup_target_branch": "force/ask-37-fix",
		"cleanup_agents": ["R2-D2", "R4-P17", "BB-8"]
	}`
	var d medicDecision
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatal(err)
	}
	if d.Decision != "cleanup" {
		t.Errorf("decision = %q", d.Decision)
	}
	if d.CleanupTargetBranch != "force/ask-37-fix" {
		t.Errorf("cleanup_target_branch = %q", d.CleanupTargetBranch)
	}
	if len(d.CleanupAgents) != 3 {
		t.Errorf("cleanup_agents len = %d, want 3", len(d.CleanupAgents))
	}
}

var _ *sql.DB // keep sql import for future-proofing auxiliary helpers
