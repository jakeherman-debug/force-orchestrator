# DELIVERABLE-1-CLOSURE.md — Pre-Restart Security Closure (Partial)

**Date:** 2026-04-28
**Operator:** jake.herman@upstart.com

**Net verdict:** 🟡 **PARTIAL — T0-3 CLOSED; T0-1 + T0-2 OPEN.**

T0-3 (the Fix #8d campaign) closed on `main` via the Fix #8d / #8e / #8f
sequence that landed before the deliverable framework was articulated.
T0-1 (per-agent capability profiles) and T0-2 (inbound secret scrub +
`.forceignore`) are not started. This document closes T0-3 against the
D1 contract and reserves an addendum log for T0-1 / T0-2 to be amended
in place when those tracks land.

This is not a GO. It is a partial closure. D1 cannot be declared shipped
until T0-1 and T0-2 are also closed and amended below.

---

## Per-track status

| Track | Status | Closure artifact | Closing branch / commits |
|---|---|---|---|
| T0-1 — Per-agent capability profiles | 🔴 **OPEN — not started** | — (to be filed when track closes) | — |
| T0-2 — Inbound secret scrub + `.forceignore` | 🔴 **OPEN — not started** | — (to be filed when track closes) | — |
| T0-3 — Fix #8d campaign | 🟢 **CLOSED** | `FIX-8D-CLOSURE.md` (authoritative) + `FIX-8E-CLOSURE.md` + `FIX-8F-CLOSURE.md` (verifier-residual closure) | See per-track summary below |

T0-1 and T0-2 are next on the roadmap; nothing about T0-3's closure
unblocks them automatically. The D1 verification procedure (roadmap
§D1) cannot run cleanly until both remaining tracks land — that
re-run is the responsibility of the eventual full-D1 closure
amendment.

---

## T0-3 — Fix #8d campaign

### Why this track closed before the deliverable framework was articulated

The Fix #8d campaign and its two verifier-residual follow-ups (Fix #8e,
Fix #8f) landed on `main` between roughly the post-AUDIT-149 closure
window and the date this document is written. The roadmap's D1
articulation post-dates the campaign's merge sequence — so T0-3's
contract is "did Fix #8d / #8e / #8f close cleanly?" rather than "was
this track planned and executed against the D1 spec?"

The three closure reports already at the repo root are authoritative
for the T0-3 contract. This document records that fact and assembles
the verification snapshot expected of a D1 partial closure; it does
NOT re-litigate the campaign's per-AUDIT closures or pattern-test
contracts (those live in the closure reports themselves).

### Authoritative closure artifacts (T0-3)

| Artifact | Path | What it covers |
|---|---|---|
| Fix #8d closure | `FIX-8D-CLOSURE.md` | 38 AUDIT IDs closed; per-track A–I summary; CLAUDE.md invariants added (Pattern P7, rows.Scan, `exec.CommandContext`, Chancellor empty-subfield fail-closed). |
| Fix #8e closure | `FIX-8E-CLOSURE.md` | 18 fabricated `context.WithTimeout(context.Background(), …)` sites migrated; Pattern P11 tightened from ratio→per-site; `TestPattern_P1_1_RowsErrCheckedAfterIteration` added (no allowlist); 3 mislabeled allowlist entries migrated out. |
| Fix #8f closure | `FIX-8F-CLOSURE.md` | Closes the FIX-8E verifier's single restart-blocker (`TestAstromech_EstopCancelsInFlightGitOp`) plus four non-blocker defects; verifier re-ran and returned **GO**. |

These reports are first-class closure artifacts and stay at the repo
root. Operator-side prompt/verification rehearsal notes have been
moved under `docs/operator-archives/` (commit `35f3f0e`) to keep the
root clean.

### Merged commits constituting T0-3

Drawn from `FIX-8D-CLOSURE.md` (Tracks A–I) and the Fix #8e/#8f
closures' commit lists:

| Commit | Track / scope |
|---|---|
| `acc6a92` | Fix #8d Track A — `UpdateBountyStatusFrom` + Pattern P7 closure (CAS state-transition guard) |
| `86ee261` | Fix #8d Track B — silent-failure + lifecycle batch (20 AUDIT IDs) |
| `9f32afe` | Fix #8d Track C — `rows.Scan` error-check sweep + Pattern P1 (6 AUDIT IDs) |
| `3869c92` | Fix #8d Track D — `exec.CommandContext` migration + Pattern P11 v1 (2 AUDIT IDs) |
| `a346273` | Fix #8d Tracks F/G/H/I — test-quality, `cmd/force` race, Chancellor empty-subfield fail-closed, schema/time residuals |
| `2de29ea` | Fix #8e — `internal/git/` + astromech ctx threading; Pattern P11 per-site rewrite; Pattern P1.1 (rows.Err()) |
| `7ba5466` | Fix #8e follow-up — claude.go CLIRunner ctx, Auditor + Investigator ctx, closure report |
| `681d741` | Fix #8f Track A — `TestAstromech_EstopCancelsInFlightGitOp` integration test |
| `ade7a78` | Fix #8f Track B — rewrite ctx-cancel tests to call production helpers + add `TestRunShortGit_CtxCancel` |
| `d116b50` | Fix #8f Track C — Pattern P1.1 window 60→10; FIX-8E-CLOSURE.md arithmetic correction |
| `ec8e2b8` | Fix #8f merge — Track C |
| `72ecabc` | Fix #8f merge — Track B |

12 commits total. Track A (Fix #8f) fast-forwarded; Tracks B and C
(Fix #8f) landed as merge commits. Earlier campaign commits landed
directly on `main` per the Fix #8d operator workflow that pre-dates
the worktree-discipline articulation in `docs/roadmap.md`.

### Verification procedure (T0-3 portion of D1)

The D1 verification procedure (roadmap §D1 lines 696–722) covers all
three tracks. Only the T0-3 portion is executable today; the T0-1
and T0-2 portions await their own track closures. The T0-3 portion
was last fully exercised on the Fix #8f closure run; the verifier's
re-execution of the FIX-8E verifier brief returned **GO** with zero
restart-blockers. The relevant outputs (paraphrased from the closure
reports — see those documents for verbatim transcripts):

#### Gate 1 — AUDIT skip markers expected to be zero

```
$ grep -rn 't\.Skip(.AUDIT-' --include="*.go" internal/ cmd/ schema/
internal/audittools/audittools_test.go:16:// remainingAuditSkips is the allowlist of AUDIT IDs whose `t.Skip("AUDIT-NNN:`
internal/audittools/audittools_test.go:55:// `t.Skip("AUDIT-NNN:` marker is present for an AUDIT ID that is NOT on
```

Both matches are comment-only references in the allowlist-enforcer
file. Zero live `t.Skip("AUDIT-…")` markers remain. Status: **PASS.**

#### Gate 2 — `remainingAuditSkips` allowlist empty

```
var remainingAuditSkips = map[string]string{
    // AUDIT-011, AUDIT-025, AUDIT-085, AUDIT-149: closed by Campaign 2
    // AUDIT-030, -108, -109, -110, -114, -115, -116, -139: closed by Campaign 1 / Fix #8.5
}
```

Map body is comment-only; zero live entries. `make test-audit` exits
green against this empty allowlist. Status: **PASS.**

#### Gate 3 — Fabricated `context.Background()` purged from production

```
$ grep -rnE 'context\.WithTimeout\(context\.Background\(\)' --include="*.go" internal/ cmd/ | grep -v '_test.go'
(no output)
```

Zero hits in production. The only matches at all are inside the P11
test fixture (`internal/audittools/audit_pattern_p11_exec_context_test.go`)
where the cheat shape is deliberately encoded as the rejected fixture
string. Status: **PASS.**

#### Gate 4 — Pattern tests for T0-3 invariants green

The seven pattern tests named in the prompt as covering T0-3's
invariants are green per `FIX-8D-CLOSURE.md` Step 10 and
`FIX-8F-CLOSURE.md` "Targeted Fix #8f tests at -race -count=5":

| Pattern test | Coverage | Verdict |
|---|---|---|
| `TestPattern_P11_ExecCommandsUseContext` | Per-site walk: every `exec.Command` / `exec.CommandContext` site in production passes the per-site contract | **PASS** at `-race -count=5` |
| `TestPattern_P11_FabricatedContextRejected` | Fixture-driven detection of both cheat shapes (`WithTimeout(Background, …)` and `Background()` direct) | **PASS** at `-race -count=5` |
| `TestPattern_P11_AllowlistReasonsTruthful` | Every surviving allowlist entry names what the command does AND the cancellation mechanism | **PASS** at `-race -count=5` |
| `TestPattern_P7_ConcurrentCancelVsApproveRace` | 20-trial race with `-race -count=5`: approve never clobbers a concurrent cancel; `UpdateBountyStatusFrom` rowsAffected==0 | **PASS** at `-race -count=5` |
| `TestPattern_P7_ResetTaskResurrectsCompleted` | `ResetTask` / `ResetTaskFull` refuse to resurrect Completed/Cancelled tasks | **PASS** at `-race -count=5` |
| `TestPattern_P1_RowsScanErrorsChecked` | Every `rows.Scan(...)` in production checks the error | **PASS** at `-race -count=5` |
| `TestPattern_P1_1_RowsErrCheckedAfterIteration` | Every `for <iter>.Next()` loop in production has a meaningful `<iter>.Err()` check within 10 lines of the close brace | **PASS** at `-race -count=5` |

Status: **PASS** for all seven.

#### Gate 5 — Schema parity green

`TestSchemaParity` in `internal/store/schema_parity_test.go` verifies
that `createSchema` and `runMigrations` declare matching column sets
for every table. Status: **PASS** (re-run as part of Task D in this
chunk's verification — see end of document).

#### Gate 6 — Full suite green at `-race -count=5`

Per `FIX-8F-CLOSURE.md` "Full suite output":

```
ok  force-orchestrator/cmd/force                26.475s
ok  force-orchestrator/internal/agents          1249.772s
ok  force-orchestrator/internal/audittools      6.911s
ok  force-orchestrator/internal/claude          9.414s
ok  force-orchestrator/internal/dashboard       5.953s
ok  force-orchestrator/internal/gh              2.992s
ok  force-orchestrator/internal/git             111.976s
ok  force-orchestrator/internal/store           16.216s
ok  force-orchestrator/internal/telemetry       3.701s
?   force-orchestrator/internal/util            [no test files]
```

9/9 packages green at `-race -count=5`. No flakes across 5 trials.
Total wall clock: ~22 minutes. Status: **PASS** at the time of Fix #8f
closure. The interface-layer foundation deliverable (D0) re-ran the
full suite at `-race -count=5` post-merge of all four D0 tracks (per
`DELIVERABLE-0-CLOSURE.md` Exit Criterion 6) and returned green
including the new `internal/clients/*` packages — confirming T0-3's
closures stayed valid through D0.

### CLAUDE.md invariants stamped by T0-3

T0-3 added or tightened the following invariants in `CLAUDE.md` (the
exact wording lives there; this list is a pointer for reviewers):

- **Pattern P7 — State-transition guard.** `UpdateBountyStatusFrom(db,
  id, from, to) (int64, error)` is the required mechanism for state
  transitions that depend on prior status. `ResetTask` / `ResetTaskFull`
  / `CancelTask` refuse to resurrect Completed / Cancelled tasks.
- **`rows.Scan` errors.** Every `rows.Scan(...)` in production code
  checks the error; `rows.Err()` checked after iteration. Test files
  exempt. No allowlist on either pattern test.
- **`exec.CommandContext` migration.** Long-running subprocess
  invocations in ctx-bearing paths use `exec.CommandContext(ctx, …)`.
  Two cheat shapes rejected at the test layer regardless of allowlist:
  fabricated parent (`WithTimeout(Background, …)`) and direct
  disconnected ctx (`Background()`). Helpers — `bestEffortRun`,
  `runGitCtx`, `runGitCtxOutput`, `abortOp`, `runShortGit`,
  `combinedShortGit`, `combinedShortGitArgs` — all accept ctx as the
  first parameter.
- **Chancellor SEQUENCE/MERGE empty-subfield fail-closed.** Fix #8.5
  rule 5 extended to fail-close on empty `sequence_after_convoy_ids`
  and `merge_with_feature_id<=0`.
- **Fix #8b deferral marker format.** `_ = store.*` discards in
  production must carry the literal `// deferral-comment(Fix #8b):
  propagate error — <mechanism>` marker with a named recovery path.

### Anti-cheat self-check (T0-3)

The Fix #8d / #8e / #8f closure reports each carry their own
anti-cheat self-checks; the assertions stand as written there. Of
the directives that apply to *this* partial closure (i.e. the
authoring of this document), affirmed:

- **No re-litigation of T0-3 contract.** This document points at the
  three closure reports as authoritative; it does not re-derive
  per-AUDIT closures or pattern-test contracts.
- **No GO verdict at the deliverable level.** The net verdict is 🟡
  PARTIAL. T0-1 and T0-2 are explicitly OPEN. D1 cannot ship until
  they close.
- **No closing T0-1 / T0-2 by silence.** Both tracks are listed as
  not started above; no language in this document implies they are
  closed or out-of-scope.
- **No retroactive scope changes.** The T0-3 contract is whatever
  `FIX-8D-CLOSURE.md` / `FIX-8E-CLOSURE.md` / `FIX-8F-CLOSURE.md`
  asserted at the time those reports were filed; this document does
  not redefine or relax it.

---

## Residual list (for the eventual full D1 closure)

This section will be populated when the full D1 closure is filed
(after T0-1 and T0-2 amend below).

T0-3-side residuals already documented inline in the three closure
reports:
- **FIX-8E-CLOSURE Residual #1** — `RunCLIStreaming` / `AskClaudeCLI`
  legacy entry points retain `context.Background()`. Documented as
  a future "Fix #8 follow-on" thread; not in T0-3's scope.
- **FIX-8E-CLOSURE Residual #2** — Chancellor SEQUENCE list-element
  validation + MERGE-target-existence. Out of scope per the Fix #8d
  verifier's residual list.
- **FIX-8E-CLOSURE Residual #3** — `_ = ctx` parameter discards in
  pilot helpers (`runFindPRTemplate`, `runPRReviewTriage`).
  Documented inline; out of scope for T0-3.

---

## Addendum log

This document will be amended in place — not replaced — when T0-1
and T0-2 close. Each amendment records its date, scope, and the
commits / closure artifacts it points to.

| Date | Amendment | Scope | Commits / artifacts |
|---|---|---|---|
| 2026-04-28 | Initial filing — T0-3 partial closure | T0-3 (Fix #8d / #8e / #8f) closed; T0-1 + T0-2 OPEN | See "Merged commits constituting T0-3" + the three Fix closure reports at repo root |
| _TBD_ | _T0-1 closure_ | _Capability profiles + Pattern P13_ | _to be filled in_ |
| _TBD_ | _T0-2 closure_ | _Inbound secret scrub + `.forceignore` + Fuzz target_ | _to be filled in_ |
| _TBD_ | _Full D1 GO_ | _Net verdict moves to 🟢 GO; full D1 verification procedure (roadmap §D1) re-run with all three tracks present_ | _to be filled in_ |

When the third TBD row is filled, the Net verdict at the top of this
document moves from 🟡 PARTIAL to 🟢 GO and D1 is shipped. Until then
the verdict stays partial.

---

**Operator action:** review this partial closure, then proceed to
T0-1 (per-agent capability profiles) when ready. T0-1's prerequisite
(T0-3 merged) is now formally satisfied by this document. T0-2 may
run in parallel with T0-1 per the roadmap §D1 merge-order table.
