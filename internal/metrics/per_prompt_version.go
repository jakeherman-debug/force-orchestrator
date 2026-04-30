package metrics

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrUnknownGroupedMetric — MetricByPromptVersion was called with a
// metric name that has no built-in aggregation (the registry's stored
// SQL bodies aren't auto-grouped because their TaskHistory binding is
// metric-specific). Adding a new grouped metric requires registering
// it via RegisterGroupedMetric.
var ErrUnknownGroupedMetric = errors.New("metrics: unknown grouped metric")

// GroupedMetric describes how to aggregate a metric across all
// TaskHistory rows in a window, grouped by prompt_version.
//
// The query is parameterized: the first ? is the `since` timestamp
// (formatted to SQLite shape), and the result columns must be
// (prompt_version TEXT, value REAL). value is metric-specific: a rate,
// a count, or a normalized score.
//
// Built-in grouped metrics live in groupedMetrics; tests and EC-
// proposed extensions can register additional ones via
// RegisterGroupedMetric.
type GroupedMetric struct {
	Name string

	// QuerySQL must select two columns:
	//   prompt_version TEXT, value REAL
	// The first parameter is `since` (a SQLite timestamp string).
	// Empty prompt_version rows are filtered (treated as legacy data
	// pre-D3 P1 that lacked the column) — see groupedQueryForName.
	QuerySQL string
}

// groupedMetrics is the built-in catalog. Each entry's QuerySQL is
// reviewed code (NOT runtime-loaded SQL); EC-proposed extensions go
// through the same RegisterMetric review path.
var groupedMetrics = map[string]GroupedMetric{
	// captain_approval_rate — fraction of Captain decisions that ended
	// in outcome="Completed" (approve), grouped by prompt_version.
	"captain_approval_rate": {
		Name: "captain_approval_rate",
		QuerySQL: `
			SELECT
				prompt_version,
				CAST(SUM(CASE WHEN outcome = 'Completed' THEN 1 ELSE 0 END) AS REAL) /
				NULLIF(COUNT(*), 0) AS value
			FROM TaskHistory
			WHERE agent LIKE 'Captain%'
			  AND created_at >= ?
			  AND IFNULL(prompt_version, '') != ''
			GROUP BY prompt_version
		`,
	},
	// council_approval_rate — fraction of Council decisions that ended
	// in Completed or AwaitingSubPRCI (the two "approve" outcomes).
	"council_approval_rate": {
		Name: "council_approval_rate",
		QuerySQL: `
			SELECT
				prompt_version,
				CAST(SUM(CASE WHEN outcome IN ('Completed', 'AwaitingSubPRCI') THEN 1 ELSE 0 END) AS REAL) /
				NULLIF(COUNT(*), 0) AS value
			FROM TaskHistory
			WHERE agent LIKE 'Council%'
			  AND created_at >= ?
			  AND IFNULL(prompt_version, '') != ''
			GROUP BY prompt_version
		`,
	},
	// captain_rejection_rate — fraction of Captain decisions that
	// ended in Rejected. Pairs with the existing
	// captain_rejection_rate registry metric (it returns a fleet-wide
	// rate; this one slices by prompt_version for ground-truth
	// correlation).
	"captain_rejection_rate": {
		Name: "captain_rejection_rate",
		QuerySQL: `
			SELECT
				prompt_version,
				CAST(SUM(CASE WHEN outcome = 'Rejected' THEN 1 ELSE 0 END) AS REAL) /
				NULLIF(COUNT(*), 0) AS value
			FROM TaskHistory
			WHERE agent LIKE 'Captain%'
			  AND created_at >= ?
			  AND IFNULL(prompt_version, '') != ''
			GROUP BY prompt_version
		`,
	},
	// medic_completion_rate — fraction of Medic decisions that ended
	// Completed. Used for per-Medic-prompt-version convoy-completion
	// rate.
	"medic_completion_rate": {
		Name: "medic_completion_rate",
		QuerySQL: `
			SELECT
				prompt_version,
				CAST(SUM(CASE WHEN outcome = 'Completed' THEN 1 ELSE 0 END) AS REAL) /
				NULLIF(COUNT(*), 0) AS value
			FROM TaskHistory
			WHERE agent LIKE 'Medic%'
			  AND created_at >= ?
			  AND IFNULL(prompt_version, '') != ''
			GROUP BY prompt_version
		`,
	},
	// task_count — number of TaskHistory rows per prompt_version
	// across all agents. Useful as a denominator sanity-check when
	// reading the rate metrics above.
	"task_count": {
		Name: "task_count",
		QuerySQL: `
			SELECT
				prompt_version,
				CAST(COUNT(*) AS REAL) AS value
			FROM TaskHistory
			WHERE created_at >= ?
			  AND IFNULL(prompt_version, '') != ''
			GROUP BY prompt_version
		`,
	},
}

// RegisterGroupedMetric adds (or overwrites) a grouped metric in the
// in-memory catalog. Used by tests + EC-proposed extensions.
func RegisterGroupedMetric(m GroupedMetric) error {
	if strings.TrimSpace(m.Name) == "" {
		return fmt.Errorf("RegisterGroupedMetric: name required")
	}
	if strings.TrimSpace(m.QuerySQL) == "" {
		return fmt.Errorf("RegisterGroupedMetric: %s: QuerySQL required", m.Name)
	}
	groupedMetrics[m.Name] = m
	return nil
}

// GroupedMetricNames returns the catalog of grouped metrics for
// dashboard / discovery use.
func GroupedMetricNames() []string {
	out := make([]string, 0, len(groupedMetrics))
	for name := range groupedMetrics {
		out = append(out, name)
	}
	return out
}

// MetricByPromptVersion returns the named metric value keyed by
// prompt_version, computed over TaskHistory rows whose created_at >=
// since. Used by EC's analysis layer to correlate downstream-outcome
// metrics with the prompt that produced the decision.
//
// Returns ErrUnknownGroupedMetric if the metric isn't in the catalog.
// An empty result map is valid (no rows in the window).
func MetricByPromptVersion(ctx context.Context, db *sql.DB, metricName string, since time.Time) (map[string]float64, error) {
	if db == nil {
		return nil, fmt.Errorf("MetricByPromptVersion: nil db")
	}
	gm, ok := groupedMetrics[metricName]
	if !ok {
		return nil, fmt.Errorf("%w: %s (known: %v)", ErrUnknownGroupedMetric, metricName, GroupedMetricNames())
	}
	sinceStr := since.UTC().Format("2006-01-02 15:04:05")
	rows, err := db.QueryContext(ctx, gm.QuerySQL, sinceStr)
	if err != nil {
		return nil, fmt.Errorf("MetricByPromptVersion %s: query: %w", metricName, err)
	}
	defer rows.Close()

	out := map[string]float64{}
	for rows.Next() {
		var version string
		var value sql.NullFloat64
		if err := rows.Scan(&version, &value); err != nil {
			return nil, fmt.Errorf("MetricByPromptVersion %s: scan: %w", metricName, err)
		}
		// Defensive — empty version should be filtered by the WHERE
		// clause but we belt-and-suspenders here so a query that
		// drops the filter doesn't leak NULL keys.
		if strings.TrimSpace(version) == "" {
			continue
		}
		// NULL value → 0 (a NULLIF in the SQL produces NULL when the
		// denominator is zero; the metric is undefined there).
		if !value.Valid {
			out[version] = 0
			continue
		}
		out[version] = value.Float64
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("MetricByPromptVersion %s: rows: %w", metricName, err)
	}
	return out, nil
}
