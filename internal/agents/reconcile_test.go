package agents

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"force-orchestrator/internal/store"
)

// ── helpers ────────────────────────────────────────────────────────────────

// reconcileFixture sets up a registered repo with a real git checkout
// under a temp dir. Returns repo path and registered repo name.
func reconcileFixture(t *testing.T, db *sql.DB, name string) (repoPath, repoName string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	makeGitRepo(t, dir, "")
	store.AddRepo(db, name, dir, "test repo")
	return dir, name
}

// seedNonTerminal creates a BountyBoard row with the given attributes
// and returns the new task ID. The row is inserted directly so we can
// pin status to a non-default value (AddBounty hard-codes 'Pending').
func seedNonTerminal(t *testing.T, db *sql.DB, repo, status, branch string) int {
	t.Helper()
	res, err := db.Exec(`INSERT INTO BountyBoard
		(parent_id, target_repo, type, status, payload, branch_name, created_at)
		VALUES (0, ?, 'CodeEdit', ?, 'do the thing', ?, datetime('now'))`,
		repo, status, branch)
	if err != nil {
		t.Fatalf("seed task: %v", err)
	}
	id, _ := res.LastInsertId()
	return int(id)
}

// makeBranchAt creates a branch in the given repo pointing at HEAD (or a
// freshly committed file when amend=true). The created branch is
// registered in the local refs but not checked out.
func makeBranchAt(t *testing.T, repoPath, branchName string) {
	t.Helper()
	cmd := exec.Command("git", "-C", repoPath, "branch", branchName)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git branch %s: %v: %s", branchName, err, out)
	}
}

