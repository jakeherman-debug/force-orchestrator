package stagegate

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// SoakMinutes is the simplest time-based gate: wait N minutes after
// `all_prs_merged_at` before flipping GatePassed. Used as the default
// gate for stages that just need to "let the change soak" before the
// next stage opens — no metric query, no operator action, just a
// timer.
//
// Config: {"minutes": int}  (must be > 0)
//
// Evaluate states:
//   - stage hasn't reached AllPRsMerged yet → ErrPending. The dog
//     re-checks next tick.
//   - elapsed < minutes → ErrPending with a "remaining" reason string
//     so the dashboard surfaces a useful countdown.
//   - elapsed >= minutes → passed=true.
type SoakMinutes struct{}

// Type implements Gate.
func (SoakMinutes) Type() string { return "soak_minutes" }

// Evaluate implements Gate.
func (SoakMinutes) Evaluate(_ context.Context, _ *sql.DB, stage StageContext) (bool, string, error) {
	var cfg struct {
		Minutes int `json:"minutes"`
	}
	if err := json.Unmarshal(stage.GateConfig, &cfg); err != nil {
		return false, "", fmt.Errorf("soak_minutes: parse config: %w", err)
	}
	if cfg.Minutes <= 0 {
		return false, "", fmt.Errorf("soak_minutes: minutes must be positive, got %d", cfg.Minutes)
	}
	if stage.AllPRsMergedAt.IsZero() {
		return false, "stage hasn't reached AllPRsMerged yet", ErrPending
	}
	soak := time.Duration(cfg.Minutes) * time.Minute
	elapsed := time.Since(stage.AllPRsMergedAt)
	if elapsed < soak {
		remaining := soak - elapsed
		return false, fmt.Sprintf("soak: %v remaining of %dmin", remaining.Round(time.Second), cfg.Minutes), ErrPending
	}
	return true, fmt.Sprintf("soak %dmin elapsed", cfg.Minutes), nil
}
