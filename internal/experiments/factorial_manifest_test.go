package experiments

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// factorialYAML2x2 is the canonical 2x2 manifest used across the
// happy-path validators. Mirrors experiments/2026-04-30-factorial-test/
// manifest.yaml in shape; declared inline so the test does not depend
// on filesystem layout for its core round-trips.
const factorialYAML2x2 = `
name: factorial-2x2
hypothesis: prompt vs rules interaction
kind: factorial
subject_agent: captain
assignment_unit: convoy
stakes_tier: low
factors:
  - name: prompt
    levels: [A, B]
  - name: rules
    levels: [tight, loose]
treatments:
  - arm_label: cell_A_tight
    prompt_template_ref: captain/A
    target_cell_weight: 0.25
    cell: {prompt: A, rules: tight}
  - arm_label: cell_A_loose
    prompt_template_ref: captain/A
    target_cell_weight: 0.25
    cell: {prompt: A, rules: loose}
  - arm_label: cell_B_tight
    prompt_template_ref: captain/B
    target_cell_weight: 0.25
    cell: {prompt: B, rules: tight}
  - arm_label: cell_B_loose
    prompt_template_ref: captain/B
    target_cell_weight: 0.25
    cell: {prompt: B, rules: loose}
metrics:
  - metric_name: approval_rate
    metric_version: "1"
    direction: higher_is_better
    is_primary: true
`