// registerWorktree creates the on-disk persistent-worktree directory
// (a stub — we don't need a full git worktree for these tests, just an
// extant directory under the conventional path) and writes the row to
// the Agents table so igit.GetAgentWorktreePath returns a non-empty
// value.
func registerWorktree(t *testing.T, db *sql.DB, agent, repoPath string) string {
	t.Helper()
	wtRoot := filepath.Join(filepath.Dir(repoPath), ".force-worktrees", filepath.Base(repoPath), agent)
	if err := os.MkdirAll(wtRoot, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	if _, err := db.Exec(`INSERT OR REPLACE INTO Agents (agent_name, repo, worktree_path) VALUES (?, ?, ?)`,
		agent, repoPath, wtRoot); err != nil {
		t.Fatalf("Agents insert: %v", err)
	}
	return wtRoot
}

// agentBranchName returns a branch name that BranchAgentName will
// successfully extract `agent` from.
func agentBranchName(agent string, taskID int) string {
	return fmt.Sprintf("agent/%s/task-%d", agent, taskID)
}

// ── Case D — worktree missing ──────────────────────────────────────────────

func TestReconcile_MissingWorktreeQueuesReset(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	repoPath, repoName := reconcileFixture(t, db, "alpha")
	taskID := seedNonTerminal(t, db, repoName, "AwaitingCouncilReview", "")
	branch := agentBranchName("R2-D2", taskID)
	if _, err := db.Exec(`UPDATE BountyBoard SET branch_name = ? WHERE id = ?`, branch, taskID); err != nil {
		t.Fatalf("set branch_name: %v", err)
	}
	makeBranchAt(t, repoPath, branch)

	// Register a worktree path but DO NOT create the directory — that's
	// the divergence the reconciler must catch.
	wtRoot := filepath.Join(filepath.Dir(repoPath), ".force-worktrees", filepath.Base(repoPath), "R2-D2")
	if _, err := db.Exec(`INSERT OR REPLACE INTO Agents (agent_name, repo, worktree_path) VALUES (?, ?, ?)`,
		"R2-D2", repoPath, wtRoot); err != nil {
		t.Fatalf("Agents insert: %v", err)
	}

	if err := ReconcileOnStartup(context.Background(), db); err != nil {
		t.Fatalf("ReconcileOnStartup: %v", err)
	}

	// Assert a WorktreeReset task was queued for the parent.
	var resetCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type = 'WorktreeReset' AND parent_id = ?`, taskID).Scan(&resetCount); err != nil {
		t.Fatalf("count worktree-reset: %v", err)
	}
	if resetCount != 1 {
		t.Fatalf("expected 1 WorktreeReset for task #%d, got %d", taskID, resetCount)
	}

	// Audit log should record the action.
	var auditDetail string
	if err := db.QueryRow(`SELECT detail FROM AuditLog WHERE task_id = ? AND action = 'worktree-reset queued' ORDER BY id DESC LIMIT 1`,
		taskID).Scan(&auditDetail); err != nil {
		t.Fatalf("audit lookup: %v", err)
	}
	if !strings.Contains(auditDetail, "missing") {
		t.Errorf("audit detail missing 'missing' marker: %q", auditDetail)
	}
}

// ── Case B — branch missing pre-Captain ────────────────────────────────────

func TestReconcile_MissingBranchPreCaptain_ReturnsToPending(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	_, repoName := reconcileFixture(t, db, "beta")
	taskID := seedNonTerminal(t, db, repoName, "Locked", "feature/missing")
	// Set owner so we can verify it gets cleared on transition.
	if _, err := db.Exec(`UPDATE BountyBoard SET owner = 'BB-8' WHERE id = ?`, taskID); err != nil {
		t.Fatalf("set owner: %v", err)
	}

	// branch "feature/missing" intentionally never created.

	if err := ReconcileOnStartup(context.Background(), db); err != nil {
		t.Fatalf("ReconcileOnStartup: %v", err)
	}

	var status, branch, owner string
	if err := db.QueryRow(`SELECT status, IFNULL(branch_name, ''), IFNULL(owner, '') FROM BountyBoard WHERE id = ?`, taskID).
		Scan(&status, &branch, &owner); err != nil {
		t.Fatalf("read row back: %v", err)
	}
	if status != "Pending" {
		t.Errorf("status = %q, want Pending", status)
	}
	if branch != "" {
		t.Errorf("branch_name = %q, want empty", branch)
	}
	if owner != "" {
		t.Errorf("owner = %q, want empty (CAS clears)", owner)
	}
}

// Pattern P7 CAS behaviour: a Locked → Cancelled landed by an operator
// while the daemon was down must be preserved by the reconciler. When
// the snapshot says Locked but the row is now Cancelled, the CAS update
// is a no-op and the cancel survives.
func TestReconcile_RaceLost_OperatorCancelPreserved(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	_, repoName := reconcileFixture(t, db, "gamma")
	taskID := seedNonTerminal(t, db, repoName, "Locked", "feature/missing")

	// Snapshot the row as Locked, then race-flip to Cancelled before the
	// reconciler's CAS UPDATE lands. We simulate this by manually
	// running the same code paths: load snaps (sees Locked), then
	// operator cancels, then reconcileRow runs.
	snaps, err := loadNonTerminalSnaps(db)
	if err != nil {
		t.Fatalf("loadNonTerminalSnaps: %v", err)
	}
	if len(snaps) != 1 {
		t.Fatalf("want 1 snap, got %d", len(snaps))
	}
	if _, err := db.Exec(`UPDATE BountyBoard SET status = 'Cancelled' WHERE id = ?`, taskID); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	logger := NewLogger("reconcile-test")
	var counters reconcileCounters
	counters.total = 1
	if err := reconcileRow(context.Background(), db, logger, snaps[0], &counters); err != nil {
		t.Fatalf("reconcileRow: %v", err)
	}
	if counters.skippedRaceLost != 1 {
		t.Errorf("skippedRaceLost = %d, want 1", counters.skippedRaceLost)
	}
	var status, branch string
	if err := db.QueryRow(`SELECT status, IFNULL(branch_name, '') FROM BountyBoard WHERE id = ?`, taskID).
		Scan(&status, &branch); err != nil {
		t.Fatalf("read: %v", err)
	}
	if status != "Cancelled" {
		t.Errorf("status = %q, want Cancelled (operator cancel preserved)", status)
	}
	if branch != "feature/missing" {
		t.Errorf("branch_name = %q, want feature/missing (no clobber)", branch)
	}
}

// ── Case C — branch missing post-Captain ───────────────────────────────────

func TestReconcile_MissingBranchPostCaptain_Escalates(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	_, repoName := reconcileFixture(t, db, "delta")
	taskID := seedNonTerminal(t, db, repoName, "AwaitingCouncilReview", "feature/missing-post")

	if err := ReconcileOnStartup(context.Background(), db); err != nil {
		t.Fatalf("ReconcileOnStartup: %v", err)
	}

	var escCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM Escalations WHERE task_id = ? AND status = 'Open'`, taskID).
		Scan(&escCount); err != nil {
		t.Fatalf("escalations count: %v", err)
	}
	if escCount != 1 {
		t.Fatalf("expected 1 Open escalation for task #%d, got %d", taskID, escCount)
	}

	var sev string
	if err := db.QueryRow(`SELECT severity FROM Escalations WHERE task_id = ? AND status = 'Open'`, taskID).
		Scan(&sev); err != nil {
		t.Fatalf("escalation read: %v", err)
	}
	if sev != string(store.SeverityMedium) {
		t.Errorf("severity = %q, want %q", sev, store.SeverityMedium)
	}

	// Operator mail with [RECONCILE] subject.
	var subj string
	if err := db.QueryRow(`SELECT subject FROM Fleet_Mail WHERE to_agent = 'operator' AND subject LIKE '[RECONCILE]%' AND task_id = ? ORDER BY id DESC LIMIT 1`,
		taskID).Scan(&subj); err != nil {
		t.Fatalf("mail lookup: %v", err)
	}
	if !strings.Contains(subj, "branch disappeared") {
		t.Errorf("mail subject = %q, want it to mention branch disappeared", subj)
	}
}

