package agents

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	igit "force-orchestrator/internal/git"
	"force-orchestrator/internal/store"
)

// nonTerminalReconcileStatuses enumerates the BountyBoard statuses that
// represent live work — anything in this set has a disk/git state the
// reconciler must verify. Adding a new task status requires updating this
// list AND the divergence matrix in CLAUDE.md (D2 T1-0).
var nonTerminalReconcileStatuses = []string{
	"Pending",
	"Locked",
	"AwaitingCaptainReview",
	"AwaitingCouncilReview",
	"AwaitingDraftPR",
	"DraftPROpen",
	"Escalated",
}

// reconcileShortGitTimeout caps each per-row git lookup so a wedged repo
// can't block the whole sweep. The 60s sweep budget for 100 tasks
// (TestReconcile_PerfBudget_100Tasks) factors this in.
const reconcileShortGitTimeout = 5 * time.Second

// reconcileErrorRateMax is the fraction of rows that may error before
// ReconcileOnStartup returns an error (treated as fatal by the daemon
// wire-in). 10% per CLAUDE.md "no silent error swallowing" guidance.
const reconcileErrorRateMax = 0.10

// reconcileCounters tallies the actions taken by a single sweep.
type reconcileCounters struct {
	total                    int
	clean                    int
	branchMissingPreCaptain  int
	branchMissingPostCaptain int
	worktreeMissing          int
	branchSHADiverged        int // Case E: branch + worktree present but recorded tree-hash unreachable (D2 T1-3.5)
	skippedRaceLost          int // CAS race-loss in Case B (operator cancelled while we were loading)
	errors                   int
}

// reconcileRowSnap is a frozen view of a BountyBoard row at sweep-start.
// We snapshot the cursor results before doing any disk/git I/O so the
// per-row work can run sequentially without holding the rows iterator.
type reconcileRowSnap struct {
	ID         int
	ParentID   int
	TargetRepo string
	Type       string
	Status     string
	BranchName string
}

// ReconcileOnStartup sweeps every non-terminal BountyBoard row against the
// actual disk/git state. It is invoked once at daemon startup, immediately
// after store.ReleaseInFlightTasks. The caller treats a non-nil return as
// fatal — continuing with an unreliable view of the fleet would compound
// the corruption (CLAUDE.md "no silent failures").
//
// D2 T1-0 — five divergence cases:
//
//	A. Clean                          — branch exists, worktree exists. No action.
//	B. Branch missing, pre-Captain    — Pending/Locked + branch_name set + branch absent.
//	                                    CAS-transition to Pending with branch_name=''.
//	C. Branch missing, post-Captain   — branch absent at AwaitingCaptainReview / AwaitingCouncilReview /
//	                                    AwaitingDraftPR / DraftPROpen / Escalated. Escalate (Medium).
//	D. Worktree missing               — branch exists but the registered .force-worktrees/<repo>/<agent>
//	                                    directory is absent. QueueWorktreeReset (idempotent).
//	E. Branch SHA diverged            — recorded tree-hash no longer reachable from the branch.
//	                                    Activated by D2 T1-3.5: reads BountyBoard.recent_commit_hashes_json
//	                                    and verifies the most recent entry is reachable via
//	                                    `git log <branch> --pretty=%T`. Empty ring = clean.
//
// Idempotence: a clean second run produces zero state mutations on rows
// the first run already moved. Cases C and E may re-fire because they
// only produce escalations, not state changes; CreateEscalation's partial
// UNIQUE on (task_id) WHERE status='Open' makes the re-fire a no-op.
func ReconcileOnStartup(ctx context.Context, db *sql.DB) error {
	logger := NewLogger("Reconcile")

	snaps, err := loadNonTerminalSnaps(db)
	if err != nil {
		return fmt.Errorf("ReconcileOnStartup: %w", err)
	}

	var counters reconcileCounters
	counters.total = len(snaps)

	for _, s := range snaps {
		if cErr := ctx.Err(); cErr != nil {
			return fmt.Errorf("ReconcileOnStartup: ctx cancelled mid-sweep: %w", cErr)
		}
		if err := reconcileRow(ctx, db, logger, s, &counters); err != nil {
			counters.errors++
			logger.Printf("[RECONCILE] ERROR task #%d: %v", s.ID, err)
			store.LogAudit(db, "reconcile", "error", s.ID, err.Error())
		}
	}

	if counters.total > 0 {
		threshold := int(float64(counters.total)*reconcileErrorRateMax) + 1
		if counters.errors >= threshold {
			return fmt.Errorf("ReconcileOnStartup: error rate too high (%d/%d, threshold=%d)",
				counters.errors, counters.total, threshold)
		}
	}

	emitReconcileSummary(db, logger, counters)
	return nil
}

