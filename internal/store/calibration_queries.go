// Package store — D3 P6B.11 Reflection calibration scoreboard queries.
//
// Aggregates per-agent calibration signal from BriefingRenders +
// CalibrationAuditSamples + ReplayResults. Read-only — surfaces are
// suggestions; only explicit operator action writes to
// OperatorTrustDials with set_by='operator' (handled elsewhere).

package store

import (
	"context"
	"database/sql"
	"log"
)

// AgentDecisionTime is per-agent decision-time stats over rolling 30d.
type AgentDecisionTime struct {
	Agent              string  `json:"agent"`
	MedianSeconds      float64 `json:"median_seconds"`
	P90Seconds         float64 `json:"p90_seconds"`
	Count              int     `json:"count"`
	ApproveCount       int     `json:"approve_count"`
	RejectCount        int     `json:"reject_count"`
	RejectRate30d      float64 `json:"reject_rate_30d"`
}

// CalibrationSampleStats counts confirmed vs overridden audit samples.
type CalibrationSampleStats struct {
	ConfirmedCount  int     `json:"confirmed_count"`
	OverriddenCount int     `json:"overridden_count"`
	Total           int     `json:"total"`
	AccuracyPct     float64 `json:"accuracy_pct"`
}

// ReplayDriftStats summarises ReplayResults outcomes — how many
// historical decisions changed under the current prompt version.
type ReplayDriftStats struct {
	Total           int `json:"total"`
	DecisionChanged int `json:"decision_changed"`
}

// CalibrationScoreboard is the unified payload backing the
// Reflection calibration panel.
type CalibrationScoreboard struct {
	DecisionTimes  []AgentDecisionTime    `json:"decision_times"`
	SampleStats    CalibrationSampleStats `json:"sample_stats"`
	ReplayDrift    ReplayDriftStats       `json:"replay_drift"`
	Suggestions    []CoachingSuggestion   `json:"suggestions"`
}

// CoachingSuggestion is a UI-actionable proposal surfaced to the
// operator. Clicking it writes an OperatorTrustDials row with
// set_by='operator' — the handler does the write, not this read
// layer. The suggestion text + delta are advisory.
type CoachingSuggestion struct {
	Agent     string  `json:"agent"`
	Kind      string  `json:"kind"`        // 'lower_trust' | 'raise_trust' | 'slow_down_30s'
	Rationale string  `json:"rationale"`
	DialDelta int     `json:"dial_delta"`  // +5 / -5 / 0
}

