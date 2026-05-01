// Package experiments — D3 Phase 4 factorial-lifecycle entry points.
//
// AuthorFactorialFromYAML, EnrollFactorialUnit, and (later in this
// commit series) TerminateFactorial are typed wrappers around the
// existing single-treatment surface. They exist to give callers (CLI,
// daemon, tests) a way to assert the experiment's kind at the API
// boundary instead of discovering a kind-mismatch deep inside the
// Bayesian framework when the cell-mean shape doesn't match the
// analyzer's expectations.
//
// Determinism contract — paired-runs.md § Assignment and Inheritance:
// EnrollFactorialUnit's cell selection is a pure function of
// (experiment_id, unit_kind, unit_id). The hash is salted with
// experiment_id so the same unit lands in different cells across
// experiments (preventing cross-experiment correlation), but the same
// (experiment, unit) pair always picks the same cell — which is the
// shape inheritance relies on.
package experiments

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"force-orchestrator/internal/store"
)

// ErrNotFactorial is returned when a factorial entry point is invoked
// against an experiment whose kind != 'factorial'. Callers can
// errors.Is on this to fall back to the single-treatment path.
var ErrNotFactorial = errors.New("experiments: experiment is not factorial")

// AuthorFactorialFromYAML parses the manifest at yamlPath, validates
// it declares kind: factorial, and delegates to AuthorFromManifest.
// Returns the new Experiments.id.
//
// Single-treatment manifests (kind blank or 'single') are rejected
// with a typed error so a caller cannot accidentally route a single-
// treatment manifest through the factorial entry point and discover
// the mismatch later (e.g. when the analyzer receives an empty cell
// catalogue).
func AuthorFactorialFromYAML(ctx context.Context, db *sql.DB, yamlPath string) (int, error) {
	body, err := os.ReadFile(yamlPath)
	if err != nil {
		return 0, fmt.Errorf("AuthorFactorialFromYAML: read %s: %w", yamlPath, err)
	}
	return AuthorFactorialFromBytes(ctx, db, body)
}

// AuthorFactorialFromBytes is the byte-shape sibling of
// AuthorFactorialFromYAML — used by tests that build a manifest in
// memory.
func AuthorFactorialFromBytes(ctx context.Context, db *sql.DB, raw []byte) (int, error) {
	var m Manifest
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return 0, fmt.Errorf("AuthorFactorialFromBytes: parse: %w", err)
	}
	declared := strings.TrimSpace(m.Kind)
	if declared != KindFactorial {
		return 0, fmt.Errorf("AuthorFactorialFromBytes: manifest kind must be 'factorial' (got %q) — single-treatment manifests should use AuthorFromYAML", declared)
	}
	return AuthorFromManifest(ctx, db, m)
}

// assertFactorialKind reads the experiment's kind column and returns
// ErrNotFactorial if it is not 'factorial'. The factorial entry points
// call this first so a misrouted call produces a typed error instead
// of a silent shape mismatch downstream.
func assertFactorialKind(ctx context.Context, db *sql.DB, experimentID int) error {
	var kind string
	err := db.QueryRowContext(ctx, `SELECT IFNULL(kind, '') FROM Experiments WHERE id = ?`, experimentID).Scan(&kind)
	if err == sql.ErrNoRows {
		return fmt.Errorf("experiments: experiment %d not found", experimentID)
	}
	if err != nil {
		return fmt.Errorf("experiments: load kind: %w", err)
	}
	if kind != KindFactorial {
		return fmt.Errorf("%w: experiment %d has kind=%q", ErrNotFactorial, experimentID, kind)
	}
	return nil
}

