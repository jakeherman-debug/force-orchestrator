// Package analysis implements the experiment-outcome math used by D3
// paired-runs to declare winners.
//
// The Bayesian Beta-Binomial framework (registered at daemon startup as
// AnalysisFrameworks.version='2026-04-29') models each arm's success
// rate as a Beta posterior; the comparison "treatment vs control" is
// answered via P(treatment > control), computed by Monte Carlo
// integration over the two posteriors. Credible intervals on each
// posterior are equal-tail Beta quantiles obtained by bisection on the
// regularised incomplete beta function.
//
// The math here is intentionally library-free: gonum is not a project
// dependency, and the algorithms below (Lentz continued fractions for
// I_x(a,b), Marsaglia-Tsang squeeze for Gamma sampling, and ratio-of-
// gammas for Beta sampling) are well-established numerical methods
// reproducible from any standard reference (Numerical Recipes 3rd ed.,
// Marsaglia & Tsang 2000).
package analysis

import (
	"errors"
	"fmt"
	"math"
	"math/rand"
)

// BetaBinomialPosterior is the Beta(alpha, beta) posterior produced by
// updating a Beta(prior_alpha, prior_beta) prior with observed
// (successes, trials-successes) Bernoulli data.
type BetaBinomialPosterior struct {
	Alpha float64
	Beta  float64
}

// NewPosterior returns the Beta-Binomial posterior after observing
// `successes` successes out of `trials` Bernoulli trials, starting from
// a Beta(priorAlpha, priorBeta) prior.
//
// Beta(a, b) + Bin(n, k) → Beta(a+k, b+n-k). This is the conjugate
// closed form; no numerical integration required.
func NewPosterior(priorAlpha, priorBeta float64, successes, trials int) *BetaBinomialPosterior {
	if priorAlpha <= 0 || priorBeta <= 0 {
		// Uninformative defaults rather than NaN propagation: a
		// degenerate prior would silently produce a degenerate
		// posterior; instead, fall back to Beta(1,1) so the caller
		// sees a well-formed distribution.
		priorAlpha, priorBeta = 1, 1
	}
	if successes < 0 {
		successes = 0
	}
	if trials < successes {
		trials = successes
	}
	failures := trials - successes
	return &BetaBinomialPosterior{
		Alpha: priorAlpha + float64(successes),
		Beta:  priorBeta + float64(failures),
	}
}

// Mean returns the posterior mean alpha / (alpha + beta).
func (p *BetaBinomialPosterior) Mean() float64 {
	if p == nil {
		return 0
	}
	return p.Alpha / (p.Alpha + p.Beta)
}

// CredibleInterval returns the equal-tail credible interval at the
// given confidence level (e.g. 0.95 for a 95% interval). The interval
// (low, high) is computed by bisecting the regularised incomplete beta
// function I_x(alpha, beta) at quantiles (1-conf)/2 and (1+conf)/2.
//
// Confidence is clamped to (0, 1); pathological values fall back to
// the full [0, 1] interval rather than returning NaN.
func (p *BetaBinomialPosterior) CredibleInterval(confidence float64) (low, high float64) {
	if p == nil || confidence <= 0 || confidence >= 1 {
		return 0, 1
	}
	tail := (1 - confidence) / 2
	low = betaQuantile(tail, p.Alpha, p.Beta)
	high = betaQuantile(1-tail, p.Alpha, p.Beta)
	return low, high
}

// ObservedCounts is the Bernoulli summary handed to DecideOutcome:
// `Successes` favourable outcomes out of `Trials` independent trials.
type ObservedCounts struct {
	Successes int
	Trials    int
}

// DecisionRule controls how DecideOutcome maps observations onto
// {treatment, control, inconclusive}.
type DecisionRule struct {
	// PriorAlpha / PriorBeta — the Beta(α, β) prior shared by both arms.
	// Defaults to Beta(1, 1) (uniform on [0,1]) when both are zero.
	PriorAlpha float64
	PriorBeta  float64

	// MinSamplesPerArm — minimum trials required per arm before the
	// decision is allowed to declare a winner. Below this threshold,
	// DecideOutcome returns Inconclusive regardless of effect size.
	// Default 30.
	MinSamplesPerArm int

	// WinnerThreshold — required posterior probability that one arm is
	// better than the other before declaring it the winner. Default
	// 0.95 (matches the medium stakes-tier threshold from
	// paired-runs.md § Scoring and Significance).
	WinnerThreshold float64

	// MonteCarloSamples — sample count for the Monte Carlo estimate of
	// P(treatment > control). Default 200000 (≈ 2 ms per call, ~3
	// decimal places of accuracy).
	MonteCarloSamples int

	// RandomSeed — RNG seed for the Monte Carlo. Tests pin this for
	// determinism; production picks an arbitrary fixed value so two
	// reads of the same observed data return the same decision.
	RandomSeed int64
}

