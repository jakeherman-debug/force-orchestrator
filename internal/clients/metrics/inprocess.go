package metrics

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// inProcessClient is the SQLite-backed Client used by every in-process
// agent (cmd/force daemon wiring, EC dispatch). The body routes each
// method through the canonical MetricVersions / ExperimentRuns tables
// that D3 introduced — there is no separate "metrics service" storage
// layer because the goal of this client is to give cross-agent callers
// a stable interface over those tables (see CLAUDE.md "Cross-agent
// service interfaces").
//
// Storage mapping:
//   - RegisterMetric / ListMetrics → MetricVersions (PK metric_name,version)
//     - MetricVersion.Body         → sql_content
//     - MetricVersion.Description  → description
//     - MetricVersion.Units +
//       MetricVersion.OwningTeam   → manifest_json (so the schema's
//                                    existing columns stay load-bearing
//                                    and we don't have to widen the table)
//   - RecordScore / Score          → ExperimentRuns.score +
//                                    ExperimentRuns.metric_version +
//                                    ExperimentRuns.score_source.
//     The runID maps to ExperimentRuns.id; metric_name is recorded as
//     score_source so a single row can carry the "who scored this and
//     against which version" provenance without a new table.
type inProcessClient struct {
	db *sql.DB
}

// NewInProcess returns a SQLite-backed Client. The caller is responsible
// for opening the holocron handle (via store.InitHolocronDSN); the client
// does not own connection lifetime.
//
// A nil db panics at first method call; the constructor itself stays
// permissive so daemon-wiring code that passes a placeholder DB during
// startup doesn't crash before reaching its real config.
func NewInProcess(db *sql.DB) Client { return &inProcessClient{db: db} }

// metricManifest is the JSON shape we stash in MetricVersions.manifest_json
// for fields the client exposes that don't have dedicated columns. Kept
// intentionally narrow — additional fields belong in a schema migration,
// not in this opaque blob.
type metricManifest struct {
	Units      string `json:"units,omitempty"`
	OwningTeam string `json:"owning_team,omitempty"`
}

