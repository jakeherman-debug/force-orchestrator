package store

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
)

// ── Task-type classification ─────────────────────────────────────────────────
//
// The fleet runs two categories of work:
//   1. Operator-facing work — tasks that reflect something the operator asked
//      for, or direct children thereof (Feature, Decompose, CodeEdit).
//   2. Fleet infrastructure — bookkeeping, git ops, rebases, memory writes,
//      diplomatic ceremony, failure triage. These are how the fleet heals and
//      maintains itself; the operator only cares when something in this
//      bucket can't self-heal (Failed / Escalated).
//
// Dashboard surfaces hide infrastructure tasks by default to cut noise and
// surface them selectively on failure. Tests, CLI tools, and internal logic
// continue to see every row.

// InfrastructureTaskTypes is the canonical list of task types considered
// "fleet plumbing" rather than operator work. Keep synchronized with the
// list referenced in SQL filters below.
var InfrastructureTaskTypes = []string{
	"FindPRTemplate",
	"CreateAskBranch",
	"CleanupAskBranch",
	"RebaseAskBranch",
	"RebaseAgentBranch",
	"RevalidateRepoConfig",
	"WriteMemory",
	"ShipConvoy",
	"CIFailureTriage",
	"MedicReview",
	"PRReviewTriage",
	"ConvoyReview",
	"WorktreeReset",
	"BoSReview", // D4 Phase 1 — Bureau of Standards commit-time review
	"ISBReview", // D4 Phase 2 — Imperial Security Bureau commit-time review
	// D9 — Archaeologist: weekly per-repo debt-pattern sweep + migration-proposal
	// fan-out. Both are infrastructure (operator only sees them on failure).
	"ArchaeologistSweep",
	"ArchaeologistProposeMigration",
	"PRHandoffSynthesis",        // D10 — auto-generated reviewer narrative on draft PRs (opt-in)
	"ConsumerIntegrationCheck",  // D8 Track 3 — synthetic integration test of consumer repos against producer's ask-branch
	"SenatorRefresh",            // D14 Phase 2 — periodic re-onboarding for active Senators (knowledge_digest, rule_suggestions, tag_suggestions)
	"MigrationClassifyProposals", // D14 Phase 5 — one-shot LLM classifier for pending PromotionProposals (knowledge vs rule)
}

var infrastructureTaskTypeSet = func() map[string]bool {
	m := make(map[string]bool, len(InfrastructureTaskTypes))
	for _, t := range InfrastructureTaskTypes {
		m[t] = true
	}
	return m
}()

// IsInfrastructureTask reports whether a task type is fleet plumbing (hidden
// from the dashboard task list by default unless it has Failed or Escalated).
func IsInfrastructureTask(taskType string) bool {
	return infrastructureTaskTypeSet[taskType]
}

// InfrastructureTaskTypesSQLList renders the infrastructure task types as a
// comma-separated SQL string-list suitable for a NOT IN (...) clause.
// Emits single-quoted identifiers, safe because InfrastructureTaskTypes is a
// hardcoded compile-time constant (no user input).
func InfrastructureTaskTypesSQLList() string {
	parts := make([]string, len(InfrastructureTaskTypes))
	for i, t := range InfrastructureTaskTypes {
		parts[i] = "'" + t + "'"
	}
	return strings.Join(parts, ",")
}

// ── BountyBoard ───────────────────────────────────────────────────────────────

func GetBounty(db *sql.DB, id int) (*Bounty, error) {
	var b Bounty
	err := db.QueryRow(`
		SELECT id, parent_id, target_repo, type, status, payload,
		       owner, retry_count, infra_failures, convoy_id, checkpoint, branch_name,
		       priority, IFNULL(task_timeout,0),
		       IFNULL(medic_requeue_count,0), IFNULL(reshard_generation,0)
		FROM BountyBoard WHERE id = ?`, id).
		Scan(&b.ID, &b.ParentID, &b.TargetRepo, &b.Type, &b.Status,
			&b.Payload, &b.Owner, &b.RetryCount, &b.InfraFailures, &b.ConvoyID,
			&b.Checkpoint, &b.BranchName, &b.Priority, &b.TaskTimeout,
			&b.MedicRequeueCount, &b.ReshardGeneration)
	return &b, err
}

// ClaimBounty atomically claims the next available task using optimistic locking.
// Higher-priority tasks (priority DESC) are claimed first; ties broken by id ASC (FIFO).
// A task is claimable only when all its dependencies (in TaskDependencies) are Completed.
//
// AUDIT-068 (Fix #8d): distinguishes sql.ErrNoRows (benign, nothing to claim)
// from real driver/schema errors — the latter are logged so a silent fleet
// stall (BountyBoard table dropped, migration gap, FK constraint surprise)
// is observable to the operator via the daemon log, rather than looking
// identical to "queue empty."
//
// D5.5 P2 γ — Pattern P-StageGate (No astromech pre-staging): stage-scoped
// tasks (BountyBoard.stage_id IS NOT NULL) are filtered out when their
// owning ConvoyStages.status is still 'Pending'. The stage transitions to
// 'Open' (or beyond) before any astromech can hold a worktree on its work.
// Tasks with stage_id IS NULL (legacy / single-mode convoys) are unaffected
// — the NULL branch of the OR short-circuits before the JOIN materialises.
// The existing idx_bounty_stage_id index keeps the lookup point-and-shoot.
func ClaimBounty(db *sql.DB, taskType string, agentName string) (*Bounty, bool) {
	var b Bounty
	// D2 T1-1: filter out spend_suspended rows. dogTaskSpendWatch sets the
	// flag when a single task's trailing-10-min cost exceeds the escalate
	// threshold; skipping at the claim query (rather than at task pickup
	// time) keeps the gate atomic — no race where the spawn loop picks up
	// the row between the Read and the suspended check.
	err := db.QueryRow(`
		SELECT id, parent_id, target_repo, type, status, payload, convoy_id, checkpoint,
		       priority, IFNULL(task_timeout,0)
		FROM BountyBoard
		WHERE status = 'Pending' AND type = ?
		  AND IFNULL(spend_suspended, 0) = 0
		  AND NOT EXISTS (
		    SELECT 1 FROM TaskDependencies td
		    JOIN BountyBoard dep ON dep.id = td.depends_on
		    WHERE td.task_id = BountyBoard.id AND dep.status != 'Completed'
		  )
		  AND (convoy_id = 0 OR NOT EXISTS (
		    SELECT 1 FROM FeatureBlockers fb
		    WHERE fb.blocked_convoy_id = BountyBoard.convoy_id AND fb.resolved_at IS NULL
		  ))
		  AND (stage_id IS NULL OR EXISTS (
		    SELECT 1 FROM ConvoyStages cs
		    WHERE cs.id = BountyBoard.stage_id AND cs.status != 'Pending'
		  ))
		ORDER BY priority DESC, id ASC
		LIMIT 1`, taskType).
		Scan(&b.ID, &b.ParentID, &b.TargetRepo, &b.Type, &b.Status,
			&b.Payload, &b.ConvoyID, &b.Checkpoint, &b.Priority, &b.TaskTimeout)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("ClaimBounty(type=%s, agent=%s): DB error (not ErrNoRows): %v", taskType, agentName, err)
		}
		return nil, false
	}
	res, err := db.Exec(`
		UPDATE BountyBoard SET status = 'Locked', owner = ?, locked_at = datetime('now')
		WHERE id = ? AND status = 'Pending'`, agentName, b.ID)
	if err != nil {
		log.Printf("ClaimBounty(id=%d, agent=%s): lock UPDATE failed: %v", b.ID, agentName, err)
		return nil, false
	}
	rows, _ := res.RowsAffected()
	if rows == 1 {
		b.Status = "Locked"
		b.Owner = agentName
		return &b, true
	}
	return nil, false
}

// ClaimBountyForWrite is the astromech-specific claim variant. It mirrors
// ClaimBounty but additionally requires the target repo to be in
// mode='write'. Repos in 'read_only' or 'quarantined' mode are skipped at
// the SQL level so the astromech never wakes up on them — the
// AssertRepoWritable guard in destructive git ops is the
// belt-and-suspenders layer for the rare race where a repo is stepped
// down between claim and push.
//
// The (target_repo IS NULL OR target_repo = '') guard accommodates legacy
// rows that pre-date the Repositories registry — they fall through the
// JOIN and are still claimable. New rows always carry a target_repo.
//
// D2 T1-4. The Repositories.mode column is NOT NULL DEFAULT 'read_only'
// (see schema.go createSchema), so the JOIN never produces NULLs except
// for the legacy-row case above.
func ClaimBountyForWrite(db *sql.DB, taskType string, agentName string) (*Bounty, bool) {
	var b Bounty
	// D5.5 P2 γ — Pattern P-StageGate: same stage_id-based gating that
	// ClaimBounty applies. Astromechs running write-mode are still
	// astromechs and must respect the "no pre-staging" anti-cheat
	// directive. See ClaimBounty's docstring for the full rationale.
	err := db.QueryRow(`
		SELECT id, parent_id, target_repo, type, status, payload, convoy_id, checkpoint,
		       priority, IFNULL(task_timeout,0)
		FROM BountyBoard
		WHERE status = 'Pending' AND type = ?
		  AND (
		    target_repo IS NULL OR target_repo = ''
		    OR EXISTS (
		      SELECT 1 FROM Repositories r
		      WHERE r.name = BountyBoard.target_repo AND r.mode = 'write'
		    )
		  )
		  AND NOT EXISTS (
		    SELECT 1 FROM TaskDependencies td
		    JOIN BountyBoard dep ON dep.id = td.depends_on
		    WHERE td.task_id = BountyBoard.id AND dep.status != 'Completed'
		  )
		  AND (convoy_id = 0 OR NOT EXISTS (
		    SELECT 1 FROM FeatureBlockers fb
		    WHERE fb.blocked_convoy_id = BountyBoard.convoy_id AND fb.resolved_at IS NULL
		  ))
		  AND (stage_id IS NULL OR EXISTS (
		    SELECT 1 FROM ConvoyStages cs
		    WHERE cs.id = BountyBoard.stage_id AND cs.status != 'Pending'
		  ))
		ORDER BY priority DESC, id ASC
		LIMIT 1`, taskType).
		Scan(&b.ID, &b.ParentID, &b.TargetRepo, &b.Type, &b.Status,
			&b.Payload, &b.ConvoyID, &b.Checkpoint, &b.Priority, &b.TaskTimeout)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("ClaimBountyForWrite(type=%s, agent=%s): DB error (not ErrNoRows): %v", taskType, agentName, err)
		}
		return nil, false
	}
	res, err := db.Exec(`
		UPDATE BountyBoard SET status = 'Locked', owner = ?, locked_at = datetime('now')
		WHERE id = ? AND status = 'Pending'`, agentName, b.ID)
	if err != nil {
		log.Printf("ClaimBountyForWrite(id=%d, agent=%s): lock UPDATE failed: %v", b.ID, agentName, err)
		return nil, false
	}
	rows, _ := res.RowsAffected()
	if rows == 1 {
		b.Status = "Locked"
		b.Owner = agentName
		return &b, true
	}
	return nil, false
}

