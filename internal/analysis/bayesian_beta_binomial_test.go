package analysis

import (
	"math"
	"testing"
)

// TestBetaBinomial_KnownFixture_PosteriorMatchesAnalytic — Beta(1,1)
// prior + 10 successes / 100 trials gives the well-known closed-form
// Beta(11, 91) posterior with mean 11/102.
func TestBetaBinomial_KnownFixture_PosteriorMatchesAnalytic(t *testing.T) {
	post := NewPosterior(1, 1, 10, 100)
	if post.Alpha != 11 || post.Beta != 91 {
		t.Fatalf("posterior shape: got Beta(%v,%v), want Beta(11, 91)", post.Alpha, post.Beta)
	}
	wantMean := 11.0 / 102.0
	if math.Abs(post.Mean()-wantMean) > 1e-12 {
		t.Errorf("posterior mean: got %v, want %v", post.Mean(), wantMean)
	}
}

// TestBetaBinomial_CredibleInterval_95pct — Beta(11,91) at 95%
// equal-tail credible interval. Reference values from scipy.stats.beta:
//   beta.ppf(0.025, 11, 91) ≈ 0.05528
//   beta.ppf(0.975, 11, 91) ≈ 0.17402
// Tolerances are generous (1e-3) — the bisection converges to ~1e-15
// but a published-vs-implementation drift of 1e-3 is well below the
// noise floor of any experiment that would consume this interval.
func TestBetaBinomial_CredibleInterval_95pct(t *testing.T) {
	post := NewPosterior(1, 1, 10, 100)
	low, high := post.CredibleInterval(0.95)
	const wantLow, wantHigh = 0.05528, 0.17402
	if math.Abs(low-wantLow) > 1e-3 {
		t.Errorf("CI low: got %v, want %v ± 1e-3", low, wantLow)
	}
	if math.Abs(high-wantHigh) > 1e-3 {
		t.Errorf("CI high: got %v, want %v ± 1e-3", high, wantHigh)
	}
	// Sanity: low < mean < high.
	if !(low < post.Mean() && post.Mean() < high) {
		t.Errorf("mean %v not bracketed by CI [%v, %v]", post.Mean(), low, high)
	}
}

// TestBetaBinomial_CredibleInterval_DegenerateInputs — boundary inputs
// (confidence ≤ 0, ≥ 1, nil receiver) fall back to the full [0, 1]
// interval rather than NaN-propagating.
func TestBetaBinomial_CredibleInterval_DegenerateInputs(t *testing.T) {
	post := NewPosterior(1, 1, 10, 100)
	for _, conf := range []float64{0, -0.1, 1.0, 1.1} {
		low, high := post.CredibleInterval(conf)
		if low != 0 || high != 1 {
			t.Errorf("conf=%v: got [%v, %v], want [0, 1]", conf, low, high)
		}
	}
	var nilPost *BetaBinomialPosterior
	if low, high := nilPost.CredibleInterval(0.95); low != 0 || high != 1 {
		t.Errorf("nil receiver: got [%v, %v], want [0, 1]", low, high)
	}
}

// TestComparePosteriors_TreatmentClearlyBetter_ProbExceeds95 — strong
// effect: treatment Beta(50,50) (mean 0.5) vs control Beta(5,95)
// (mean 0.05). P(treatment > control) is essentially 1.0.
func TestComparePosteriors_TreatmentClearlyBetter_ProbExceeds95(t *testing.T) {
	treatment := &BetaBinomialPosterior{Alpha: 50, Beta: 50}
	control := &BetaBinomialPosterior{Alpha: 5, Beta: 95}
	prob := ComparePosteriors(treatment, control)
	if prob <= 0.95 {
		t.Errorf("ComparePosteriors: got %v, want > 0.95", prob)
	}
}

// TestComparePosteriors_FlippedArgs_ComplementsToOne — sanity check on
// the symmetry of P(treatment > control) + P(control > treatment) ≈ 1
// (with a tiny gap from ties at the Monte Carlo resolution).
func TestComparePosteriors_FlippedArgs_ComplementsToOne(t *testing.T) {
	a := &BetaBinomialPosterior{Alpha: 30, Beta: 20}
	b := &BetaBinomialPosterior{Alpha: 25, Beta: 25}
	pAB := ComparePosteriors(a, b)
	pBA := ComparePosteriors(b, a)
	// pAB and pBA were computed with the same seed, so the same
	// underlying Beta samples were drawn — the sum is ~1 minus
	// the fraction of ties, which is ≤ 1e-6 for continuous Beta.
	if math.Abs(pAB+pBA-1) > 0.005 {
		t.Errorf("P(A>B) + P(B>A) = %v + %v = %v; want ~1", pAB, pBA, pAB+pBA)
	}
}

// TestComparePosteriors_NilArgs — nil safe-guard returns 0 rather
// than panicking.
func TestComparePosteriors_NilArgs(t *testing.T) {
	if got := ComparePosteriors(nil, &BetaBinomialPosterior{Alpha: 1, Beta: 1}); got != 0 {
		t.Errorf("nil treatment: got %v, want 0", got)
	}
	if got := ComparePosteriors(&BetaBinomialPosterior{Alpha: 1, Beta: 1}, nil); got != 0 {
		t.Errorf("nil control: got %v, want 0", got)
	}
}