// RegisterMetric INSERTs a (name, version) pair into MetricVersions. The
// primary key (metric_name, version) enforces immutability: re-registering
// the same pair returns ErrMetricExists.
func (c *inProcessClient) RegisterMetric(ctx context.Context, metric MetricVersion) error {
	if c.db == nil {
		return fmt.Errorf("metrics: NewInProcess called with nil db")
	}
	if metric.Name == "" || metric.Version == "" {
		return fmt.Errorf("metrics: RegisterMetric requires non-empty Name and Version")
	}
	manifestJSON, err := json.Marshal(metricManifest{
		Units:      metric.Units,
		OwningTeam: metric.OwningTeam,
	})
	if err != nil {
		// json.Marshal of a struct with string fields never fails in
		// practice; surface defensively.
		return fmt.Errorf("metrics: marshal manifest: %w", err)
	}
	res, err := c.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO MetricVersions
			(metric_name, version, sql_content, test_content, manifest_json,
			 published_at, published_by, description)
		VALUES (?, ?, ?, '', ?, datetime('now'), '', ?)
	`, metric.Name, metric.Version, metric.Body, string(manifestJSON), metric.Description)
	if err != nil {
		return fmt.Errorf("metrics: insert MetricVersions: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("metrics: rows affected: %w", err)
	}
	if n == 0 {
		// INSERT OR IGNORE swallowed the conflict — the (name, version)
		// pair already exists. Versions are immutable per the interface
		// docstring; surface ErrMetricExists so the caller can choose
		// to bump version instead of overwriting.
		return ErrMetricExists
	}
	return nil
}

// Score returns the recorded score for the given run, validating that
// the stored metric_version matches the caller's requested version. The
// metricName argument is checked against score_source as a defensive
// guard against cross-metric reads; mismatches return ErrNoScore.
func (c *inProcessClient) Score(ctx context.Context, runID int, metricName, version string) (float64, error) {
	if c.db == nil {
		return 0, fmt.Errorf("metrics: NewInProcess called with nil db")
	}
	var (
		score          sql.NullFloat64
		storedVersion  string
		storedSource   string
	)
	err := c.db.QueryRowContext(ctx, `
		SELECT score, IFNULL(metric_version, ''), IFNULL(score_source, '')
		  FROM ExperimentRuns
		 WHERE id = ?
	`, runID).Scan(&score, &storedVersion, &storedSource)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNoScore
	}
	if err != nil {
		return 0, fmt.Errorf("metrics: query ExperimentRuns: %w", err)
	}
	if !score.Valid {
		return 0, ErrNoScore
	}
	if storedVersion != version {
		return 0, ErrNoScore
	}
	// metric_name is recorded into score_source by RecordScore. When the
	// caller asks for a specific metric name, require the source to match
	// — this catches "I scored against metric X but you're now asking
	// about metric Y on the same run" cross-talk. If score_source is
	// empty (legacy rows written before the client landed), we accept the
	// read on metric_version alone.
	if storedSource != "" && storedSource != metricName {
		return 0, ErrNoScore
	}
	return score.Float64, nil
}

// RecordScore writes (score, metric_version, score_source=metricName) onto
// ExperimentRuns. Idempotent — calling twice with the same arguments
// leaves the same row state.
//
// Returns sql.ErrNoRows if the runID does not exist (callers should not
// invoke RecordScore for a run they did not first record assigned via
// store.RecordExperimentRun-style helpers).
func (c *inProcessClient) RecordScore(ctx context.Context, runID int, metricName, version string, score float64) error {
	if c.db == nil {
		return fmt.Errorf("metrics: NewInProcess called with nil db")
	}
	if metricName == "" || version == "" {
		return fmt.Errorf("metrics: RecordScore requires non-empty metricName and version")
	}
	res, err := c.db.ExecContext(ctx, `
		UPDATE ExperimentRuns
		   SET score          = ?,
		       metric_version = ?,
		       score_source   = ?
		 WHERE id = ?
	`, score, version, metricName, runID)
	if err != nil {
		return fmt.Errorf("metrics: update ExperimentRuns: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("metrics: rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("metrics: RecordScore: no ExperimentRuns row with id=%d", runID)
	}
	return nil
}

// ListMetrics returns every row in MetricVersions ordered by
// (metric_name, version) so the dashboard renders deterministically.
// Deprecated versions are NOT filtered out — the caller decides whether
// deprecated_at matters; the interface docstring promises "every
// registered metric (across versions)."
func (c *inProcessClient) ListMetrics(ctx context.Context) ([]MetricVersion, error) {
	if c.db == nil {
		return nil, fmt.Errorf("metrics: NewInProcess called with nil db")
	}
	rows, err := c.db.QueryContext(ctx, `
		SELECT metric_name,
		       version,
		       IFNULL(description, ''),
		       IFNULL(manifest_json, '{}'),
		       IFNULL(sql_content, '')
		  FROM MetricVersions
		 ORDER BY metric_name, version
	`)
	if err != nil {
		return nil, fmt.Errorf("metrics: query MetricVersions: %w", err)
	}
	defer rows.Close()

	var out []MetricVersion
	for rows.Next() {
		var (
			mv           MetricVersion
			manifestJSON string
		)
		if err := rows.Scan(&mv.Name, &mv.Version, &mv.Description, &manifestJSON, &mv.Body); err != nil {
			return nil, fmt.Errorf("metrics: scan MetricVersions row: %w", err)
		}
		// Best-effort manifest decode. A malformed manifest (legacy rows
		// written by MetricAuthor with a different shape) is tolerated:
		// Units / OwningTeam stay empty but the row is still returned.
		var manifest metricManifest
		_ = json.Unmarshal([]byte(manifestJSON), &manifest)
		mv.Units = manifest.Units
		mv.OwningTeam = manifest.OwningTeam
		out = append(out, mv)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("metrics: iterate MetricVersions: %w", err)
	}
	return out, nil
}