// EnrollFactorialUnit assigns a natural unit to one of the factorial
// experiment's CELLS, deterministically per (experiment_id, unit_kind,
// unit_id). Returns the chosen ExperimentTreatments.id.
//
// Idempotent: a re-call for the same (experiment, kind, unit) returns
// the same treatment_id and does not insert a duplicate ExperimentRuns
// row.
//
// Errors with ErrNotFactorial when the experiment's kind != 'factorial'.
// Determinism: hashFraction is salted with experiment_id, so the same
// unit lands in different cells across experiments. The picker walks
// cumulative target_cell_weight buckets in ExperimentTreatments id-order
// (the same shape as pickArm in lifecycle.go and pickTreatment in
// internal/treatments/live.go).
func EnrollFactorialUnit(ctx context.Context, db *sql.DB, experimentID int, unitKind string, unitID int) (int, error) {
	if err := assertFactorialKind(ctx, db, experimentID); err != nil {
		return 0, err
	}

	// Idempotency check — a prior assignment wins regardless of what
	// the picker would produce now (cell weights could have been
	// edited after the first enrollment, but inheritance contract
	// (paired-runs.md § Assignment and Inheritance) is set-once).
	var existing int
	err := db.QueryRowContext(ctx, `
		SELECT treatment_id FROM ExperimentRuns
		WHERE experiment_id = ? AND natural_unit_kind = ? AND natural_unit_id = ?
		ORDER BY id LIMIT 1
	`, experimentID, unitKind, unitID).Scan(&existing)
	switch {
	case err == nil:
		return existing, nil
	case err != sql.ErrNoRows:
		return 0, fmt.Errorf("EnrollFactorialUnit: lookup existing: %w", err)
	}

	treats, err := loadFactorialTreatments(ctx, db, experimentID)
	if err != nil {
		return 0, err
	}
	if len(treats) == 0 {
		return 0, fmt.Errorf("EnrollFactorialUnit: experiment %d has no treatments", experimentID)
	}
	picked := pickFactorialCell(experimentID, unitKind, unitID, treats)

	if _, err := db.ExecContext(ctx, `
		INSERT INTO ExperimentRuns
			(experiment_id, treatment_id, cell_json, natural_unit_kind, natural_unit_id,
			 mode, agent_name, assigned_at)
		VALUES (?, ?, ?, ?, ?, 'paired_real', '', ?)
	`, experimentID, picked.ID, picked.CellJSON, unitKind, unitID, store.NowSQLite()); err != nil {
		return 0, fmt.Errorf("EnrollFactorialUnit: insert run: %w", err)
	}
	return picked.ID, nil
}

// factorialCell mirrors armRow but additionally carries the per-cell
// canonical JSON so the picker can stamp it on the ExperimentRuns row
// without a second lookup.
type factorialCell struct {
	ID               int
	ArmLabel         string
	CellJSON         string
	TargetCellWeight float64
}

func loadFactorialTreatments(ctx context.Context, db *sql.DB, experimentID int) ([]factorialCell, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, arm_label, IFNULL(cell_json, '{}'), IFNULL(target_cell_weight, 0)
		FROM ExperimentTreatments
		WHERE experiment_id = ?
		ORDER BY id
	`, experimentID)
	if err != nil {
		return nil, fmt.Errorf("loadFactorialTreatments: %w", err)
	}
	defer rows.Close()
	var out []factorialCell
	for rows.Next() {
		var c factorialCell
		if err := rows.Scan(&c.ID, &c.ArmLabel, &c.CellJSON, &c.TargetCellWeight); err != nil {
			return nil, fmt.Errorf("loadFactorialTreatments scan: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("loadFactorialTreatments rows: %w", err)
	}
	return out, nil
}

// pickFactorialCell is the cumulative-weight bucket picker. It mirrors
// pickArm and treatments.pickTreatment so the determinism contract is
// preserved. When all weights are zero, falls back to a uniform mod
// pick over the cell list — this matches the single-treatment fallback
// shape and means a manifest that omits target_cell_weight still gets
// a balanced spread over cells.
func pickFactorialCell(experimentID int, kind string, unitID int, cells []factorialCell) factorialCell {
	if len(cells) == 1 {
		return cells[0]
	}
	totalWeight := 0.0
	for _, c := range cells {
		if c.TargetCellWeight > 0 {
			totalWeight += c.TargetCellWeight
		}
	}
	if totalWeight <= 0 {
		idx := int(hashUint64(experimentID, kind, unitID) % uint64(len(cells)))
		return cells[idx]
	}
	v := hashFraction(experimentID, kind, unitID) * totalWeight
	cum := 0.0
	for _, c := range cells {
		w := c.TargetCellWeight
		if w <= 0 {
			continue
		}
		cum += w
		if v < cum {
			return c
		}
	}
	return cells[len(cells)-1]
}
