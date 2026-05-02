// Package agents — Senate ↔ Chancellor integration hook (D4 Phase 3).
//
// This file documents and implements the integration point between
// Commander's ProposedConvoy emit and the Chancellor's
// AwaitingChancellorReview claim. Per docs/next-gen-agents.md § "Senate"
// (line ~256, "Trigger"), the Senate slots in BETWEEN those two writes:
//
//	Commander → ProposedConvoys row + AwaitingSenateReview transition
//	            → Senate (parallel Senator reviews)
//	            → AwaitingChancellorReview (auto-advance on concur)
//	            ↓ (or block to Pending on high-confidence dissent)
//	Chancellor → AwaitingChancellorReview claim
//
// The integration is implemented by a small, testable helper called
// from Commander's runDecomposeTask AFTER StoreProposedConvoy succeeds.
// QueueSenateReviewHook decides between two routes:
//
//   - At least one active Senator exists  → write Feature status to
//     'AwaitingSenateReview' and queue a SenateReview task. The Senate
//     reviewer's runSenateReviewTask handler is the one that flips
//     status to 'AwaitingChancellorReview' (or returns to Pending).
//
//   - No active Senator                    → write Feature status
//     directly to 'AwaitingChancellorReview' (the existing pre-D4-P3
//     behaviour). Spec: "Zero cost, zero delay" when Senate is skipped.
//
// Anti-cheat: the hook MUST be idempotent — Commander's transitions are
// already idempotent under SQLite's write-serialization; this hook
// preserves that invariant by routing ALL state writes through
// store.UpdateBountyStatus / UpdateBountyStatusFrom (no raw SQL).
package agents

import (
	"database/sql"
	"fmt"

	"force-orchestrator/internal/store"
)

// QueueSenateReviewHook is called by Commander immediately after
// StoreProposedConvoy. The chosen status (returned as the second
// return) is the status the Commander should record in TaskHistory and
// the operator-facing notification mail. Errors propagate so Commander
// can route through its existing FailBounty path.
func QueueSenateReviewHook(db *sql.DB, featureID int, targetRepo string) (string, error) {
	// Count active Senators. We use a direct count rather than reading
	// the full chamber slice — the affected-Senator routing fanout
	// happens inside runSenateReviewTask, where the plan_json is
	// available. The hook only decides "any active Senator at all?".
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM SenateChambers WHERE status = 'active'`).Scan(&n); err != nil {
		return "", fmt.Errorf("QueueSenateReviewHook: count active chambers: %w", err)
	}
	if n == 0 {
		// No Senators yet — go straight to AwaitingChancellorReview to
		// preserve the pre-D4-P3 flow. Spec: zero-cost path.
		if err := store.UpdateBountyStatus(db, featureID, "AwaitingChancellorReview"); err != nil {
			return "", fmt.Errorf("QueueSenateReviewHook: direct AwaitingChancellorReview transition: %w", err)
		}
		return "AwaitingChancellorReview", nil
	}

	// Senators present — Feature first sits in AwaitingSenateReview so
	// runSenateReviewTask is the only path that can advance it. Queue
	// the review task BEFORE the status transition so a transient claim-
	// loop race doesn't see AwaitingSenateReview without the work item.
	if _, err := store.QueueSenateReview(db, featureID, targetRepo); err != nil {
		return "", fmt.Errorf("QueueSenateReviewHook: QueueSenateReview: %w", err)
	}
	if err := store.UpdateBountyStatus(db, featureID, "AwaitingSenateReview"); err != nil {
		return "", fmt.Errorf("QueueSenateReviewHook: AwaitingSenateReview transition: %w", err)
	}
	return "AwaitingSenateReview", nil
}
