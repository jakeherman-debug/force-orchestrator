---
audience: agent
scope: Every supply rule that catches ErrTokenExpired must record a SecurityFindings deferral.
owner: D13
last_reviewed: 2026-05-05
title: Pattern P-SupplyDeferral — D5 token-expired non-passthrough invariant
type: pattern-doc
pattern: P-SupplyDeferral
---

# Pattern P-SupplyDeferral — D5 token-expired non-passthrough invariant

## Rationale

Every CodeArtifact registry-call site in
`internal/isb/rules/supply_*.go` that catches an auth-class error
(`errors.Is(err, codeartifact.ErrTokenExpired)`) MUST emit a
`SecurityFindings` deferral row via
`supplydeferral.RecordDeferral` — either directly on the branch, or
via a same-file helper named `recordDeferral` that the branch
invokes. Roadmap reference: docs/roadmap.md § Deliverable 5
"No silent token-expired passthroughs".

A rule that catches the auth error without leaving a row behind is
the worst failure mode: the recovery dog (D5 P4) has nothing to
replay when the operator runs `umt artifacts`.

## What it checks

Two sub-tests, plus inline synthetic fixtures:

1. `TestPattern_PSupplyDeferral_DeferralOnTokenExpired` — file-level
   AST scan of every production `supply_*.go` rule. For each file:
   - `callsCA` — does the file call any
     `codeartifact.Client` method (`DescribePackageVersion`,
     `ListPackages`, `Health`)?
   - `callsTokenExpired` — does it reference
     `codeartifact.ErrTokenExpired`?
   - `callsRecordDeferral` — does it call
     `supplydeferral.RecordDeferral`?
   - If `callsCA && callsTokenExpired && !callsRecordDeferral`, the
     file is a silent-passthrough offender.
   - If `callsCA && !callsTokenExpired`, the auth-error path is
     unhandled — also an offender.
2. `TestPattern_PSupplyDeferral_NoSilentReturnNilOnAuth` — branch-
   level AST scan: every `if errors.Is(err,
   codeartifact.ErrTokenExpired) { ... }` block whose body lacks
   any call to `supplydeferral.RecordDeferral` OR a same-file
   helper method named `recordDeferral` is an offender.
3. **Synthetic fixtures** — both tests parse hand-crafted "bad" and
   "good" snippets to pin the matcher behaviour, so a future refactor
   can't silently weaken the AST walker.

## How it fails

```
Pattern P-SupplyDeferral (D5-P1): N supply rule file(s) silently swallow token-expired errors. Per docs/roadmap.md § D5 anti-cheat "No silent token-expired passthroughs", every auth-error path must call supplydeferral.RecordDeferral:
  internal/isb/rules/supply_002.go — calls CodeArtifact + checks ErrTokenExpired but never calls supplydeferral.RecordDeferral — silent token-expired passthrough

Fix: on the `errors.Is(err, codeartifact.ErrTokenExpired)` branch, call supplydeferral.RecordDeferral(db, taskID, payload) before returning. The recovery dog (D5 P4) replays these rows when the operator runs `umt artifacts`.
```

## How to fix

On the auth-error branch, record a deferral:

```go
_, err := r.client.DescribePackageVersion(ctx, codeartifact.EcosystemPyPI, name, version)
if errors.Is(err, codeartifact.ErrTokenExpired) {
    if defErr := r.recordDeferral(db, taskID, supplydeferral.DeferralPayload{
        Rule:    "SUPPLY-001",
        Package: name,
        Reason:  "token expired during describe-package-version",
    }); defErr != nil {
        log.Printf("record deferral: %v", defErr)
    }
    return nil, nil
}
```

Or call `supplydeferral.RecordDeferral` directly. SUPPLY-001's
canonical shape uses a same-file `recordDeferral` helper method.

## Test reference

- File: `internal/audittools/audit_pattern_p_supply_deferral_test.go`
- Core assertions:
  - `TestPattern_PSupplyDeferral_DeferralOnTokenExpired` (lines 74–190)
  - `TestPattern_PSupplyDeferral_NoSilentReturnNilOnAuth` (lines 209–305)
- Helpers: `pSupplyDeferralScanFile`,
  `pSupplyDeferralScanForSilentReturn`,
  `pSupplyDeferralCondMatchesTokenExpired`,
  `pSupplyDeferralBodyHasDeferralCall`.

## See also

- `internal/isb/supplydeferral/` — the deferral helper.
- `internal/clients/codeartifact/` — the registry client.
