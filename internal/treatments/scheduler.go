package treatments

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
)

// ExperimentDescriptor is the in-memory shape the scheduler reasons
// about. Loaded from Experiments + ExperimentTreatments +
// TreatmentSpecs + ExperimentMetrics. It captures every dimension on
// which a unit can be confounded by simultaneously enrolling in two
// experiments — the union of the conflict signals enumerated in
// ConflictsWith.
//
// ID is the Experiments primary key; the scheduler uses it as the
// deterministic tie-break for SelectOrthogonalEnrollments (lowest id
// wins). Kind is 'single' or 'factorial' (paired-runs.md § Factorial
// Scoring). Factors holds the factor-name set parsed from
// Experiments.factors_json (always [] for kind='single').
// PromptTemplateRefs collects every distinct prompt_template_ref the
// experiment's TreatmentSpecs touch — a single-treatment experiment
// "owns" that prompt slot for its (subject_agent, assignment_unit)
// scope. PrimaryMetric is the metric_name where ExperimentMetrics
// .is_primary=1 (empty string if unset).
type ExperimentDescriptor struct {
	ID                 int
	Kind               string
	SubjectAgent       string
	AssignmentUnit     string
	Factors            []string
	PromptTemplateRefs []string
	PrimaryMetric      string
}

// ConflictsWith reports whether two experiments touch overlapping
// dimensions and therefore cannot enroll the same unit
// simultaneously without confounding (paired-runs.md § Orthogonal
// dimension invariant). The conflict definition is the union of:
//
//   - Shared factor name in factors_json (factorial overlap). If
//     either experiment declares a non-empty Factors set, conflict
//     fires iff a.Factors ∩ b.Factors is non-empty. This is the
//     canonical rule for factorial vs factorial and factorial vs
//     single experiments.
//   - Shared subject_agent + shared prompt_template_ref via any of
//     their treatments (single-treatment overlap on the prompt
//     slot). For two single-treatment experiments operating on the
//     same agent, simultaneously rewriting the same prompt template
//     is a confound. The rule scopes by subject_agent so two
//     experiments on different agents that happen to reference the
//     same template ref (unlikely, but possible) don't spuriously
//     conflict.
//   - Shared metric_name in their primary metrics (overlap on the
//     scoring channel). Two experiments competing for the same
//     metric on the same unit would have their treatment effects
//     bleed into each other's score signal.
//
// Returns true if any of these overlap predicates fires. The
// factor-overlap rule is the canonical conflict signal; the prompt
// and metric rules are the fallback for single-treatment experiments
// where factors_json is empty.
func ConflictsWith(a, b *ExperimentDescriptor) bool {
	if a == nil || b == nil {
		return false
	}

	// (1) Factor overlap — the canonical factorial rule. If either
	// side declares factors, intersect on factor name.
	if len(a.Factors) > 0 || len(b.Factors) > 0 {
		if hasIntersection(a.Factors, b.Factors) {
			return true
		}
	}

	// (2) Single-treatment fallback: same subject_agent + shared
	// prompt_template_ref. Same-agent constraint avoids spurious
	// conflicts across agents that happen to share a ref string.
	if a.SubjectAgent != "" && a.SubjectAgent == b.SubjectAgent {
		if hasIntersection(a.PromptTemplateRefs, b.PromptTemplateRefs) {
			return true
		}
	}

	// (3) Shared primary metric. Empty string means "unset" — two
	// unset experiments don't conflict on metric.
	if a.PrimaryMetric != "" && a.PrimaryMetric == b.PrimaryMetric {
		return true
	}

	return false
}

// SelectOrthogonalEnrollments returns the maximal subset of
// candidates that the unit can enroll in without internal conflicts
// (paired-runs.md § Orthogonal dimension invariant). Greedy
// selection in id-order: pick the lowest id first, then add each
// subsequent candidate iff it does not conflict with any
// already-selected experiment.
//
// The id-order tie-break makes the result deterministic per
// (unit, candidate-set) — same input always produces the same
// output, which is the load-bearing property for sticky assignment
// across Medic retries (paired-runs.md § Sticky task retries).
//
// unitKind / unitID are accepted for symmetry with the Apply call
// site and to leave room for a future per-unit conflict rule (e.g.
// "this specific feature opted out of experiment X"); the current
// implementation does not consume them.
func SelectOrthogonalEnrollments(
	unitKind string,
	unitID int,
	candidates []*ExperimentDescriptor,
) []*ExperimentDescriptor {
	if len(candidates) == 0 {
		return nil
	}

	// Sort a copy by ID (ascending) for deterministic tie-break.
	sorted := make([]*ExperimentDescriptor, 0, len(candidates))
	for _, c := range candidates {
		if c == nil {
			continue
		}
		sorted = append(sorted, c)
	}
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].ID < sorted[j].ID
	})

	selected := make([]*ExperimentDescriptor, 0, len(sorted))
	for _, cand := range sorted {
		conflict := false
		for _, picked := range selected {
			if ConflictsWith(cand, picked) {
				conflict = true
				break
			}
		}
		if !conflict {
			selected = append(selected, cand)
		}
	}
	return selected
}

