---
audience: agent
scope: Astromechs cannot claim Pending-stage tasks; every Claim* SQL includes the stage_id gate.
owner: D13
last_reviewed: 2026-05-05
title: Pattern P-StageGate — D5.5 staged-convoy gate enforcement
type: pattern-doc
pattern: P-StageGate
---

# Pattern P-StageGate — D5.5 staged-convoy gate enforcement

## Rationale

Astromechs cannot hold a worktree on a `Pending` stage. Every
dispatch-time SELECT against BountyBoard must include a stage gate
that excludes Pending-stage rows. Roadmap reference: D5.5
§ Deliverable 5.5 anti-cheat directive "No astromech pre-staging".

The skeleton landed in D5.5 P1. P2 γ (Wave 2 slice γ) graduated to
the full structural enforcer captured here.

## What it checks

Three sub-tests:

1. `TestPattern_PStageGate_PackageWiringPresent` — verifies the
   `internal/stagegate/` package exists with the load-bearing files
   (`gate.go`, `soak_minutes.go`, `operator_confirm.go`,
   `null_gate.go`, `compound.go`, `baseline.go`), the required
   exports inside `gate.go` (interface, registry, ErrPending,
   MaxNestingDepth), the leaf gate `Type()` strings, the
   `RegisterBaselineGates` wires all five gates, and the
   `convoy-stage-watch` dog is registered in `dogs.go`.
2. `TestPattern_PStageGate_ClaimBountyHasStageFilter` — AST-walks
   `internal/store/tasks.go`, finds every top-level FuncDecl whose
   name starts with `Claim`, and for each function whose body issues
   a Pending-status SELECT against BountyBoard, asserts the SQL
   string contains `stage_id IS NULL`. Functions whose SELECT
   targets non-Pending rows (`ClaimForReview`,
   `ClaimForCaptainReview`) are out of scope.
3. `TestPattern_PStageGate_NoUngatedClaimSQL` — AST-walks every
   `internal/` and `cmd/` `*.go` file (non-test) for backtick SQL
   literals. A file is "claim-shaped" only if it contains BOTH:
   - a Pending-status SELECT against BountyBoard with `LIMIT 1`, AND
   - an `UPDATE BountyBoard SET status = 'Locked'` literal.
   Each Pending SELECT lacking `stage_id IS NULL` is an offender.
   `stageGateBypassAllowlist` (empty by default) carries any
   maintenance-sweep exemption.
4. `TestPattern_PStageGate_ClaimPathSurfaceProbed` — ensures
   `store.ClaimBounty` still exists; if it moves, retarget the
   per-function audit.

## How it fails

```
Pattern P-StageGate: N ungated claim-shaped SQL site(s):
  internal/store/tasks.go:142 — Pending-status SELECT against BountyBoard (LIMIT 1) lacks the stage_id gate (stage_id IS NULL). Astromechs would be able to claim Pending-stage tasks.

Fix: add the stage gate predicate `(stage_id IS NULL OR EXISTS (SELECT 1 FROM ConvoyStages cs WHERE cs.id = BountyBoard.stage_id AND cs.status != 'Pending'))` to the WHERE clause, OR add the file to stageGateBypassAllowlist with a justification (only for non-dispatch maintenance queries).
```

## How to fix

Add the stage gate predicate to the WHERE clause:

```sql
SELECT id, ...
FROM BountyBoard
WHERE status = 'Pending'
  AND (stage_id IS NULL
       OR EXISTS (
         SELECT 1 FROM ConvoyStages cs
         WHERE cs.id = BountyBoard.stage_id
           AND cs.status != 'Pending'))
ORDER BY ...
LIMIT 1
```

If the SQL is structurally a maintenance sweep (not dispatch), add
the file to `stageGateBypassAllowlist` with a justification.

## Test reference

- File: `internal/audittools/audit_pattern_p_stage_gate_test.go`
- Core assertions:
  - `TestPattern_PStageGate_PackageWiringPresent` (lines 95–168)
  - `TestPattern_PStageGate_ClaimBountyHasStageFilter` (lines 180–269)
  - `TestPattern_PStageGate_NoUngatedClaimSQL` (lines 295–429)
  - `TestPattern_PStageGate_ClaimPathSurfaceProbed` (lines 435–471)
- Constants: `claimFunctionPrefix`, `stageGateRequiredFragment`
  (lines 80, 89).

## See also

- [P-StagingPromotionConfirm](p-staging-promotion-confirm.md)
- `internal/stagegate/` — gate registry and leaf gates.
- `internal/agents/dogs_convoy_stage_watch.go`
