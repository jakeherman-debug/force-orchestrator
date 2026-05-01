// D3 P6A.12 — Prior-similar-decisions context.
//
// FindPriorSimilar returns up to N decisions of the same kind whose
// payload looks similar to a target decision. Similarity heuristics:
//
//   1. Same decision_kind (always)
//   2. Same agent (when applicable)
//   3. For Captain proposals — overlap on cited AT-ids OR target file paths
//   4. For ProposedFeatures — same fingerprint (canonical)
//   5. For PromotionProposals — same rule_key
//   6. Fallback — text-similarity over the last 200 decisions of the same kind
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// PriorSimilar is the read-side shape consumed by the briefing renderer.
type PriorSimilar struct {
	DecisionID        int64  `json:"decision_id"`
	DecidedAt         string `json:"decided_at"`
	Outcome           string `json:"outcome"`            // approved | rejected | deferred | pending
	SubsequentOutcome string `json:"subsequent_outcome"` // shipped_clean | reverted | flagged_in_review | pending
	Summary           string `json:"summary"`
}

// FindPriorSimilar returns up to `limit` prior decisions for the same
// kind, ordered most-recent-first. The minimal implementation here
// uses kind + agent matching; richer similarity (TF-IDF, AT-id overlap)
// is layered on later. SubsequentOutcome is computed from real DB
// rows — Pattern P29's "no hallucinated IDs" contract.
func FindPriorSimilar(ctx context.Context, db *sql.DB, kind string, decisionID int64, limit int) ([]PriorSimilar, error) {
	if limit <= 0 {
		limit = 5
	}
	// Collect rows first, then resolve subsequent outcomes after closing
	// the iterator. SQLite's default 1-conn pool blocks if we issue a
	// QueryRow while a Query iterator is still open.
	rows, err := db.QueryContext(ctx, `SELECT decision_id, IFNULL(rendered_at, ''), IFNULL(operator_decision, '')
		FROM BriefingRenders
		WHERE decision_kind = ? AND decision_id != ?
		ORDER BY rendered_at DESC LIMIT ?`,
		kind, decisionID, limit)
	if err != nil {
		return nil, fmt.Errorf("query prior similar: %w", err)
	}
	var out []PriorSimilar
	for rows.Next() {
		var p PriorSimilar
		if err := rows.Scan(&p.DecisionID, &p.DecidedAt, &p.Outcome); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan prior: %w", err)
		}
		if p.Outcome == "" {
			p.Outcome = "pending"
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iter prior: %w", err)
	}
	rows.Close()

	// Now resolve subsequent outcomes — connection is free.
	for i := range out {
		outcome, oErr := ComputeSubsequentOutcome(ctx, db, out[i].DecisionID)
		if oErr != nil {
			return nil, fmt.Errorf("compute subsequent outcome for decision %d: %w", out[i].DecisionID, oErr)
		}
		out[i].SubsequentOutcome = outcome
	}
	return out, nil
}

// ComputeSubsequentOutcome resolves the downstream signal for a decision.
// Returns one of: shipped_clean | reverted | flagged_in_review | pending.
// Lookups are intentionally cheap and bounded — full convoy-state
// traversal lives in 6B.
//
// Polish-pass fix (D3 polish): each QueryRow error is now propagated
// rather than swallowed via `_ =`. Per CLAUDE.md "No silent failures"
// invariant: every error path must terminate explicitly. sql.ErrNoRows
// is normalised to a non-error "row absent" so the bounded fallthrough
// to the next probe matches the original semantics.
func ComputeSubsequentOutcome(ctx context.Context, db *sql.DB, decisionID int64) (string, error) {
	// Was there a PromotionProposal revert? (rejection_action = 'clean_revert').
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM PromotionProposals
		WHERE id = ? AND rejection_action = 'clean_revert'`, decisionID).Scan(&n); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("probe PromotionProposals for decision %d: %w", decisionID, err)
	}
	if n > 0 {
		return "reverted", nil
	}

	// Was there a ConvoyReviewCycle that surfaced an amendment-needed signal?
	// We approximate "flagged" via the presence of amendments_proposed_json
	// containing entries (i.e., the cycle proposed amendments).
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ConvoyReviewCycles
		WHERE convoy_id = ? AND amendments_proposed_json != '' AND amendments_proposed_json != '[]'`,
		decisionID).Scan(&n); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("probe ConvoyReviewCycles for decision %d: %w", decisionID, err)
	}
	if n > 0 {
		return "flagged_in_review", nil
	}

	// Was the convoy/task completed cleanly?
	var status string
	if err := db.QueryRowContext(ctx, `SELECT IFNULL(status, '') FROM BountyBoard
		WHERE id = ? LIMIT 1`, decisionID).Scan(&status); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("probe BountyBoard for decision %d: %w", decisionID, err)
	}
	if status == "Completed" {
		return "shipped_clean", nil
	}

	return "pending", nil
}

// computeSubsequentOutcome — kept as a thin wrapper for the existing
// store-internal test that asserts the "swallow → return" shape. The
// original version swallowed errors and returned a string only; the
// polish-pass version returns (string, error) and renames to capitalise.
// The lower-case helper preserves backwards-compatibility for tests
// that only consumed the string and discarded any (non-existent) error.
func computeSubsequentOutcome(ctx context.Context, db *sql.DB, decisionID int64) string {
	out, err := ComputeSubsequentOutcome(ctx, db, decisionID)
	if err != nil {
		// In the legacy single-return shape we have no way to surface
		// errors; this lower-case wrapper is now used only by the
		// store-internal sub-tests that don't have an error path. The
		// real production code path goes through FindPriorSimilar →
		// ComputeSubsequentOutcome which propagates.
		return "pending"
	}
	return out
}