func (r DecisionRule) withDefaults() DecisionRule {
	if r.PriorAlpha == 0 && r.PriorBeta == 0 {
		r.PriorAlpha, r.PriorBeta = 1, 1
	}
	if r.MinSamplesPerArm <= 0 {
		r.MinSamplesPerArm = 30
	}
	if r.WinnerThreshold <= 0 {
		r.WinnerThreshold = 0.95
	}
	if r.MonteCarloSamples <= 0 {
		r.MonteCarloSamples = 200000
	}
	if r.RandomSeed == 0 {
		r.RandomSeed = 0xD3B7B1003
	}
	return r
}

// Decision is the output of DecideOutcome: which arm won (or
// "inconclusive"), how confident we are, and whether the minimum
// sample-size precondition was met.
type Decision struct {
	Winner             string  // 'treatment' | 'control' | 'inconclusive'
	Confidence         float64 // P(winner > loser) — undefined when Winner == 'inconclusive'
	ProbTreatmentBetter float64
	MinSampleSizeMet   bool
	TreatmentPosterior BetaBinomialPosterior
	ControlPosterior   BetaBinomialPosterior
}

// ComparePosteriors estimates P(treatment > control) by Monte Carlo
// sampling N pairs from each Beta posterior and counting the fraction
// for which treatment > control. Result is in [0, 1]. The seed makes
// the estimate deterministic for tests.
func ComparePosteriors(treatment, control *BetaBinomialPosterior) float64 {
	return compareWithSeed(treatment, control, 200000, 0xD3B7B1003)
}

func compareWithSeed(treatment, control *BetaBinomialPosterior, samples int, seed int64) float64 {
	if treatment == nil || control == nil {
		return 0
	}
	if samples <= 0 {
		samples = 200000
	}
	rng := rand.New(rand.NewSource(seed))
	var hits int
	for i := 0; i < samples; i++ {
		t := sampleBeta(rng, treatment.Alpha, treatment.Beta)
		c := sampleBeta(rng, control.Alpha, control.Beta)
		if t > c {
			hits++
		}
	}
	return float64(hits) / float64(samples)
}

// DecideOutcome is the framework's top-level decision entry point. It
// builds Beta-Binomial posteriors for each arm under the rule's prior,
// estimates P(treatment > control) by Monte Carlo, and returns a
// Decision tagged with whether the minimum-sample-size precondition
// was met. Below MinSamplesPerArm on either arm, the result is always
// Inconclusive regardless of observed effect.
func DecideOutcome(treatmentObs, controlObs ObservedCounts, rule DecisionRule) (Decision, error) {
	if treatmentObs.Trials < 0 || controlObs.Trials < 0 {
		return Decision{}, errors.New("DecideOutcome: trials must be non-negative")
	}
	if treatmentObs.Successes > treatmentObs.Trials || controlObs.Successes > controlObs.Trials {
		return Decision{}, fmt.Errorf("DecideOutcome: successes (%d/%d, %d/%d) cannot exceed trials",
			treatmentObs.Successes, treatmentObs.Trials,
			controlObs.Successes, controlObs.Trials)
	}
	rule = rule.withDefaults()
	tPost := NewPosterior(rule.PriorAlpha, rule.PriorBeta, treatmentObs.Successes, treatmentObs.Trials)
	cPost := NewPosterior(rule.PriorAlpha, rule.PriorBeta, controlObs.Successes, controlObs.Trials)
	probTBetter := compareWithSeed(tPost, cPost, rule.MonteCarloSamples, rule.RandomSeed)
	minMet := treatmentObs.Trials >= rule.MinSamplesPerArm && controlObs.Trials >= rule.MinSamplesPerArm

	d := Decision{
		ProbTreatmentBetter: probTBetter,
		MinSampleSizeMet:    minMet,
		TreatmentPosterior:  *tPost,
		ControlPosterior:    *cPost,
	}
	if !minMet {
		d.Winner = "inconclusive"
		return d, nil
	}
	switch {
	case probTBetter >= rule.WinnerThreshold:
		d.Winner = "treatment"
		d.Confidence = probTBetter
	case (1 - probTBetter) >= rule.WinnerThreshold:
		d.Winner = "control"
		d.Confidence = 1 - probTBetter
	default:
		d.Winner = "inconclusive"
	}
	return d, nil
}

// ──────────────────────────────────────────────────────────────────────────
// Numerical helpers
// ──────────────────────────────────────────────────────────────────────────

