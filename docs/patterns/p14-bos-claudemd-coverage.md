---
audience: agent
scope: Every CLAUDE.md invariant has at least one BoS rule (or an allowlisted process-only exemption).
owner: D13
last_reviewed: 2026-05-05
title: Pattern P14 — BoS rules cover CLAUDE.md invariants
type: pattern-doc
pattern: P14
---

# Pattern P14 — BoS rules cover CLAUDE.md invariants

## Rationale

CLAUDE.md is the documented contract; without enforcement, the
documentation drifts from the code (Domain 25 in the audit).
Bureau of Standards (BoS) rules are the AST checks that close the
loop: every invariant in CLAUDE.md becomes at least one rule, OR
appears in an allowlist with a truthful reason explaining why
enforcement is process-only (e.g. commit hooks, CI, code review).
Pattern P14 is the deferred D3 slot graduated in D4 Phase 1.

## What it checks

Three sub-tests:

1. `TestPattern_P14_BoSRulesCoverCLAUDEMDInvariants` — extracts every
   `**Title.**` bold-prefix invariant label and every `## H2` heading
   from `CLAUDE.md`. Each title must either match a BoS rule's
   `CLAUDEMDAnchor()` (substring match, decoration-stripped, case-
   insensitive via `normaliseInvariantText`) or appear in
   `p14AllowlistedInvariants` with a reason.
2. `TestPattern_P14_AllowlistReasonsTruthful` — every allowlist value
   must be ≥30 chars and not start with `TODO` / contain `fixme`.
3. `TestPattern_P14_AllowlistedTitlesExist` — every allowlist key
   must be a real CLAUDE.md title (catches typos that would silently
   exempt nothing).

## How it fails

```
Pattern P14: N CLAUDE.md invariant(s) not covered by any BoS rule and not allowlisted:
  - "Some New Invariant Title" (add a BoS rule with CLAUDEMDAnchor() containing this title, or add to p14AllowlistedInvariants with a truthful reason)
```

Or for the truthfulness test:

```
allowlist entry "Foo" has trivial reason (<30 chars): "todo"
```

## How to fix

Either author a BoS rule whose `CLAUDEMDAnchor()` contains the title,
or add the title to `p14AllowlistedInvariants` with a ≥30-char
rationale naming the actual enforcement surface (CI test, pre-commit
hook, code review). Adding a rule is the strongly-preferred path for
anything with an AST footprint.

## Test reference

- File: `internal/audittools/audit_pattern_p14_bos_claudemd_coverage_test.go`
- Core assertions:
  - `TestPattern_P14_BoSRulesCoverCLAUDEMDInvariants` (lines 138–174)
  - `TestPattern_P14_AllowlistReasonsTruthful` (lines 179–190)
  - `TestPattern_P14_AllowlistedTitlesExist` (lines 196–208)
- Helpers: `extractInvariantTitles`, `anchorMatches`,
  `normaliseInvariantText`.

## See also

- `internal/bos/rules/` — every BoS rule registers via the
  `init()` hook in this package.
- [P18 — Render coherence](p18-render-coherence.md)
