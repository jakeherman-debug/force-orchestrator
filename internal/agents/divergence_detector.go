package agents

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	igit "force-orchestrator/internal/git"
	"force-orchestrator/internal/store"
)

// recentCommitHashRingDepth is the size of the commit-tree-hash ring kept
// per BountyBoard row. Five is enough to catch A-B-A oscillations and
// short cycles like A-B-C-A while staying compact in the JSON column.
// Hardcoded — not operator-tunable in this track (D2 T1-3.5).
const recentCommitHashRingDepth = 5

// divergenceTreeHashLookupTimeout caps the bounded git rev-parse call.
const divergenceTreeHashLookupTimeout = 5 * time.Second

// CommitHashRing wraps the JSON-array column
// `BountyBoard.recent_commit_hashes_json`. Hashes are appended in commit
// order; once at capacity, oldest entries are dropped.
type CommitHashRing struct {
	Hashes []string
}

// Push appends hash to the ring and truncates to the most recent
// recentCommitHashRingDepth entries.
func (r *CommitHashRing) Push(hash string) {
	r.Hashes = append(r.Hashes, hash)
	if len(r.Hashes) > recentCommitHashRingDepth {
		r.Hashes = r.Hashes[len(r.Hashes)-recentCommitHashRingDepth:]
	}
}

// IsCircle returns true if `latest` matches any non-immediate prior
// entry in the ring. The most-recent entry is excluded so an `--amend`-
// equivalent re-commit of the same tree is NOT flagged as a circle
// (per D2 T1-3.5 edge case).
func (r *CommitHashRing) IsCircle(latest string) bool {
	if latest == "" {
		return false
	}
	if len(r.Hashes) < 2 {
		return false
	}
	for i := 0; i < len(r.Hashes)-1; i++ {
		if r.Hashes[i] == latest {
			return true
		}
	}
	return false
}

// loadCommitHashRingTx reads the JSON-array column inside the given
// transaction. An empty / null column produces an empty ring.
func loadCommitHashRingTx(tx *sql.Tx, taskID int) (*CommitHashRing, error) {
	var raw string
	if err := tx.QueryRow(
		`SELECT IFNULL(recent_commit_hashes_json, '[]') FROM BountyBoard WHERE id = ?`,
		taskID,
	).Scan(&raw); err != nil {
		return nil, fmt.Errorf("loadCommitHashRingTx(%d): %w", taskID, err)
	}
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return &CommitHashRing{}, nil
	}
	var hashes []string
	if err := json.Unmarshal([]byte(raw), &hashes); err != nil {
		return nil, fmt.Errorf("loadCommitHashRingTx(%d): unmarshal: %w", taskID, err)
	}
	return &CommitHashRing{Hashes: hashes}, nil
}

// saveCommitHashRingTx writes the ring back as a plain JSON array.
func saveCommitHashRingTx(tx *sql.Tx, taskID int, r *CommitHashRing) error {
	hashes := r.Hashes
	if hashes == nil {
		hashes = []string{}
	}
	raw, err := json.Marshal(hashes)
	if err != nil {
		return fmt.Errorf("saveCommitHashRingTx(%d): marshal: %w", taskID, err)
	}
	if _, err := tx.Exec(
		`UPDATE BountyBoard SET recent_commit_hashes_json = ? WHERE id = ?`,
		string(raw), taskID,
	); err != nil {
		return fmt.Errorf("saveCommitHashRingTx(%d): %w", taskID, err)
	}
	return nil
}

// RecordCommitAndCheckCircle atomically loads the ring, pushes the new
// tree-hash, persists the result, and reports whether the new tree-hash
// matches a non-immediate prior entry. Load + push + save runs inside a
// single transaction so concurrent astromech sessions on the same row
// can't lose updates to each other.
func RecordCommitAndCheckCircle(ctx context.Context, db *sql.DB, taskID int, treeHash string) (bool, error) {
	if treeHash == "" {
		return false, fmt.Errorf("RecordCommitAndCheckCircle(%d): empty tree hash", taskID)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("RecordCommitAndCheckCircle(%d): begin tx: %w", taskID, err)
	}
	defer func() { _ = tx.Rollback() }()

	ring, err := loadCommitHashRingTx(tx, taskID)
	if err != nil {
		return false, err
	}
	circled := ring.IsCircle(treeHash)
	ring.Push(treeHash)
	if saveErr := saveCommitHashRingTx(tx, taskID, ring); saveErr != nil {
		return false, saveErr
	}
	if commitErr := tx.Commit(); commitErr != nil {
		return false, fmt.Errorf("RecordCommitAndCheckCircle(%d): commit: %w", taskID, commitErr)
	}
	return circled, nil
}

