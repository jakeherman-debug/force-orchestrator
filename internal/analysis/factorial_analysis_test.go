package analysis

import (
	"context"
	"database/sql"
	"math"
	"testing"
)

// ──────────────────────────────────────────────────────────────────────────
// Test fixtures
//
// All math fixtures here are NON-TAUTOLOGICAL: the expected posterior
// means and contrast estimates are hand-computed from the analytic
// closed form before the test is written, then asserted against the
// implementation. The closed form for a Beta-Binomial posterior under
// a Beta(1,1) prior is:
//
//     mean = (1 + successes) / (2 + trials)
//
// e.g. for the 2x2 fixture below (prompt=A pooled across rules):
//
//     successes = 80 + 60 = 140
//     trials    = 100 + 100 = 200
//     posterior = Beta(141, 61)
//     mean      = 141 / 202 ≈ 0.69802
//
// ──────────────────────────────────────────────────────────────────────────

// seedFactorialExperiment writes (Experiments, ExperimentTreatments,
// ExperimentRuns) rows for a hand-built factorial fixture. Each cell
// is given `successes` rows with score=1.0 and (trials - successes)
// rows with score=0.0. The Bernoulli interpretation
// (score >= 0.5 = success) matches lifecycle.go computeOutcome's
// scoring shape so the analyzer here reads the same projection
// production termination uses.
//
// Returns the experiment_id.
func seedFactorialExperiment(t *testing.T, db *sql.DB, factors []factorSpec, cellCounts map[string]struct{ Successes, Trials int }) int {
	t.Helper()
	ctx := context.Background()

	factorsJSON := buildFactorsJSON(factors)

	res, err := db.ExecContext(ctx, `
		INSERT INTO Experiments (name, kind, factors_json, status, min_practical_effect)
		VALUES (?, 'factorial', ?, 'running', 0)
	`, t.Name(), factorsJSON)
	if err != nil {
		t.Fatalf("seed Experiments: %v", err)
	}
	expID64, _ := res.LastInsertId()
	expID := int(expID64)

	// One ExperimentTreatments row per cell (mirrors authoring path).
	for cellKeyText, counts := range cellCounts {
		cell := parseFixtureCellKey(cellKeyText)
		cellJSON := buildCellJSON(factors, cell)

		tres, err := db.ExecContext(ctx, `
			INSERT INTO ExperimentTreatments
				(experiment_id, arm_label, cell_json, treatment_spec_id, target_cell_weight)
			VALUES (?, ?, ?, 0, 0.25)
		`, expID, cellKeyText, cellJSON)
		if err != nil {
			t.Fatalf("seed ExperimentTreatments: %v", err)
		}
		treatID64, _ := tres.LastInsertId()
		treatID := int(treatID64)

		for i := 0; i < counts.Trials; i++ {
			score := 0.0
			if i < counts.Successes {
				score = 1.0
			}
			_, err := db.ExecContext(ctx, `
				INSERT INTO ExperimentRuns
					(experiment_id, treatment_id, cell_json,
					 natural_unit_kind, natural_unit_id, mode,
					 score, score_source, completed_at)
				VALUES (?, ?, ?, 'task', ?, 'paired_real',
				        ?, 'downstream_verdict', datetime('now'))
			`, expID, treatID, cellJSON, i+1, score)
			if err != nil {
				t.Fatalf("seed ExperimentRuns: %v", err)
			}
		}
	}
	return expID
}

func buildFactorsJSON(factors []factorSpec) string {
	if len(factors) == 0 {
		return "[]"
	}
	out := "["
	for i, f := range factors {
		if i > 0 {
			out += ","
		}
		out += `{"name":"` + f.Name + `","levels":[`
		for j, l := range f.Levels {
			if j > 0 {
				out += ","
			}
			out += `"` + l + `"`
		}
		out += "]}"
	}
	out += "]"
	return out
}

func buildCellJSON(factors []factorSpec, cell map[string]string) string {
	out := "{"
	for i, f := range factors {
		if i > 0 {
			out += ","
		}
		out += `"` + f.Name + `":"` + cell[f.Name] + `"`
	}
	out += "}"
	return out
}

// parseFixtureCellKey decodes "prompt=A,rules=tight" into
// {"prompt":"A","rules":"tight"}. Used only by tests.
func parseFixtureCellKey(s string) map[string]string {
	out := map[string]string{}
	for _, pair := range splitOn(s, ",") {
		kv := splitOn(pair, "=")
		if len(kv) == 2 {
			out[kv[0]] = kv[1]
		}
	}
	return out
}

