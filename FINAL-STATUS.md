# Final status — audit fix campaign

## Fixes shipped (10 of 11 in the Prioritized Fix Plan)

| Fix | Branch | Merge SHA | AUDIT IDs closed |
|-----|--------|-----------|------------------|
| #0 — Protected-branch guard | `fix-0-protected-branch-guard` | `1cceef6` | 102, 103, 104, 121, 122, 124 |
| #1 — Spend cap + effective e-stop | `fix/spend-cap-and-estop` | `234a7cc` | 004, 020, 060, 061, 065, 105, 106, 107, 152 (+ P5, P11) |
| #4 — Hot-table indexes | `fix/hot-table-indexes` | `28035d8` | 009, 010, 023, 024, 058, 059, 079, 080, 081, 134 |
| #5 — staleConvoys terminal check | `fix/stale-convoys-terminal-check` | `9f27a0d` | 012, 087 |
| #2 — Dashboard hardening | `fix/dashboard-hardening` | `0de93f2` | 001, 002, 003, 053, 054, 064 |
| #6 — Medic-requeue cap | `fix/medic-requeue-cap` | `1630cc2` | 005, 028, 033, 118, 119, 133 |
| #10 — Outbound-channel hardening | `fix/redact-and-outbound` | `78b2585` | 016, 017, 055, 056, 057 (+ P9; also fixed a pre-existing telemetry OTLP race) |
| #9 — Ref/path/URL validators | `fix/ref-path-validators` | `15c391c` | 018, 019, 049, 050, 051, 098, 123, 140, 153, 154 (+ P10) |
| #3 — Idempotency partial UNIQUE | `fix/idempotency-unique` | `2ad0302` | 008, 011 (write-side), 034, 035, 036, 048, 074, 112 |
| #7 — Tighten ConvoyReview | `fix/convoy-review-tightening` | `c94bfd6` | 006, 007, 029, 031, 032, 111, 113, 117, 120, 135, 136, 138, 161, 162 |
| #8a — Self-heal terminator signatures (Phase A) | `fix/error-signatures-phase-a` | `0d49877` | 013, 014, 022, 041 (+ P1) |

**Fix #8.5** (LLM prompt boundary markers + DisallowUnknownFields) was excluded from the initial worktree batch per the operator's explicit list; it remains pending. AUDIT IDs still open on its account: 030, 108, 109, 114, 115, 116, 139.

**Fix #8 Phases B and C** remain scheduled. Phase A closed the three terminator signatures and two one-liners; Phase B is a per-package sweep migrating the ~108 non-hot-path call sites marked with `TODO(Fix #8b):` comments across 18 files; Phase C is the final void-returning store mutator conversion.

## Findings status

- **Criticals (35):** 35 closed. All priority-0 cost-burn and security exposures are addressed. The three silent-failure findings (AUDIT-013, -014, -022) were closed by Phase A; the remaining silent-failure work is test-infrastructure only (assertions that inspect `_, _ =` patterns which Phase B will clean up).
- **Highs (67):** ~50 closed. Open ones are overwhelmingly in Fix #8b/8c (error-return signature migration — non-urgent because the three terminators already return error) and Fix #8.5 (prompt injection — future PR).
- **Mediums (60):** ~30 individually closed; the remainder are pattern-covered by the fixes listed above.
- **Lows (4):** 0 individually closed (all are pattern-covered per the manifest).

The per-finding Closed-by column in `AUDIT-TEST-MANIFEST.md` is authoritative; this file summarises it.

## New test counts by type

Totalled across all 11 merged fixes (excludes the red-phase audit tests, which already existed):

| Type | Count |
|------|-------|
| Unit (including table-driven subcases) | ~90 |
| Integration | ~30 |
| E2E | 6 |
| Acceptance | ~18 |
| Feature | 4 |
| Smoke | 3 |
| Fuzz | 6 (`FuzzValidateRef`, `FuzzValidateRepoPath`, `FuzzValidateRemoteURL`, `FuzzRedactSecrets`, `FuzzIdempotencyKeyNormalization`, `FuzzIdempotencyKey_TerminalAllowsNewInsert`) |

Test LOC added is approximately +4,500 lines across the new `*_test.go` files. The fuzz targets collectively ran ~15 M executions across their 10-second-per-target runs during the fix campaigns with zero crashes found.

## Audit-skip state

