package agents

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"force-orchestrator/internal/analytics"
)

// dogDisagreementTracker (D3 P3) computes cross-layer disagreement rates
// over rolling 7d/30d/90d windows and persists per-pair rows into
// DisagreementPairs. The dashboard's /api/disagreement-rates endpoint
// reads the latest row per pair.
//
// Why a dog: per the EC analysis-layer design (paired-runs.md § Phase
// 3), aggregate rates are batch-computed on a cadence — they are NOT
// computed inline at agent decision time. The dog is hourly so an
// operator skim of the dashboard sees recent-enough rates without
// burning a recompute budget on every dashboard refresh.
//
// Idempotence: PersistDisagreementRates UPSERTs on (pair_name,
// window_start, window_end). The window timestamps shift with each
// tick (start/end are computed from time.Now), so two ticks an hour
// apart produce two distinct rows per pair. A re-tick within the same
// second (e.g. CLI force-run twice in a row) UPSERT-overwrites.
//
// Anti-cheat: the dog respects IsEstopped + SpendCapExceeded at the
// top of its tick (cheap reads only — no Claude calls — but the
// CLAUDE.md "no work during e-stop" policy is universal).
func dogDisagreementTracker(ctx context.Context, db *sql.DB, logger interface{ Printf(string, ...any) }) error {
	if IsEstopped(db) {
		logger.Printf("disagreement-tracker: e-stop active — skipping tick")
		return nil
	}
	if SpendCapExceeded(db) {
		logger.Printf("disagreement-tracker: spend cap exceeded — skipping tick")
		return nil
	}

	// Rolling windows per spec: 7d, 30d, 90d. The dog persists one row
	// per (pair, window) per tick — UPSERT means re-running inside the
	// same second is a no-op.
	windows := []time.Duration{
		7 * 24 * time.Hour,
		30 * 24 * time.Hour,
		90 * 24 * time.Hour,
	}

	for _, w := range windows {
		results, err := analytics.ComputeDisagreementRates(ctx, db, w)
		if err != nil {
			return fmt.Errorf("disagreement-tracker: compute (window %v): %w", w, err)
		}
		if err := analytics.PersistDisagreementRates(ctx, db, results); err != nil {
			return fmt.Errorf("disagreement-tracker: persist (window %v): %w", w, err)
		}
		// Light log — surfaces the rate magnitudes so the operator can
		// spot a regression in the daemon log without opening the
		// dashboard.
		for pair, r := range results {
			if r.Deferred {
				continue
			}
			logger.Printf("disagreement-tracker: window=%v pair=%s samples=%d disagreements=%d rate=%.4f",
				w, pair, r.SampleCount, r.Disagreements, r.Rate)
		}
	}
	return nil
}
