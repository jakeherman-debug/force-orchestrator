package engineering_corps

import (
	"context"
	"database/sql"
	"fmt"
	"log"

	"force-orchestrator/internal/agents/capabilities"
	"force-orchestrator/internal/store"
)

// HoldoutMonitor — Phase 3 minimal scope.
//
// Per docs/paired-runs.md § Global Holdout, the full lifecycle and
// model-deprecation watch lives in P5/P6. The Phase 3 deliverable here
// is the bounty-claim plumbing: the handler reads the GlobalHoldouts
// table to confirm a holdout exists (or doesn't), logs a "no model
// deprecation detected" debug line, and completes the bounty cleanly.
//
// SQL-only: no LLM call needed. The full model-availability probe
// (issuing minimal-cost availability calls per model identifier in
// active experiments / holdouts; recording ModelAvailability rows;
// emitting [HOLDOUT AT RISK] mail) is deferred to the
// model-availability-watch dog landing in Phase 5/6.
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

	logger.Printf("[%s] HoldoutMonitor #%d: %d active holdout(s) — no model deprecation detected (full availability watch deferred to Phase 5/6)",
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
