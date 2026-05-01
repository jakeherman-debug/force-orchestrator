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

// ──────────────────────────────────────────────────────────────────────────
// 2-way interactions — known-fixture tests
// ──────────────────────────────────────────────────────────────────────────

// Test2WayInteractions_NoInteraction seeds cells where the two main
// effects compose ADDITIVELY: prompt adds 0.10 regardless of rules
// level, and rules adds 0.20 regardless of prompt level. Interaction
// estimate should be near zero, and ProbNonzero should NOT cross the
// WinnerThreshold (0.95 default).
//
//	Cells (successes/trials, all n=200):
//	  (prompt=A, rules=tight) = 140/200 (0.70)
//	  (prompt=A, rules=loose) = 100/200 (0.50)
//	  (prompt=B, rules=tight) = 120/200 (0.60)
//	  (prompt=B, rules=loose) =  80/200 (0.40)
//
//	raw interaction = (0.70 − 0.60) − (0.50 − 0.40) = 0.10 − 0.10 = 0.00
func Test2WayInteractions_NoInteraction(t *testing.T) {
	db := openMemoryDB(t)
	factors := []factorSpec{
		{Name: "prompt", Levels: []string{"A", "B"}},
		{Name: "rules", Levels: []string{"tight", "loose"}},
	}
	expID := seedFactorialExperiment(t, db, factors, map[string]struct{ Successes, Trials int }{
		"prompt=A,rules=tight": {140, 200},
		"prompt=A,rules=loose": {100, 200},
		"prompt=B,rules=tight": {120, 200},
		"prompt=B,rules=loose": {80, 200},
	})

	rule := DecisionRule{RandomSeed: 11111}
	rows, err := Compute2WayInteractionsWithRule(context.Background(), db, expID, rule)
	if err != nil {
		t.Fatalf("Compute2WayInteractions: %v", err)
	}
	// Locate the canonical (la=B, lb=loose) anchor row — the only row
	// with a non-degenerate contrast against the references (refA=A,
	// refB=tight). When level_a == refA OR level_b == refB the
	// contrast collapses algebraically to zero; those rows still
	// appear in the surface (so 3+-level factors retain coverage)
	// but carry no interaction information.
	var anchor *Interaction2Way
	for i := range rows {
		r := rows[i]
		if r.LevelA != "B" || r.LevelB != "loose" {
			continue
		}
		anchor = &rows[i]
	}
	if anchor == nil {
		t.Fatalf("no anchor row for (B, loose); rows=%+v", rows)
	}
	// Hand-computed posterior-mean of the interaction. Beta(1,1)
	// prior makes the cell-posterior means almost exactly the raw
	// rates with a tiny shrinkage toward 0.5 — the contrast is the
	// difference of differences and should be ~0.0 within 0.05.
	if math.Abs(anchor.InteractionEstimate) >= 0.05 {
		t.Errorf("|interaction| should be < 0.05 for additive cells; got %v", anchor.InteractionEstimate)
	}
	if anchor.ProbNonzero > 0.95 {
		t.Errorf("ProbNonzero should be ≤ 0.95 for null-interaction fixture; got %v", anchor.ProbNonzero)
	}
}