// loadNonTerminalSnaps reads every BountyBoard row in a non-terminal
// status into memory, releasing the cursor before any disk/git I/O.
func loadNonTerminalSnaps(db *sql.DB) ([]reconcileRowSnap, error) {
	placeholders := make([]string, len(nonTerminalReconcileStatuses))
	args := make([]any, len(nonTerminalReconcileStatuses))
	for i, s := range nonTerminalReconcileStatuses {
		placeholders[i] = "?"
		args[i] = s
	}
	q := fmt.Sprintf(`SELECT id, IFNULL(parent_id, 0), IFNULL(target_repo, ''), IFNULL(type, ''),
		status, IFNULL(branch_name, '')
		FROM BountyBoard WHERE status IN (%s) ORDER BY id`,
		strings.Join(placeholders, ","))

	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("query non-terminal rows: %w", err)
	}
	defer rows.Close()

	var snaps []reconcileRowSnap
	for rows.Next() {
		var s reconcileRowSnap
		if scanErr := rows.Scan(&s.ID, &s.ParentID, &s.TargetRepo, &s.Type, &s.Status, &s.BranchName); scanErr != nil {
			return nil, fmt.Errorf("scan: %w", scanErr)
		}
		snaps = append(snaps, s)
	}
	if rErr := rows.Err(); rErr != nil {
		return nil, fmt.Errorf("rows iter: %w", rErr)
	}
	return snaps, nil
}

// reconcileRow runs the divergence matrix against a single row.
func reconcileRow(ctx context.Context, db *sql.DB, logger *log.Logger, s reconcileRowSnap, c *reconcileCounters) error {
	// A row with no branch_name has no disk/git state to verify — the task
	// hasn't started coding yet (or already cleared its branch). Treat as
	// clean; second-run idempotency relies on Case B leaving rows in
	// exactly this state.
	if s.BranchName == "" {
		c.clean++
		logger.Printf("[RECONCILE] task #%d clean (no branch_name)", s.ID)
		return nil
	}

	if s.TargetRepo == "" {
		c.clean++
		logger.Printf("[RECONCILE] task #%d skipped (no target_repo)", s.ID)
		return nil
	}
	repo := store.GetRepo(db, s.TargetRepo)
	if repo == nil || repo.LocalPath == "" {
		c.clean++
		logger.Printf("[RECONCILE] task #%d skipped (repo %q not registered)", s.ID, s.TargetRepo)
		return nil
	}

	branchPresent := branchExistsLocal(ctx, repo.LocalPath, s.BranchName)

	if !branchPresent {
		return reconcileBranchMissing(db, logger, s, c)
	}

	// Branch exists. Check the persistent worktree for this agent+repo,
	// if one is registered. A row whose branch encodes no agent name
	// (legacy `agent/task-N` or non-persistent branches) has no
	// expectation of a registered worktree, so we treat it as clean.
	agentName := BranchAgentName(s.BranchName)
	if agentName == "" {
		c.clean++
		logger.Printf("[RECONCILE] task #%d clean (branch has no agent name; nothing to reconcile)", s.ID)
		return nil
	}

	worktreePath := igit.GetAgentWorktreePath(db, agentName, repo.LocalPath)
	if worktreePath == "" {
		// Agent has never registered a worktree for this repo. Nothing
		// to reconcile against.
		c.clean++
		logger.Printf("[RECONCILE] task #%d clean (no registered worktree for agent=%s repo=%s)", s.ID, agentName, s.TargetRepo)
		return nil
	}

	if _, statErr := os.Stat(worktreePath); statErr != nil {
		if os.IsNotExist(statErr) {
			return reconcileWorktreeMissing(ctx, db, logger, s, repo.LocalPath, agentName, c)
		}
		return fmt.Errorf("os.Stat worktree %q: %w", worktreePath, statErr)
	}

	// Case E (D2 T1-3.5): branch + worktree present, but the most recent
	// tree-hash recorded by the divergence detector is no longer reachable
	// from the branch — i.e., the branch was force-pushed externally and
	// our task-owned commit was lost. Escalate.
	diverged, divErr := branchDivergedFromRecordedTree(ctx, db, repo.LocalPath, s)
	if divErr != nil {
		return fmt.Errorf("branchDivergedFromRecordedTree: %w", divErr)
	}
	if diverged {
		return reconcileBranchDiverged(db, logger, s, c)
	}

	// Case A: clean.
	c.clean++
	logger.Printf("[RECONCILE] task #%d clean", s.ID)
	return nil
}

