// Package experiments — D3 Phase 4 factorial manifest parsing.
//
// Factorial experiments declare a `factors` block (name + levels) and
// list one ExperimentTreatments arm per cell. Each treatment's `cell`
// map pins one level per factor; the cross-product of factor levels
// must equal the set of declared cells (full-factorial coverage). The
// validator catches the shape errors at author time so EnrollUnit and
// the analysis layer never have to defend against malformed factor
// declarations downstream.
//
// The parser is deliberately separated from the single-treatment
// authoring path: the single-treatment Manifest shape (lifecycle.go) is
// the canonical one; ManifestFactor / ManifestCell are additive fields
// that single-treatment manifests omit. validateFactorial fires only
// when Manifest.Kind == "factorial".
package experiments

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Kind values for Manifest. Default 'single' preserves Phase 2 behavior.
const (
	KindSingle    = "single"
	KindFactorial = "factorial"
)

// ManifestFactor declares one factor and its discrete level set. A 2x2
// factorial has two ManifestFactor entries with two levels each; a 3x2
// has factors of length 3 and 2.
type ManifestFactor struct {
	Name   string   `yaml:"name"`
	Levels []string `yaml:"levels"`
}

// validateFactors enforces the structural invariants on the factor
// catalog. Returns ordered errors so a manifest with multiple
// problems still surfaces a useful first failure.
func validateFactors(factors []ManifestFactor) error {
	if len(factors) < 2 {
		return fmt.Errorf("manifest: factorial requires at least 2 factors (got %d) — single-factor manifests should declare kind='single'", len(factors))
	}
	seen := map[string]struct{}{}
	for i, f := range factors {
		name := strings.TrimSpace(f.Name)
		if name == "" {
			return fmt.Errorf("manifest: factor[%d] missing name", i)
		}
		if _, dup := seen[name]; dup {
			return fmt.Errorf("manifest: factor name %q is duplicated", name)
		}
		seen[name] = struct{}{}
		if len(f.Levels) < 2 {
			return fmt.Errorf("manifest: factor %q must have at least 2 levels (got %d)", name, len(f.Levels))
		}
		levelSeen := map[string]struct{}{}
		for j, lvl := range f.Levels {
			if strings.TrimSpace(lvl) == "" {
				return fmt.Errorf("manifest: factor %q level[%d] is empty", name, j)
			}
			if _, dup := levelSeen[lvl]; dup {
				return fmt.Errorf("manifest: factor %q level %q is duplicated", name, lvl)
			}
			levelSeen[lvl] = struct{}{}
		}
	}
	return nil
}

// expectedCellCount returns the cardinality of the full-factorial
// cross-product over the declared factor levels.
func expectedCellCount(factors []ManifestFactor) int {
	if len(factors) == 0 {
		return 0
	}
	n := 1
	for _, f := range factors {
		n *= len(f.Levels)
	}
	return n
}