func splitOn(s, sep string) []string {
	out := []string{}
	cur := ""
	for i := 0; i < len(s); i++ {
		if i+len(sep) <= len(s) && s[i:i+len(sep)] == sep {
			out = append(out, cur)
			cur = ""
			i += len(sep) - 1
			continue
		}
		cur += string(s[i])
	}
	out = append(out, cur)
	return out
}

// ──────────────────────────────────────────────────────────────────────────
// Main effects — known-fixture tests
// ──────────────────────────────────────────────────────────────────────────

// TestMainEffects_2x2_KnownFixture seeds a 2x2 factorial (prompt ∈
// {A,B} × rules ∈ {tight, loose}) with hand-computed marginals and
// asserts ComputeMainEffects produces matching posterior means.
//
//	Cells (successes/trials):
//	  (prompt=A, rules=tight) = 80/100
//	  (prompt=A, rules=loose) = 60/100
//	  (prompt=B, rules=tight) = 70/100
//	  (prompt=B, rules=loose) = 50/100
//
//	Marginal pools:
//	  prompt=A : 80+60 = 140 / 200  → Beta(141, 61) → mean = 141/202 ≈ 0.69802
//	  prompt=B : 70+50 = 120 / 200  → Beta(121, 81) → mean = 121/202 ≈ 0.59901
//	  rules=tight : 80+70 = 150 / 200 → Beta(151, 51) → mean = 151/202 ≈ 0.74752
//	  rules=loose : 60+50 = 110 / 200 → Beta(111, 91) → mean = 111/202 ≈ 0.54950
//
//	Main effect of prompt (A − B):  0.69802 − 0.59901 = 0.09901
//	Main effect of rules (tight − loose): 0.74752 − 0.54950 = 0.19802
func TestMainEffects_2x2_KnownFixture(t *testing.T) {
	db := openMemoryDB(t)
	factors := []factorSpec{
		{Name: "prompt", Levels: []string{"A", "B"}},
		{Name: "rules", Levels: []string{"tight", "loose"}},
	}
	expID := seedFactorialExperiment(t, db, factors, map[string]struct{ Successes, Trials int }{
		"prompt=A,rules=tight": {80, 100},
		"prompt=A,rules=loose": {60, 100},
		"prompt=B,rules=tight": {70, 100},
		"prompt=B,rules=loose": {50, 100},
	})

	effects, err := ComputeMainEffectsWithRule(context.Background(), db, expID, DecisionRule{RandomSeed: 12345})
	if err != nil {
		t.Fatalf("ComputeMainEffects: %v", err)
	}

	want := map[string]float64{
		"prompt:A":    141.0 / 202.0, // ≈ 0.69802
		"prompt:B":    121.0 / 202.0, // ≈ 0.59901
		"rules:tight": 151.0 / 202.0, // ≈ 0.74752
		"rules:loose": 111.0 / 202.0, // ≈ 0.54950
	}
	got := map[string]float64{}
	for _, e := range effects {
		got[e.Factor+":"+e.Level] = e.Mean
	}
	for k, w := range want {
		g, ok := got[k]
		if !ok {
			t.Fatalf("missing main effect for %q", k)
		}
		if math.Abs(g-w) > 1e-3 {
			t.Errorf("%s: got mean %.5f, want %.5f", k, g, w)
		}
	}

	// Hand-computed main-effect deltas. Assert directly.
	wantPromptDelta := 0.69802 - 0.59901
	wantRulesDelta := 0.74752 - 0.54950
	gotPromptDelta := got["prompt:A"] - got["prompt:B"]
	gotRulesDelta := got["rules:tight"] - got["rules:loose"]
	if math.Abs(gotPromptDelta-wantPromptDelta) > 1e-3 {
		t.Errorf("main effect of prompt (A−B): got %.5f, want %.5f", gotPromptDelta, wantPromptDelta)
	}
	if math.Abs(gotRulesDelta-wantRulesDelta) > 1e-3 {
		t.Errorf("main effect of rules (tight−loose): got %.5f, want %.5f", gotRulesDelta, wantRulesDelta)
	}

	// Reference levels (first-declared) get ProbBetterThanRef = 0.5 by definition.
	for _, e := range effects {
		if (e.Factor == "prompt" && e.Level == "A") || (e.Factor == "rules" && e.Level == "tight") {
			if math.Abs(e.ProbBetterThanRef-0.5) > 1e-9 {
				t.Errorf("reference level %s=%s should have ProbBetterThanRef=0.5; got %v", e.Factor, e.Level, e.ProbBetterThanRef)
			}
		}
	}
}