// branchDivergedFromRecordedTree implements the Case E predicate. Returns
// true iff the BountyBoard.recent_commit_hashes_json column has at least
// one entry AND the most recent entry is NOT reachable from the recorded
// branch via `git log <branch> --pretty=%T`. An empty ring (no recorded
// commit) is NOT divergence — it just means the task hasn't committed
// anything yet that we'd want to verify.
func branchDivergedFromRecordedTree(ctx context.Context, db *sql.DB, repoLocalPath string, s reconcileRowSnap) (bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	ring, err := loadCommitHashRingTx(tx, s.ID)
	if err != nil {
		return false, err
	}
	if commitErr := tx.Commit(); commitErr != nil {
		return false, fmt.Errorf("commit tx: %w", commitErr)
	}
	if len(ring.Hashes) == 0 {
		return false, nil
	}
	expected := strings.TrimSpace(ring.Hashes[len(ring.Hashes)-1])
	if expected == "" {
		return false, nil
	}
	if err := igit.ValidateRef(s.BranchName); err != nil {
		// Defensive — the snapshot already passed branchExistsLocal which
		// runs the same validator, so this would be a ref invalidated
		// between the two checks. Fall through to "not diverged" so we
		// don't escalate on a transient validation race.
		return false, nil
	}
	lookupCtx, cancel := context.WithTimeout(ctx, reconcileShortGitTimeout)
	defer cancel()
	out, gitErr := igit.LogAndRun(lookupCtx, igit.OpContext{Branch: s.BranchName},
		"log", "git", "-C", repoLocalPath,
		"log", "--pretty=%T", s.BranchName, "--")
	if gitErr != nil {
		return false, fmt.Errorf("git log %s: %w", s.BranchName, gitErr)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == expected {
			return false, nil
		}
	}
	return true, nil
}

// reconcileBranchDiverged handles Case E.
func reconcileBranchDiverged(db *sql.DB, logger *log.Logger, s reconcileRowSnap, c *reconcileCounters) error {
	msg := fmt.Sprintf(
		"branch %s for task at status %s no longer contains the task-owned commit (recorded tree-hash unreachable). "+
			"The branch was likely force-pushed or rewritten externally.",
		s.BranchName, s.Status,
	)
	if _, escErr := CreateEscalation(db, s.ID, store.SeverityMedium, msg); escErr != nil {
		if failErr := store.FailBounty(db, s.ID, fmt.Sprintf(
			"reconcile: branch %s diverged; escalation insert failed: %v", s.BranchName, escErr,
		)); failErr != nil {
			return fmt.Errorf("CreateEscalation failed (%v) AND FailBounty failed (%v)", escErr, failErr)
		}
	}
	subj := fmt.Sprintf("[RECONCILE] branch diverged for task #%d at status %s", s.ID, s.Status)
	store.SendMail(db, "Reconcile", "operator", subj, msg, s.ID, store.MailTypeAlert)
	store.LogAudit(db, "reconcile", "branch-diverged → escalate", s.ID,
		fmt.Sprintf("branch=%s status=%s", s.BranchName, s.Status))
	logger.Printf("[RECONCILE] task #%d branch diverged → escalated (status=%s, branch=%s)",
		s.ID, s.Status, s.BranchName)
	c.branchSHADiverged++
	return nil
}