// validateFactorialTreatments checks that each treatment's cell is a
// valid level combination from the declared factors AND that the union
// of treatment cells equals the full cross-product. Partial-factorial
// designs are NOT supported in this skeleton — paired-runs.md § Cell
// balance requires full-factorial coverage for orthogonal scoring.
func validateFactorialTreatments(factors []ManifestFactor, treats []ManifestTreatment) error {
	if len(treats) == 0 {
		return errors.New("manifest: factorial requires at least one treatment per cell")
	}

	// Build a name → set(levels) lookup for cell-membership checks.
	allowed := map[string]map[string]struct{}{}
	for _, f := range factors {
		levels := map[string]struct{}{}
		for _, lvl := range f.Levels {
			levels[lvl] = struct{}{}
		}
		allowed[f.Name] = levels
	}

	cellsSeen := map[string]string{} // canonical-cell-key → arm_label
	armLabels := map[string]struct{}{}
	for i, tr := range treats {
		if strings.TrimSpace(tr.ArmLabel) == "" {
			return fmt.Errorf("manifest: factorial treatment[%d] missing arm_label", i)
		}
		if _, dup := armLabels[tr.ArmLabel]; dup {
			return fmt.Errorf("manifest: factorial arm_label %q is duplicated", tr.ArmLabel)
		}
		armLabels[tr.ArmLabel] = struct{}{}

		if len(tr.Cell) != len(factors) {
			return fmt.Errorf("manifest: factorial treatment %q cell pins %d factors; expected %d (one level per declared factor)", tr.ArmLabel, len(tr.Cell), len(factors))
		}
		for fname, level := range tr.Cell {
			levels, ok := allowed[fname]
			if !ok {
				return fmt.Errorf("manifest: factorial treatment %q cell references unknown factor %q", tr.ArmLabel, fname)
			}
			if _, ok := levels[level]; !ok {
				return fmt.Errorf("manifest: factorial treatment %q cell sets factor %q=%q but %q is not in declared levels", tr.ArmLabel, fname, level, level)
			}
		}
		key := canonicalCellKey(factors, tr.Cell)
		if prior, dup := cellsSeen[key]; dup {
			return fmt.Errorf("manifest: factorial cell %s appears in arms %q and %q — each cell must have exactly one arm", key, prior, tr.ArmLabel)
		}
		cellsSeen[key] = tr.ArmLabel
	}

	expected := expectedCellCount(factors)
	if len(cellsSeen) != expected {
		return fmt.Errorf("manifest: factorial requires full-factorial coverage — declared factors expand to %d cells but %d arm(s) were provided", expected, len(cellsSeen))
	}

	return nil
}

// canonicalCellKey serialises a cell map into a stable, comparable
// string. Factors are emitted in declaration order so different
// orderings of the same cell collapse to one key.
func canonicalCellKey(factors []ManifestFactor, cell map[string]string) string {
	parts := make([]string, 0, len(factors))
	for _, f := range factors {
		parts = append(parts, fmt.Sprintf("%s=%s", f.Name, cell[f.Name]))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// canonicalCellJSON returns a JSON object with factor keys ordered by
// declaration so two equivalent cells round-trip to the same string.
// Used as the cell_json column body in ExperimentTreatments and
// ExperimentRuns.
func canonicalCellJSON(factors []ManifestFactor, cell map[string]string) string {
	if len(cell) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(factors))
	for _, f := range factors {
		if _, ok := cell[f.Name]; ok {
			keys = append(keys, f.Name)
		}
	}
	// Append any cell keys not in factors (shouldn't happen post-validation,
	// but the skeleton stays defensive — sorted lexically for determinism).
	extra := []string{}
	declared := map[string]struct{}{}
	for _, f := range factors {
		declared[f.Name] = struct{}{}
	}
	for k := range cell {
		if _, ok := declared[k]; !ok {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	keys = append(keys, extra...)

	var b strings.Builder
	b.WriteString("{")
	for i, k := range keys {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`"`)
		b.WriteString(jsonEscape(k))
		b.WriteString(`":"`)
		b.WriteString(jsonEscape(cell[k]))
		b.WriteString(`"`)
	}
	b.WriteString("}")
	return b.String()
}

// factorsJSON serialises the factor catalog into the on-disk
// factors_json column body. Uses a compact, deterministic shape:
// [{"name":"prompt","levels":["A","B"]},...]. Levels preserve their
// declaration order so consumers can iterate without a re-sort.
func factorsJSON(factors []ManifestFactor) string {
	if len(factors) == 0 {
		return "[]"
	}
	var b strings.Builder
	b.WriteString("[")
	for i, f := range factors {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"name":"`)
		b.WriteString(jsonEscape(f.Name))
		b.WriteString(`","levels":[`)
		for j, lvl := range f.Levels {
			if j > 0 {
				b.WriteString(",")
			}
			b.WriteString(`"`)
			b.WriteString(jsonEscape(lvl))
			b.WriteString(`"`)
		}
		b.WriteString("]}")
	}
	b.WriteString("]")
	return b.String()
}

// jsonEscape produces a minimal JSON-string escaping for the chars
// we expect in factor / level identifiers. The full encoding/json path
// would be overkill — factor names are constrained identifiers, so the
// escape set is small.
func jsonEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