// TestFactorialManifest_ParseAndValidate2x2 — happy path: a 2x2 factorial
// manifest parses and AuthorFromBytes commits Experiments.kind='factorial'
// + factors_json + per-treatment cell_json. Verifies the canonical cell
// JSON ordering matches factor declaration order.
func TestFactorialManifest_ParseAndValidate2x2(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	id, err := AuthorFromBytes(ctx, db, []byte(factorialYAML2x2))
	if err != nil {
		t.Fatalf("AuthorFromBytes: %v", err)
	}
	if id <= 0 {
		t.Fatalf("expected positive experiment id, got %d", id)
	}

	var kind, factorsJSON string
	if err := db.QueryRowContext(ctx, `SELECT kind, factors_json FROM Experiments WHERE id = ?`, id).Scan(&kind, &factorsJSON); err != nil {
		t.Fatalf("SELECT Experiments: %v", err)
	}
	if kind != KindFactorial {
		t.Fatalf("kind: got %q want %q", kind, KindFactorial)
	}
	wantFactors := `[{"name":"prompt","levels":["A","B"]},{"name":"rules","levels":["tight","loose"]}]`
	if factorsJSON != wantFactors {
		t.Fatalf("factors_json:\n got: %s\nwant: %s", factorsJSON, wantFactors)
	}

	rows, err := db.QueryContext(ctx, `SELECT arm_label, cell_json FROM ExperimentTreatments WHERE experiment_id = ? ORDER BY id`, id)
	if err != nil {
		t.Fatalf("SELECT treatments: %v", err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var arm, cell string
		if err := rows.Scan(&arm, &cell); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[arm] = cell
	}
	want := map[string]string{
		"cell_A_tight": `{"prompt":"A","rules":"tight"}`,
		"cell_A_loose": `{"prompt":"A","rules":"loose"}`,
		"cell_B_tight": `{"prompt":"B","rules":"tight"}`,
		"cell_B_loose": `{"prompt":"B","rules":"loose"}`,
	}
	for arm, cell := range want {
		if got[arm] != cell {
			t.Errorf("arm %q cell_json: got %q want %q", arm, got[arm], cell)
		}
	}
	if len(got) != len(want) {
		t.Errorf("arms: got %d want %d (%v)", len(got), len(want), got)
	}
}

// TestFactorialManifest_ParseAndValidate3x2 — asymmetric factorial. The
// validator must accept 3x2 (6 cells) and emit factors_json with each
// factor's levels in declaration order.
func TestFactorialManifest_ParseAndValidate3x2(t *testing.T) {
	const yaml3x2 = `
name: factorial-3x2
hypothesis: model x prompt
kind: factorial
subject_agent: captain
assignment_unit: convoy
factors:
  - name: model
    levels: [haiku, sonnet, opus]
  - name: prompt
    levels: [A, B]
treatments:
  - arm_label: haiku_A
    prompt_template_ref: captain/A
    target_cell_weight: 0.166
    cell: {model: haiku, prompt: A}
  - arm_label: haiku_B
    prompt_template_ref: captain/B
    target_cell_weight: 0.166
    cell: {model: haiku, prompt: B}
  - arm_label: sonnet_A
    prompt_template_ref: captain/A
    target_cell_weight: 0.166
    cell: {model: sonnet, prompt: A}
  - arm_label: sonnet_B
    prompt_template_ref: captain/B
    target_cell_weight: 0.166
    cell: {model: sonnet, prompt: B}
  - arm_label: opus_A
    prompt_template_ref: captain/A
    target_cell_weight: 0.166
    cell: {model: opus, prompt: A}
  - arm_label: opus_B
    prompt_template_ref: captain/B
    target_cell_weight: 0.166
    cell: {model: opus, prompt: B}
metrics:
  - metric_name: approval_rate
    metric_version: "1"
    direction: higher_is_better
    is_primary: true
`
	db := openDB(t)
	ctx := context.Background()
	id, err := AuthorFromBytes(ctx, db, []byte(yaml3x2))
	if err != nil {
		t.Fatalf("AuthorFromBytes: %v", err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ExperimentTreatments WHERE experiment_id = ?`, id).Scan(&n); err != nil {
		t.Fatalf("count treatments: %v", err)
	}
	if n != 6 {
		t.Fatalf("3x2 expected 6 treatments, got %d", n)
	}
}

// TestFactorialManifest_RejectsMalformed — each branch of the factorial
// validator rejects the manifest with a descriptive error. Table-driven
// so a regression in one branch does not silently pass the others.
func TestFactorialManifest_RejectsMalformed(t *testing.T) {
	cases := []struct {
		name      string
		manifest  string
		wantSubst string
	}{
		{
			name:      "single factor",
			wantSubst: "at least 2 factors",
			manifest: factorialBase(`
factors:
  - name: prompt
    levels: [A, B]
treatments:
  - arm_label: a1
    cell: {prompt: A}
    target_cell_weight: 0.5
  - arm_label: a2
    cell: {prompt: B}
    target_cell_weight: 0.5
`),
		},
		{
			name:      "factor with one level",
			wantSubst: "at least 2 levels",
			manifest: factorialBase(`
factors:
  - name: prompt
    levels: [A, B]
  - name: rules
    levels: [tight]
treatments:
  - arm_label: cell_A_tight
    cell: {prompt: A, rules: tight}
    target_cell_weight: 0.5
  - arm_label: cell_B_tight
    cell: {prompt: B, rules: tight}
    target_cell_weight: 0.5
`),
		},
		{
			name:      "duplicate factor name",
			wantSubst: "duplicated",
			manifest: factorialBase(`
factors:
  - name: prompt
    levels: [A, B]
  - name: prompt
    levels: [tight, loose]
treatments:
  - arm_label: a
    cell: {prompt: A}
    target_cell_weight: 0.5
  - arm_label: b
    cell: {prompt: B}
    target_cell_weight: 0.5
`),
		},
		{
			name:      "treatment cell missing factor",
			wantSubst: "pins 1 factors; expected 2",
			manifest: factorialBase(`
factors:
  - name: prompt
    levels: [A, B]
  - name: rules
    levels: [tight, loose]
treatments:
  - arm_label: cell_A_tight
    cell: {prompt: A}
    target_cell_weight: 0.25
  - arm_label: cell_A_loose
    cell: {prompt: A, rules: loose}
    target_cell_weight: 0.25
  - arm_label: cell_B_tight
    cell: {prompt: B, rules: tight}
    target_cell_weight: 0.25
  - arm_label: cell_B_loose
    cell: {prompt: B, rules: loose}
    target_cell_weight: 0.25
`),
		},
		{
			name:      "treatment cell unknown level",
			wantSubst: "is not in declared levels",
			manifest: factorialBase(`
factors:
  - name: prompt
    levels: [A, B]
  - name: rules
    levels: [tight, loose]
treatments:
  - arm_label: cell_A_tight
    cell: {prompt: A, rules: tight}
    target_cell_weight: 0.25
  - arm_label: cell_A_loose
    cell: {prompt: A, rules: loose}
    target_cell_weight: 0.25
  - arm_label: cell_B_tight
    cell: {prompt: B, rules: tight}
    target_cell_weight: 0.25
  - arm_label: cell_B_strict
    cell: {prompt: B, rules: strict}
    target_cell_weight: 0.25
`),
		},
		{
			name:      "duplicate cell",
			wantSubst: "appears in arms",
			manifest: factorialBase(`
factors:
  - name: prompt
    levels: [A, B]
  - name: rules
    levels: [tight, loose]
treatments:
  - arm_label: cell_A_tight_v1
    cell: {prompt: A, rules: tight}
    target_cell_weight: 0.25
  - arm_label: cell_A_tight_v2
    cell: {prompt: A, rules: tight}
    target_cell_weight: 0.25
  - arm_label: cell_B_tight
    cell: {prompt: B, rules: tight}
    target_cell_weight: 0.25
  - arm_label: cell_B_loose
    cell: {prompt: B, rules: loose}
    target_cell_weight: 0.25
`),
		},
		{
			name:      "partial-factorial coverage (3 of 4 cells)",
			wantSubst: "full-factorial coverage",
			manifest: factorialBase(`
factors:
  - name: prompt
    levels: [A, B]
  - name: rules
    levels: [tight, loose]
treatments:
  - arm_label: cell_A_tight
    cell: {prompt: A, rules: tight}
    target_cell_weight: 0.33
  - arm_label: cell_A_loose
    cell: {prompt: A, rules: loose}
    target_cell_weight: 0.33
  - arm_label: cell_B_tight
    cell: {prompt: B, rules: tight}
    target_cell_weight: 0.33
`),
		},
		{
			name:      "unknown kind",
			wantSubst: "kind must be one of",
			manifest: `
name: bogus
hypothesis: x
subject_agent: captain
assignment_unit: convoy
kind: fractional
treatments:
  - arm_label: a
    target_cell_weight: 0.5
  - arm_label: b
    target_cell_weight: 0.5
metrics:
  - metric_name: m
    metric_version: "1"
    direction: higher_is_better
    is_primary: true
`,
		},
	}
	db := openDB(t)
	ctx := context.Background()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := AuthorFromBytes(ctx, db, []byte(tc.manifest))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantSubst)
			}
			if !strings.Contains(err.Error(), tc.wantSubst) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantSubst)
			}
		})
	}
}

// factorialBase wraps a factors+treatments fragment with the manifest
// header so each test case stays focused on the validation branch
// under exercise.
func factorialBase(body string) string {
	return `
name: ` + sanitizedName(body) + `
hypothesis: factorial validator test
subject_agent: captain
assignment_unit: convoy
kind: factorial
` + strings.TrimLeft(body, "\n") + `
metrics:
  - metric_name: approval_rate
    metric_version: "1"
    direction: higher_is_better
    is_primary: true
`
}

// sanitizedName derives a unique-ish name from the body so SQLite
// inserts of the manifest into a shared db do not collide on indexes
// (Experiments has no UNIQUE on name, but a stable name aids debugging).
func sanitizedName(body string) string {
	h := 0
	for _, r := range body {
		h = (h*131 + int(r)) % 0x7fffffff
	}
	return "factorial-validator-case-" + intToString(h)
}

func intToString(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// TestAuthorFromYAML_FactorialFromFile — round-trip through the
// canonical sample manifest under experiments/. Catches drift between
// the parser and the on-disk reference if either changes without the
// other being updated.
func TestAuthorFromYAML_FactorialFromFile(t *testing.T) {
	root := repoRootForTest(t)
	path := filepath.Join(root, "experiments", "2026-04-30-factorial-test", "manifest.yaml")
	db := openDB(t)
	ctx := context.Background()
	id, err := AuthorFromYAML(ctx, db, path)
	if err != nil {
		t.Fatalf("AuthorFromYAML(%s): %v", path, err)
	}
	var kind string
	var n int
	if err := db.QueryRowContext(ctx, `SELECT kind FROM Experiments WHERE id = ?`, id).Scan(&kind); err != nil {
		t.Fatalf("SELECT kind: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ExperimentTreatments WHERE experiment_id = ?`, id).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if kind != KindFactorial {
		t.Errorf("kind: got %q want %q", kind, KindFactorial)
	}
	if n != 4 {
		t.Errorf("expected 4 cells in 2x2, got %d", n)
	}
}

// TestAuthorFromYAML_SinglePathUnchanged — backward-compat anchor: an
// old-shape manifest (no kind field, no factors, no per-treatment cell)
// continues to author with kind='single' and empty factors_json. The
// existing TestLifecycle_EndToEnd_ShakedownExperiment exercises the full
// happy path; this asserts the column shape directly so a regression in
// AuthorFromManifest's resolved-kind logic surfaces with a focused error.
func TestAuthorFromYAML_SinglePathUnchanged(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	id, err := AuthorFromBytes(ctx, db, []byte(minimalManifest))
	if err != nil {
		t.Fatalf("AuthorFromBytes: %v", err)
	}
	var kind, factorsJSON string
	if err := db.QueryRowContext(ctx, `SELECT kind, factors_json FROM Experiments WHERE id = ?`, id).Scan(&kind, &factorsJSON); err != nil {
		t.Fatalf("SELECT: %v", err)
	}
	if kind != KindSingle {
		t.Errorf("kind: got %q want %q", kind, KindSingle)
	}
	if factorsJSON != "[]" {
		t.Errorf("factors_json: got %q want %q", factorsJSON, "[]")
	}
	var cellJSON string
	if err := db.QueryRowContext(ctx, `SELECT cell_json FROM ExperimentTreatments WHERE experiment_id = ? ORDER BY id LIMIT 1`, id).Scan(&cellJSON); err != nil {
		t.Fatalf("SELECT cell_json: %v", err)
	}
	if cellJSON != "{}" {
		t.Errorf("single-treatment cell_json: got %q want %q", cellJSON, "{}")
	}
}

// repoRootForTest walks up from the test working directory until it
// finds go.mod. Mirrors the schema-parity test's own root-finder.
func repoRootForTest(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	dir := wd
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatalf("could not locate repo root from %s", wd)
	return ""
}