// ── Case E — branch SHA divergence ─────────────────────────────────────────

// TestReconcile_BranchDivergedFromExpectedSHA_Escalates exercises Case E:
// branch + worktree present, but the most recent tree-hash recorded in
// recent_commit_hashes_json is no longer reachable from the branch. We
// stamp the row with a tree-hash that does NOT exist anywhere in the
// repo (a fake all-zeros placeholder) so the unreachability is
// deterministic without having to force-push.
func TestReconcile_BranchDivergedFromExpectedSHA_Escalates(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	repoPath, repoName := reconcileFixture(t, db, "epsilon")
	taskID := seedNonTerminal(t, db, repoName, "AwaitingCouncilReview", "")
	branch := agentBranchName("R5-D4", taskID)
	if _, err := db.Exec(`UPDATE BountyBoard SET branch_name = ? WHERE id = ?`, branch, taskID); err != nil {
		t.Fatalf("set branch_name: %v", err)
	}
	makeBranchAt(t, repoPath, branch)
	registerWorktree(t, db, "R5-D4", repoPath)

	// Stamp recent_commit_hashes_json with an unreachable tree-hash. The
	// real reachable tree-hashes come from `git log <branch> --pretty=%T`;
	// 40 zeros is syntactically valid but not part of the branch's history.
	unreachable := "0000000000000000000000000000000000000000"
	if _, err := db.Exec(`UPDATE BountyBoard SET recent_commit_hashes_json = ? WHERE id = ?`,
		fmt.Sprintf(`["%s"]`, unreachable), taskID); err != nil {
		t.Fatalf("stamp ring: %v", err)
	}

	if err := ReconcileOnStartup(context.Background(), db); err != nil {
		t.Fatalf("ReconcileOnStartup: %v", err)
	}

	// Assert: Open escalation created with severity Medium.
	var escCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM Escalations WHERE task_id = ? AND status = 'Open'`, taskID).
		Scan(&escCount); err != nil {
		t.Fatalf("escalations count: %v", err)
	}
	if escCount != 1 {
		t.Fatalf("expected 1 Open escalation for task #%d, got %d", taskID, escCount)
	}
	var sev string
	if err := db.QueryRow(`SELECT severity FROM Escalations WHERE task_id = ? AND status = 'Open'`, taskID).
		Scan(&sev); err != nil {
		t.Fatalf("escalation read: %v", err)
	}
	if sev != string(store.SeverityMedium) {
		t.Errorf("severity = %q, want %q", sev, store.SeverityMedium)
	}

	// Operator mail with [RECONCILE] subject mentioning divergence.
	var subj string
	if err := db.QueryRow(`SELECT subject FROM Fleet_Mail WHERE to_agent = 'operator' AND subject LIKE '[RECONCILE]%' AND task_id = ? ORDER BY id DESC LIMIT 1`,
		taskID).Scan(&subj); err != nil {
		t.Fatalf("mail lookup: %v", err)
	}
	if !strings.Contains(subj, "diverged") {
		t.Errorf("mail subject = %q, want it to mention diverged", subj)
	}

	// Audit log records the action.
	var auditDetail string
	if err := db.QueryRow(`SELECT detail FROM AuditLog WHERE task_id = ? AND action = 'branch-diverged → escalate' ORDER BY id DESC LIMIT 1`,
		taskID).Scan(&auditDetail); err != nil {
		t.Fatalf("audit lookup: %v", err)
	}
	if !strings.Contains(auditDetail, branch) {
		t.Errorf("audit detail = %q, want it to contain branch name %q", auditDetail, branch)
	}
}

// TestReconcile_BranchSHA_EmptyRing_NoAction asserts the Case E gate
// short-circuits to clean when the ring is empty, so freshly-created
// rows don't false-positive.
func TestReconcile_BranchSHA_EmptyRing_NoAction(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	repoPath, repoName := reconcileFixture(t, db, "zeta")
	taskID := seedNonTerminal(t, db, repoName, "AwaitingCouncilReview", "")
	branch := agentBranchName("BB-9", taskID)
	if _, err := db.Exec(`UPDATE BountyBoard SET branch_name = ? WHERE id = ?`, branch, taskID); err != nil {
		t.Fatalf("set branch_name: %v", err)
	}
	makeBranchAt(t, repoPath, branch)
	registerWorktree(t, db, "BB-9", repoPath)
	// recent_commit_hashes_json defaults to '[]' — leave it that way.

	if err := ReconcileOnStartup(context.Background(), db); err != nil {
		t.Fatalf("ReconcileOnStartup: %v", err)
	}

	var escCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM Escalations WHERE task_id = ?`, taskID).Scan(&escCount); err != nil {
		t.Fatalf("escalations count: %v", err)
	}
	if escCount != 0 {
		t.Errorf("expected 0 escalations for empty-ring row, got %d", escCount)
	}
}

