// Package audittools: Pattern P24 — proposer score distribution
// monitor.
//
// Roadmap reference: D3 § anti-cheat directive "No proposer score-
// distribution skew" (line 1301).
//
// Invariant: the value-score distribution from any single proposer
// source (Investigator, Captain, EC, ConvoyReview, operator) over a
// rolling window MUST NOT exceed 70% in any single bucket
// (low/medium/high). Long-tail flat distributions are healthy;
// bimodal-toward-high indicates the proposer LLM is treating
// "value=high" as a default. The roadmap describes this as a
// dashboard surface (warning to operator, not CI-fail) — but the
// behavioral test here asserts the threshold logic itself, so
// dashboard wiring can rely on a known-correct evaluator.
//
// Slice α of D3 fix-loop-1 authors this test as a BEHAVIORAL test
// over a fake fixture. The test:
//
//  1. Defines `p24EvaluateScoreDistribution`, a pure function that
//     takes a per-source score histogram and returns a slice of
//     "skewed" warnings.
//
//  2. Exercises the evaluator with three fixtures:
//     - balanced (no warning)
//     - high-skewed (warning emitted, source named, bucket named)
//     - all-low (warning emitted)
//
//  3. The evaluator is the source-of-truth implementation —
//     dashboard / CI surfaces should call this function (or an
//     equivalent in slice δ's dashboard package). When slice δ
//     ships its dashboard widget, it imports the evaluator from
//     this test layer's adjacent production helper, NOT from
//     audittools (which is test-only). The contract here is that
//     the THRESHOLD LOGIC is correct; the dashboard wires the
//     query that produces the histogram.
//
// Pattern P24 graduates to a BoS commit-time rule when D4 ships,
// at which point the evaluator moves to a production package and
// the dashboard / CI surfaces call into it directly.
package audittools

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

// p24SkewThreshold is the maximum fraction any single bucket may
// occupy before a warning fires. Roadmap value: 0.70.
const p24SkewThreshold = 0.70

// p24Histogram is a per-source score-distribution snapshot.
//   key: source ("investigator", "captain", "ec", "convoy-review",
//                "operator")
//   value: bucket-keyed counts ("low", "medium", "high")
type p24Histogram map[string]map[string]int

// p24Warning describes one skew-event the evaluator surfaces.
type p24Warning struct {
	Source    string
	Bucket    string
	Fraction  float64
	N         int
	Threshold float64
}

// p24EvaluateScoreDistribution returns a sorted list of warnings
// for any source whose value-score distribution exceeds the skew
// threshold in any single bucket.
//
// Sources with N < 5 are skipped — too few data points to
// distinguish skew from noise. (Roadmap says "recent proposals per
// source" without a fixed minimum; 5 is a defensible floor that
// the dashboard widget can override.)
func p24EvaluateScoreDistribution(h p24Histogram) []p24Warning {
	var warnings []p24Warning
	for source, buckets := range h {
		total := 0
		for _, c := range buckets {
			total += c
		}
		if total < 5 {
			continue
		}
		for bucket, c := range buckets {
			frac := float64(c) / float64(total)
			if frac > p24SkewThreshold {
				warnings = append(warnings, p24Warning{
					Source: source, Bucket: bucket, Fraction: frac,
					N: total, Threshold: p24SkewThreshold,
				})
			}
		}
	}
	sort.Slice(warnings, func(i, j int) bool {
		if warnings[i].Source != warnings[j].Source {
			return warnings[i].Source < warnings[j].Source
		}
		return warnings[i].Bucket < warnings[j].Bucket
	})
	return warnings
}

// TestPattern_P24_ScoreDistributionMonitor exercises the evaluator
// over three fixtures (balanced, high-skewed, all-low). The
// behavioral assertions assert the threshold logic is correct;
// dashboard wiring is downstream.
func TestPattern_P24_ScoreDistributionMonitor(t *testing.T) {
	// Fixture A — balanced. No source exceeds 70% in any bucket.
	balanced := p24Histogram{
		"investigator":   {"low": 4, "medium": 4, "high": 4},
		"captain":        {"low": 3, "medium": 5, "high": 4},
		"convoy-review":  {"low": 5, "medium": 3, "high": 5},
	}
	warnings := p24EvaluateScoreDistribution(balanced)
	if len(warnings) != 0 {
		t.Errorf("Fixture A (balanced): expected 0 warnings, got %d:\n%s",
			len(warnings), formatP24Warnings(warnings))
	}

	// Fixture B — high-skewed Captain. 9/10 high → 90% > 70%, warning.
	highSkewed := p24Histogram{
		"captain":      {"low": 0, "medium": 1, "high": 9},
		"investigator": {"low": 4, "medium": 4, "high": 4}, // balanced control
	}
	warnings = p24EvaluateScoreDistribution(highSkewed)
	if len(warnings) != 1 {
		t.Fatalf("Fixture B (high-skewed Captain): expected 1 warning, got %d:\n%s",
			len(warnings), formatP24Warnings(warnings))
	}
	w := warnings[0]
	if w.Source != "captain" || w.Bucket != "high" {
		t.Errorf("Fixture B: expected (source=captain, bucket=high), got (source=%s, bucket=%s)", w.Source, w.Bucket)
	}
	if w.Fraction < 0.85 {
		t.Errorf("Fixture B: expected fraction >= 0.85 for 9/10, got %.4f", w.Fraction)
	}

	// Fixture C — all-low Investigator. 8/8 low → 100% > 70%.
	allLow := p24Histogram{
		"investigator": {"low": 8, "medium": 0, "high": 0},
	}
	warnings = p24EvaluateScoreDistribution(allLow)
	if len(warnings) != 1 {
		t.Fatalf("Fixture C (all-low Investigator): expected 1 warning, got %d:\n%s",
			len(warnings), formatP24Warnings(warnings))
	}
	if warnings[0].Bucket != "low" {
		t.Errorf("Fixture C: expected bucket=low, got %s", warnings[0].Bucket)
	}

	// Fixture D — under N=5 floor. 4/4 high but total < 5 → no warning.
	tooFew := p24Histogram{
		"investigator": {"low": 0, "medium": 0, "high": 4},
	}
	warnings = p24EvaluateScoreDistribution(tooFew)
	if len(warnings) != 0 {
		t.Errorf("Fixture D (N<5): expected 0 warnings (suppressed by floor), got %d:\n%s",
			len(warnings), formatP24Warnings(warnings))
	}

	// Fixture E — at exactly the threshold. 7/10 high → 70.0% NOT
	// strictly greater than 0.70 → no warning. (Use strict-greater
	// so the boundary is unambiguous.)
	atThreshold := p24Histogram{
		"investigator": {"low": 1, "medium": 2, "high": 7},
	}
	warnings = p24EvaluateScoreDistribution(atThreshold)
	if len(warnings) != 0 {
		t.Errorf("Fixture E (at-threshold 70%%): expected 0 warnings (strict-greater boundary), got %d:\n%s",
			len(warnings), formatP24Warnings(warnings))
	}
}

func formatP24Warnings(ws []p24Warning) string {
	if len(ws) == 0 {
		return "  (no warnings)"
	}
	var b strings.Builder
	for _, w := range ws {
		fmt.Fprintf(&b, "  source=%s bucket=%s fraction=%.4f N=%d threshold=%.2f\n",
			w.Source, w.Bucket, w.Fraction, w.N, w.Threshold)
	}
	return b.String()
}