// ClaimForReview atomically claims the next task awaiting council review.
// Higher-priority tasks are reviewed first, matching the claim order used by Astromechs.
// AUDIT-068 (Fix #8d): non-ErrNoRows errors are logged.
func ClaimForReview(db *sql.DB, agentName string) (*Bounty, bool) {
	var b Bounty
	err := db.QueryRow(`
		SELECT id, parent_id, target_repo, payload, retry_count, branch_name, convoy_id, priority
		FROM BountyBoard WHERE status = 'AwaitingCouncilReview'
		  AND IFNULL(spend_suspended, 0) = 0
		ORDER BY priority DESC, id ASC
		LIMIT 1`).
		Scan(&b.ID, &b.ParentID, &b.TargetRepo, &b.Payload, &b.RetryCount, &b.BranchName, &b.ConvoyID, &b.Priority)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("ClaimForReview(agent=%s): DB error (not ErrNoRows): %v", agentName, err)
		}
		return nil, false
	}
	res, err := db.Exec(`
		UPDATE BountyBoard SET status = 'UnderReview', owner = ?, locked_at = datetime('now')
		WHERE id = ? AND status = 'AwaitingCouncilReview'`, agentName, b.ID)
	if err != nil {
		log.Printf("ClaimForReview(id=%d, agent=%s): claim UPDATE failed: %v", b.ID, agentName, err)
		return nil, false
	}
	rows, _ := res.RowsAffected()
	if rows == 1 {
		return &b, true
	}
	return nil, false
}

// ClaimForCaptainReview atomically claims the next task awaiting captain review.
// AUDIT-068 (Fix #8d): non-ErrNoRows errors are logged.
func ClaimForCaptainReview(db *sql.DB, agentName string) (*Bounty, bool) {
	var b Bounty
	err := db.QueryRow(`
		SELECT id, parent_id, target_repo, payload, retry_count, branch_name, convoy_id, priority
		FROM BountyBoard WHERE status = 'AwaitingCaptainReview'
		  AND IFNULL(spend_suspended, 0) = 0
		ORDER BY priority DESC, id ASC
		LIMIT 1`).
		Scan(&b.ID, &b.ParentID, &b.TargetRepo, &b.Payload, &b.RetryCount, &b.BranchName, &b.ConvoyID, &b.Priority)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("ClaimForCaptainReview(agent=%s): DB error (not ErrNoRows): %v", agentName, err)
		}
		return nil, false
	}
	res, err := db.Exec(`
		UPDATE BountyBoard SET status = 'UnderCaptainReview', owner = ?, locked_at = datetime('now')
		WHERE id = ? AND status = 'AwaitingCaptainReview'`, agentName, b.ID)
	if err != nil {
		log.Printf("ClaimForCaptainReview(id=%d, agent=%s): claim UPDATE failed: %v", b.ID, agentName, err)
		return nil, false
	}
	rows, _ := res.RowsAffected()
	if rows == 1 {
		return &b, true
	}
	return nil, false
}

// IsConvoyCoordinated reports whether a convoy routes completed tasks through
// the Captain before council review.
func IsConvoyCoordinated(db *sql.DB, convoyID int) bool {
	if convoyID == 0 {
		return false
	}
	var coordinated int
	db.QueryRow(`SELECT coordinated FROM Convoys WHERE id = ?`, convoyID).Scan(&coordinated)
	return coordinated == 1
}

// SetConvoyCoordinated marks a convoy as coordinated so Astromech completions
// route to AwaitingCaptainReview instead of AwaitingCouncilReview.
func SetConvoyCoordinated(db *sql.DB, convoyID int) {
	db.Exec(`UPDATE Convoys SET coordinated = 1 WHERE id = ?`, convoyID)
}

// UpdateBountyStatus transitions a task to newStatus, clearing owner/locked_at,
// and fires the webhook on terminal transitions (Completed/Failed/Escalated).
//
// Fix #8 Phase A: Returns error so callers can observe DB failures instead of
// silently believing the UPDATE succeeded. See CLAUDE.md's "No silent failures"
// invariant — a dropped DB error here leaves the task in its prior state while
// every downstream path (webhook listener, convoy-completion check, operator
// dashboard) acts as though the transition happened. The webhook fires only
// when the UPDATE itself returned no error; otherwise the caller gets the
// error and can escalate / retry.
func UpdateBountyStatus(db *sql.DB, id int, newStatus string) error {
	if _, err := db.Exec(`UPDATE BountyBoard SET status = ?, owner = '', locked_at = '' WHERE id = ?`, newStatus, id); err != nil {
		return fmt.Errorf("UpdateBountyStatus(id=%d, status=%s): %w", id, newStatus, err)
	}
	if newStatus == "Completed" || newStatus == "Failed" || newStatus == "Escalated" {
		FireWebhook(db, id, newStatus)
	}
	return nil
}

// UpdateBountyStatusFrom is the source-status-guarded sibling of
// UpdateBountyStatus. The UPDATE succeeds only if the task's current status
// equals `from`; a racing writer that already transitioned the task to a
// different status leaves us with rowsAffected=0 and the caller is expected
// to detect the lost race and skip any side effects.
//
// Pattern P7 (Fix #8d, closes AUDIT-026, AUDIT-027, AUDIT-072): state
// transitions that depend on the prior status MUST go through this helper
// rather than the blind UpdateBountyStatus. Without the guard, a CancelTask
// that lands first can be clobbered by a stale Jedi Council approval — in
// the P7 pre-fix regression test this happened 20/20 trials.
//
// The webhook fires only when the UPDATE actually landed (rowsAffected=1)
// AND the new status is a webhook-observed terminal; a lost-race no-op
// stays silent.
func UpdateBountyStatusFrom(db *sql.DB, id int, from, to string) (int64, error) {
	res, err := db.Exec(`UPDATE BountyBoard SET status = ?, owner = '', locked_at = ''
		WHERE id = ? AND status = ?`, to, id, from)
	if err != nil {
		return 0, fmt.Errorf("UpdateBountyStatusFrom(id=%d, from=%s, to=%s): %w", id, from, to, err)
	}
	n, _ := res.RowsAffected()
	if n == 1 && (to == "Completed" || to == "Failed" || to == "Escalated") {
		FireWebhook(db, id, to)
	}
	return n, nil
}