// TestDecideOutcome_Inconclusive_BelowMinSampleSize — even with a
// 100% vs 0% observed effect, n=5 trials per arm cannot meet the
// default MinSamplesPerArm=30 threshold. Decision is Inconclusive.
func TestDecideOutcome_Inconclusive_BelowMinSampleSize(t *testing.T) {
	tObs := ObservedCounts{Successes: 5, Trials: 5}
	cObs := ObservedCounts{Successes: 0, Trials: 5}
	d, err := DecideOutcome(tObs, cObs, DecisionRule{})
	if err != nil {
		t.Fatalf("DecideOutcome: unexpected error %v", err)
	}
	if d.Winner != "inconclusive" {
		t.Errorf("Winner: got %q, want %q", d.Winner, "inconclusive")
	}
	if d.MinSampleSizeMet {
		t.Errorf("MinSampleSizeMet should be false at n=5 < 30")
	}
}

// TestDecideOutcome_TreatmentWins_ClearEffect — n=200 per arm, strong
// effect (60% vs 30%). Decision is Winner='treatment' with confidence
// > 0.95 and MinSampleSizeMet=true.
func TestDecideOutcome_TreatmentWins_ClearEffect(t *testing.T) {
	tObs := ObservedCounts{Successes: 120, Trials: 200}
	cObs := ObservedCounts{Successes: 60, Trials: 200}
	d, err := DecideOutcome(tObs, cObs, DecisionRule{})
	if err != nil {
		t.Fatalf("DecideOutcome: unexpected error %v", err)
	}
	if d.Winner != "treatment" {
		t.Errorf("Winner: got %q, want %q (prob=%v)", d.Winner, "treatment", d.ProbTreatmentBetter)
	}
	if d.Confidence <= 0.95 {
		t.Errorf("Confidence: got %v, want > 0.95", d.Confidence)
	}
	if !d.MinSampleSizeMet {
		t.Errorf("MinSampleSizeMet should be true at n=200")
	}
}

// TestDecideOutcome_ControlWins_NegativeEffect — symmetric flip of
// TestDecideOutcome_TreatmentWins, control should win.
func TestDecideOutcome_ControlWins_NegativeEffect(t *testing.T) {
	tObs := ObservedCounts{Successes: 60, Trials: 200}
	cObs := ObservedCounts{Successes: 120, Trials: 200}
	d, err := DecideOutcome(tObs, cObs, DecisionRule{})
	if err != nil {
		t.Fatalf("DecideOutcome: unexpected error %v", err)
	}
	if d.Winner != "control" {
		t.Errorf("Winner: got %q, want %q (prob=%v)", d.Winner, "control", d.ProbTreatmentBetter)
	}
	if d.Confidence <= 0.95 {
		t.Errorf("Confidence: got %v, want > 0.95", d.Confidence)
	}
}

// TestDecideOutcome_Inconclusive_NullEffect — equal arms (50/100 vs
// 50/100), large n. Decision should be inconclusive: P(treatment >
// control) ≈ 0.5, neither arm crosses the 0.95 threshold.
func TestDecideOutcome_Inconclusive_NullEffect(t *testing.T) {
	obs := ObservedCounts{Successes: 50, Trials: 100}
	d, err := DecideOutcome(obs, obs, DecisionRule{})
	if err != nil {
		t.Fatalf("DecideOutcome: unexpected error %v", err)
	}
	if d.Winner != "inconclusive" {
		t.Errorf("Winner: got %q, want %q (prob=%v)", d.Winner, "inconclusive", d.ProbTreatmentBetter)
	}
	if !d.MinSampleSizeMet {
		t.Errorf("MinSampleSizeMet should be true at n=100")
	}
}

// TestDecideOutcome_RejectsImpossibleObservations — successes >
// trials returns an error rather than silently corrupting the
// posterior.
func TestDecideOutcome_RejectsImpossibleObservations(t *testing.T) {
	bad := ObservedCounts{Successes: 5, Trials: 3}
	if _, err := DecideOutcome(bad, ObservedCounts{Successes: 1, Trials: 5}, DecisionRule{}); err == nil {
		t.Errorf("expected error for successes>trials, got nil")
	}
}

// TestDecideOutcome_Idempotent_SameInputSameOutput — running the
// same observation through DecideOutcome twice with the same rule
// returns identical decisions. This is the determinism guarantee
// that the AnalysisFrameworks reproducibility property rests on.
func TestDecideOutcome_Idempotent_SameInputSameOutput(t *testing.T) {
	tObs := ObservedCounts{Successes: 70, Trials: 100}
	cObs := ObservedCounts{Successes: 50, Trials: 100}
	rule := DecisionRule{RandomSeed: 12345}
	d1, err := DecideOutcome(tObs, cObs, rule)
	if err != nil {
		t.Fatalf("DecideOutcome (first call): %v", err)
	}
	d2, err := DecideOutcome(tObs, cObs, rule)
	if err != nil {
		t.Fatalf("DecideOutcome (second call): %v", err)
	}
	if d1 != d2 {
		t.Errorf("DecideOutcome not idempotent:\n  first:  %+v\n  second: %+v", d1, d2)
	}
}
