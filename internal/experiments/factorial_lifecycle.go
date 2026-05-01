// Package experiments — D3 Phase 4 factorial-lifecycle entry points.
//
// AuthorFactorialFromYAML, EnrollFactorialUnit, and TerminateFactorial
// are typed wrappers around the existing single-treatment surface.
// They exist to give callers (CLI, daemon, tests) a way to assert the
// experiment's kind at the API boundary instead of discovering a
// kind-mismatch deep inside the Bayesian framework when the cell-mean
// shape doesn't match the analyzer's expectations.
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
	"encoding/json"
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

// TerminateFactorial transitions a running factorial experiment to
// 'terminated' and writes an ExperimentOutcomes row whose
// cell_means_json maps each cell's canonical key to its observed mean
// score. The full main-effects + 2-way-interactions computation is
// performed by the internal/analysis factorial analyzer (sub-agent B
// owns that surface). TerminateFactorial just persists the per-cell
// summary so that downstream layer can read it and write its own rows.
//
// Like Terminate, errors if the experiment is not in 'running' or
// 'confirming' status (CAS), and errors with ErrNotFactorial if the
// experiment's kind != 'factorial'.
func TerminateFactorial(ctx context.Context, db *sql.DB, experimentID int, reason string) error {
	if err := assertFactorialKind(ctx, db, experimentID); err != nil {
		return err
	}
	if strings.TrimSpace(reason) == "" {
		reason = "operator_closed"
	}

	// Status CAS — only running/confirming experiments can be terminated.
	res, err := db.ExecContext(ctx, `
		UPDATE Experiments
		SET status = ?, terminated_at = datetime('now'), termination_reason = ?
		WHERE id = ? AND status IN (?, ?)
	`, StatusTerminated, reason, experimentID, StatusRunning, StatusConfirming)
	if err != nil {
		return fmt.Errorf("TerminateFactorial: update: %w", err)
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return fmt.Errorf("TerminateFactorial: experiment %d is not running/confirming — refusing to flip", experimentID)
	}

	cellMeans, err := computeCellMeans(ctx, db, experimentID)
	if err != nil {
		return fmt.Errorf("TerminateFactorial: compute cell means: %w", err)
	}
	body, err := json.Marshal(cellMeans)
	if err != nil {
		return fmt.Errorf("TerminateFactorial: marshal cell means: %w", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO ExperimentOutcomes
			(experiment_id, terminated_at, termination_reason, cell_means_json)
		VALUES (?, datetime('now'), ?, ?)
	`, experimentID, reason, string(body)); err != nil {
		return fmt.Errorf("TerminateFactorial: insert outcome: %w", err)
	}
	return nil
}

// computeCellMeans groups ExperimentRuns by the parent treatment's
// cell_json (which carries the canonical factor-ordered cell key) and
// emits one entry per cell. Bernoulli interpretation: a row with
// score >= 0.5 counts as a success. Cells with no recorded runs map
// to 0 — matching computeOutcome's single-treatment shape.
//
// The cell key emitted to JSON is a compact stringified form of the
// canonical cell ordering ("prompt=A,rules=tight"), NOT the cell_json
// blob itself, so callers (sub-agent B's analyzer + dashboards) can
// index without re-parsing every value. The ordering is preserved by
// canonicalCellJSON at author time, so the keys are stable and
// comparable across experiments that share factor declarations.
func computeCellMeans(ctx context.Context, db *sql.DB, experimentID int) (map[string]float64, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT IFNULL(t.cell_json, '{}') AS cell_json,
		       COUNT(r.id) AS trials,
		       IFNULL(SUM(CASE WHEN r.score >= 0.5 THEN 1 ELSE 0 END), 0) AS successes
		FROM ExperimentTreatments t
		LEFT JOIN ExperimentRuns r ON r.treatment_id = t.id
		WHERE t.experiment_id = ?
		GROUP BY t.id
		ORDER BY t.id
	`, experimentID)
	if err != nil {
		return nil, fmt.Errorf("computeCellMeans: query: %w", err)
	}
	defer rows.Close()

	out := map[string]float64{}
	for rows.Next() {
		var cellJSON string
		var trials, successes int
		if err := rows.Scan(&cellJSON, &trials, &successes); err != nil {
			return nil, fmt.Errorf("computeCellMeans: scan: %w", err)
		}
		key := cellJSONToKey(cellJSON)
		mean := 0.0
		if trials > 0 {
			mean = float64(successes) / float64(trials)
		}
		out[key] = mean
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("computeCellMeans: rows: %w", err)
	}
	return out, nil
}

// cellJSONToKey converts the on-disk canonical cell_json into the
// "factor=level,..." string used as the key in cell_means_json. The
// input is canonical (factor declaration order preserved by
// canonicalCellJSON), so a streaming token decode in iteration order
// gives a stable key. We intentionally avoid Go's map iteration order:
// the canonical writer preserves ordering, so the streaming decoder
// recovers it.
//
// Treats malformed input defensively — an empty / unparseable cell
// returns the empty string, matching the schema default.
func cellJSONToKey(cellJSON string) string {
	cellJSON = strings.TrimSpace(cellJSON)
	if cellJSON == "" || cellJSON == "{}" {
		return ""
	}
	var ordered []string
	dec := json.NewDecoder(strings.NewReader(cellJSON))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return ""
	}
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			return ""
		}
		ks, ok := k.(string)
		if !ok {
			return ""
		}
		v, err := dec.Token()
		if err != nil {
			return ""
		}
		vs, ok := v.(string)
		if !ok {
			return ""
		}
		ordered = append(ordered, ks+"="+vs)
	}
	return strings.Join(ordered, ",")
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