// UpdateBountyStatusFromTx is the transactional sibling of
// UpdateBountyStatusFrom. The caller fires the webhook (if appropriate)
// AFTER commit — tx variants deliberately skip side effects so a rolled-back
// transaction doesn't emit spurious notifications.
func UpdateBountyStatusFromTx(tx *sql.Tx, id int, from, to string) (int64, error) {
	res, err := tx.Exec(`UPDATE BountyBoard SET status = ?, owner = '', locked_at = ''
		WHERE id = ? AND status = ?`, to, id, from)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// UpdateBountyStatusTx updates task status inside an existing transaction.
// The caller is responsible for firing the webhook AFTER commit — tx variants
// deliberately skip side effects so a rolled-back transaction doesn't emit
// spurious notifications.
func UpdateBountyStatusTx(tx *sql.Tx, id int, newStatus string) error {
	_, err := tx.Exec(`UPDATE BountyBoard SET status = ?, owner = '', locked_at = '' WHERE id = ?`, newStatus, id)
	return err
}

// UpdateBountyStatusWithErrorTx also clears error_log / sets a new error_log in the same UPDATE.
// Runs errorLog through RedactSecrets so wrapped gh stderr containing a
// ghp_/Bearer/url-basic-auth token never lands in the database (Fix #10
// / AUDIT-055 defense in depth — the gh client already redacts at the
// wrap site, but a future caller may forget).
func UpdateBountyStatusWithErrorTx(tx *sql.Tx, id int, newStatus, errorLog string) error {
	_, err := tx.Exec(`UPDATE BountyBoard SET status = ?, owner = '', locked_at = '', error_log = ? WHERE id = ?`,
		newStatus, RedactSecrets(errorLog), id)
	return err
}

func AddBounty(db *sql.DB, parentID int, taskType, payload string) int {
	res, _ := db.Exec(`INSERT INTO BountyBoard (parent_id, type, status, payload, created_at) VALUES (?, ?, 'Pending', ?, datetime('now'))`,
		parentID, taskType, payload)
	id, _ := res.LastInsertId()
	return int(id)
}

// AddBountyTx is the transactional sibling of AddBounty.
func AddBountyTx(tx *sql.Tx, parentID int, taskType, payload string) (int, error) {
	res, err := tx.Exec(`INSERT INTO BountyBoard (parent_id, type, status, payload, created_at) VALUES (?, ?, 'Pending', ?, datetime('now'))`,
		parentID, taskType, payload)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return int(id), nil
}

// AddFeatureTaskTx inserts a top-level Feature task targeting a specific repo.
// Used by PRReviewTriage's out_of_scope branch — the suggestion becomes a
// standalone Feature that Commander will plan and Chancellor will approve.
// parent_id is 0 (top-level), status='Pending', priority is caller-chosen.
func AddFeatureTaskTx(tx *sql.Tx, repo, payload string, priority int) (int, error) {
	if repo == "" {
		return 0, fmt.Errorf("AddFeatureTaskTx: repo required")
	}
	res, err := tx.Exec(
		`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, priority, created_at)
		 VALUES (0, ?, 'Feature', 'Pending', ?, ?, datetime('now'))`,
		repo, payload, priority)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return int(id), nil
}

// AddBountyClassifying inserts a task with type='Auto' and status='Classifying'.
// The Inquisitor will classify it and transition it to Pending.
// idempotencyKey is stored immediately so duplicate-check queries can find the row.
func AddBountyClassifying(db *sql.DB, repo, payload string, priority int, idempotencyKey string) (int, error) {
	res, err := db.Exec(
		`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, priority, idempotency_key, created_at)
		 VALUES (0, ?, 'Auto', 'Classifying', ?, ?, ?, datetime('now'))`,
		repo, payload, priority, idempotencyKey)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return int(id), nil
}

// FailBounty marks a task Failed with an error_log, clears owner/locked_at,
// and fires the Failed webhook.
//
// Fix #8 Phase A: Returns error — a silent failure here is especially
// pathological because it leaves a task stuck in its prior status while
// the rest of the fleet believes the failure was recorded. The webhook
// fires only when the UPDATE actually succeeded.
//
// Fix #10 (AUDIT-055): errorMsg is scrubbed with RedactSecrets before the
// write so a wrapped gh/claude error containing a ghp_/Bearer/URL-basic-auth
// token cannot leak into BountyBoard.error_log (which renders on the
// dashboard).
func FailBounty(db *sql.DB, id int, errorMsg string) error {
	if _, err := db.Exec(`UPDATE BountyBoard SET status = 'Failed', owner = '', locked_at = '', error_log = ? WHERE id = ?`,
		RedactSecrets(errorMsg), id); err != nil {
		return fmt.Errorf("FailBounty(id=%d): %w", id, err)
	}
	FireWebhook(db, id, "Failed")
	return nil
}

// MarkConflictPending transitions a task to ConflictPending, indicating it was
// approved by the council but couldn't merge due to a conflict. A resolution
// task has been spawned and will complete this task's work.
func MarkConflictPending(db *sql.DB, id int, msg string) {
	db.Exec(`UPDATE BountyBoard SET status = 'ConflictPending', owner = '', locked_at = '', error_log = ? WHERE id = ?`,
		RedactSecrets(msg), id)
}

// MarkConflictPendingTx is the transactional sibling of MarkConflictPending.
func MarkConflictPendingTx(tx *sql.Tx, id int, msg string) error {
	_, err := tx.Exec(`UPDATE BountyBoard SET status = 'ConflictPending', owner = '', locked_at = '', error_log = ? WHERE id = ?`,
		RedactSecrets(msg), id)
	return err
}

// CancelTask marks a task as Cancelled with a reason. Cancelled is distinct
// from Failed — it reflects deliberate operator action, not an agent error.
// No-op if the task is already Completed or Cancelled (terminal states);
// returns true if the task transitioned to Cancelled.
//
// Pattern P7 (Fix #8d, closes AUDIT-027, AUDIT-072): the UPDATE is a
// source-status CAS via read-then-UpdateBountyStatusFromTx. An operator
// cancel racing with a Jedi Council approve that has also been migrated to
// UpdateBountyStatusFrom — whichever transition lands first wins, and the
// loser sees rowsAffected=0 and returns without clobbering.
func CancelTask(db *sql.DB, id int, reason string) bool {
	var currentStatus string
	if err := db.QueryRow(`SELECT status FROM BountyBoard WHERE id = ?`, id).Scan(&currentStatus); err != nil {
		return false
	}
	if currentStatus == "Completed" || currentStatus == "Cancelled" {
		return false
	}
	res, err := db.Exec(`UPDATE BountyBoard SET status = 'Cancelled', owner = '', locked_at = '', error_log = ?
		WHERE id = ? AND status = ?`, reason, id, currentStatus)
	if err != nil {
		return false
	}
	n, _ := res.RowsAffected()
	return n > 0
}

// ResetTask resets an escalated/failed task, preserving committed coding work.
// If the task has a branch_name set, coding work already exists on that branch —
// the task is sent directly to AwaitingCouncilReview (or AwaitingCaptainReview
// for coordinated convoys) so Jedi Council reviews the existing work rather than
// an astromech redoing it from scratch.
// If no branch_name is set (no coding work yet), it resets to Pending.
// In both cases error/lock state is cleared and the convoy is auto-recovered.
//
// Pattern P7 (Fix #8d, closes AUDIT-026): a Completed or Cancelled task is
// refused — these are terminal states and a retry endpoint racing with a
// stale dashboard view must not resurrect finished work. The UPDATE's
// AND status NOT IN ('Completed','Cancelled') clause makes the refusal
// atomic with the read so an in-flight completion cannot slip between the
// status check and the write. Returns true iff the reset landed.
func ResetTask(db *sql.DB, id int) bool {
	var convoyID int
	var branchName string
	if err := db.QueryRow(`SELECT convoy_id, IFNULL(branch_name,'') FROM BountyBoard WHERE id = ?`, id).
		Scan(&convoyID, &branchName); err != nil {
		return false
	}
	var res sql.Result
	var err error
	if branchName != "" {
		targetStatus := "AwaitingCouncilReview"
		if IsConvoyCoordinated(db, convoyID) {
			targetStatus = "AwaitingCaptainReview"
		}
		res, err = db.Exec(`UPDATE BountyBoard SET status = ?, owner = '', error_log = '',
			retry_count = 0, infra_failures = 0, locked_at = '', checkpoint = ''
			WHERE id = ? AND status NOT IN ('Completed','Cancelled')`, targetStatus, id)
	} else {
		res, err = db.Exec(`UPDATE BountyBoard SET status = 'Pending', owner = '', error_log = '',
			retry_count = 0, infra_failures = 0, locked_at = '', checkpoint = '', branch_name = ''
			WHERE id = ? AND status NOT IN ('Completed','Cancelled')`, id)
	}
	if err != nil {
		return false
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return false
	}
	AutoRecoverConvoy(db, convoyID, nil)
	return true
}

// ResetTaskFull always resets a task to Pending and clears branch_name regardless
// of committed work. Used by Medic when re-running the coding phase with new
// guidance — the astromech needs a fresh attempt, not a review of a bad branch.
//
// Fix #6 (AUDIT-005): retry_count and infra_failures are deliberately PRESERVED.
// Zeroing them on every Medic requeue let the Astromech→Council→Medic→Astromech
// loop run indefinitely because every cycle started fresh from the caller's
// perspective. The counters now accumulate so downstream gates
// (auto-shard on timeout, permanent-fail on retries) remain bounded even
// across multiple Medic-driven retries, and the `medic_requeue_count` cap
// (see applyMedicRequeue) is the final bounded-lives budget.
//
// Pattern P7 (Fix #8d): Completed/Cancelled tasks are refused — Medic has no
// business resurrecting a task that the fleet already finished. The atomic
// AND status NOT IN (...) clause protects against a Medic decision landing
// on a task that completed between the failure-triage dispatch and the
// requeue write. Returns true iff the reset landed.
func ResetTaskFull(db *sql.DB, id int) bool {
	var convoyID int
	if err := db.QueryRow(`SELECT convoy_id FROM BountyBoard WHERE id = ?`, id).Scan(&convoyID); err != nil {
		return false
	}
	res, err := db.Exec(`UPDATE BountyBoard SET status = 'Pending', owner = '', error_log = '',
		locked_at = '', checkpoint = '', branch_name = ''
		WHERE id = ? AND status NOT IN ('Completed','Cancelled')`, id)
	if err != nil {
		return false
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return false
	}
	AutoRecoverConvoy(db, convoyID, nil)
	return true
}

// ResetAllFailed resets all Failed tasks to Pending. Returns the number of tasks reset.
func ResetAllFailed(db *sql.DB) int {
	res, _ := db.Exec(`UPDATE BountyBoard SET status = 'Pending', owner = '', error_log = '',
		retry_count = 0, infra_failures = 0, locked_at = '', checkpoint = '', branch_name = ''
		WHERE status = 'Failed'`)
	n, _ := res.RowsAffected()
	return int(n)
}

// ReturnTaskForRework sends a task back to Pending with a new payload (feedback injected).
// branch_name is intentionally preserved so the agent can resume from prior work rather
// than redoing everything from scratch.
func ReturnTaskForRework(db *sql.DB, id int, newPayload string) {
	db.Exec(`UPDATE BountyBoard SET status = 'Pending', owner = '', locked_at = '', payload = ?, checkpoint = ''
		WHERE id = ?`, newPayload, id)
}

// QueueMedicReview spawns a MedicReview task for a permanently-failed bounty.
// The Medic will analyze the failure and decide whether to requeue, shard, or escalate.
// Inherits target_repo, convoy_id, and priority from the source bounty.
func QueueMedicReview(db *sql.DB, b *Bounty, failureType, errorDetail string) int {
	payload := fmt.Sprintf(`{"failure_type":%q,"error":%q}`, failureType, errorDetail)
	res, _ := db.Exec(
		`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, convoy_id, priority, created_at)
		 VALUES (?, ?, 'MedicReview', 'Pending', ?, ?, ?, datetime('now'))`,
		b.ID, b.TargetRepo, payload, b.ConvoyID, b.Priority)
	id, _ := res.LastInsertId()
	return int(id)
}

// QueueBoSReview spawns a BoSReview task for a freshly-committed bounty.
// Astromech post-commit hook calls this BEFORE transitioning to Captain
// review so BoS gets a chance to scan the commit's diff. The
// `branchName` and `commitSHA` are stamped into the payload so the BoS
// reviewer can locate the diff against the base. CLAUDE.md "Convoy-
// scoped queries use convoy_id not LIKE": the inserted row carries
// convoy_id so future scans against `WHERE convoy_id = ?` find it.
//
// Returns the new BoSReview task id and any insertion error.
func QueueBoSReview(db *sql.DB, b *Bounty, branchName, commitSHA string) (int, error) {
	if b == nil || b.ID == 0 {
		return 0, fmt.Errorf("QueueBoSReview: nil/empty source bounty")
	}
	payload := fmt.Sprintf(
		`{"source_task_id":%d,"branch":%q,"commit_sha":%q,"target_repo":%q}`,
		b.ID, branchName, commitSHA, b.TargetRepo,
	)
	res, err := db.Exec(
		`INSERT INTO BountyBoard
		   (parent_id, target_repo, type, status, payload, convoy_id, priority, branch_name, created_at)
		 VALUES (?, ?, 'BoSReview', 'Pending', ?, ?, ?, ?, datetime('now'))`,
		b.ID, b.TargetRepo, payload, b.ConvoyID, b.Priority, branchName)
	if err != nil {
		return 0, fmt.Errorf("QueueBoSReview(parent=%d): %w", b.ID, err)
	}
	id, _ := res.LastInsertId()
	return int(id), nil
}

// QueueISBReview spawns an ISBReview task for a freshly-committed
// bounty. D4 Phase 2 — runs in PARALLEL with BoSReview at the same
// pre-Captain point in the pipeline; the dual-gate logic in the BoS/
// ISB reviewers ensures the source task only advances when BOTH have
// approved (or both have advise-only findings). Same payload shape as
// QueueBoSReview so a future shared reviewer harness can consume both
// from one parser.
//
// CLAUDE.md "Convoy-scoped queries use convoy_id not LIKE": the
// inserted row carries convoy_id.
//
// Returns the new ISBReview task id and any insertion error.
func QueueISBReview(db *sql.DB, b *Bounty, branchName, commitSHA string) (int, error) {
	if b == nil || b.ID == 0 {
		return 0, fmt.Errorf("QueueISBReview: nil/empty source bounty")
	}
	payload := fmt.Sprintf(
		`{"source_task_id":%d,"branch":%q,"commit_sha":%q,"target_repo":%q}`,
		b.ID, branchName, commitSHA, b.TargetRepo,
	)
	res, err := db.Exec(
		`INSERT INTO BountyBoard
		   (parent_id, target_repo, type, status, payload, convoy_id, priority, branch_name, created_at)
		 VALUES (?, ?, 'ISBReview', 'Pending', ?, ?, ?, ?, datetime('now'))`,
		b.ID, b.TargetRepo, payload, b.ConvoyID, b.Priority, branchName)
	if err != nil {
		return 0, fmt.Errorf("QueueISBReview(parent=%d): %w", b.ID, err)
	}
	id, _ := res.LastInsertId()
	return int(id), nil
}

// QueueStageSenateReview spawns a SenateReview task scoped to one stage of a
// staged convoy (D5.5 P2 β). Distinct from QueueSenateReview, which is
// scoped to a Commander-emitted Feature plan: this variant carries
// `convoy_id` + `stage_id` instead of `feature_id`, and the Senate handler
// reads the stage's `intent_text` + the stage-scoped diff to apply its
// memory-driven advice. The task_type is still `SenateReview` so the
// existing Senate claim loop picks it up; the payload shape's `stage_id`
// field is the discriminator the handler branches on.
//
// Idempotent: returns (0, nil) if a Pending/Locked SenateReview already
// exists for this (convoy, stage). The dedup key is
// `senate-review-stage:<convoyID>:<stageID>` and is backed by the partial
// UNIQUE idx_bounty_idem; two concurrent callers cannot both land a row.
func QueueStageSenateReview(db *sql.DB, convoyID, stageID int) (int, error) {
	if convoyID <= 0 {
		return 0, fmt.Errorf("QueueStageSenateReview: convoyID required")
	}
	if stageID <= 0 {
		return 0, fmt.Errorf("QueueStageSenateReview: stageID required")
	}
	payload := fmt.Sprintf(`{"convoy_id":%d,"stage_id":%d}`, convoyID, stageID)
	key := fmt.Sprintf("senate-review-stage:%d:%d", convoyID, stageID)
	id, existed, err := AddIdempotentTask(db, key,
		0, "", "SenateReview", payload, convoyID, 0, "Pending")
	if err != nil {
		return 0, fmt.Errorf("QueueStageSenateReview(convoy=%d stage=%d): %w", convoyID, stageID, err)
	}
	if existed {
		// Match QueueConvoyReview's "already queued" contract — return 0
		// rather than the existing id so callers in the ConvoyReview hook
		// only count newly-queued work for log lines and tests.
		return 0, nil
	}
	return id, nil
}

// QueueSenateReview spawns a SenateReview task for a Feature whose
// Commander-emitted plan is sitting in ProposedConvoys. D4 Phase 3 —
// queued by the Senate-router hook (agents.QueueSenateReviewHook)
// AFTER StoreProposedConvoy and BEFORE the Feature transitions to
// AwaitingSenateReview. The Senate reviewer's runSenateReviewTask
// handler aggregates per-Senator verdicts and either advances the
// Feature to AwaitingChancellorReview (all concur) or returns it to
// Pending (any high-confidence dissent).
//
// Returns the new SenateReview task id and any insertion error.
func QueueSenateReview(db *sql.DB, featureID int, targetRepo string) (int, error) {
	if featureID == 0 {
		return 0, fmt.Errorf("QueueSenateReview: featureID required")
	}
	payload := fmt.Sprintf(`{"feature_id":%d,"target_repo":%q}`, featureID, targetRepo)
	res, err := db.Exec(
		`INSERT INTO BountyBoard
		   (parent_id, target_repo, type, status, payload, priority, created_at)
		 VALUES (?, ?, 'SenateReview', 'Pending', ?, 0, datetime('now'))`,
		featureID, targetRepo, payload)
	if err != nil {
		return 0, fmt.Errorf("QueueSenateReview(feature=%d): %w", featureID, err)
	}
	id, _ := res.LastInsertId()
	return int(id), nil
}

// QueueSenatorOnboarding spawns a SenatorOnboarding task for the named
// repo. D4 Phase 3 — queued by `force add-repo` and at daemon start for
// force-orchestrator (the recursive first Senator). The handler
// (agents.runSenatorOnboardingTask) seeds the SenateChambers row in
// 'onboarding' status, calls librarian.BootstrapSenatorRules, emits
// each candidate as a PromotionProposal, and seeds initial
// SenateMemory entries.
//
// triggeredBy is a free-form provenance string ("daemon-start",
// "operator-add-repo", "test"). It lands in the payload for audit
// purposes only; the handler does not act on it.
//
// Returns the new SenatorOnboarding task id and any insertion error.
func QueueSenatorOnboarding(db *sql.DB, repoID, triggeredBy string) (int, error) {
	if repoID == "" {
		return 0, fmt.Errorf("QueueSenatorOnboarding: repoID required")
	}
	payload := fmt.Sprintf(`{"repo_id":%q,"scope":%q,"triggered_by":%q}`,
		repoID, "repo:"+repoID, triggeredBy)
	res, err := db.Exec(
		`INSERT INTO BountyBoard
		   (parent_id, target_repo, type, status, payload, priority, created_at)
		 VALUES (0, ?, 'SenatorOnboarding', 'Pending', ?, 0, datetime('now'))`,
		repoID, payload)
	if err != nil {
		return 0, fmt.Errorf("QueueSenatorOnboarding(repo=%s): %w", repoID, err)
	}
	id, _ := res.LastInsertId()
	return int(id), nil
}

// QueueSenatorRefresh enqueues a SenatorRefresh task for the named repo if no
// non-terminal SenatorRefresh task already exists for it (dedup). The task is
// picked up by SpawnSenate → runSenatorRefreshTask which re-runs the 3-output
// onboarding LLM pass (knowledge_digest, rule_suggestions, tag_suggestions)
// against the current repo state and appends fresh SenateMemory rows.
//
// Returns (taskID, alreadyExisted, error). alreadyExisted=true means the dedup
// gate fired and no new row was inserted.
func QueueSenatorRefresh(db *sql.DB, repoID, triggeredBy string) (int, bool, error) {
	if repoID == "" {
		return 0, false, fmt.Errorf("QueueSenatorRefresh: repoID required")
	}
	// Dedup: if a non-terminal SenatorRefresh already exists for this repo, skip.
	var existing int
	if err := db.QueryRow(`
		SELECT COUNT(*) FROM BountyBoard
		 WHERE type = 'SenatorRefresh'
		   AND target_repo = ?
		   AND status IN ('Pending', 'Locked')`, repoID).Scan(&existing); err != nil {
		return 0, false, fmt.Errorf("QueueSenatorRefresh(%s): dedup check: %w", repoID, err)
	}
	if existing > 0 {
		return 0, true, nil
	}
	payload := fmt.Sprintf(`{"repo_id":%q,"scope":%q,"triggered_by":%q}`,
		repoID, "repo:"+repoID, triggeredBy)
	res, err := db.Exec(
		`INSERT INTO BountyBoard
		   (parent_id, target_repo, type, status, payload, priority, created_at)
		 VALUES (0, ?, 'SenatorRefresh', 'Pending', ?, 0, datetime('now'))`,
		repoID, payload)
	if err != nil {
		return 0, false, fmt.Errorf("QueueSenatorRefresh(%s): %w", repoID, err)
	}
	id, _ := res.LastInsertId()
	return int(id), false, nil
}

// AddConvoyTask creates a CodeEdit subtask within a convoy. status should be
// 'Pending' or 'Planned'. Returns the new task ID and any insertion error.
func AddConvoyTask(db *sql.DB, parentID int, repo, payload string, convoyID, priority int, status string) (int, error) {
	res, err := db.Exec(
		`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, convoy_id, priority, created_at)
		 VALUES (?, ?, 'CodeEdit', ?, ?, ?, ?, datetime('now'))`,
		parentID, repo, status, payload, convoyID, priority)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return int(id), nil
}

// AddConvoyTaskIdempotent is the dedup-aware sibling of AddConvoyTask. If a
// non-terminal CodeEdit task already exists with the same idempotencyKey, it
// returns that task's ID and (alreadyExisted=true) without inserting. Terminal
// statuses (Completed / Cancelled / Failed) do not block a new insert — past
// runs are "done" and the next pass is allowed to try again.
//
// Callers should use stable keys that describe the work, not the moment:
//
//	rebase-conflict:branch:<branch>         — one outstanding conflict per agent branch
//	rebase-conflict:askbranch:<askbranch>   — one outstanding conflict per ask-branch
//	convoy-review-fix:<convoyID>:<hash>     — one outstanding fix per review finding
//	convoy-review:<convoyID>                — one outstanding ConvoyReview per convoy
//	worktree-reset:<parent_task_id>         — one outstanding WorktreeReset per parent
//	rebase-agent:<sub_pr_row_id>            — one outstanding RebaseAgentBranch per sub-PR
//	create-askbranch:<convoyID>             — one outstanding CreateAskBranch per convoy
//	rebase-askbranch:<convoyID>:<repo>      — one outstanding RebaseAskBranch per (convoy,repo)
//	pr-review-triage:<convoyID>             — one outstanding PRReviewTriage per convoy
//	ci-failure-triage:<sub_pr_row_id>       — one outstanding CIFailureTriage per sub-PR row
//
// idempotencyKey must be non-empty; callers that can't produce a stable key
// should use plain AddConvoyTask instead.
//
// Race-safety (Fix #3, AUDIT-008): backed by the partial UNIQUE index
// idx_bounty_idem on BountyBoard(idempotency_key) WHERE idempotency_key != ''
// AND status NOT IN ('Completed','Cancelled','Failed'). The insert uses
// INSERT ... ON CONFLICT(idempotency_key) ... DO NOTHING RETURNING id so that
// under concurrent callers the second insert is rejected atomically and we
// fall back to SELECT-ing the row that won the race. No TOCTOU window.
func AddConvoyTaskIdempotent(db *sql.DB, idempotencyKey string, parentID int, repo, payload string, convoyID, priority int, status string) (id int, alreadyExisted bool, err error) {
	if idempotencyKey == "" {
		return 0, false, fmt.Errorf("AddConvoyTaskIdempotent: idempotencyKey required")
	}
	return addTaskIdempotent(db, idempotencyKey, parentID, repo, "CodeEdit", payload, convoyID, priority, status)
}

// AddIdempotentTask is the typed sibling of AddConvoyTaskIdempotent: same
// race-safe plumbing via the partial UNIQUE idx_bounty_idem, but with taskType
// as a parameter so callers outside the CodeEdit path (Queue* helpers for
// infrastructure tasks like ConvoyReview, WorktreeReset, RebaseAgentBranch,
// PRReviewTriage, etc.) can share the same atomic insert.
//
// Returns (existingID, true, nil) when a live non-terminal row already claims
// the key; (newID, false, nil) on a fresh insert. idempotencyKey must be
// non-empty; callers that can't produce a canonical key should use the
// non-idempotent inserters instead.
func AddIdempotentTask(db *sql.DB, idempotencyKey string, parentID int, repo, taskType, payload string, convoyID, priority int, status string) (id int, alreadyExisted bool, err error) {
	if idempotencyKey == "" {
		return 0, false, fmt.Errorf("AddIdempotentTask: idempotencyKey required")
	}
	if taskType == "" {
		return 0, false, fmt.Errorf("AddIdempotentTask: taskType required")
	}
	return addTaskIdempotent(db, idempotencyKey, parentID, repo, taskType, payload, convoyID, priority, status)
}

// AddIdempotentTaskTx is the transactional sibling of AddIdempotentTask. Uses
// the same partial UNIQUE idx_bounty_idem-backed INSERT ... ON CONFLICT DO
// NOTHING RETURNING id, so a caller already inside a transaction (e.g.
// onSubPRCIFailed's failure-count + triage-queue atomic block) still benefits
// from the dedup. On conflict (DO NOTHING), falls back to SELECT-existing via
// the same tx.
func AddIdempotentTaskTx(tx *sql.Tx, idempotencyKey string, parentID int, repo, taskType, payload string, convoyID, priority int, status string) (id int, alreadyExisted bool, err error) {
	if idempotencyKey == "" {
		return 0, false, fmt.Errorf("AddIdempotentTaskTx: idempotencyKey required")
	}
	if taskType == "" {
		return 0, false, fmt.Errorf("AddIdempotentTaskTx: taskType required")
	}
	var newID int
	scanErr := tx.QueryRow(
		`INSERT INTO BountyBoard
		    (parent_id, target_repo, type, status, payload, convoy_id, priority, idempotency_key, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, datetime('now'))
		 ON CONFLICT(idempotency_key)
		 WHERE idempotency_key != '' AND status NOT IN ('Completed','Cancelled','Failed')
		 DO NOTHING
		 RETURNING id`,
		parentID, repo, taskType, status, payload, convoyID, priority, idempotencyKey,
	).Scan(&newID)
	if scanErr == nil {
		return newID, false, nil
	}
	if scanErr != sql.ErrNoRows {
		return 0, false, fmt.Errorf("idempotent insert (tx): %w", scanErr)
	}
	var existing int
	// `idempotency_key != ''` is required for SQLite's partial-index planner
	// to pick up idx_bounty_idem. See the addTaskIdempotent comment.
	if sErr := tx.QueryRow(
		`SELECT id FROM BountyBoard
		 WHERE idempotency_key = ?
		   AND idempotency_key != ''
		   AND status NOT IN ('Completed','Cancelled','Failed')
		 LIMIT 1`, idempotencyKey).Scan(&existing); sErr != nil {
		return 0, false, fmt.Errorf("post-conflict lookup (tx): %w", sErr)
	}
	return existing, true, nil
}

// addTaskIdempotent inserts a BountyBoard row gated on idempotency_key via the
// partial UNIQUE idx_bounty_idem. Returns (existingID, true, nil) when another
// caller won the race under the same key; (newID, false, nil) on fresh insert.
//
// taskType is parameterised so the different Queue* helpers (WorktreeReset,
// ConvoyReview, RebaseAgentBranch, etc.) can share the same race-safe plumbing.
func addTaskIdempotent(db *sql.DB, idempotencyKey string, parentID int, repo, taskType, payload string, convoyID, priority int, status string) (id int, alreadyExisted bool, err error) {
	var newID int
	scanErr := db.QueryRow(
		`INSERT INTO BountyBoard
		    (parent_id, target_repo, type, status, payload, convoy_id, priority, idempotency_key, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, datetime('now'))
		 ON CONFLICT(idempotency_key)
		 WHERE idempotency_key != '' AND status NOT IN ('Completed','Cancelled','Failed')
		 DO NOTHING
		 RETURNING id`,
		parentID, repo, taskType, status, payload, convoyID, priority, idempotencyKey,
	).Scan(&newID)
	if scanErr == nil {
		return newID, false, nil
	}
	if scanErr != sql.ErrNoRows {
		return 0, false, fmt.Errorf("idempotent insert: %w", scanErr)
	}
	// DO NOTHING fired — some other caller won the race (or we saw an existing
	// live row). Look up the existing row; scope to non-terminal statuses so we
	// only return the row that actually blocked the insert.
	var existing int
	// The `idempotency_key != ''` predicate below is redundant with the
	// equality match when idempotencyKey is non-empty (we enforce that above),
	// but it's required for SQLite's partial-index planner to pick up
	// idx_bounty_idem — without it, the planner falls back to SCAN BountyBoard.
	if sErr := db.QueryRow(
		`SELECT id FROM BountyBoard
		 WHERE idempotency_key = ?
		   AND idempotency_key != ''
		   AND status NOT IN ('Completed','Cancelled','Failed')
		 LIMIT 1`, idempotencyKey).Scan(&existing); sErr != nil {
		return 0, false, fmt.Errorf("post-conflict lookup: %w", sErr)
	}
	return existing, true, nil
}

// AddCodeEditTask creates a CodeEdit task with full field support. Returns the new task ID.
func AddCodeEditTask(db *sql.DB, repo, payload string, convoyID, priority, taskTimeout int) int {
	res, _ := db.Exec(
		`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, convoy_id, priority, task_timeout)
		 VALUES (0, ?, 'CodeEdit', 'Pending', ?, ?, ?, ?)`,
		repo, payload, convoyID, priority, taskTimeout)
	id, _ := res.LastInsertId()
	return int(id)
}

func IncrementRetryCount(db *sql.DB, id int) int {
	db.Exec(`UPDATE BountyBoard SET retry_count = retry_count + 1 WHERE id = ?`, id)
	var count int
	db.QueryRow(`SELECT retry_count FROM BountyBoard WHERE id = ?`, id).Scan(&count)
	return count
}

func IncrementInfraFailures(db *sql.DB, id int) int {
	db.Exec(`UPDATE BountyBoard SET infra_failures = infra_failures + 1 WHERE id = ?`, id)
	var count int
	db.QueryRow(`SELECT infra_failures FROM BountyBoard WHERE id = ?`, id).Scan(&count)
	return count
}

// IncrementMedicRequeue bumps the medic_requeue_count counter and returns the
// new value. Used by Medic's requeue path (Fix #6) to enforce a hard cap on
// the Astromech→Council→Medic→Astromech loop — past the cap, Medic forces an
// escalate decision instead of spending another Claude cycle.
func IncrementMedicRequeue(db *sql.DB, id int) int {
	db.Exec(`UPDATE BountyBoard SET medic_requeue_count = medic_requeue_count + 1 WHERE id = ?`, id)
	var count int
	db.QueryRow(`SELECT IFNULL(medic_requeue_count, 0) FROM BountyBoard WHERE id = ?`, id).Scan(&count)
	return count
}

// GetMedicRequeueCount returns the current medic_requeue_count for a task.
// Used by the Medic decision path to decide whether to honor a requeue
// decision or force-escalate (Fix #6).
func GetMedicRequeueCount(db *sql.DB, id int) int {
	var count int
	db.QueryRow(`SELECT IFNULL(medic_requeue_count, 0) FROM BountyBoard WHERE id = ?`, id).Scan(&count)
	return count
}

// GetReshardGeneration returns the reshard_generation stamp for a task.
// Used by queueReshardDecompose (Fix #6) to refuse cascading auto-reshards
// past the generation cap — bounds the 1→3→9→27 fanout.
func GetReshardGeneration(db *sql.DB, id int) int {
	var gen int
	db.QueryRow(`SELECT IFNULL(reshard_generation, 0) FROM BountyBoard WHERE id = ?`, id).Scan(&gen)
	return gen
}

// SetReshardGeneration stamps a task with its reshard generation. Used when
// autoInsertReshardTasks writes shards so the next generation's failures
// can be detected and refused.
func SetReshardGeneration(db *sql.DB, id, generation int) {
	db.Exec(`UPDATE BountyBoard SET reshard_generation = ? WHERE id = ?`, generation, id)
}

// IncrementFailedRebaseAttempts bumps ConvoyAskBranches.failed_rebase_attempts
// for a (convoy, repo) pair and returns the new value. Used by
// runRebaseAskBranch / dogMainDriftWatch (Fix #6 — AUDIT-028, AUDIT-119) to
// bound the ask-branch rebase-conflict retry loop.
func IncrementFailedRebaseAttempts(db *sql.DB, convoyID int, repo string) int {
	db.Exec(`UPDATE ConvoyAskBranches SET failed_rebase_attempts = IFNULL(failed_rebase_attempts, 0) + 1
		WHERE convoy_id = ? AND repo = ?`, convoyID, repo)
	var count int
	db.QueryRow(`SELECT IFNULL(failed_rebase_attempts, 0) FROM ConvoyAskBranches
		WHERE convoy_id = ? AND repo = ?`, convoyID, repo).Scan(&count)
	return count
}

// GetFailedRebaseAttempts reads the counter without incrementing. Used by
// main-drift-watch to decide whether to queue another rebase or stand down.
func GetFailedRebaseAttempts(db *sql.DB, convoyID int, repo string) int {
	var count int
	db.QueryRow(`SELECT IFNULL(failed_rebase_attempts, 0) FROM ConvoyAskBranches
		WHERE convoy_id = ? AND repo = ?`, convoyID, repo).Scan(&count)
	return count
}

// ResetFailedRebaseAttempts clears the counter — called by runRebaseAskBranch
// on a clean rebase (the ask-branch caught up, so earlier failures are no
// longer relevant).
func ResetFailedRebaseAttempts(db *sql.DB, convoyID int, repo string) {
	db.Exec(`UPDATE ConvoyAskBranches SET failed_rebase_attempts = 0
		WHERE convoy_id = ? AND repo = ?`, convoyID, repo)
}

func UpdateCheckpoint(db *sql.DB, id int, checkpoint string) {
	db.Exec(`UPDATE BountyBoard SET checkpoint = ? WHERE id = ?`, checkpoint, id)
}

// SetBranchName records the branch_name for a task. Validates via
// validateRefName so a downstream `exec.Command("git", ..., branchName)`
// never sees a CVE-2017-1000117-class string. On validation failure
// (including empty string — which downstream is a "no branch yet"
// sentinel that triggers fallback behaviour) the write is SKIPPED.
//
// Callers that need to know whether the write landed should use
// SetBranchNameTx.
//
// NOTE: The "clear branch on WorktreeReset" path uses a direct db.Exec
// rather than this setter, so the stricter empty-string rejection here
// does not break that flow. Conflict-resolution task bootstrap in
// jedi_council uses SetBranchNameTx which returns an error on empty;
// callers there handle the error (converted to a sentinel branch name
// in Fix #9 — see ClearBranchNameTx).
func SetBranchName(db *sql.DB, id int, branchName string) {
	if err := validateRefName(branchName); err != nil {
		return
	}
	db.Exec(`UPDATE BountyBoard SET branch_name = ? WHERE id = ?`, branchName, id)
}

// SetBranchNameTx is the transactional sibling of SetBranchName. Fix #9
// adds the ref validator at ingress so a corrupt branch name (CVE-
// 2017-1000117 class: `--upload-pack=/tmp/evil`, `-rm`, control chars,
// `..`, etc.) is rejected BEFORE landing in BountyBoard.branch_name.
// Returns the validator error up so the caller rolls back the txn.
//
// Empty string is also rejected here. Callers that legitimately want to
// clear the branch (e.g. spawn a fresh conflict-resolution task) must
// use ClearBranchNameTx, which is a dedicated explicit-clear entry
// point that skips the validator.
func SetBranchNameTx(tx *sql.Tx, id int, branchName string) error {
	if err := validateRefName(branchName); err != nil {
		return err
	}
	_, err := tx.Exec(`UPDATE BountyBoard SET branch_name = ? WHERE id = ?`, branchName, id)
	return err
}

// ClearBranchNameTx sets branch_name = '' for a task. Split from
// SetBranchNameTx so Fix #9's strict validator can reject empty-string
// inputs at that ingress while still allowing callers that DO need to
// clear the branch (e.g. jedi_council when spawning a conflict-
// resolution task) to do so via an explicit, audit-visible call.
func ClearBranchNameTx(tx *sql.Tx, id int) error {
	_, err := tx.Exec(`UPDATE BountyBoard SET branch_name = '' WHERE id = ?`, id)
	return err
}

func SetBountyPriority(db *sql.DB, id, priority int) {
	db.Exec(`UPDATE BountyBoard SET priority = ? WHERE id = ?`, priority, id)
}

// ── Task dependencies ─────────────────────────────────────────────────────────

// AddDependency records that taskID depends on dependsOn.
// No-op if the dependency already exists (INSERT OR IGNORE).
func AddDependency(db *sql.DB, taskID, dependsOn int) {
	db.Exec(`INSERT OR IGNORE INTO TaskDependencies (task_id, depends_on) VALUES (?, ?)`, taskID, dependsOn)
}

// AddConvoyTaskTx creates a CodeEdit subtask within a convoy using an existing transaction.
// Mirrors AddConvoyTask for use inside a caller-owned transaction.
func AddConvoyTaskTx(tx *sql.Tx, parentID int, repo, payload string, convoyID, priority int, status string) (int, error) {
	res, err := tx.Exec(
		`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, convoy_id, priority, created_at)
		 VALUES (?, ?, 'CodeEdit', ?, ?, ?, ?, datetime('now'))`,
		parentID, repo, status, payload, convoyID, priority)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return int(id), nil
}

// AddConvoyTaskWithStageTx creates a CodeEdit subtask within a convoy, stamping
// BountyBoard.stage_id at insert time. stageID must be > 0 — callers that don't
// have a stage assignment use AddConvoyTaskTx instead and let stage_id default
// to NULL. (D5.5 P2: multi-stage convoy task creation path.)
func AddConvoyTaskWithStageTx(tx *sql.Tx, parentID int, repo, payload string, convoyID, priority, stageID int, status string) (int, error) {
	if stageID <= 0 {
		return 0, fmt.Errorf("AddConvoyTaskWithStageTx: stageID must be > 0 (got %d) — use AddConvoyTaskTx for stage-less tasks", stageID)
	}
	res, err := tx.Exec(
		`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, convoy_id, priority, stage_id, created_at)
		 VALUES (?, ?, 'CodeEdit', ?, ?, ?, ?, ?, datetime('now'))`,
		parentID, repo, status, payload, convoyID, priority, stageID)
	if err != nil {
		return 0, fmt.Errorf("AddConvoyTaskWithStageTx: insert convoy=%d stage=%d: %w", convoyID, stageID, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("AddConvoyTaskWithStageTx: LastInsertId: %w", err)
	}
	return int(id), nil
}

// AddDependencyTx records that taskID depends on dependsOn using an existing transaction.
// Returns an error so callers can roll back on failure.
func AddDependencyTx(tx *sql.Tx, taskID, dependsOn int) error {
	_, err := tx.Exec(`INSERT OR IGNORE INTO TaskDependencies (task_id, depends_on) VALUES (?, ?)`, taskID, dependsOn)
	return err
}

// GetDependencies returns all task IDs that taskID depends on (its blockers).
func GetDependencies(db *sql.DB, taskID int) []int {
	rows, err := db.Query(`SELECT depends_on FROM TaskDependencies WHERE task_id = ? ORDER BY depends_on ASC`, taskID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var ids []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			log.Printf("GetDependencies: scan failed: %v", err)
			continue
		}
		ids = append(ids, id)
	}
	if rErr := rows.Err(); rErr != nil {
		log.Printf("tasks.go:GetDependencies: rows iter error: %v", rErr)
	}
	return ids
}

// RemoveDependenciesOf removes all dependencies FOR a task (the task is unblocked entirely).
func RemoveDependenciesOf(db *sql.DB, taskID int) {
	db.Exec(`DELETE FROM TaskDependencies WHERE task_id = ?`, taskID)
}

// UnblockDependentsOf removes all dependency edges that point TO id, freeing any tasks
// that were waiting solely on id. Non-recursive — ClaimBounty handles claimability dynamically.
// Returns the number of dependency edges removed.
func UnblockDependentsOf(db *sql.DB, id int) int {
	res, _ := db.Exec(`DELETE FROM TaskDependencies WHERE depends_on = ?`, id)
	n, _ := res.RowsAffected()
	return int(n)
}

// UnblockDependentsOfTx is the transactional sibling of UnblockDependentsOf.
// Returns (rowsDeleted, error). Does not swallow the exec error.
func UnblockDependentsOfTx(tx *sql.Tx, id int) (int, error) {
	res, err := tx.Exec(`DELETE FROM TaskDependencies WHERE depends_on = ?`, id)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ── Cost computation ──────────────────────────────────────────────────────────

// Pricing constants for Claude Sonnet (per million tokens).
const (
	PriceInputPerMillion  = 3.0  // $3.00/M input tokens
	PriceOutputPerMillion = 15.0 // $15.00/M output tokens
)

// TaskCostDollars computes the cost in dollars for a given token usage.
func TaskCostDollars(tokensIn, tokensOut int) float64 {
	return float64(tokensIn)*PriceInputPerMillion/1_000_000 +
		float64(tokensOut)*PriceOutputPerMillion/1_000_000
}

// TotalSpendDollars returns the total spend in dollars across all TaskHistory rows.
func TotalSpendDollars(db *sql.DB) float64 {
	var tokensIn, tokensOut int64
	db.QueryRow(`SELECT COALESCE(SUM(tokens_in),0), COALESCE(SUM(tokens_out),0) FROM TaskHistory`).
		Scan(&tokensIn, &tokensOut)
	return float64(tokensIn)*PriceInputPerMillion/1_000_000 +
		float64(tokensOut)*PriceOutputPerMillion/1_000_000
}

// SpendRateDollars returns the total spend across all TaskHistory rows with
// created_at within the given window (e.g. "1 hours", "30 minutes"). Used by
// the spend-burn-watch dog and the /api/status endpoint to surface in-flight
// burn rate rather than lifetime spend.
//
// Window is a SQLite-compatible modifier (minus sign is added internally).
func SpendRateDollars(db *sql.DB, window string) float64 {
	var tokensIn, tokensOut int64
	db.QueryRow(
		`SELECT COALESCE(SUM(tokens_in),0), COALESCE(SUM(tokens_out),0)
		 FROM TaskHistory
		 WHERE created_at > datetime('now', ?)`,
		"-"+window,
	).Scan(&tokensIn, &tokensOut)
	return float64(tokensIn)*PriceInputPerMillion/1_000_000 +
		float64(tokensOut)*PriceOutputPerMillion/1_000_000
}

// AttemptsInWindow returns the count of TaskHistory rows created within the
// given SQLite modifier window (e.g. "1 hours"). Drives the attempts_last_hour
// stat surfaced on /api/status — a thrashing fleet shows up here as a sustained
// high attempt rate even when spend happens to be moderate.
func AttemptsInWindow(db *sql.DB, window string) int {
	var n int
	db.QueryRow(
		`SELECT COUNT(*) FROM TaskHistory WHERE created_at > datetime('now', ?)`,
		"-"+window,
	).Scan(&n)
	return n
}

// ── Task history ──────────────────────────────────────────────────────────────

// RecordTaskHistory inserts a history entry and returns its row ID.
func RecordTaskHistory(db *sql.DB, taskID int, agent, sessionID, output, outcome string) int64 {
	var attempt int
	db.QueryRow(`SELECT COUNT(*) FROM TaskHistory WHERE task_id = ?`, taskID).Scan(&attempt)
	res, _ := db.Exec(`INSERT INTO TaskHistory (task_id, attempt, agent, session_id, claude_output, outcome)
		VALUES (?, ?, ?, ?, ?, ?)`, taskID, attempt+1, agent, sessionID, output, outcome)
	id, _ := res.LastInsertId()
	return id
}

// RecordTaskHistoryTx is the transactional sibling of RecordTaskHistory.
func RecordTaskHistoryTx(tx *sql.Tx, taskID int, agent, sessionID, output, outcome string) (int64, error) {
	var attempt int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM TaskHistory WHERE task_id = ?`, taskID).Scan(&attempt); err != nil {
		return 0, err
	}
	res, err := tx.Exec(`INSERT INTO TaskHistory (task_id, attempt, agent, session_id, claude_output, outcome)
		VALUES (?, ?, ?, ?, ?, ?)`, taskID, attempt+1, agent, sessionID, output, outcome)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return id, nil
}

// UpdateTaskHistoryTokens records token usage on an existing history row.
func UpdateTaskHistoryTokens(db *sql.DB, historyID int64, tokensIn, tokensOut int) {
	db.Exec(`UPDATE TaskHistory SET tokens_in = ?, tokens_out = ? WHERE id = ?`, tokensIn, tokensOut, historyID)
}

// UpdateTaskHistoryCost (D2 T1-1) records token usage AND the per-attempt
// cost estimate on an existing history row. Callers compute costUSD via
// claude.pricing.CostUSD(model, tokensIn, tokensOut) so the model→cost
// table stays the single source of truth and dashboard sums don't have to
// re-derive prices from the tokens column.
//
// Returns error per CLAUDE.md "new mutator policy" — a write failure here
// silently dropped per-task cost data, blinding the spend-watch dog to
// runaway agents.
func UpdateTaskHistoryCost(db *sql.DB, historyID int64, tokensIn, tokensOut int, costUSD float64) error {
	_, err := db.Exec(
		`UPDATE TaskHistory SET tokens_in = ?, tokens_out = ?, cost_usd_estimate = ? WHERE id = ?`,
		tokensIn, tokensOut, costUSD, historyID)
	return err
}

// SetSpendSuspended (D2 T1-1) flips BountyBoard.spend_suspended for one task.
// Used by dogTaskSpendWatch when a task crosses the per-task escalate
// threshold; ClaimBounty / ClaimForReview / ClaimForCaptainReview all filter
// on the flag so the next claim cycle can't pick up the runaway row.
//
// Returns error per CLAUDE.md "new mutator policy" — a silent failure here
// would leave a runaway task claimable on the next tick.
func SetSpendSuspended(db *sql.DB, taskID int, suspended bool) error {
	v := 0
	if suspended {
		v = 1
	}
	_, err := db.Exec(`UPDATE BountyBoard SET spend_suspended = ? WHERE id = ?`, v, taskID)
	return err
}

// GetSpendSuspended returns the spend_suspended flag for one task. Used by
// the dashboard to surface the suspended-state badge.
func GetSpendSuspended(db *sql.DB, taskID int) bool {
	var v int
	db.QueryRow(`SELECT IFNULL(spend_suspended, 0) FROM BountyBoard WHERE id = ?`, taskID).Scan(&v)
	return v == 1
}

// StampHistoryMemoryIDs records which FleetMemory rows were injected into an
// attempt's prompt. Called right after RecordTaskHistory so the dashboard can
// later show the operator the EXACT memories the agent saw rather than re-
// querying at display time (which would show current-state memories, not the
// ones that were live when the agent ran). memoryIDs is persisted as a CSV
// string for simplicity — keeps join queries easy and avoids JSON parsing.
func StampHistoryMemoryIDs(db *sql.DB, historyID int64, memoryIDs []int) {
	if historyID <= 0 || len(memoryIDs) == 0 {
		return
	}
	parts := make([]string, len(memoryIDs))
	for i, id := range memoryIDs {
		parts[i] = strconv.Itoa(id)
	}
	db.Exec(`UPDATE TaskHistory SET memory_ids = ? WHERE id = ?`, strings.Join(parts, ","), historyID)
}

// GetFleetMemoriesByIDs returns the specific memory rows named by the given
// IDs, preserving the order of the input. Used by the dashboard task-detail
// handler to show exactly the memories that were injected into an attempt's
// context (rather than re-querying, which would show today's matches).
func GetFleetMemoriesByIDs(db *sql.DB, ids []int) []FleetMemoryEntry {
	if len(ids) == 0 {
		return nil
	}
	// Build IN (?, ?, ...) dynamically.
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	q := `SELECT id, repo, task_id, outcome, summary, files_changed, IFNULL(topic_tags, ''), created_at
		FROM FleetMemory WHERE id IN (` + strings.Join(placeholders, ",") + `)`
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	byID := make(map[int]FleetMemoryEntry, len(ids))
	for rows.Next() {
		var e FleetMemoryEntry
		if err := rows.Scan(&e.ID, &e.Repo, &e.TaskID, &e.Outcome, &e.Summary, &e.FilesChanged, &e.TopicTags, &e.CreatedAt); err == nil {
			byID[e.ID] = e
		}
	}
	if rErr := rows.Err(); rErr != nil {
		log.Printf("tasks.go:GetFleetMemoriesByIDs: rows iter error: %v", rErr)
	}
	// Preserve caller's order; skip any IDs that no longer exist.
	out := make([]FleetMemoryEntry, 0, len(ids))
	for _, id := range ids {
		if e, ok := byID[id]; ok {
			out = append(out, e)
		}
	}
	return out
}

// ParseMemoryIDsCSV parses a TaskHistory.memory_ids CSV string into a slice of
// ints. Returns nil on empty/invalid input — callers treat that as "no
// snapshot available for this attempt" and may fall back to re-querying.
func ParseMemoryIDsCSV(csv string) []int {
	csv = strings.TrimSpace(csv)
	if csv == "" {
		return nil
	}
	parts := strings.Split(csv, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err == nil && n > 0 {
			out = append(out, n)
		}
	}
	return out
}

func GetTaskHistory(db *sql.DB, taskID int) []TaskHistoryEntry {
	rows, err := db.Query(`SELECT id, task_id, attempt, agent, session_id, claude_output, outcome,
		IFNULL(tokens_in,0), IFNULL(tokens_out,0), IFNULL(cost_usd_estimate,0), IFNULL(memory_ids, ''), created_at
		FROM TaskHistory WHERE task_id = ? ORDER BY attempt ASC`, taskID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var entries []TaskHistoryEntry
	for rows.Next() {
		var e TaskHistoryEntry
		if err := rows.Scan(&e.ID, &e.TaskID, &e.Attempt, &e.Agent, &e.SessionID, &e.ClaudeOutput, &e.Outcome,
			&e.TokensIn, &e.TokensOut, &e.CostUSDEstimate, &e.MemoryIDs, &e.CreatedAt); err != nil {
			log.Printf("GetTaskHistory: scan failed: %v", err)
			continue
		}
		entries = append(entries, e)
	}
	if rErr := rows.Err(); rErr != nil {
		log.Printf("tasks.go:GetTaskHistory: rows iter error: %v", rErr)
	}
	return entries
}

// LifetimeCostUSD (D2 T1-1) returns the sum of cost_usd_estimate across every
// TaskHistory row for the given task. Surfaced on the dashboard's task
// detail view so the operator sees actual lifetime cost from the per-
// model price table, not a re-derived estimate from tokens alone.
func LifetimeCostUSD(db *sql.DB, taskID int) float64 {
	var total float64
	db.QueryRow(`SELECT COALESCE(SUM(cost_usd_estimate), 0) FROM TaskHistory WHERE task_id = ?`, taskID).
		Scan(&total)
	return total
}

// ── Fleet memory ──────────────────────────────────────────────────────────────

// StoreFleetMemory saves a lesson learned from a completed or failed task.
// outcome should be "success" or "failure".
// The FTS index is updated explicitly after the main insert — an FTS failure
// is non-fatal and will not roll back the memory record.
// StoreFleetMemory saves a lesson learned from a completed or failed task.
// topicTags is a comma-separated list of 3-5 short keywords (optional — pass
// "" if none). Tags are indexed in FTS5 alongside the summary to broaden
// recall — a memory tagged "authentication, jwt" matches a query about
// "login" even when the summary prose doesn't mention "login" literally.
func StoreFleetMemory(db *sql.DB, repo string, taskID int, outcome, summary, filesChanged, topicTags string) {
	res, err := db.Exec(
		`INSERT INTO FleetMemory (repo, task_id, outcome, summary, files_changed, topic_tags)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		repo, taskID, outcome, summary, filesChanged, topicTags)
	if err != nil {
		return
	}
	if id, idErr := res.LastInsertId(); idErr == nil {
		db.Exec(`INSERT INTO FleetMemory_fts(rowid, summary, files_changed, topic_tags)
			VALUES (?, ?, ?, ?)`,
			id, summary, filesChanged, topicTags)
	}
}

// DeleteFleetMemory removes a single memory entry by ID from both the main table
// and the FTS index.
func DeleteFleetMemory(db *sql.DB, id int) bool {
	db.Exec(`DELETE FROM FleetMemory_fts WHERE rowid = ?`, id)
	res, _ := db.Exec(`DELETE FROM FleetMemory WHERE id = ?`, id)
	n, _ := res.RowsAffected()
	return n > 0
}

// memoryStopWords is the English stop-word list used when tokenizing a query
// for FleetMemory retrieval. Domain-specific tokens (task, convoy, pilot,
// etc.) are DELIBERATELY excluded — BM25's IDF downweights them automatically
// once the corpus grows, and over-aggressive stripping costs more recall than
// it gains in precision. Keep this list conservative.
var memoryStopWords = map[string]bool{
	// articles / determiners / conjunctions
	"the": true, "and": true, "any": true, "all": true, "but": true,
	"for": true, "nor": true, "yet": true, "not": true, "its": true,
	"that": true, "this": true, "these": true, "those": true,
	"with": true, "from": true, "into": true, "onto": true,
	"than": true, "also": true, "such": true, "each": true,
	// auxiliary / modal verbs
	"are": true, "was": true, "were": true, "been": true, "being": true,
	"can": true, "will": true, "would": true, "could": true, "should": true,
	"may": true, "might": true, "must": true, "has": true, "have": true,
	"had": true, "does": true, "did": true, "doing": true, "done": true,
	// interrogatives / connectors
	"how": true, "why": true, "where": true, "when": true, "which": true,
	"who": true, "whom": true, "whose": true, "what": true,
	// quantifiers / adjectives of frequency
	"more": true, "most": true, "some": true, "few": true, "many": true,
	"much": true, "one": true, "two": true, "too": true, "very": true,
	"now": true, "then": true, "here": true, "there": true,
	// common instructive verbs that appear in most task payloads
	"use": true, "uses": true, "using": true, "used": true,
	"see": true, "seen": true, "saw": true, "show": true, "shows": true,
	"new": true, "old": true, "get": true, "gets": true,
	"let": true, "lets": true, "make": true, "makes": true, "making": true,
	"ensure": true, "please": true, "only": true, "just": true,
}

// maxMemoryQueryTerms caps how many unique tokens we feed into FTS to prevent
// the AND query from becoming over-restrictive on long task payloads. BM25
// weighting within the first N terms is sufficient for ranking quality.
const maxMemoryQueryTerms = 20

// extractMemoryQueryTerms tokenizes free-form text into FTS-ready terms,
// lowercasing, filtering stop words, requiring ≥3 chars, and deduplicating.
// Returns the distinct terms in first-seen order.
func extractMemoryQueryTerms(q string) []string {
	var b strings.Builder
	for _, r := range q {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune(' ')
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, w := range strings.Fields(b.String()) {
		if len(w) < 3 {
			continue
		}
		lw := strings.ToLower(w)
		if memoryStopWords[lw] {
			continue
		}
		if seen[lw] {
			continue
		}
		seen[lw] = true
		out = append(out, lw)
	}
	return out
}

// sanitizeFTSQuery is kept for backwards compatibility with any caller that
// built an OR query directly. New code should use extractMemoryQueryTerms +
// ftsMemoryLookup via GetFleetMemories.
func sanitizeFTSQuery(q string) string {
	terms := extractMemoryQueryTerms(q)
	if len(terms) == 0 {
		return ""
	}
	return strings.Join(terms, " OR ")
}

// ftsMemoryLookup executes an FTS5 MATCH query and returns repo-filtered
// FleetMemory rows in BM25 rank order, up to limit entries. Returns nil (not
// empty slice) on error or no matches — callers rely on the zero-result path
// to decide whether to try a looser query.
func ftsMemoryLookup(db *sql.DB, repo, ftsQ string, limit int) []FleetMemoryEntry {
	// Over-fetch to leave room for repo filtering (FTS index doesn't know repo).
	ftsRows, err := db.Query(
		`SELECT rowid FROM FleetMemory_fts WHERE FleetMemory_fts MATCH ? ORDER BY rank LIMIT ?`,
		ftsQ, limit*3)
	if err != nil {
		return nil
	}
	var rankedIDs []int
	for ftsRows.Next() {
		var id int
		if ftsRows.Scan(&id) == nil {
			rankedIDs = append(rankedIDs, id)
		}
	}
	if rErr := ftsRows.Err(); rErr != nil {
		log.Printf("tasks.go:ftsMemoryLookup: rows iter error: %v", rErr)
	}
	ftsRows.Close()
	if len(rankedIDs) == 0 {
		return nil
	}
	var entries []FleetMemoryEntry
	for _, id := range rankedIDs {
		if len(entries) >= limit {
			break
		}
		var e FleetMemoryEntry
		err := db.QueryRow(
			`SELECT id, repo, task_id, outcome, summary, files_changed, IFNULL(topic_tags, ''), created_at
			 FROM FleetMemory WHERE id = ? AND repo = ?`, id, repo).
			Scan(&e.ID, &e.Repo, &e.TaskID, &e.Outcome, &e.Summary, &e.FilesChanged, &e.TopicTags, &e.CreatedAt)
		if err == nil {
			entries = append(entries, e)
		}
	}
	if len(entries) == 0 {
		return nil
	}
	return entries
}

// recencyMemoryLookup returns the most-recent memories for a repo. Used as a
// fallback ONLY when the caller didn't provide a query or the query yielded
// no usable terms. Never triggered when a real query returned zero matches —
// irrelevant recent memories are worse than no memories (they actively
// mislead agent reasoning, as demonstrated by the task-248 bug where a stale
// "ConvoyEvents was reverted" memory was injected via recency and convinced
// the astromech that task 247's work didn't exist).
func recencyMemoryLookup(db *sql.DB, repo string, limit int) []FleetMemoryEntry {
	rows, err := db.Query(`
		SELECT id, repo, task_id, outcome, summary, files_changed, IFNULL(topic_tags, ''), created_at
		FROM FleetMemory
		WHERE repo = ?
		ORDER BY created_at DESC, id DESC
		LIMIT ?`, repo, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var entries []FleetMemoryEntry
	for rows.Next() {
		var e FleetMemoryEntry
		if err := rows.Scan(&e.ID, &e.Repo, &e.TaskID, &e.Outcome, &e.Summary, &e.FilesChanged, &e.TopicTags, &e.CreatedAt); err != nil {
			log.Printf("recencyMemoryLookup: scan failed: %v", err)
			continue
		}
		entries = append(entries, e)
	}
	if rErr := rows.Err(); rErr != nil {
		log.Printf("tasks.go:recencyMemoryLookup: rows iter error: %v", rErr)
	}
	return entries
}

// GetFleetMemories returns memory entries relevant to `query` within `repo`.
//
// Retrieval strategy (precision → recall → abstain):
//  1. Extract content-bearing terms from the query (stop-words stripped,
//     ≥3 chars, unique, capped at maxMemoryQueryTerms).
//  2. Try an AND query: every term must appear. This gives the highest
//     precision — a memory only matches when it shares multiple content
//     tokens with the current task. BM25 ranks by overlap strength.
//  3. If AND returns zero, fall back to OR: any shared token matches.
//  4. If OR also returns zero, return nil. We deliberately DO NOT fall
//     back to recency here — irrelevant memories poison agent reasoning
//     more than absent ones. The caller sees an empty slice and skips
//     the memory-injection block.
//
// Recency is only used when the caller provides no query (or the query
// consists entirely of stop words), i.e. when there's no signal to rank by.
func GetFleetMemories(db *sql.DB, repo, query string, limit int) []FleetMemoryEntry {
	if query == "" {
		return recencyMemoryLookup(db, repo, limit)
	}
	terms := extractMemoryQueryTerms(query)
	if len(terms) == 0 {
		return recencyMemoryLookup(db, repo, limit)
	}
	if len(terms) > maxMemoryQueryTerms {
		terms = terms[:maxMemoryQueryTerms]
	}

	// Precision first: AND query.
	if results := ftsMemoryLookup(db, repo, strings.Join(terms, " AND "), limit); len(results) > 0 {
		return results
	}
	// Recall fallback: OR query.
	if results := ftsMemoryLookup(db, repo, strings.Join(terms, " OR "), limit); len(results) > 0 {
		return results
	}
	// No relevant memories — return empty, NEVER fall back to recency.
	return nil
}

// ListAllFleetMemories returns all memories, optionally filtered by repo.
func ListAllFleetMemories(db *sql.DB, repo string, limit int) []FleetMemoryEntry {
	var rows *sql.Rows
	var err error
	if repo != "" {
		rows, err = db.Query(`SELECT id, repo, task_id, outcome, summary, files_changed, IFNULL(topic_tags, ''), created_at FROM FleetMemory WHERE repo = ? ORDER BY created_at DESC, id DESC LIMIT ?`, repo, limit)
	} else {
		rows, err = db.Query(`SELECT id, repo, task_id, outcome, summary, files_changed, IFNULL(topic_tags, ''), created_at FROM FleetMemory ORDER BY created_at DESC, id DESC LIMIT ?`, limit)
	}
	if err != nil {
		return nil
	}
	defer rows.Close()
	var entries []FleetMemoryEntry
	for rows.Next() {
		var e FleetMemoryEntry
		if err := rows.Scan(&e.ID, &e.Repo, &e.TaskID, &e.Outcome, &e.Summary, &e.FilesChanged, &e.TopicTags, &e.CreatedAt); err != nil {
			log.Printf("ListAllFleetMemories: scan failed: %v", err)
			continue
		}
		entries = append(entries, e)
	}
	if rErr := rows.Err(); rErr != nil {
		log.Printf("tasks.go:ListAllFleetMemories: rows iter error: %v", rErr)
	}
	return entries
}