// reconcileBranchMissing handles Cases B and C.
func reconcileBranchMissing(db *sql.DB, logger *log.Logger, s reconcileRowSnap, c *reconcileCounters) error {
	preCaptain := s.Status == "Pending" || s.Status == "Locked"
	if preCaptain {
		// Case B: re-pending with cleared branch_name. Pattern P7:
		// CAS the status transition so a concurrent operator cancel
		// (Locked → Cancelled) cannot be clobbered.
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("begin tx: %w", err)
		}
		n, casErr := store.UpdateBountyStatusFromTx(tx, s.ID, s.Status, "Pending")
		if casErr != nil {
			_ = tx.Rollback()
			return fmt.Errorf("UpdateBountyStatusFromTx: %w", casErr)
		}
		if n == 0 {
			_ = tx.Rollback()
			c.skippedRaceLost++
			logger.Printf("[RECONCILE] task #%d branch missing pre-Captain but row state changed since load (lost race) — skipping", s.ID)
			return nil
		}
		if clearErr := store.ClearBranchNameTx(tx, s.ID); clearErr != nil {
			_ = tx.Rollback()
			return fmt.Errorf("ClearBranchNameTx: %w", clearErr)
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return fmt.Errorf("commit tx: %w", commitErr)
		}
		c.branchMissingPreCaptain++
		store.LogAudit(db, "reconcile", "branch-missing-pre-captain → re-pending", s.ID,
			fmt.Sprintf("branch=%s status_was=%s", s.BranchName, s.Status))
		logger.Printf("[RECONCILE] task #%d branch missing pre-Captain → re-pending (was=%s, branch=%s)",
			s.ID, s.Status, s.BranchName)
		return nil
	}

	// Case C: post-Captain — escalate.
	msg := fmt.Sprintf("branch %s disappeared for task at status %s; reconcile-on-startup detected. "+
		"Either the work was lost (force-pushed externally / branch deleted on remote) or the DB row is stale.",
		s.BranchName, s.Status)
	if _, escErr := CreateEscalation(db, s.ID, store.SeverityMedium, msg); escErr != nil {
		// CreateEscalation's contract: even on failure, fall back to
		// FailBounty + operator mail per Fix #8 Phase A. Reconcile is
		// the system-level last line of defence — surface the failure
		// rather than swallowing it.
		if failErr := store.FailBounty(db, s.ID, fmt.Sprintf("reconcile: branch %s disappeared; escalation insert failed: %v", s.BranchName, escErr)); failErr != nil {
			return fmt.Errorf("CreateEscalation failed (%v) AND FailBounty failed (%v)", escErr, failErr)
		}
	}
	subj := fmt.Sprintf("[RECONCILE] branch disappeared for task #%d at status %s", s.ID, s.Status)
	store.SendMail(db, "Reconcile", "operator", subj, msg, s.ID, store.MailTypeAlert)
	store.LogAudit(db, "reconcile", "branch-missing-post-captain → escalate", s.ID,
		fmt.Sprintf("branch=%s status=%s", s.BranchName, s.Status))
	logger.Printf("[RECONCILE] task #%d branch missing post-Captain → escalated (status=%s, branch=%s)",
		s.ID, s.Status, s.BranchName)
	c.branchMissingPostCaptain++
	return nil
}

// reconcileWorktreeMissing handles Case D.
func reconcileWorktreeMissing(ctx context.Context, db *sql.DB, logger *log.Logger, s reconcileRowSnap, repoLocalPath, agentName string, c *reconcileCounters) error {
	defBranch := igit.GetDefaultBranch(ctx, repoLocalPath)
	if defBranch == "" {
		// Defensive — GetDefaultBranch falls back to "main" itself, but
		// keep the guard explicit so a future refactor can't silently
		// queue WorktreeReset with TargetBranch=''.
		return fmt.Errorf("could not determine default branch for repo %s", s.TargetRepo)
	}
	payload := worktreeResetPayload{
		ParentTaskID: s.ID,
		Repo:         s.TargetRepo,
		TargetBranch: defBranch,
		Agents:       []string{agentName},
		Reason:       "reconcile-on-startup: worktree missing",
	}
	if _, qErr := QueueWorktreeReset(db, payload); qErr != nil {
		return fmt.Errorf("QueueWorktreeReset: %w", qErr)
	}
	c.worktreeMissing++
	store.LogAudit(db, "reconcile", "worktree-reset queued", s.ID,
		fmt.Sprintf("agent=%s repo=%s reason=missing", agentName, s.TargetRepo))
	logger.Printf("[RECONCILE] task #%d worktree-reset queued reason=missing (agent=%s)", s.ID, agentName)
	return nil
}

