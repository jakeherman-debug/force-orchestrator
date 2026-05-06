---
audience: agent
scope: Per-source value-score histogram skew >70% in any single bucket emits an operator warning.
owner: D13
last_reviewed: 2026-05-05
title: Pattern P24 — Score-distribution monitor
type: pattern-doc
pattern: P24
---

# Pattern P24 — Score-distribution monitor

## Rationale

The value-score distribution from any single proposer source
(Investigator, Captain, EC, ConvoyReview, operator) over a rolling
window must NOT exceed 70% in any single bucket (low / medium / high).
A bimodal-toward-high distribution indicates the proposer LLM is
treating "value=high" as a default. Roadmap reference: D3 § anti-cheat
directive "No proposer score-distribution skew".

The roadmap describes this as a dashboard surface (warning to operator,
not CI-fail). The behavioral test pins the threshold logic so the
dashboard wiring can rely on a known-correct evaluator. P24 graduates
to a BoS commit-time rule when D4 ships, at which point the evaluator
moves to a production package and dashboard / CI surfaces call into
it directly.

## What it checks

`TestPattern_P24_ScoreDistributionMonitor` exercises
`p24EvaluateScoreDistribution` (a pure function defined in the test
file) over five fixtures:

1. **Balanced.** No source exceeds 70% — zero warnings.
2. **High-skewed Captain.** 9/10 high → warning emitted with
   `Source=captain`, `Bucket=high`, `Fraction>=0.85`.
3. **All-low Investigator.** 8/8 low → warning emitted with
   `Bucket=low`.
4. **N<5 floor.** 4/4 high but `N<5` → suppressed (too few data
   points).
5. **At-threshold boundary.** 7/10 high → no warning (strict-greater
   on 0.70 makes the boundary unambiguous).

`p24SkewThreshold = 0.70` is the constant under test.

## How it fails

```
Fixture A (balanced): expected 0 warnings, got 1:
  source=captain bucket=high fraction=0.9000 N=10 threshold=0.70
```

The evaluator emits `p24Warning{Source, Bucket, Fraction, N, Threshold}`
sorted by `(Source, Bucket)`.

## How to fix

Adjust `p24EvaluateScoreDistribution` so the threshold logic matches
the spec:

- Sources with `total < 5` skip (noise floor).
- Strict-greater comparison against `p24SkewThreshold`.
- One warning per (Source, Bucket) pair that crosses the threshold.

When this evaluator graduates to a production package, dashboard /
CI surfaces call into it directly rather than re-implementing.

## Test reference

- File: `internal/audittools/audit_pattern_p24_score_distribution_monitor_test.go`
- Core assertion: `TestPattern_P24_ScoreDistributionMonitor` (lines 110–175)
- Pure function under test: `p24EvaluateScoreDistribution` (lines 77–104)
- Constant: `p24SkewThreshold = 0.70` (line 52).

## See also

- [P22 — Fingerprint determinism](p22-fingerprint-determinism.md)
- [P23 — Proposer write discipline](p23-proposer-write-discipline.md)