`make test-audit` passes. 58 `t.Skip("AUDIT-NNN:` markers remain, all on the allowlist in
`internal/audittools/audittools_test.go` with their follow-up fix (Fix #8b,
Fix #8c, Fix #8.5, or documented scope-deferrals like AUDIT-025/-085/-149).
The allowlist is the enforced ratchet: any new AUDIT-skip added elsewhere
fails `make test-audit`, and removing one from the allowlist requires the
matching `t.Skip` also be removed (or the test explicitly re-added to the
allowlist with a successor fix name). Zero is still the goal.

## Makefile targets added

- `make smoke` — daemon-boot + DB-init + spend-cap + protected-branch-guard smoke path. Runs in ~15 seconds (budget 30). Exercises AssertNotDefaultBranch, SpendCap defaults, `/healthz`, `/api/status.hourly_spend_dollars`.
- `make fuzz` — runs every `Fuzz*` target in `internal/git` and `internal/store` with `-fuzztime=30s`. No crashes on last run.
- `make test-audit` — the Go-test-backed ratchet described above.

## Full-suite status

- `go build -tags sqlite_fts5 ./...` — clean.
- `go test -tags sqlite_fts5 -timeout 600s -count=1 ./...` — green (all 9 packages including `internal/audittools`).
- `make smoke` — green in ~15s.
- `make test-audit` — green (allowlist clean).
- `-race -count=1`: green across every package EXCEPT one remaining race in `cmd/force/testhelpers_test.go::captureOutput` (hot-swaps `os.Stdout` without sync). Pre-existed on main at `1cceef6` before any Fix #10 change; documented in Fix #10's FIX-LOG entry as "out-of-scope race." The other pre-existing race (`TestEmitEvent_WithOTLPEndpoint`) WAS fixed — Fix #10's agent folded the fix into its OTLP migration.

## Merge order actually used

Matches the operator's priority order with a small deviation: the "any order" bucket (Fixes #2, #5, #6, #7, #9, #10) was merged in roughly the order their sub-agents completed — #5, #2, #6, #10, #9, #7 — because Fix #3 had not yet finished. The Fix #3 guard ("must land before any fix adding a new Queue\* helper") was satisfied because no other fix added Queue\* helpers that conflicted; #7's new Queue paths all used existing helpers.

## The five most dangerous patterns I observed that the audit didn't capture

1. **Schema-column-drift between `createSchema` and `runMigrations`.** The CLAUDE.md directive to add columns to both is explicit, but every single fix that added a column (Fix #4, #6, #7) had to manually reconcile. A second defensive layer — a test that compares the union of columns in both paths against the shipped schema.sql — would have caught Fix #6's initial `created_at` drop when it bumped the two other columns. The audit flagged drift in principle (AUDIT-023); it didn't flag that the reconciliation is hand-written per migration and therefore error-prone. A generated schema.sql or a per-column PRAGMA match test would ratchet this down.

2. **`_, _ = return` as a lint smell, not a bug smell.** Phase A's discovery: 108 call sites use `_ = store.FailBounty(...)` or `_, _ = CreateEscalation(...)` now. Each is a silent failure mode in disguise. A custom golangci-lint rule — `no_drop_store_errors` — would flag the pattern at write time rather than at the next audit. Phase B will carry this forward, but the rule itself is a standing defense.

3. **Parallel worktree conflicts on shared-docs files.** Every fix wrote to `AUDIT-TEST-MANIFEST.md`, `FIX-LOG.md`, `CLAUDE.md`, and `schema/schema.sql`. Every merge required 3-4 manual conflict resolutions even though the file changes were additive. The underlying issue is that these three markdowns are line-sequentially-appended rather than structurally-composable. Next round should probably either (a) use an append-only change log with timestamped entries rather than per-section edits, or (b) generate the manifest Closed-by column from commit metadata rather than hand-editing.

4. **Tests that document pre-fix behavior without a post-fix counterpart.** Fix #6's `TestApplyMedicRequeue_AdversarialLLM` asserted "4 Open escalations after 4 cap-breaches." Fix #3's partial UNIQUE collapsed the invariant to "1 Open escalation per task," and the Fix #6 test went red at integration time. The audit catalogued individual test debts (AUDIT-111/113/135/136) but didn't flag the cross-fix invariant collision risk. A post-merge test-fixup pass is evidently mandatory; the test should have asserted `countEscalations==1 AND severity=max_observed` from the start.

5. **Conflict-gate allowlists for in-flight work.** When we attempted to rebase all 9 remaining worktrees against main after Fix #1 merged, 6 conflicted because of Fix #1's cross-cutting `context.Context` threading. Those conflicts were routine but expensive. A dependency DAG constraint — "fixes that touch context.Context or Spawn\* signatures must merge before others" — would have parallelised more of the work. The audit treated the 10 fixes as independent; in practice they are only independent per-package, and Fix #1 was universally shared.

(Bonus sixth: the prior agent's RGR test scaffolding used a custom `extractFuncBody` helper with a latent bug around inline interface types in Go signatures. Fix #0's first run flushed that out, and the fix carried forward. The helper hadn't been exercised against functions with `logger interface{ Printf(...) }` parameters before, so the bug was invisible until the skip was removed.)

## Known out-of-scope items at close

- **`cmd/force/testhelpers_test.go::captureOutput` `os.Stdout` race** — pre-existing, documented in Fix #10.
- **Fix #8.5 (prompt injection)** — 7 AUDIT IDs awaiting a dedicated PR.
- **Fix #8b/c** — 108 call-site migrations across 18 files; scoped as a per-package sweep.
- **AUDIT-025/-085/-149** — each requires a focused fix outside the umbrella of Fixes #0–#10.

## Operator read-path

- `FIX-LOG.md` — narrative per fix.
- `AUDIT-TEST-MANIFEST.md` — per-finding closed-by column.
- `CLAUDE.md` — current invariants (all Fix #0/#1/#2/#3/#6/#7/#9/#10 directives added).
- `internal/audittools/audittools_test.go` — the `remainingAuditSkips` allowlist is the single source of truth for what's still open.
