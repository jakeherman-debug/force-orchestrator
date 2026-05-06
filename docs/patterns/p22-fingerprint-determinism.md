---
audience: agent
scope: ProposedFeatures fingerprint helper produces byte-equal hashes for identical canonical inputs.
owner: D13
last_reviewed: 2026-05-05
title: Pattern P22 — Fingerprint determinism
type: pattern-doc
pattern: P22
---

# Pattern P22 — Fingerprint determinism

## Rationale

The canonical fingerprint helper for ProposedFeatures must produce
byte-equal hashes for byte-equal canonical inputs. The canonical input
shape is:

```
sha256(canonical(source, topic, sorted_code_paths,
                 sorted_at_refs, sorted_fleetrule_refs))
```

Fields explicitly EXCLUDED from the canonical input — timestamps, run
IDs, session IDs, random salts, occurrence_count, monotonic counters —
are drift-detectors. Inclusion of any such field would break the
dedup-on-conflict path documented at the schema layer
(`idx_pf_active_fingerprint` partial UNIQUE).

Roadmap reference: D3 § "Investigator expansion + ProposedFeatures
queue management" anti-cheat directive "No non-deterministic
ProposedFeatures fingerprints". Slice α scaffolded; slice β shipped
`store.Fingerprint`; slice ζ wired the helper into the audit hook.

P22 graduates to a BoS commit-time rule when D4 ships.

## What it checks

`TestPattern_P22_FingerprintDeterminism` exercises the production
helper through `p22Helper` (wired by the sibling
`audit_pattern_p22_helper_wiring_test.go`'s `init`):

1. **Determinism.** Same canonical input twice → byte-equal output.
2. **Sort idempotence.** A shuffled-input variant (different order in
   `CodePaths`, `ATRefs`, `FleetRuleRefs`) MUST produce the same
   digest as the sorted form — the canonical-input builder must sort
   before hashing.
3. **Sensitivity.** A different `Topic` MUST produce a different
   digest (proves the helper isn't constant).

A nil `p22Helper` is a hard fail — it means the wiring file was
deleted. The prior "scaffold pending" early return was removed in D3
fix-loop iter 2 (slice ζ).

The wiring file (`audit_pattern_p22_helper_wiring_test.go`) is a
separate file by design: deleting either side of the contract surfaces
as a build error before the determinism check can silently regress.

## How it fails

```
Pattern P22: fingerprint helper is non-deterministic — two identical inputs produced different digests:
  first:  abc123...
  second: def456...
```

Or for the sort idempotence prong:

```
Pattern P22: fingerprint helper is order-sensitive — sorted-vs-shuffled inputs produced different digests
Fix: canonical-input builder MUST sort code_paths / at_refs / fleetrule_refs before hashing.
```

## How to fix

Ensure `store.Fingerprint`:

- sorts `code_paths`, `at_refs`, `fleetrule_refs` deterministically
  before hashing,
- includes only the canonical fields (no timestamps, run IDs, salts,
  counters),
- runs through SHA-256 (or any deterministic digest) and returns a
  stable encoding.

The current production shape is at
`internal/store/proposed_features.go:142`.

## Test reference

- Files:
  - `internal/audittools/audit_pattern_p22_fingerprint_determinism_test.go`
    (the audit)
  - `internal/audittools/audit_pattern_p22_helper_wiring_test.go`
    (the production-helper hook)
- Core assertion: `TestPattern_P22_FingerprintDeterminism` (lines 106–154)
- Wiring `init()` (helper_wiring_test.go lines 28–38).

## See also

- `internal/store/proposed_features.go::Fingerprint` (line 142).
- [P23 — Proposer write discipline](p23-proposer-write-discipline.md)
- [P24 — Score-distribution monitor](p24-score-distribution-monitor.md)