// branchExistsLocal returns true if the named branch can be resolved by
// `git rev-parse --verify` in repoPath. The trailing `--` enforces ref
// positionality per Fix #9 / Pattern P10. Validation is run first so a
// bogus branch_name returned from the DB never reaches the shell.
func branchExistsLocal(ctx context.Context, repoPath, branch string) bool {
	if err := igit.ValidateRef(branch); err != nil {
		return false
	}
	lookupCtx, cancel := context.WithTimeout(ctx, reconcileShortGitTimeout)
	defer cancel()
	_, err := igit.LogAndRun(lookupCtx, igit.OpContext{Branch: branch},
		"rev-parse", "git", "-C", repoPath, "rev-parse", "--verify", branch, "--")
	return err == nil
}

// emitReconcileSummary writes a [RECONCILE SUMMARY] line to the log and,
// when any divergence was handled, sends an operator mail describing the
// counts. Silent reconciles are forbidden per CLAUDE.md "no silent
// failures."
func emitReconcileSummary(db *sql.DB, logger *log.Logger, c reconcileCounters) {
	logger.Printf("[RECONCILE SUMMARY] total=%d clean=%d branch-missing-pre-captain=%d branch-missing-post-captain=%d worktree-missing=%d branch-diverged=%d race-lost=%d errors=%d",
		c.total, c.clean,
		c.branchMissingPreCaptain, c.branchMissingPostCaptain,
		c.worktreeMissing, c.branchSHADiverged,
		c.skippedRaceLost, c.errors)

	divergence := c.branchMissingPreCaptain + c.branchMissingPostCaptain + c.worktreeMissing + c.branchSHADiverged
	if divergence == 0 {
		return
	}

	var body strings.Builder
	fmt.Fprintf(&body, "Reconcile-on-startup handled %d divergence(s):\n\n", divergence)
	if c.branchMissingPreCaptain > 0 {
		fmt.Fprintf(&body, "  - %d task(s) had a branch_name set but the branch was absent and status was Pending/Locked: re-set to Pending with branch_name='' (auto-recovered).\n", c.branchMissingPreCaptain)
	}
	if c.branchMissingPostCaptain > 0 {
		fmt.Fprintf(&body, "  - %d task(s) had their branch disappear post-Captain: escalated for operator review.\n", c.branchMissingPostCaptain)
	}
	if c.worktreeMissing > 0 {
		fmt.Fprintf(&body, "  - %d task(s) had a missing persistent worktree: WorktreeReset queued (auto-recovered).\n", c.worktreeMissing)
	}
	if c.branchSHADiverged > 0 {
		fmt.Fprintf(&body, "  - %d task(s) had branch SHA diverged from recorded SHA: escalated for operator review.\n", c.branchSHADiverged)
	}
	if c.skippedRaceLost > 0 {
		fmt.Fprintf(&body, "\n  (%d row(s) skipped: state changed mid-sweep, no action taken.)\n", c.skippedRaceLost)
	}
	if c.errors > 0 {
		fmt.Fprintf(&body, "\n  (%d row(s) errored — see [RECONCILE] ERROR lines in fleet.log.)\n", c.errors)
	}
	fmt.Fprintf(&body, "\nTotal non-terminal tasks at startup: %d\n", c.total)

	store.SendMail(db, "Reconcile", "operator",
		"[RECONCILE SUMMARY] divergences handled at daemon start",
		body.String(), 0, store.MailTypeAlert)
}