// loadExperimentDescriptors hydrates the descriptor shape from the
// Experiments + ExperimentTreatments + TreatmentSpecs +
// ExperimentMetrics rows. Used by treatments.Apply via applyLive.
//
// Filters: status='running' AND subject_agent=? AND assignment_unit=?
// (the same predicate as the Phase 2 loadActiveExperiments — the
// scheduler is the new selection layer on top of that load step).
func loadExperimentDescriptors(ctx context.Context, db *sql.DB, agent, unitKind string) ([]*ExperimentDescriptor, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, IFNULL(kind, 'single'), subject_agent, assignment_unit, IFNULL(factors_json, '[]')
		FROM Experiments
		WHERE status = 'running'
		  AND subject_agent = ?
		  AND assignment_unit = ?
		ORDER BY id
	`, agent, unitKind)
	if err != nil {
		return nil, fmt.Errorf("loadExperimentDescriptors query: %w", err)
	}
	defer rows.Close()

	var out []*ExperimentDescriptor
	for rows.Next() {
		var (
			d           ExperimentDescriptor
			factorsJSON string
		)
		if err := rows.Scan(&d.ID, &d.Kind, &d.SubjectAgent, &d.AssignmentUnit, &factorsJSON); err != nil {
			return nil, fmt.Errorf("loadExperimentDescriptors scan: %w", err)
		}
		d.Factors = parseFactors(factorsJSON)
		out = append(out, &d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("loadExperimentDescriptors rows: %w", err)
	}

	// Hydrate per-experiment prompt template refs and primary metric.
	// Done as separate queries (rather than a 3-way join) so a missing
	// ExperimentMetrics row doesn't suppress the experiment.
	for _, d := range out {
		refs, err := loadPromptTemplateRefs(ctx, db, d.ID)
		if err != nil {
			return nil, err
		}
		d.PromptTemplateRefs = refs
		metric, err := loadPrimaryMetric(ctx, db, d.ID)
		if err != nil {
			return nil, err
		}
		d.PrimaryMetric = metric
	}
	return out, nil
}

// loadPromptTemplateRefs returns the distinct, non-empty
// prompt_template_ref values from every TreatmentSpec attached to
// the experiment via ExperimentTreatments. The "distinct + non-empty"
// shape is so an experiment with one treatment that doesn't touch
// prompt templates contributes nothing to the conflict surface.
func loadPromptTemplateRefs(ctx context.Context, db *sql.DB, experimentID int) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT DISTINCT IFNULL(s.prompt_template_ref, '')
		FROM ExperimentTreatments t
		LEFT JOIN TreatmentSpecs s ON s.id = t.treatment_spec_id
		WHERE t.experiment_id = ?
	`, experimentID)
	if err != nil {
		return nil, fmt.Errorf("loadPromptTemplateRefs query: %w", err)
	}
	defer rows.Close()
	var refs []string
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			return nil, fmt.Errorf("loadPromptTemplateRefs scan: %w", err)
		}
		if ref != "" {
			refs = append(refs, ref)
		}
	}
	return refs, rows.Err()
}

// loadPrimaryMetric returns the metric_name where is_primary=1 for
// the experiment, or "" if no primary metric is registered.
func loadPrimaryMetric(ctx context.Context, db *sql.DB, experimentID int) (string, error) {
	var name string
	err := db.QueryRowContext(ctx, `
		SELECT metric_name
		FROM ExperimentMetrics
		WHERE experiment_id = ? AND is_primary = 1
		LIMIT 1
	`, experimentID).Scan(&name)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("loadPrimaryMetric: %w", err)
	}
	return name, nil
}

// parseFactors extracts factor names from factors_json. The accepted
// shape is the one paired-runs.md § Factorial Scoring documents:
// `[{"name": "<factor>", "levels": [...]}]`. A bare string array
// (`["prompt", "rules"]`) is also accepted as a convenience for
// hand-authored manifests / tests. Empty or unparseable input yields
// an empty slice — the caller treats "no factors" as "single
// experiment, fall through to prompt/metric conflict rules."
func parseFactors(raw string) []string {
	if raw == "" || raw == "[]" {
		return nil
	}
	// Try the canonical shape first.
	var objs []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(raw), &objs); err == nil && len(objs) > 0 {
		out := make([]string, 0, len(objs))
		for _, o := range objs {
			if o.Name != "" {
				out = append(out, o.Name)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	// Fallback: bare string array.
	var bare []string
	if err := json.Unmarshal([]byte(raw), &bare); err == nil {
		out := make([]string, 0, len(bare))
		for _, s := range bare {
			if s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// hasIntersection returns true if the two slices share any element.
// Empty slices never intersect (the caller upstream uses this as the
// signal "no factors declared, fall through to other rules").
func hasIntersection(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	set := make(map[string]struct{}, len(a))
	for _, s := range a {
		if s != "" {
			set[s] = struct{}{}
		}
	}
	for _, s := range b {
		if s == "" {
			continue
		}
		if _, ok := set[s]; ok {
			return true
		}
	}
	return false
}