// EscalateOnCircle terminates a task that has produced a circular commit
// tree-hash. The status transition uses Pattern P7 CAS (UpdateBountyStatusFrom)
// so a concurrent operator cancel cannot be clobbered. Sets spend_suspended=1
// so claim queries refuse to re-issue the row to an astromech.
//
// fromStatus is the caller's snapshot of the prior status (typically "Locked"
// when invoked from an astromech session). A zero-rows-affected CAS result
// returns a non-nil error so the caller can log the lost race; the task is
// left untouched in that case.
func EscalateOnCircle(ctx context.Context, db *sql.DB, taskID int, fromStatus string) error {
	if fromStatus == "" {
		return fmt.Errorf("EscalateOnCircle(%d): empty fromStatus", taskID)
	}
	n, err := store.UpdateBountyStatusFrom(db, taskID, fromStatus, "Escalated")
	if err != nil {
		return fmt.Errorf("EscalateOnCircle(%d): status CAS: %w", taskID, err)
	}
	if n == 0 {
		return fmt.Errorf("EscalateOnCircle(%d): lost race on status CAS (expected status %q)", taskID, fromStatus)
	}
	if _, err := db.ExecContext(ctx, `UPDATE BountyBoard SET spend_suspended = 1 WHERE id = ?`, taskID); err != nil {
		return fmt.Errorf("EscalateOnCircle(%d): spend_suspended write: %w", taskID, err)
	}
	msg := fmt.Sprintf(
		"Task #%d produced a circular commit pattern: a tree-hash matched a non-immediate prior entry within the last %d commits. The astromech is rewriting the same content; spend_suspended=1 set so the claim loop will not re-issue. Operator review required.",
		taskID, recentCommitHashRingDepth,
	)
	if _, escErr := CreateEscalation(db, taskID, store.SeverityMedium, msg); escErr != nil {
		// Fix #8a (AUDIT-041) pattern: the row is already Escalated; CreateEscalation
		// failed to insert the row in the Escalations table. Fall back to FailBounty
		// + operator mail so the task doesn't sit Escalated with no Escalations row.
		if failErr := store.FailBounty(db, taskID,
			fmt.Sprintf("circular commits detected; CreateEscalation failed: %v", escErr)); failErr != nil {
			return fmt.Errorf("EscalateOnCircle(%d): CreateEscalation failed (%v) AND FailBounty failed (%v)", taskID, escErr, failErr)
		}
	}
	// P27 burn-down: budget-gate the operator emit before SendMail.
	// On allowed=false the helper has already drop/digested per the
	// configured budget. Fail-open on err so a transient SQLite
	// glitch never silences a high-stakes alert.
	if allowed, _ := store.RespectNotificationBudget(
		context.Background(), db, "operator", "DivergenceDetector", "email", "{}",
		store.StakesHigh,
	); !allowed {
		// budget exhausted (StakesHigh always punches through, so
		// this branch only fires on a real config-set 0-cap row).
	} else {
		_ = allowed
	}
	store.SendMail(db, "DivergenceDetector", "operator",
		fmt.Sprintf("[CIRCULAR COMMITS] task #%d", taskID),
		msg, taskID, store.MailTypeAlert)
	store.LogAudit(db, "DivergenceDetector", "circular-commits-detected", taskID, msg)
	return nil
}

// readWorktreeTreeHash returns the tree-hash of HEAD in the given
// worktree. The bounded timeout follows the shortGitTimeout convention
// in astromech.go; ctx threads from the caller so daemon shutdown can
// cancel the lookup.
func readWorktreeTreeHash(ctx context.Context, worktreeDir string) (string, error) {
	lookupCtx, cancel := context.WithTimeout(ctx, divergenceTreeHashLookupTimeout)
	defer cancel()
	out, err := igit.LogAndRun(lookupCtx, igit.OpContext{}, "rev-parse", "git", "-C", worktreeDir, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// runDivergenceCheckHook is the per-commit hook called from astromech.go
// after each successful commit. It captures the worktree's HEAD tree-hash,
// records it in the BountyBoard ring, and on circle detection escalates
// the task. Returns true iff the caller should stop processing this task
// immediately (do NOT transition to nextReviewStatus).
func runDivergenceCheckHook(ctx context.Context, db *sql.DB, taskID int, worktreeDir, fromStatus string, logger interface {
	Printf(string, ...any)
}) (escalated bool) {
	treeHash, err := readWorktreeTreeHash(ctx, worktreeDir)
	if err != nil || treeHash == "" {
		if err != nil {
			logger.Printf("Task %d: divergence-detector: rev-parse HEAD^{tree} failed (%v) — skipping circle check", taskID, err)
		}
		return false
	}
	circled, recErr := RecordCommitAndCheckCircle(ctx, db, taskID, treeHash)
	if recErr != nil {
		logger.Printf("Task %d: divergence-detector: RecordCommitAndCheckCircle failed (%v) — skipping circle check", taskID, recErr)
		return false
	}
	if !circled {
		return false
	}
	logger.Printf("Task %d: divergence-detector: CIRCULAR COMMITS detected (tree=%s); escalating", taskID, treeHash)
	if escErr := EscalateOnCircle(ctx, db, taskID, fromStatus); escErr != nil {
		logger.Printf("Task %d: divergence-detector: EscalateOnCircle failed (%v); stale-lock detector will recover", taskID, escErr)
	}
	return true
}