// Test2WayInteractions_StrongInteraction seeds cells where the
// interaction is large: A and B reverse on the rules dimension.
//
//	Cells (successes/trials, all n=200):
//	  (prompt=A, rules=tight) = 180/200 (0.90)
//	  (prompt=A, rules=loose) =  60/200 (0.30)
//	  (prompt=B, rules=tight) =  60/200 (0.30)
//	  (prompt=B, rules=loose) = 180/200 (0.90)
//
//	raw interaction = (0.90 − 0.30) − (0.30 − 0.90) = 0.60 − (−0.60) = 1.20
//
// With Beta(1,1) prior and n=200/cell the cell-posterior means
// shrink toward 0.5 by ~0.005 each, so the contrast is very close
// to 1.20 (≥ 1.15).
func Test2WayInteractions_StrongInteraction(t *testing.T) {
	db := openMemoryDB(t)
	factors := []factorSpec{
		{Name: "prompt", Levels: []string{"A", "B"}},
		{Name: "rules", Levels: []string{"tight", "loose"}},
	}
	expID := seedFactorialExperiment(t, db, factors, map[string]struct{ Successes, Trials int }{
		"prompt=A,rules=tight": {180, 200},
		"prompt=A,rules=loose": {60, 200},
		"prompt=B,rules=tight": {60, 200},
		"prompt=B,rules=loose": {180, 200},
	})

	rule := DecisionRule{RandomSeed: 22222}
	rows, err := Compute2WayInteractionsWithRule(context.Background(), db, expID, rule)
	if err != nil {
		t.Fatalf("Compute2WayInteractions: %v", err)
	}
	// Canonical non-degenerate anchor: (la=B, lb=loose) with
	// references (refA=A, refB=tight). Contrast =
	//
	//   [mean(B,loose) - mean(A,loose)] - [mean(B,tight) - mean(A,tight)]
	//   raw : (0.90 - 0.30) - (0.30 - 0.90) = 0.60 - (-0.60) = 1.20
	var anchor *Interaction2Way
	for i := range rows {
		r := rows[i]
		if r.LevelA != "B" || r.LevelB != "loose" {
			continue
		}
		anchor = &rows[i]
	}
	if anchor == nil {
		t.Fatalf("no anchor row for (B, loose); rows=%+v", rows)
	}
	if anchor.InteractionEstimate < 1.15 {
		t.Errorf("interaction estimate should be near 1.20 for crossover fixture; got %v", anchor.InteractionEstimate)
	}
	if anchor.ProbNonzero <= 0.95 {
		t.Errorf("ProbNonzero should be > 0.95 for strong-interaction fixture; got %v", anchor.ProbNonzero)
	}
}

