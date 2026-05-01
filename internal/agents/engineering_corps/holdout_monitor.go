package engineering_corps

import (
	"context"
	"database/sql"
	"fmt"
	"log"

	"force-orchestrator/internal/agents/capabilities"
	"force-orchestrator/internal/store"
)

// HoldoutMonitor — bounty-claim plumbing for the GlobalHoldouts heartbeat.
//
// This handler emits a debug heartbeat against the GlobalHoldouts
// table — confirming a holdout exists (or doesn't), logging the
// active-count, and completing the bounty cleanly. SQL-only: no LLM
// call needed.
//
// The model-availability probe (issuing minimal-cost availability
// calls per model identifier in active experiments / holdouts;
// recording ModelAvailability rows) lives in the
// `model-availability-watch` dog (D3 fix-loop-1 / slice δ —
// internal/agents/model_availability_dog.go). The two paths are
// deliberately split: this handler is per-claim heartbeat plumbing;
// the dog is per-fleet ledger maintenance. Conflating them muddies
// which row-set is authoritative when a [HOLDOUT AT RISK] signal
// fires.
//
// No operator routing: this handler does not produce ratifiable
// output — it only emits a debug heartbeat. Any actionable signal
// (deprecation detection, holdout drift) lands as a separate
// PromotionProposal / operator mail in Phase 5/6.
func handleHoldoutMonitor(
	_ context.Context,
	cfg EngineeringCorpsConfig,
	_ *capabilities.Profile,
	agentName string,
	bounty *store.Bounty,
	logger *log.Logger,
) error {
	db := cfg.DB

	// Count active holdouts (retired_at = ''). Empty fleet is fine —
	// the holdout-mint dog hasn't run yet on a brand-new daemon. The
	// handler is idempotent: zero rows OR many rows both result in a
	// successful run.
	activeCount, err := countActiveHoldouts(db)
	if err != nil {
		// Surface the read failure cleanly so the operator sees it
		// (CLAUDE.md no-silent-failures invariant). The bounty is
		// failed by the dispatcher's failBountyOrLog after this return.
		return fmt.Errorf("HoldoutMonitor: count active holdouts: %w", err)
	}

	logger.Printf("[%s] HoldoutMonitor #%d: %d active holdout(s) — heartbeat ok (probe ledger maintained by model-availability-watch dog)",
		agentName, bounty.ID, activeCount)

	// Mark the bounty Completed — the heartbeat ran successfully.
	if err := store.UpdateBountyStatus(db, bounty.ID, "Completed"); err != nil {
		return fmt.Errorf("HoldoutMonitor: complete bounty: %w", err)
	}
	return nil
}

// countActiveHoldouts is a thin SQL helper kept inline here (rather
// than promoted to internal/store) so the EC handler's read path is
// easy to find and test in isolation. If a second EC handler ever
// needs the same query it can lift into store/.
func countActiveHoldouts(db *sql.DB) (int, error) {
	var n int
	row := db.QueryRow(`SELECT COUNT(*) FROM GlobalHoldouts WHERE IFNULL(retired_at, '') = ''`)
	if err := row.Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}
