---
audience: agent
scope: Proposer code paths only INSERT (or dedup ON CONFLICT) — no archive/suppression mutations.
owner: D13
last_reviewed: 2026-05-05
title: Pattern P23 — Proposer write discipline
type: pattern-doc
pattern: P23
---

# Pattern P23 — Proposer write discipline

## Rationale

Proposer code paths (Investigator, Captain mid-cycle amendment,
Engineering Corps experiment wrap, ConvoyReview cross-classification,
manual operator filing) only INSERT rows or use the dedup ON CONFLICT
path. Direct writes to `archived_at`, `archive_reason`, or any column
on `ProposedFeatureSuppressions` from a proposer are forbidden. Only
operator-routed handlers and the housekeeping dog may write archive
state.

Roadmap reference: D3 § anti-cheat directive "No proposer mutation of
archive/suppression state". P23 graduates to a BoS commit-time rule
when D4 ships.

## What it checks

`TestPattern_P23_ProposerWriteDiscipline` walks the files in
`p23ProposerFiles`:

- `internal/agents/investigator.go`
- `internal/agents/captain.go`
- `internal/agents/convoy_review.go`
- `internal/agents/engineering_corps/experiment_author.go`
- `internal/agents/engineering_corps/metric_author.go`
- `internal/agents/engineering_corps/promotion_author.go`

For every string literal in each file:

1. **Archive-write prong.** `p23ArchiveWriteRe` matches `UPDATE
   ProposedFeatures` or `INSERT INTO ProposedFeatures` whose body
   touches `archived_at` / `archive_reason`. Any hit is an offender.
2. **Suppression-write prong.** `p23SuppressionWriteRe` matches
   `INSERT INTO ProposedFeatureSuppressions`,
   `UPDATE ProposedFeatureSuppressions`, or `DELETE FROM
   ProposedFeatureSuppressions`. Any hit is an offender.

Files in `p23ProposerFiles` that don't yet exist (slice β/δ
not-yet-shipped) are skipped with a log line.

## How it fails

```
Pattern P23 (D3 anti-cheat): N proposer-file write(s) violate the write-discipline contract:
  internal/agents/captain.go:42 — archive-state write (archived_at / archive_reason) from a proposer file
      preview: UPDATE ProposedFeatures SET archived_at = ? WHERE ...
...
Fix: proposers only INSERT (or dedup ON CONFLICT). Archive state writes (archived_at, archive_reason) belong to the operator dashboard handler OR the proposed-features-housekeeping dog. Suppression writes are operator-only.
```

## How to fix

Move the archive-state mutation into the housekeeping dog
(`internal/agents/dogs_proposed_features_housekeeping.go`) or the
operator dashboard handler. Move suppression writes behind the
operator endpoint.

If a proposer is renamed or split, update the `p23ProposerFiles` map
in the same commit so the new file inherits the discipline.

## Test reference

- File: `internal/audittools/audit_pattern_p23_proposer_write_discipline_test.go`
- Core assertion: `TestPattern_P23_ProposerWriteDiscipline` (lines 89–172)
- Regexes: `p23ArchiveWriteRe`, `p23SuppressionWriteRe` (lines 77, 82).

## See also

- [P22 — Fingerprint determinism](p22-fingerprint-determinism.md)
- [P24 — Score-distribution monitor](p24-score-distribution-monitor.md)
- [P34 — Senate no self-promote](p34-senate-no-self-promote.md)