// Test2WayInteractions_PersistsToTable verifies that
// Compute2WayInteractions writes one row per (factor_a, factor_b,
// level_a, level_b) tuple into ExperimentInteractions, and that
// re-running the analyzer replaces (not duplicates) the prior rows.
func Test2WayInteractions_PersistsToTable(t *testing.T) {
	db := openMemoryDB(t)
	ctx := context.Background()
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
	rule := DecisionRule{RandomSeed: 33333}
	if _, err := Compute2WayInteractionsWithRule(ctx, db, expID, rule); err != nil {
		t.Fatalf("first run: %v", err)
	}

	var count int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM ExperimentInteractions WHERE experiment_id = ?`,
		expID,
	).Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	// 1 ordered pair × 2 levels × 2 levels = 4 rows.
	if count != 4 {
		t.Errorf("row count after first run: got %d, want 4", count)
	}

	// Re-run — should replace, not duplicate.
	if _, err := Compute2WayInteractionsWithRule(ctx, db, expID, rule); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM ExperimentInteractions WHERE experiment_id = ?`,
		expID,
	).Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if count != 4 {
		t.Errorf("row count after re-run: got %d, want 4 (idempotent)", count)
	}

	// Inspect a row's column values — they should match the in-memory
	// shape exactly.
	var (
		fa, fb, la, lb string
		estimate       float64
		alpha, beta    float64
		probNonzero    float64
	)
	if err := db.QueryRowContext(ctx, `
		SELECT factor_a, factor_b, level_a, level_b,
		       interaction_estimate, posterior_alpha, posterior_beta,
		       posterior_prob_nonzero
		FROM ExperimentInteractions
		WHERE experiment_id = ? AND level_a = 'A' AND level_b = 'tight'
	`, expID).Scan(&fa, &fb, &la, &lb, &estimate, &alpha, &beta, &probNonzero); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if fa != "prompt" || fb != "rules" {
		t.Errorf("(factor_a, factor_b): got (%q, %q), want (prompt, rules)", fa, fb)
	}
	if alpha != 81 { // 80 successes + Beta(1,1) prior alpha
		t.Errorf("posterior_alpha: got %v, want 81", alpha)
	}
	if beta != 21 { // 20 failures + Beta(1,1) prior beta
		t.Errorf("posterior_beta: got %v, want 21", beta)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Decision rule
// ──────────────────────────────────────────────────────────────────────────

// TestDecideFactorialOutcome_DeclaredWinner — additive cells where
// one cell clearly dominates. Reason should be 'declared_winner' and
// BestCell should be set.
func TestDecideFactorialOutcome_DeclaredWinner(t *testing.T) {
	db := openMemoryDB(t)
	factors := []factorSpec{
		{Name: "prompt", Levels: []string{"A", "B"}},
		{Name: "rules", Levels: []string{"tight", "loose"}},
	}
	// Big effect, big N: (A, tight) clearly dominates and the
	// remaining cells stay additive — no significant interactions.
	expID := seedFactorialExperiment(t, db, factors, map[string]struct{ Successes, Trials int }{
		"prompt=A,rules=tight": {180, 200}, // 0.90
		"prompt=A,rules=loose": {120, 200}, // 0.60
		"prompt=B,rules=tight": {100, 200}, // 0.50
		"prompt=B,rules=loose": {40, 200},  // 0.20
	})

	rule := DecisionRule{RandomSeed: 44444}
	dec, err := DecideFactorialOutcome(context.Background(), db, expID, rule)
	if err != nil {
		t.Fatalf("DecideFactorialOutcome: %v", err)
	}
	if dec.Reason != "declared_winner" {
		t.Fatalf("Reason: got %q, want 'declared_winner' (decision=%+v)", dec.Reason, dec)
	}
	if dec.BestCell == nil {
		t.Fatalf("BestCell should be set; got nil")
	}
	if dec.BestCell["prompt"] != "A" || dec.BestCell["rules"] != "tight" {
		t.Errorf("BestCell: got %+v, want {prompt:A, rules:tight}", dec.BestCell)
	}
	if dec.BestCellPosterior <= 0.95 {
		t.Errorf("BestCellPosterior: got %v, want > 0.95", dec.BestCellPosterior)
	}
}

// TestDecideFactorialOutcome_SignificantInteraction — strong-
// interaction fixture; expect Reason='significant_interaction' and
// SignificantInteractions populated. BestCell should be nil because
// the operator must read the interaction surface.
func TestDecideFactorialOutcome_SignificantInteraction(t *testing.T) {
	db := openMemoryDB(t)
	factors := []factorSpec{
		{Name: "prompt", Levels: []string{"A", "B"}},
		{Name: "rules", Levels: []string{"tight", "loose"}},
	}
	expID := seedFactorialExperiment(t, db, factors, map[string]struct{ Successes, Trials int }{
		"prompt=A,rules=tight": {180, 200}, // 0.90
		"prompt=A,rules=loose": {60, 200},  // 0.30
		"prompt=B,rules=tight": {60, 200},  // 0.30
		"prompt=B,rules=loose": {180, 200}, // 0.90
	})

	rule := DecisionRule{RandomSeed: 55555}
	dec, err := DecideFactorialOutcome(context.Background(), db, expID, rule)
	if err != nil {
		t.Fatalf("DecideFactorialOutcome: %v", err)
	}
	if dec.Reason != "significant_interaction" {
		t.Fatalf("Reason: got %q, want 'significant_interaction' (decision=%+v)", dec.Reason, dec)
	}
	if len(dec.SignificantInteractions) == 0 {
		t.Errorf("SignificantInteractions should be populated; got empty")
	}
	if dec.BestCell != nil {
		t.Errorf("BestCell should be nil when interactions are significant; got %+v", dec.BestCell)
	}
}

// TestDecideFactorialOutcome_Inconclusive — small-N cells. Reason
// should be 'inconclusive' and BestCell nil.
func TestDecideFactorialOutcome_Inconclusive(t *testing.T) {
	db := openMemoryDB(t)
	factors := []factorSpec{
		{Name: "prompt", Levels: []string{"A", "B"}},
		{Name: "rules", Levels: []string{"tight", "loose"}},
	}
	// n=10/cell — well below MinSamplesPerArm=30 default. Counts kept
	// near 50/50 so no interaction crosses ProbNonzero threshold and
	// the outcome lands on 'inconclusive' rather than
	// 'significant_interaction'.
	expID := seedFactorialExperiment(t, db, factors, map[string]struct{ Successes, Trials int }{
		"prompt=A,rules=tight": {5, 10},
		"prompt=A,rules=loose": {5, 10},
		"prompt=B,rules=tight": {5, 10},
		"prompt=B,rules=loose": {5, 10},
	})

	rule := DecisionRule{RandomSeed: 66666}
	dec, err := DecideFactorialOutcome(context.Background(), db, expID, rule)
	if err != nil {
		t.Fatalf("DecideFactorialOutcome: %v", err)
	}
	if dec.Reason != "inconclusive" {
		t.Errorf("Reason: got %q, want 'inconclusive' (decision=%+v)", dec.Reason, dec)
	}
	if dec.BestCell != nil {
		t.Errorf("BestCell should be nil; got %+v", dec.BestCell)
	}
}

// TestDecideFactorialOutcome_Idempotent — running the same fixture
// through DecideFactorialOutcome twice with the same rule returns
// identical decisions (modulo map ordering on BestCell).
func TestDecideFactorialOutcome_Idempotent(t *testing.T) {
	db := openMemoryDB(t)
	factors := []factorSpec{
		{Name: "prompt", Levels: []string{"A", "B"}},
		{Name: "rules", Levels: []string{"tight", "loose"}},
	}
	expID := seedFactorialExperiment(t, db, factors, map[string]struct{ Successes, Trials int }{
		"prompt=A,rules=tight": {180, 200},
		"prompt=A,rules=loose": {120, 200},
		"prompt=B,rules=tight": {100, 200},
		"prompt=B,rules=loose": {40, 200},
	})

	rule := DecisionRule{RandomSeed: 77777}
	d1, err := DecideFactorialOutcome(context.Background(), db, expID, rule)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	d2, err := DecideFactorialOutcome(context.Background(), db, expID, rule)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if d1.Reason != d2.Reason {
		t.Errorf("Reason: first %q, second %q", d1.Reason, d2.Reason)
	}
	if math.Abs(d1.BestCellPosterior-d2.BestCellPosterior) > 1e-9 {
		t.Errorf("BestCellPosterior: first %v, second %v", d1.BestCellPosterior, d2.BestCellPosterior)
	}
	if !sameCell(d1.BestCell, d2.BestCell) {
		t.Errorf("BestCell mismatch: first %+v, second %+v", d1.BestCell, d2.BestCell)
	}
}

func sameCell(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// ──────────────────────────────────────────────────────────────────────────
// Factorial framework registration
// ──────────────────────────────────────────────────────────────────────────

// TestRegisterBayesianBetaBinomialFactorial_HappyPath inserts the
// factorial framework row, asserts the row's name/version/decomposition
// match the package constants, and confirms the sibling shape (the
// single-treatment row is unaffected).
func TestRegisterBayesianBetaBinomialFactorial_HappyPath(t *testing.T) {
	db := openMemoryDB(t)
	ctx := context.Background()

	// Pre-register the single-treatment row so we can verify the
	// factorial row is a sibling, not a replacement.
	if err := RegisterBayesianBetaBinomial(ctx, db); err != nil {
		t.Fatalf("RegisterBayesianBetaBinomial: %v", err)
	}
	if err := RegisterBayesianBetaBinomialFactorial(ctx, db); err != nil {
		t.Fatalf("RegisterBayesianBetaBinomialFactorial: %v", err)
	}

	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM AnalysisFrameworks`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Errorf("AnalysisFrameworks row count: got %d, want 2 (single + factorial)", count)
	}

	var version, hash, configContent string
	err := db.QueryRowContext(ctx, `
		SELECT version, config_hash, config_content
		FROM AnalysisFrameworks WHERE version = ?
	`, BayesianBetaBinomialFactorialVersion).Scan(&version, &hash, &configContent)
	if err != nil {
		t.Fatalf("read factorial row: %v", err)
	}
	if version != BayesianBetaBinomialFactorialVersion {
		t.Errorf("version: got %q, want %q", version, BayesianBetaBinomialFactorialVersion)
	}
	if hash == "" {
		t.Errorf("config_hash should be non-empty")
	}
	// decomposition key must be present and == 'main_effects_plus_2way'
	// per paired-runs.md § Factorial Scoring.
	if !contains(configContent, `"decomposition":"main_effects_plus_2way"`) {
		t.Errorf("config_content missing decomposition='main_effects_plus_2way': %s", configContent)
	}
}

// TestRegisterBayesianBetaBinomialFactorial_Idempotent — calling
// register twice produces exactly one row.
func TestRegisterBayesianBetaBinomialFactorial_Idempotent(t *testing.T) {
	db := openMemoryDB(t)
	ctx := context.Background()
	if err := RegisterBayesianBetaBinomialFactorial(ctx, db); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if err := RegisterBayesianBetaBinomialFactorial(ctx, db); err != nil {
		t.Fatalf("second call: %v", err)
	}
	var count int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM AnalysisFrameworks WHERE version = ?`,
		BayesianBetaBinomialFactorialVersion,
	).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("row count: got %d, want 1", count)
	}
}

// TestRegisterBayesianBetaBinomialFactorial_NilDB — nil db is a
// programmer error, not a runtime panic.
func TestRegisterBayesianBetaBinomialFactorial_NilDB(t *testing.T) {
	if err := RegisterBayesianBetaBinomialFactorial(context.Background(), nil); err == nil {
		t.Errorf("expected error for nil db, got nil")
	}
}

// contains is a substring helper used by the registration test —
// chosen over importing strings.Contains to keep the test file's
// dependency set minimal.
func contains(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
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
