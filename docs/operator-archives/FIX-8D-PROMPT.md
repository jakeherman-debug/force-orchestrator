# Fix #8d — Code Red Full Closure Campaign

## Mission

The Code Red fix campaign (Fixes #0 through #10 + Campaigns 1–4) closed the $300 cost-burn vector and the headline security classes. Independent verification (`FIX-VERIFICATION.md`) confirmed the primary mission: daemon-restart is safe. But the verifier found that **29 AUDIT IDs were marked "pattern-covered" in the allowlist without actually being closed**, plus **Pattern P7 was effectively downgraded by skip-labels referencing a function (`UpdateBountyStatusFrom`) that was never written**, plus **Fix #8b declared "complete" with 8 bare hot-path terminator calls and ~28 unmarked `_ = store.*` discards in production**.

The operator will NOT restart the daemon until every one of these is **genuinely closed** — not relabeled, not re-annotated, not moved to a new allowlist bucket. When this campaign finishes, `make test-audit` must pass against an **empty allowlist** (`remainingAuditSkips = map[string]string{}`), and `grep -rn 't\.Skip(.AUDIT-' --include="*.go"` across the module must return **zero hits** outside `.fix-worktrees/` / `.force-worktrees/` / `vendor/`.

Every other Code Red fix (#0, #1, #2, #3, #4, #5, #6, #7, #8a, #8c, #8.5, #9, #10, Campaign 2) is **ACCEPTED** and must not be regressed. This campaign adds to them; it does not modify them.

## Required reading before starting

Read these in full before writing any code. Do not skim.

1. `/Users/jake.herman/code/force-orchestrator/CLAUDE.md` — especially the "No silent failures" / Fix #8 Phase A invariants, and the rule about `_ = store.*` requiring `// deferral-comment(Fix #8b): propagate error` markers.
2. `/Users/jake.herman/code/force-orchestrator/FIX-VERIFICATION.md` — the entire report. This is the authority on what's open and what's closed.
3. `/Users/jake.herman/code/force-orchestrator/AUDIT.md` — find each AUDIT ID listed in the acceptance criteria below. The AUDIT entry is the spec for what "closed" means.
4. `/Users/jake.herman/code/force-orchestrator/FIX-LOG.md` — prior campaign history; learn the commit shape and PR style used.
5. `/Users/jake.herman/code/force-orchestrator/internal/audittools/audittools_test.go` — the `remainingAuditSkips` allowlist. Every entry in this map is a work item for this campaign.

## Exit criteria (all must hold before you report completion)

Each is mechanically verifiable. If any one fails, the campaign is not done.

1. **Zero AUDIT skip markers.** `grep -rn 't\.Skip(.AUDIT-' --include="*.go" internal/ cmd/ schema/` must return 0 hits.
2. **Empty allowlist.** `remainingAuditSkips` in `internal/audittools/audittools_test.go` must be `map[string]string{}`. The `TestNoAuditSkipMarkersRemain` test must pass against the empty map.
3. **`UpdateBountyStatusFrom` exists and is the guard mechanism.** `grep -rn "func UpdateBountyStatusFrom" internal/store/` must return a definition. `ResetTaskFull` and `CancelTask` must route through it. The signature is `UpdateBountyStatusFrom(db *sql.DB, id int, from, to string) (rowsAffected int64, err error)` — conditional UPDATE with `WHERE id = ? AND status = ?`. Zero rows affected means the caller's assumption about current status was wrong; caller handles that (typically by logging and returning without side-effects).
4. **P7 pattern test runs clean.** Both `TestPattern_P7_ConcurrentCancelVsApproveRace` and `TestPattern_P7_ResetTaskResurrectsCompleted` must have their `t.Skip(...)` lines **deleted** (not commented, not replaced), and must pass under `-race -count=5`.
5. **Zero bare terminator calls in hot-path files.** For every line in `internal/agents/{astromech,captain,medic,medic_ci,jedi_council,diplomat,pilot,pilot_worktree_reset,convoy_review}.go` that calls `store.FailBounty(...)` or `store.UpdateBountyStatus(...)`, the call must be one of: (a) `if err := store.X(...); err != nil { ... }` with meaningful recovery or log-with-hint; (b) bound to a variable and checked; (c) `_ = store.X(...)` with `// deferral-comment(Fix #8b): propagate error` on the same line or the line directly above, per CLAUDE.md. There is no (d) "bare call."
6. **Zero unmarked `_ = store.*` discards in production code.** `grep -rn '_ = store\.' --include="*.go" internal/ cmd/` must only return matches that have the `// deferral-comment(Fix #8b):` marker within 1 line before OR on the same line. Test files are exempt.
7. **`rows.Scan` error-checking sweep.** Every `rows.Scan(...)` call in production code (excluding `_test.go`) must have its error checked. AUDIT-090/091/094/095/100 are the audit anchors; the real scope is whatever grep finds.
8. **`exec.Command` → `exec.CommandContext` migration.** Every long-running `exec.Command(...)` in production code that can reasonably be cancelled via context must be migrated. Short shell-outs (e.g., `git rev-parse HEAD`, taking <1s) may stay as `exec.Command` if the caller does not hold a context. Each migration must pass the context through. AUDIT-127/158/165 are the anchors; the verifier counted ~100 candidate sites, most of which will take the migration; document any that legitimately do not.
9. **`cmd/force/testhelpers_test.go::captureOutput` race closed.** Replace the `os.Stdout` hot-swap pattern with a thread-safe mechanism. Test-infra only, but the operator's mandate is zero-known-issues. Options: per-test pipe injected via a function parameter, or a `sync.Mutex` around the swap with all callers contending. Pick whichever yields the smaller blast radius; the fix must not regress any existing test.
10. **Chancellor SEQUENCE/MERGE empty-subfield path is fail-closed.** `internal/agents/chancellor.go`: when the ruling's MERGE sub-decisions have an empty `Task` string or missing required subfield, route to `store.FailBounty` + `[CHANCELLOR FAIL-CLOSED]` operator mail, not `approveProposal(..., chancellorRuling{}, ...)`. Add a dedicated test that supplies a decision with one valid and one empty-subfield arm and asserts the escalation + no approval.
11. **Full suite green under race.** `go test -tags sqlite_fts5 -race -count=5 ./...` must pass with no flakes. The cmd/force race (item 9) must be resolved for this to be possible.
12. **Pattern tests green under race-count=5.** `TestPattern_P{1,2,3,4,6,7,8,9,10,11,12}` all pass under `-race -count=5`. No t.Skip in any of them.
13. **Allowlist cleanup in the same commits that close each AUDIT ID.** When you close AUDIT-NNN, the commit that closes it must ALSO remove the `t.Skip("AUDIT-NNN: ...")` line AND remove the entry from `remainingAuditSkips`. No "allowlist sweep commit" at the end — the allowlist change rides with the fix.
14. **CLAUDE.md updated to reflect new invariants** (item 3's `UpdateBountyStatusFrom` becomes a CLAUDE.md invariant; item 7's "rows.Scan must check error" may belong there depending on scope).

## Work tracks

The 39 AUDIT skips + 4 non-AUDIT items below cluster into independent tracks. Each track is a candidate for an isolated `.fix-worktrees/fix-8d-<track>/` working directory so you can run them in parallel. Merge-order is specified per track.

### Track A — State-transition guard (P7 closure)

**Covers:** AUDIT-026, 027, 072, 156, 159. Pattern P7.

**Scope:**
- `internal/store/tasks.go`: add `func UpdateBountyStatusFrom(db *sql.DB, id int, from, to string) (rowsAffected int64, err error)` using conditional UPDATE with `WHERE id = ? AND status = ?`.
- `ResetTaskFull` (`tasks.go`): route through `UpdateBountyStatusFrom(id, "Completed", "Pending")` AND all other terminal→non-terminal transitions. Zero rows affected → log "task N already in non-terminal status Y; refusing to reset" and return without mutating anything else. Preserve all existing counter-preservation behavior (retry_count, infra_failures — Fix #6).
- `CancelTask` (`tasks.go`): route through `UpdateBountyStatusFrom(id, expectedStatus, "Cancelled")` where `expectedStatus` is the status at operator-request time (pass as parameter or read-then-CAS).
- Unskip `TestPattern_P7_ConcurrentCancelVsApproveRace` and `TestPattern_P7_ResetTaskResurrectsCompleted` by **deleting** the `t.Skip` lines.
- Both tests must pass under `-race -count=5`. If the test as written needs adjustment for the new API, adjust the test — but only if the adjustment **tightens** the contract, never loosens it.
- Remove AUDIT-026, 027, 072, 156, 159 from `remainingAuditSkips`.
- Update CLAUDE.md "No silent failures" section with a new invariant: "State transitions that depend on the prior status MUST use `UpdateBountyStatusFrom(id, from, to)`. Blind `UpdateBountyStatus` is only acceptable when the caller genuinely does not care about prior status (e.g., force-to-Failed from an infrastructure error). Pattern P7 test enforces."

**Red-phase evidence to preserve:** Before deleting the skips, run the tests WITHOUT the skip to confirm the 20/20 clobber and ResetTask-resurrects failures reproduce on main. Then implement the fix. Then re-run; both must now pass.

**Merge first.** This blocks Track B + Track E.

### Track B — Bare hot-path terminator sweep (Fix #8b tail)

**Covers:** AUDIT-015, 040, 042, 043, 046, 047, 068, 069, 125, 126, 129, 151, 155, 164. The 8 bare hot-path terminator calls + the ~28 unmarked `_ = store.*` discards.

**Scope:**
- For every bare call in `internal/agents/pilot_worktree_reset.go` (lines 78, 83, 91, 99, 111, 129): migrate to `if err := store.FailBounty(...); err != nil { logger.Printf("worktree-reset #%d: FailBounty failed (%v); stale-lock detector will recover", bounty.ID, err); return }`. Same pattern for the `UpdateBountyStatus` call. The log message must name the recovery mechanism per CLAUDE.md.
- `internal/agents/medic_ci.go:170` and `internal/agents/astromech.go:601`: same treatment.
- Run `grep -rn '_ = store\.' --include="*.go" internal/ cmd/` and audit every match. For each production match (not `_test.go`):
  - If the error is genuinely unrecoverable and the call site is on a path where a higher level will clean up: add `// deferral-comment(Fix #8b): propagate error — <specific recovery mechanism>` on the line above.
  - If the error should actually be handled: migrate to `if err := ... ; err != nil { ... }`.
  - **"It's fine because we've never seen it fail" is not a valid justification. The comment must name a concrete recovery path.**
- Run `grep -rn 'store\.FailBounty\|store\.UpdateBountyStatus' --include="*.go" internal/agents/` and audit every match for bare-call form. Each bare call must move to a guarded form per CLAUDE.md.
- Remove AUDIT-015, 040, 042, 043, 046, 047, 068, 069, 125, 126, 129, 151, 155, 164 from `remainingAuditSkips` as each is closed. Drop the `t.Skip` line from the corresponding test in the same commit.

**Verification grep at exit:**
```
grep -rnE 'store\.(FailBounty|UpdateBountyStatus)\(' --include="*.go" internal/ cmd/ \
  | grep -v '_test.go' \
  | grep -vE '(^[[:space:]]*if err)|(:= store\.)|(// deferral-comment)|(err :=)'
```
Must return 0 hits.

**Merges after Track A** (to pick up `UpdateBountyStatusFrom` where it's the right mechanism).

### Track C — `rows.Scan` error sweep

**Covers:** AUDIT-090, 091, 094, 095, 100.

**Scope:**
- `grep -rnP 'rows\.Scan\(' --include="*.go" internal/ cmd/` in production files.
- For each call, check the error. Typical pattern:
  ```go
  if err := rows.Scan(&a, &b); err != nil {
      logger.Printf("<contextual-descriptor>: scan failed: %v", err)
      continue  // or break, depending on loop semantics
  }
  ```
- Some sites may tolerate partial failure (a sweep dog that keeps going on a bad row); other sites must abort the whole query. Use judgment; document non-obvious cases with a comment.
- Add a pattern test `TestPattern_P1_RowsScanErrorsChecked` in `internal/audittools/` that walks production code and fails if any `rows.Scan(...)` call is not error-checked. Pattern test is your anti-regression.
- Remove AUDIT-090, 091, 094, 095, 100 from `remainingAuditSkips`; drop skips.

**Parallelizable with other tracks.**

### Track D — `exec.CommandContext` migration

**Covers:** AUDIT-127, 158, 165.

**Scope:**
- `grep -rnE '\bexec\.Command\b' --include="*.go" internal/ cmd/` in production.
- For each call, decide: does the caller have a `context.Context` in scope? If yes, migrate to `exec.CommandContext(ctx, ...)`. If no, check whether one SHOULD be passed in (i.e., is this on a path that has a daemon-shutdown obligation?). Usually yes for long-running git operations, no for synchronous lookups that complete in milliseconds.
- When migrating, add the ctx parameter to the caller's signature if needed. Threading ctx through is part of this track's scope; don't short-circuit by pretending the local function doesn't need it.
- Every migrated call site must pass a context that the caller's e-stop path can cancel (Fix #1 invariant).
- Add a pattern test `TestPattern_P11_ExecCommandsUseContext` that lists every `exec.Command(...)` (non-Context form) and fails if it appears outside a documented allowlist of short-running commands. Allowlist lives in the test; entries need a reason.
- Remove AUDIT-127, 158, 165 from `remainingAuditSkips`; drop skips.

**Parallelizable with other tracks.**

### Track E — Store-layer concurrency + transactions batch

**Covers:** AUDIT-045, 066, 069, 092, 093, 096, 097.

**Scope:** (read the AUDIT entries for exact defects; summary below)
- AUDIT-069: `ResolveFeatureBlockers` multi-write without transaction → wrap in `db.Begin() / tx.Commit()`.
- AUDIT-092, 093, 096, 097: various concurrency-batch issues in store-layer writes → apply either transaction wrapping or `UpdateBountyStatusFrom`-style source-status guarding per the AUDIT entry.
- AUDIT-066: `PruneFleet` unparameterised interval → add a parameter + wire config.
- AUDIT-045: concurrency finding → apply appropriate fix per AUDIT entry.
- Tests: each AUDIT ID should have at least one dedicated test that reproduces the race or defect under `-race -count=5` before the fix and passes after.
- Remove from `remainingAuditSkips`; drop skips in the same commits.

**Depends on Track A** (for `UpdateBountyStatusFrom`).

### Track F — Test-quality + pattern-covered residuals

**Covers:** AUDIT-099, 137.

**Scope:**
- AUDIT-099: outbound redaction test-quality finding → strengthen test per the AUDIT entry.
- AUDIT-137: test-quality finding → strengthen per the AUDIT entry.
- Remove from `remainingAuditSkips`; drop skips.

**Parallelizable.**

### Track G — cmd/force testhelpers race

**Covers:** the pre-existing `captureOutput` race under `-race -count=1` on `TestRunCommandCenter_WithTasks`.

**Scope:**
- `cmd/force/testhelpers_test.go`: replace the global `os.Stdout` hot-swap pattern. Recommended approach: accept an `io.Writer` as a function parameter (`captureOutput(f func(w io.Writer)) string`), and update every caller to pass the writer explicitly. Restoration of `os.Stdout` no longer happens because nothing is swapped.
- Run `go test -tags sqlite_fts5 -race -count=5 ./cmd/force/...` to confirm the race is gone.
- This is test-infrastructure only; does not affect production binary.

**Parallelizable.**

### Track H — Chancellor SEQUENCE/MERGE empty-subfield fail-closed

**Covers:** The Fix #8.5 "accepted caveat" called out in FIX-VERIFICATION.md. No AUDIT ID currently but file one (AUDIT-Fix8d-H) and add it to the tracking.

**Scope:**
- `internal/agents/chancellor.go`: when `ruling.Kind == "MERGE"` or `"SEQUENCE"` and any sub-element is missing a required subfield (e.g., empty `Task`, empty `Rationale` when required), route to `store.FailBounty` + `[CHANCELLOR FAIL-CLOSED]` operator mail with a specific reason ("merge subfield `Task` empty at index N"). Do not call `approveProposal(..., chancellorRuling{}, ...)`.
- Add `TestChancellor_MERGE_EmptySubfield_FailsClosed` with a ruling containing one valid and one empty-subfield arm; assert escalation, zero approval, and operator-mail dispatch.
- Update CLAUDE.md "Chancellor fail-closed on Claude/parse error" section with the stronger invariant.

**Parallelizable.**

### Track I — Schema + time residuals

**Covers:** AUDIT-130, 131, 132 (if not already closed by Fix #8c — verify first by re-reading AUDIT entries).

**Scope:** Per AUDIT entries. May already be closed; if so, drop from allowlist and skip-line only.

**Parallelizable.**

## Anti-cheat directives

Every cheat pattern the verifier found in the prior campaign is explicitly forbidden here. This is non-negotiable.

1. **No allowlist relabeling.** You may not move an AUDIT ID from one allowlist reason to another. If an ID is on the allowlist at the end of this campaign, the campaign is not done. The allowlist must end empty.
2. **No ghost functions.** If a skip message says "remove when `FooBar()` exists," the closure commit must define `FooBar()`, have it referenced by production code, and delete the skip. Not define a function with a similar name, not add a comment saying `FooBar` "conceptually exists" via some other mechanism.
3. **No pattern-test downgrade.** If a pattern test has `t.Skip` in any subtest, that subtest must be unskipped by the closure. The pattern test's body must execute. You may not add a second pattern test that exercises a narrower version of the same class and declare the broader one "covered."
4. **No "pattern-covered" annotations.** The string `pattern-covered` must not appear in the allowlist, in skip messages, or in test comments anywhere in the tree at the end of this campaign. `grep -rn 'pattern-covered' --include="*.go"` must return 0 hits.
5. **No softened assertions in red-to-green transitions.** If you reference a red-phase test's failure message to justify that the defect was real, the same assertion must be present in the green-phase test. The green test asserts the positive contract; the red test's failure is evidence, not a baseline to relax.
6. **No `_ = store.*` without the exact comment marker.** The marker is `// deferral-comment(Fix #8b): propagate error — <mechanism>`. Not a variant, not a paraphrase. The mechanism names the concrete recovery path (e.g., "stale-lock detector recovers within 120s"). "fleet tolerates" is not a mechanism.
7. **No reduced `-count`.** Every race-enabled test added in this campaign must pass at `-race -count=5`, not `-count=1`. Flakes count as failures.
8. **No "scoped out" for in-scope AUDIT IDs.** Every ID on the allowlist is in scope for this campaign. If you believe an ID is genuinely out-of-scope — e.g., the AUDIT entry names a future feature that doesn't exist yet — surface it in your report with evidence; the operator decides.
9. **No fix lands without a test.** Every AUDIT closure requires: (a) a test whose body exercises the post-fix contract, and (b) that test being added or un-skipped in the same commit as the fix.
10. **No skipping items the verifier flagged.** The 8 bare-terminator sites and the ~28 `_ = store.*` sites are specific; your report must account for all 8 + all 28 (or however many the grep finds). "We fixed most of them" is a fail.

## RGR discipline

For every AUDIT closure:

1. **Red:** Confirm the test (or a new test you write) fails before the fix. Capture the failure message in the commit message body or in `FIX-LOG.md` under a dated section for Fix #8d.
2. **Green:** Implement the fix. Confirm the test passes at `-race -count=5`.
3. **Refactor:** If the fix introduces duplication or awkward patterns, clean it up. Tests must still pass after refactor.

The red-phase evidence is the proof that the defect was real. Do not skip this step. If a test was already failing on main before your changes (i.e., it was one of the existing `t.Skip`'d tests), running it without the skip IS the red-phase — capture that output.

## Worktree isolation

Each track may run in its own `.fix-worktrees/fix-8d-<track>/` checkout. Track A (state-transition guard) must merge first because Tracks B and E depend on `UpdateBountyStatusFrom`. Within B and E, file-level conflict possibility exists; merge serially in the order presented. Tracks C, D, F, G, H, I are independent and may merge in any order after A.

Follow the same worktree discipline from the prior campaign:
- One track per worktree directory.
- Rebase-on-merge, no fast-forward via merge commits without squash.
- Commit message style per `FIX-LOG.md`.
- Never use `--no-verify`.

## Verification procedure (what to run before declaring done)

Run in order. Each step must pass before the next.

1. `grep -rn 't\.Skip(.AUDIT-' --include="*.go" internal/ cmd/ schema/` → **0 hits**
2. `grep -rn 'pattern-covered' --include="*.go"` → **0 hits**
3. Inspect `internal/audittools/audittools_test.go` → `remainingAuditSkips` is `map[string]string{}`
4. `grep -rnE '_ = store\.' --include="*.go" internal/ cmd/ | grep -v '_test.go'` → every match has the deferral comment on same or prior line
5. `grep -rnE 'store\.(FailBounty|UpdateBountyStatus)\(' --include="*.go" internal/agents/ | grep -v '_test.go' | grep -vE '(if err)|(:= store\.)|(_ = store\.)'` → **0 hits**
6. `grep -rn "func UpdateBountyStatusFrom" internal/store/` → **1 hit** (the definition)
7. `go test -tags sqlite_fts5 ./...` → **all green**
8. `go test -tags sqlite_fts5 -race -count=5 ./...` → **all green, no flakes**
9. `go test -tags sqlite_fts5 -run TestPattern_P7 -race -count=5 ./internal/store/...` → **both subtests run and pass**
10. `go test -tags sqlite_fts5 -run TestPattern_ -race -count=5 ./...` → **P1–P12 (minus P5) green**
11. `make smoke` → **green**
12. `make fuzz` → **green** (all fuzz targets run their budget)
13. `make test-audit` → **green** against empty allowlist

## Deliverables

When you report completion, produce `FIX-8D-CLOSURE.md` at the repo root with:

1. **Per-AUDIT closure table.** One row per ID. Columns: AUDIT ID, commit SHA that closed it, test(s) that now pass (file:line), grep-verifiable evidence of closure (pattern + expected match count).
2. **Per-track summary.** Track letter, scope, files touched, lines changed, tests added, tests unskipped.
3. **Verification output.** Paste the output of steps 1–13 above verbatim. If any step fails, the campaign is not done and the report must not be filed.
4. **Residual list.** If any AUDIT ID genuinely cannot be closed in this campaign (e.g., it turned out to describe a future feature), list it with operator-actionable evidence: the AUDIT entry, what the fix would require, and why it can't happen now. Operator decides whether to remove the ID from the audit tracking or defer to a future campaign.
5. **Anti-cheat self-check.** Re-read the 10 anti-cheat directives and affirm, for each, that you did not perform it. If you cannot affirm honestly, flag the item.
6. **Updated CLAUDE.md.** Note what invariants were added (state-transition guard, rows.Scan check, exec.CommandContext migration, Chancellor SEQUENCE fail-closed).

## Scope sweep (in addition to named items)

While working, if you encounter **any other** code smell, incomplete fix, known-unknown, funky test, or dubious assertion that relates to the Code Red mission but isn't explicitly named above, surface it in the FIX-8D-CLOSURE.md residual list. The operator wants zero-known-issues before restart. Do not fix it unilaterally (scope creep); do surface it for operator decision.

Examples of what "surface it" means:
- A test that passes but whose assertion could plausibly be meaningless (tautological assertion, unreachable branch).
- A production function that looks like it should be error-checking but isn't, even if the verifier didn't name it.
- A CLAUDE.md invariant that code appears to contradict.
- A dog that fires on an interval that feels wrong for what it does.

Each surfaced item gets: file:line, brief description, proposed action (fix in this campaign / defer to next / benign). Operator decides.

## Restart gate

The operator will restart the daemon ONLY when:

- All 13 verification steps pass.
- The allowlist is empty.
- The FIX-8D-CLOSURE.md report is filed and reviewed.
- Zero CONDITIONAL items remain from the prior FIX-VERIFICATION.md report.
- No new CONDITIONAL items were introduced.

Anything short of that, the daemon stays down. Do not cut corners to get there faster — the cost of a false "done" is worse than the cost of three more days of honest work.

Good luck.