// TestReconcile_BranchSHA_ReachableTree_NoAction confirms a recorded
// tree-hash that IS reachable via `git log <branch> --pretty=%T` does not
// trigger Case E.
func TestReconcile_BranchSHA_ReachableTree_NoAction(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	repoPath, repoName := reconcileFixture(t, db, "eta")
	taskID := seedNonTerminal(t, db, repoName, "AwaitingCouncilReview", "")
	branch := agentBranchName("R7-A7", taskID)
	if _, err := db.Exec(`UPDATE BountyBoard SET branch_name = ? WHERE id = ?`, branch, taskID); err != nil {
		t.Fatalf("set branch_name: %v", err)
	}
	makeBranchAt(t, repoPath, branch)
	registerWorktree(t, db, "R7-A7", repoPath)

	// Read the actual tree-hash for HEAD and stamp it into the ring.
	out, err := exec.Command("git", "-C", repoPath, "rev-parse", branch+"^{tree}").Output()
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	tree := strings.TrimSpace(string(out))
	if _, err := db.Exec(`UPDATE BountyBoard SET recent_commit_hashes_json = ? WHERE id = ?`,
		fmt.Sprintf(`["%s"]`, tree), taskID); err != nil {
		t.Fatalf("stamp ring: %v", err)
	}

	if err := ReconcileOnStartup(context.Background(), db); err != nil {
		t.Fatalf("ReconcileOnStartup: %v", err)
	}

	var escCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM Escalations WHERE task_id = ?`, taskID).Scan(&escCount); err != nil {
		t.Fatalf("escalations count: %v", err)
	}
	if escCount != 0 {
		t.Errorf("expected 0 escalations for reachable-tree row, got %d", escCount)
	}
}

// ── Happy path — clean fleet, no actions ───────────────────────────────────

func TestReconcile_CleanState_NoActions(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	repoPath, repoName := reconcileFixture(t, db, "clean")

	// Five clean rows: branches present, persistent worktrees on-disk for
	// rows that have an agent name in the branch.
	for i := 0; i < 5; i++ {
		taskID := seedNonTerminal(t, db, repoName, "AwaitingCouncilReview", "")
		agent := fmt.Sprintf("Agent-%d", i)
		branch := agentBranchName(agent, taskID)
		if _, err := db.Exec(`UPDATE BountyBoard SET branch_name = ? WHERE id = ?`, branch, taskID); err != nil {
			t.Fatalf("set branch: %v", err)
		}
		makeBranchAt(t, repoPath, branch)
		registerWorktree(t, db, agent, repoPath)
	}

	mailBefore := mailCount(t, db)
	escBefore := escalationCount(t, db)
	resetBefore := worktreeResetCount(t, db)

	if err := ReconcileOnStartup(context.Background(), db); err != nil {
		t.Fatalf("ReconcileOnStartup: %v", err)
	}

	if got := worktreeResetCount(t, db); got != resetBefore {
		t.Errorf("WorktreeReset count delta = %d, want 0 (clean fleet)", got-resetBefore)
	}
	if got := escalationCount(t, db); got != escBefore {
		t.Errorf("Open escalation count delta = %d, want 0 (clean fleet)", got-escBefore)
	}
	if got := mailCount(t, db); got != mailBefore {
		t.Errorf("Fleet_Mail count delta = %d, want 0 (no summary mail when divergence=0)", got-mailBefore)
	}

	// Idempotence — a second run on the same clean state must also be a no-op.
	if err := ReconcileOnStartup(context.Background(), db); err != nil {
		t.Fatalf("ReconcileOnStartup (2nd pass): %v", err)
	}
	if got := worktreeResetCount(t, db); got != resetBefore {
		t.Errorf("idempotence: WorktreeReset count drifted on second pass: %d (want %d)", got, resetBefore)
	}
	if got := escalationCount(t, db); got != escBefore {
		t.Errorf("idempotence: escalation count drifted on second pass: %d (want %d)", got, escBefore)
	}
}

// Idempotence after Case B fires: row gets re-pended with empty branch on
// pass 1, then pass 2 sees an empty branch_name and treats it as clean
// (no further state mutations).
func TestReconcile_Idempotent_AfterCaseB(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	_, repoName := reconcileFixture(t, db, "epsilon")
	taskID := seedNonTerminal(t, db, repoName, "Locked", "feature/missing")

	// Pass 1.
	if err := ReconcileOnStartup(context.Background(), db); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	var status1, branch1 string
	db.QueryRow(`SELECT status, IFNULL(branch_name, '') FROM BountyBoard WHERE id = ?`, taskID).Scan(&status1, &branch1)

	// Snapshot mail counts before pass 2.
	mailBeforePass2 := mailCount(t, db)

	// Pass 2.
	if err := ReconcileOnStartup(context.Background(), db); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	var status2, branch2 string
	db.QueryRow(`SELECT status, IFNULL(branch_name, '') FROM BountyBoard WHERE id = ?`, taskID).Scan(&status2, &branch2)

	if status1 != status2 || branch1 != branch2 {
		t.Errorf("idempotence broken: pass 1 (%q, %q) vs pass 2 (%q, %q)", status1, branch1, status2, branch2)
	}
	if got := mailCount(t, db); got != mailBeforePass2 {
		t.Errorf("idempotence: pass 2 sent %d new mails (want 0 — divergence=0 on second pass)", got-mailBeforePass2)
	}
}

// ── Performance budget ─────────────────────────────────────────────────────

func TestReconcile_PerfBudget_100Tasks(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping perf test in -short mode")
	}
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	repoPath, repoName := reconcileFixture(t, db, "perf")

	// Mix of cases: 25 clean (with worktree), 25 clean (no agent name —
	// short-circuit), 25 branch-missing-pre-captain, 25 branch-missing-post-captain.
	for i := 0; i < 25; i++ {
		id := seedNonTerminal(t, db, repoName, "AwaitingCouncilReview", "")
		agent := fmt.Sprintf("Perf-A%d", i)
		branch := agentBranchName(agent, id)
		db.Exec(`UPDATE BountyBoard SET branch_name = ? WHERE id = ?`, branch, id)
		makeBranchAt(t, repoPath, branch)
		registerWorktree(t, db, agent, repoPath)
	}
	for i := 0; i < 25; i++ {
		id := seedNonTerminal(t, db, repoName, "AwaitingCaptainReview", "")
		branch := fmt.Sprintf("plain-branch-%d", id)
		db.Exec(`UPDATE BountyBoard SET branch_name = ? WHERE id = ?`, branch, id)
		makeBranchAt(t, repoPath, branch)
	}
	for i := 0; i < 25; i++ {
		id := seedNonTerminal(t, db, repoName, "Locked", "")
		branch := fmt.Sprintf("missing-pre-%d", id)
		db.Exec(`UPDATE BountyBoard SET branch_name = ? WHERE id = ?`, branch, id)
	}
	for i := 0; i < 25; i++ {
		id := seedNonTerminal(t, db, repoName, "AwaitingCouncilReview", "")
		branch := fmt.Sprintf("missing-post-%d", id)
		db.Exec(`UPDATE BountyBoard SET branch_name = ? WHERE id = ?`, branch, id)
	}

	start := time.Now()
	if err := ReconcileOnStartup(context.Background(), db); err != nil {
		t.Fatalf("ReconcileOnStartup: %v", err)
	}
	elapsed := time.Since(start)

	const budget = 60 * time.Second
	if elapsed > budget {
		t.Fatalf("perf budget broken: %v > %v (100-task sweep)", elapsed, budget)
	}
	t.Logf("100-task sweep: %v (budget %v)", elapsed, budget)
}

// ── small DB-state helpers ─────────────────────────────────────────────────

func mailCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM Fleet_Mail`).Scan(&n); err != nil {
		t.Fatalf("count Fleet_Mail: %v", err)
	}
	return n
}

func escalationCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM Escalations WHERE status = 'Open'`).Scan(&n); err != nil {
		t.Fatalf("count Escalations: %v", err)
	}
	return n
}

func worktreeResetCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type = 'WorktreeReset'`).Scan(&n); err != nil {
		t.Fatalf("count WorktreeReset: %v", err)
	}
	return n
}