// TestMainEffects_3x2_KnownFixture extends to 3 levels on one factor.
//
//	Factors: model ∈ {X, Y, Z}, rules ∈ {tight, loose}. 6 cells, 100/cell.
//
//	Cells (successes/trials):
//	  (model=X, rules=tight) = 80/100
//	  (model=X, rules=loose) = 60/100
//	  (model=Y, rules=tight) = 70/100
//	  (model=Y, rules=loose) = 50/100
//	  (model=Z, rules=tight) = 40/100
//	  (model=Z, rules=loose) = 20/100
//
//	Marginal pools:
//	  model=X : 140 / 200 → Beta(141, 61)  → mean = 141/202 ≈ 0.69802
//	  model=Y : 120 / 200 → Beta(121, 81)  → mean = 121/202 ≈ 0.59901
//	  model=Z :  60 / 200 → Beta( 61, 141) → mean =  61/202 ≈ 0.30198
//	  rules=tight : 80+70+40 = 190 / 300 → Beta(191, 111) → mean = 191/302 ≈ 0.63245
//	  rules=loose : 60+50+20 = 130 / 300 → Beta(131, 171) → mean = 131/302 ≈ 0.43377
func TestMainEffects_3x2_KnownFixture(t *testing.T) {
	db := openMemoryDB(t)
	factors := []factorSpec{
		{Name: "model", Levels: []string{"X", "Y", "Z"}},
		{Name: "rules", Levels: []string{"tight", "loose"}},
	}
	expID := seedFactorialExperiment(t, db, factors, map[string]struct{ Successes, Trials int }{
		"model=X,rules=tight": {80, 100},
		"model=X,rules=loose": {60, 100},
		"model=Y,rules=tight": {70, 100},
		"model=Y,rules=loose": {50, 100},
		"model=Z,rules=tight": {40, 100},
		"model=Z,rules=loose": {20, 100},
	})

	effects, err := ComputeMainEffectsWithRule(context.Background(), db, expID, DecisionRule{RandomSeed: 67890})
	if err != nil {
		t.Fatalf("ComputeMainEffects: %v", err)
	}

	want := map[string]float64{
		"model:X":     141.0 / 202.0, // ≈ 0.69802
		"model:Y":     121.0 / 202.0, // ≈ 0.59901
		"model:Z":     61.0 / 202.0,  // ≈ 0.30198
		"rules:tight": 191.0 / 302.0, // ≈ 0.63245
		"rules:loose": 131.0 / 302.0, // ≈ 0.43377
	}
	got := map[string]float64{}
	for _, e := range effects {
		got[e.Factor+":"+e.Level] = e.Mean
	}
	for k, w := range want {
		g, ok := got[k]
		if !ok {
			t.Fatalf("missing main effect for %q", k)
		}
		if math.Abs(g-w) > 1e-3 {
			t.Errorf("%s: got mean %.5f, want %.5f", k, g, w)
		}
	}

	// Sanity: P(Y > X) and P(Z > X) on Monte Carlo. X is the reference
	// (highest mean), so Y and Z should both have ProbBetterThanRef
	// substantially below 0.5.
	for _, e := range effects {
		switch e.Factor + ":" + e.Level {
		case "model:Y":
			if e.ProbBetterThanRef >= 0.5 {
				t.Errorf("model=Y should have P(Y > X) < 0.5; got %v", e.ProbBetterThanRef)
			}
		case "model:Z":
			if e.ProbBetterThanRef >= 0.1 {
				t.Errorf("model=Z (mean 0.30) vs X (mean 0.70) at n=200/level should have P(Z > X) ≪ 0.1; got %v", e.ProbBetterThanRef)
			}
		}
	}
}

// TestBetaBinomial_StillPasses asserts that the existing single-
// treatment tests in bayesian_beta_binomial_test.go still cover the
// untouched single-treatment path. We re-call the public surface
// here as a smoke check; the full assertions live in the dedicated
// test file.
func TestBetaBinomial_StillPasses(t *testing.T) {
	post := NewPosterior(1, 1, 10, 100)
	if post.Alpha != 11 || post.Beta != 91 {
		t.Errorf("single-treatment posterior shape regressed: got Beta(%v,%v), want Beta(11, 91)", post.Alpha, post.Beta)
	}
	if math.Abs(post.Mean()-11.0/102.0) > 1e-12 {
		t.Errorf("single-treatment posterior mean regressed: got %v, want %v", post.Mean(), 11.0/102.0)
	}
	tObs := ObservedCounts{Successes: 120, Trials: 200}
	cObs := ObservedCounts{Successes: 60, Trials: 200}
	d, err := DecideOutcome(tObs, cObs, DecisionRule{})
	if err != nil {
		t.Fatalf("single-treatment DecideOutcome regressed: %v", err)
	}
	if d.Winner != "treatment" {
		t.Errorf("single-treatment DecideOutcome winner regressed: got %q, want treatment", d.Winner)
	}
}