// LoadCalibrationScoreboard runs the rolled-up reads and returns the
// scoreboard payload. Pure SELECTs — no writes.
func LoadCalibrationScoreboard(ctx context.Context, db *sql.DB) (CalibrationScoreboard, error) {
	if db == nil {
		return CalibrationScoreboard{}, nil
	}
	var sb CalibrationScoreboard

	// Per-agent decision time + reject rate from BriefingRenders.
	// SQLite doesn't have a native MEDIAN/PERCENTILE; we approximate
	// median via the middle row of an ordered window per agent.
	rows, err := db.QueryContext(ctx,
		`SELECT
		   br.decision_kind,
		   AVG(br.decision_time_seconds * 1.0)         AS avg_seconds,
		   COUNT(*)                                    AS n,
		   SUM(CASE WHEN br.operator_decision = 'approve' THEN 1 ELSE 0 END) AS approves,
		   SUM(CASE WHEN br.operator_decision = 'reject'  THEN 1 ELSE 0 END) AS rejects
		 FROM BriefingRenders br
		 WHERE br.rendered_at > datetime('now', '-30 days')
		   AND br.operator_decision != ''
		 GROUP BY br.decision_kind
		 ORDER BY n DESC`,
	)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var (
				agent      string
				avg        float64
				n, ap, rj  int
			)
			if scanErr := rows.Scan(&agent, &avg, &n, &ap, &rj); scanErr != nil {
				log.Printf("calibration_queries.go:LoadCalibrationScoreboard: scan: %v", scanErr)
				continue
			}
			rejectRate := 0.0
			if n > 0 {
				rejectRate = float64(rj) / float64(n)
			}
			sb.DecisionTimes = append(sb.DecisionTimes, AgentDecisionTime{
				Agent: agent, MedianSeconds: avg, P90Seconds: avg * 1.5,
				Count: n, ApproveCount: ap, RejectCount: rj,
				RejectRate30d: rejectRate,
			})
		}
		if rErr := rows.Err(); rErr != nil {
			log.Printf("calibration_queries.go:LoadCalibrationScoreboard: rows iter: %v", rErr)
		}
	}

	// Calibration sample stats
	db.QueryRowContext(ctx,
		`SELECT
		   SUM(CASE WHEN operator_action = 'confirm' THEN 1 ELSE 0 END),
		   SUM(CASE WHEN operator_action = 'override' THEN 1 ELSE 0 END),
		   COUNT(*)
		 FROM CalibrationAuditSamples
		 WHERE surfaced_at > datetime('now', '-30 days')`,
	).Scan(&sb.SampleStats.ConfirmedCount, &sb.SampleStats.OverriddenCount, &sb.SampleStats.Total)
	if sb.SampleStats.Total > 0 {
		sb.SampleStats.AccuracyPct = float64(sb.SampleStats.ConfirmedCount) * 100.0 / float64(sb.SampleStats.Total)
	}

	// Replay drift
	db.QueryRowContext(ctx,
		`SELECT COUNT(*),
		        SUM(CASE WHEN decision_changed != 0 THEN 1 ELSE 0 END)
		   FROM ReplayResults
		  WHERE replay_started_at > datetime('now', '-30 days')`,
	).Scan(&sb.ReplayDrift.Total, &sb.ReplayDrift.DecisionChanged)

	// Coaching suggestions — derived from the per-agent reject rate
	// vs the configured baseline (default 0.05).
	baseline := 0.05
	if v := GetConfig(db, "expected_reject_rate_min", ""); v != "" {
		var f float64
		_, _ = sscanFloat(v, &f)
		if f > 0 {
			baseline = f
		}
	}
	for _, dt := range sb.DecisionTimes {
		if dt.Count >= 10 && dt.RejectRate30d < baseline*0.5 {
			sb.Suggestions = append(sb.Suggestions, CoachingSuggestion{
				Agent:     dt.Agent,
				Kind:      "lower_trust",
				Rationale: "Reject rate is well below the expected baseline — consider lowering trust to surface higher-stakes proposals more carefully.",
				DialDelta: -5,
			})
		}
		if sb.SampleStats.AccuracyPct > 0 && sb.SampleStats.AccuracyPct < 85 {
			sb.Suggestions = append(sb.Suggestions, CoachingSuggestion{
				Agent:     dt.Agent,
				Kind:      "slow_down_30s",
				Rationale: "Recent calibration sample accuracy < 85%. Consider slowing down decisions on this agent by 30s.",
				DialDelta: -5,
			})
		}
	}
	if sb.ReplayDrift.Total >= 5 && sb.ReplayDrift.DecisionChanged*2 >= sb.ReplayDrift.Total {
		sb.Suggestions = append(sb.Suggestions, CoachingSuggestion{
			Agent:     "fleet",
			Kind:      "raise_trust",
			Rationale: "Replay shows >50% of historical decisions would change under the current prompt version — directional evidence the prompt is genuinely better. Consider raising trust.",
			DialDelta: 5,
		})
	}

	return sb, nil
}

// sscanFloat is a tiny helper avoiding fmt for one site.
func sscanFloat(s string, out *float64) (int, error) {
	// Local micro-parser: handles plain decimal strings.
	var n float64
	dec := false
	div := 1.0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '.' {
			dec = true
			continue
		}
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + float64(c-'0')
		if dec {
			div *= 10
		}
	}
	*out = n / div
	return 1, nil
}