// regularizedIncompleteBeta returns I_x(a, b) — the regularised
// incomplete beta function. Implementation follows the canonical
// Numerical-Recipes layout: continued-fraction expansion via Lentz's
// algorithm, with the symmetry I_x(a,b) = 1 - I_{1-x}(b,a) used to
// keep the continued-fraction argument in its fastest-convergence
// region (x < (a+1)/(a+b+2)).
func regularizedIncompleteBeta(x, a, b float64) float64 {
	if x <= 0 {
		return 0
	}
	if x >= 1 {
		return 1
	}
	if a <= 0 || b <= 0 {
		return math.NaN()
	}
	bt := math.Exp(lgamma(a+b) - lgamma(a) - lgamma(b) +
		a*math.Log(x) + b*math.Log(1-x))
	if x < (a+1)/(a+b+2) {
		return bt * betaContinuedFraction(x, a, b) / a
	}
	return 1 - bt*betaContinuedFraction(1-x, b, a)/b
}

// betaContinuedFraction evaluates the modified Lentz continued fraction
// for the incomplete beta. See NR §6.4 — the recurrence terms d2m and
// d2m+1 are encoded inline below.
func betaContinuedFraction(x, a, b float64) float64 {
	const (
		maxIters = 200
		eps      = 3e-12
		fpmin    = 1e-300
	)
	qab := a + b
	qap := a + 1
	qam := a - 1
	c := 1.0
	d := 1.0 - qab*x/qap
	if math.Abs(d) < fpmin {
		d = fpmin
	}
	d = 1 / d
	h := d
	for m := 1; m <= maxIters; m++ {
		mf := float64(m)
		m2 := 2 * mf
		// Even step.
		aa := mf * (b - mf) * x / ((qam + m2) * (a + m2))
		d = 1 + aa*d
		if math.Abs(d) < fpmin {
			d = fpmin
		}
		c = 1 + aa/c
		if math.Abs(c) < fpmin {
			c = fpmin
		}
		d = 1 / d
		h *= d * c
		// Odd step.
		aa = -(a + mf) * (qab + mf) * x / ((a + m2) * (qap + m2))
		d = 1 + aa*d
		if math.Abs(d) < fpmin {
			d = fpmin
		}
		c = 1 + aa/c
		if math.Abs(c) < fpmin {
			c = fpmin
		}
		d = 1 / d
		del := d * c
		h *= del
		if math.Abs(del-1) < eps {
			break
		}
	}
	return h
}

// betaQuantile inverts I_x(a, b) at probability p — i.e. returns the
// equal-tail quantile of Beta(a, b) at level p. Implemented as a
// bracketed bisection: I_x is monotonic in x, so 60 iterations on
// [0, 1] is sufficient for ~18 decimal places of precision.
func betaQuantile(p, a, b float64) float64 {
	if p <= 0 {
		return 0
	}
	if p >= 1 {
		return 1
	}
	lo, hi := 0.0, 1.0
	for i := 0; i < 80; i++ {
		mid := 0.5 * (lo + hi)
		if regularizedIncompleteBeta(mid, a, b) < p {
			lo = mid
		} else {
			hi = mid
		}
	}
	return 0.5 * (lo + hi)
}

// lgamma wraps math.Lgamma's two-return shape into a single value;
// the sign return is irrelevant for positive arguments (which is the
// only case we hit — Beta parameters are strictly positive).
func lgamma(x float64) float64 {
	g, _ := math.Lgamma(x)
	return g
}

// sampleBeta draws a single Beta(a, b) variate via the ratio of two
// Gamma variates: Beta(a,b) ≡ X/(X+Y), X~Gamma(a,1), Y~Gamma(b,1).
// Gamma sampling uses Marsaglia-Tsang's squeeze method (a > 1) with
// the Boost reduction Gamma(a,1) = Gamma(a+1,1) * U^(1/a) for a < 1.
func sampleBeta(rng *rand.Rand, a, b float64) float64 {
	x := sampleGamma(rng, a)
	y := sampleGamma(rng, b)
	if x+y <= 0 {
		return 0.5
	}
	return x / (x + y)
}

// sampleGamma draws a single Gamma(shape, 1) variate. Marsaglia-Tsang
// for shape >= 1; Boost reduction for shape < 1.
func sampleGamma(rng *rand.Rand, shape float64) float64 {
	if shape < 1 {
		// Boost: Gamma(α) = Gamma(α+1) * U^(1/α).
		g := sampleGamma(rng, shape+1)
		u := rng.Float64()
		if u <= 0 {
			u = 1e-300
		}
		return g * math.Pow(u, 1/shape)
	}
	d := shape - 1.0/3.0
	c := 1.0 / math.Sqrt(9.0*d)
	for {
		var x, v float64
		for {
			x = rng.NormFloat64()
			v = 1 + c*x
			if v > 0 {
				break
			}
		}
		v = v * v * v
		u := rng.Float64()
		if u < 1-0.0331*x*x*x*x {
			return d * v
		}
		if math.Log(u) < 0.5*x*x+d*(1-v+math.Log(v)) {
			return d * v
		}
	}
}
