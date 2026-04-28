# Fix Verification Report

## Verdict: **CONDITIONAL-GO**

The fix campaign genuinely closed the load-bearing cost-burn and security classes that caused the
$300 incident: Fix #0 (protected-branch guard), Fix #1 (spend cap + effective e-stop), Fix #2
(dashboard hardening), Fix #3 (idempotency partial UNIQUE), Fix #4 (hot-table indexes), Fix #7
(ConvoyReview tightening), Fix #8.5 (LLM prompt injection defense), Fix #9 (shell-ref/URL
validators), and Fix #10 (outbound-channel redaction) all verify with real production-level closure
— not test-level special-casing. The +161 new Test functions (9,737 test LOC added, 1,532 removed;
~6:1 ratio) are substantive and the fuzz targets actually fuzz (1M+ execs/target, coverage growth
observed). Pattern tests P1, P2, P3, P4, P6, P8, P9, P10, P11, P12 all pass under `-race -count=5`
with no cheating.

However, two fixes cheated their own contract in observable ways that the operator must gate on:

1. **Fix #8b Phase B is incomplete.** FINAL-STATUS claims "Phase B (complete) migrated all 108
   non-hot-path call sites." Forensic scan finds **8 unchecked bare-call store.FailBounty /
   store.UpdateBountyStatus sites in CLAUDE.md-named hot-path files** (6 in
   `internal/agents/pilot_worktree_reset.go`, 1 at `internal/agents/medic_ci.go:170`, 1 at
   `internal/agents/astromech.go:601`), plus **~28 `_ = store.*` discards in production without the
   required `// deferral-comment(Fix #8b): propagate error` comment marker** that CLAUDE.md mandates. These are direct
   violations of the Fix #8a invariant that the allowlist claims are closed. Severity: MEDIUM
   (silent failures in narrow terminal paths; stale-lock detector recovers, so no cost burn
   enabled — but CLAUDE.md contract is broken).

2. **Pattern P7 (concurrent cancel-vs-approve + ResetTask resurrect-Completed) is not closed.**
   The allowlist at `internal/audittools/audittools_test.go:44-45, 55` annotates AUDIT-026, 027,
   072 as *"Fix #8 (ResetTask resurrect)"* and *"Fix #8b pattern-covered (P7)"*. In fact
   `UpdateBountyStatusFrom(id, from, to)` — the variant both skips reference as the fix mechanism —
   **does not exist anywhere in the code**. `grep -rn "UpdateBountyStatusFrom"` returns only the
   test-skip messages and an allowlist comment. `internal/store/audit_pattern_p7_test.go` skips at
   lines 31 and 174 so the pattern test "passes" only because it never runs its body. The
   underlying races (CancelTask clobbered by Council approval 20/20 trials; ResetTask re-queuing a
   Completed task on operator-retry-while-dashboard-stale) are still reproducible. Severity:
   MEDIUM (data integrity, not cost burn — retry loops are independently bounded by Fix #6
   medic_requeue cap and Fix #3 idempotency, so the race cannot compound).

Additional residuals (CONDITIONAL items only — not blockers):

3. **`cmd/force/testhelpers_test.go::captureOutput` race under `-race -count=1`.** Pre-existing
   at the audit baseline commit fd67620, documented by FINAL-STATUS as "out-of-scope." The race
   is in test infra (hot-swap of `os.Stdout`), not production. No fix agent introduced it.

4. **Several allowlist "pattern-covered" claims are overstated.** AUDIT-090/091/094/095
   (production `rows.Scan` errors silently discarded) are annotated *"Fix #8 pattern-covered
   (P1)"*, but P1 only covered the three terminator signatures; `rows.Scan` returns remain
   unchecked at 30+ production sites. AUDIT-127/158/165 (`exec.CommandContext` migration) are
   annotated pattern-covered but only 1 of 100+ `exec.Command` calls was migrated. These are
   known-unknown silent-failure vectors, not cost-burn-enabling.

### Operator gate items before restart

The restart is **safe for the primary goal** (preventing another $300 cost burn). The operator
should accept the following known-unknowns:

- **G1**: `Cancel` may be clobbered under concurrent Council approval (AUDIT-027/072). Do not
  depend on Cancel as a hard stop; for a hard stop use the e-stop (Fix #1 guarantees it works).
- **G2**: Operator `retry` on a stale dashboard row whose task just Completed may resurrect it
  (AUDIT-026). Bounded by Fix #3 idempotency + Fix #6 Medic cap, so it won't loop, but may cause
  one duplicate run.
- **G3**: If an uncommon edge case triggers one of the 8 bare-terminator call sites in
  `pilot_worktree_reset.go` / `medic_ci.go:170` / `astromech.go:601`, the task stays Locked and is
  recovered by the stale-lock detector — not by the terminator.
- **G4**: The cmd/force test-helper race is test-only; does not affect daemon runtime.

A follow-up `Fix #8d` sweep should: (a) restore error-checks at the 8 bare-terminator hot-path
sites, (b) add `UpdateBountyStatusFrom(id, from, to)` and route ResetTask + CancelTask through it
to close P7, (c) reclassify AUDIT-090/091/094/095/127/158/165 in the allowlist with concrete
follow-up fix references rather than false "pattern-covered" annotations.

---

## Independent suite results

| Check | Result |
|---|---|
| `go test -tags sqlite_fts5 ./...` (no race) | **PASS** — all 9 packages green (agents 263.8s, store 5.3s, git 23.5s, dashboard 3.2s, telemetry 3.5s, claude 4.4s, gh 2.1s, audittools 1.2s, cmd/force 7.5s) |
| `go test -tags sqlite_fts5 -race -count=1` | **PASS** for 8 of 9 packages; **FAIL** for `cmd/force` on `TestRunCommandCenter_WithTasks` due to pre-existing `captureOutput` race (documented in FINAL-STATUS; confirmed present at fd67620 audit baseline) |
| Pattern tests `TestPattern_P{1,2,3,4,6,7,8,9,10,11,12}` under `-race -count=5` | **PASS** for all 11; P7 passes only because both subtests `t.Skip` — test body does not execute |
| AUDIT skip markers surviving in *.go | **39** (all on `remainingAuditSkips` allowlist in `internal/audittools/audittools_test.go`) — violates GO-requirement "Zero AUDIT- skip markers surviving" |
| `make smoke` | **PASS** in 3.5s (budget 30s) |
| `make fuzz` spot-check (6 targets × 3s each) | **PASS** — 42k–1M execs/target/3s, coverage growth observed, zero crashes |
| `make test-audit` | **PASS** (allowlist ratchet green) |
| New test function delta (`git diff 4872598..HEAD -- '*_test.go' \| grep -cE '^\+func Test'`) | **+161** (target ≥ +60; comfortably exceeds) |
| Test-LOC delta | +9,737 / −1,532 across 79 files (6:1 ratio — healthy) |
| Coverage (packages touched by fixes) | store 61.1%, agents 65.2%, git 67.2%, dashboard 45.9% |

---

## Per-fix verification

### Fix #0 — Protected-branch guard (merge `1cceef6`)

| Check | Result |
|---|---|
| AUDIT IDs claimed closed | 102, 103, 104, 121, 122, 124 |
| AUDIT IDs verified closed | All 6 PASS under `-race -count=5` |
| Skip markers in scope | Zero |
| Red-phase test body integrity | **Strengthened**: AUDIT-102 expanded from single `"main"` case to `{"main","master","MAIN","refs/heads/main","origin/master"}` loop + post-reject `SELECT COUNT(*)==0` assertion; helper `extractFuncBody` improved |
| Class closure | All five destructive ops guarded: `ForcePushBranch` (askbranch.go:315), `TriggerCIRerun` (askbranch.go:349), `DeleteAskBranch` (askbranch.go:113), `MergeAndCleanup` (git.go:396), `completeAskBranchResolution` (pr_flow.go:170). Plus store ingress `UpsertConvoyAskBranch` rejects protected names at DB-write time |
| Out-of-scope changes | None — 12 files all related |
| CLAUDE.md consistency | Matches — "Protected-branch guard (Fix #0)" section lists exactly the five ops + store ingress |
| New test quality | 5/5 avg (`fix0_addrepo_protected_test.go`, `protected_test.go` both use real git init/commit) |
| **Verdict** | **ACCEPTED** |

### Fix #1 — Spend cap + effective e-stop (merge `234a7cc`)

| Check | Result |
|---|---|
| AUDIT IDs claimed closed | 004, 020, 060, 061, 065, 105, 106, 107, 152 (+ P5, P11) |
| AUDIT IDs verified closed | All 9 + both patterns PASS under `-race -count=5` (3s wall budget on AUDIT-107 honoured in ~3.01s) |
| Spawn-loop compliance (CLAUDE.md Rule 1) | 11/11 claim loops call BOTH `IsEstopped` AND `SpendCapExceeded`: astromech, captain, medic, council, diplomat, commander, pilot, chancellor, investigator, auditor, librarian. Static test `TestSpendCapExceeded_GuardsAgentClaimLoops` enumerates and enforces. Inquisitor is a dog runner not a claim loop — correctly excluded |
| time.Sleep audit (CLAUDE.md Rule 2) | Long rate-limit backoff (`astromech.go:530`, up to 10 min) wrapped with `SleepUnlessEstopped`. Short 1–10s inter-claim jitters left as raw `time.Sleep` — correct (shorter than e-stop poll). One minor gap: `util.go:214 handleInfraFailure` uses raw Sleep up to 60s cap — doesn't spawn new work, doesn't re-open burn vector |
| Heartbeat ctx-cancel (CLAUDE.md Rule 3) | `astromech.go:446-469` dedicated `estopTicker` calls `cancelClaude()` on e-stop; mechanism test `TestHeartbeatCancelsClaudeOnEstop` verifies <500ms cancellation |
| Dog honours e-stop | `dogs.go:95-98` first-statement check; `dogSpendBurnWatch` registered first in `dogOrder` |
| Defaults ($25 soft / $200 hard) | Correct: `DefaultHourlySpendCapUSD=25`, `DefaultHourlySpendEstopUSD=200` (`spend_cap.go:43,56`) |
| Red-phase integrity | Strengthened (AUDIT-107 now exercises `SleepUnlessEstopped` and checks `interrupted` bool) |
| Skip markers in scope | Zero in spend_cap_test.go and audit_pattern_p11_test.go |
| Out-of-scope changes | None; 31-file diff all on cost/estop scope |
| CLAUDE.md consistency | Three load-bearing rules match code |
| Test quality | 5/5 avg across 4 new tests (real DB, no mocks) |
| **Verdict** | **ACCEPTED** |

### Fix #2 — Dashboard hardening (merge `0de93f2`)

| Check | Result |
|---|---|
| AUDIT IDs claimed closed | 001, 002, 003, 053, 054, 064 |
| AUDIT IDs verified closed | All 6 PASS under `-race -count=5` |
| Loopback bind | `loopbackBindAddr(port)` returns `"127.0.0.1:PORT"` (`security.go:44`), called at `dashboard.go:66` |
| Origin/Referer allowlist | `securityMiddleware` wraps every mutating method; 403 on foreign Origin |
| 256 KB body cap | `http.MaxBytesReader(w, r.Body, 256<<10)` on every mutation; 413 translation present |
| CSP + security headers | `setSecurityHeaders` writes CSP, X-CTO nosniff, X-Frame-Options DENY, Referrer-Policy no-referrer; belt-and-suspenders `<meta http-equiv>` in index.html |
| innerHTML audit | `marked.parse` absent from app.js; two residual bare `innerHTML` lines contain only constant strings or `escHtml`-sanitized values |
| High-escalation banner | `#high-esc-banner` present (index.html:70); threshold enforced at `app.js:190` via `< 3 / >= 3` |
| Red-phase integrity | Strengthened (AUDIT-003 check expanded to 3 CDN hosts) |
| Skip markers in scope | Zero |
| Out-of-scope | None; all 13 files dashboard-scoped |
| Test quality | 5/5 (`security_test.go` 409 LOC uses real `httptest.Server`, exercises 403+413+CSP on 403s) |
| **Verdict** | **ACCEPTED** |

### Fix #3 — Partial UNIQUE idempotency (merge `2ad0302`)

| Check | Result |
|---|---|
| AUDIT IDs claimed closed | 008, 011 (write-side), 034, 035, 036, 048, 074, 112 (+ P2) |
| AUDIT IDs verified closed | All PASS under `-race -count=5` (25 iterations total, no flake) |
| Partial UNIQUE indexes | All three present in createSchema AND runMigrations: `idx_bounty_idem` on `BountyBoard(idempotency_key) WHERE idempotency_key != '' AND status NOT IN ('Completed','Cancelled','Failed')`, `idx_escalations_open_task` on `Escalations(task_id) WHERE status = 'Open'`, `idx_feature_blockers_open` on `FeatureBlockers(blocked_convoy_id, blocking_feature_id) WHERE resolved_at IS NULL` |
| `ON CONFLICT WHERE DO NOTHING RETURNING` pattern | Present at `tasks.go:488-491` (`addTaskIdempotent`) and `tasks.go:526-529` (`AddIdempotentTaskTx`); predicates match index predicates literally |
| Canonical keys | All 9 canonical keys in CLAUDE.md verified present (rebase-conflict/ask-branch, convoy-review, worktree-reset, rebase-agent, create-askbranch, rebase-askbranch, pr-review-triage, ci-failure-triage) |
| ReadInboxForAgent rewrite | Single-statement `UPDATE ... WHERE id IN (SELECT ...) RETURNING ...` at `fleet_mail.go:89-100` |
| Fuzz | `FuzzIdempotencyKeyNormalization` 147k execs clean; `FuzzIdempotencyKey_TerminalAllowsNewInsert` 642k execs clean |
| Schema parity | `TestSchemaParity` PASSES |
| Skip markers in scope | Zero |
| Out-of-scope | None |
| Test quality | 9/10 avg (50-goroutine start-gate race + fuzz w/ homoglyph/whitespace seeds) |
| **Verdict** | **ACCEPTED** |

### Fix #4 — Hot-table indexes (merge `28035d8`)

| Check | Result |
|---|---|
| AUDIT IDs claimed closed | 009, 010, 023, 024, 058, 059, 079, 080, 081, 134 (+ P4) |
| AUDIT IDs verified closed | All 10 + P4 PASS under `-race -count=5` |
| Indexes added | ~20 indexes across BountyBoard, TaskHistory, Fleet_Mail, Escalations, AuditLog, FleetMemory, AskBranchPRs, ConvoyAskBranches, PRReviewComments. Each appears in BOTH createSchema and runMigrations |
| EXPLAIN QUERY PLAN coverage | Real — `TestHotTableIndexes_ClaimQueryUsesIndex_10kRows` at 10k rows asserts `USING INDEX`/`USING COVERING INDEX`; composite `(task_id, id DESC)` validated for escalation-sweeper GROUP BY |
| Schema parity | `TestSchemaParity` PASSES |
| Skip markers in scope | Zero (one `-short` skip is legitimate) |
| Out-of-scope | None; 9 files all DB-layer |
| Test quality | 9/10 (`hot_table_indexes_test.go` 497 LOC — PRAGMA + EXPLAIN + FK cascade + on-disk idempotence) |
| **Verdict** | **ACCEPTED** |

### Fix #5 — staleConvoys Failed/Escalated terminal check (merge `9f27a0d`)

| Check | Result |
|---|---|
| AUDIT IDs claimed closed | 012, 087 |
| AUDIT IDs verified closed | Both PASS under `-race -count=5`; full P6 outer + all 3 sub-tests green |
| Source-status guard | `runStaleConvoysReport` at `dogs.go:521-637` uses `WHERE status='Active'` on the query and every mutating UPDATE (lines 529, 596, 617). Split path: `problemCount > 0` → Failed + operator mail; all-Completed/Cancelled children → auto-complete |
| Idempotent mail | `SELECT COUNT(*) FROM Fleet_Mail WHERE subject=? AND read_at=''` gate at line 601 |
| Skip markers in scope | Zero (outer P6 skip removed; sub-tests B/C rewritten as active regression guards) |
| Out-of-scope | None (4 files) |
| Test quality | 5/5 (`TestStaleConvoysReport_FullLoopFromPendingToFailedDoesNotShipConvoy` asserts status + mail + no ShipConvoy spawned + idempotence on re-run) |
| **Verdict** | **ACCEPTED** |

### Fix #6 — Medic-requeue cap + preserve retry counters (merge `1630cc2`)

| Check | Result |
|---|---|
| AUDIT IDs claimed closed | 005, 028, 033, 118, 119, 133 |
| AUDIT IDs verified closed | All 6 PASS under `-race -count=5` |
| `maxMedicRequeues = 2` | Present at `medic.go:325`; cap check before reset/increment (line 333) |
| `medic_requeue_count` column | In createSchema, runMigrations ALTER, and schema.sql |
| `ResetTaskFull` preserves counters | `tasks.go:362-369` UPDATE deliberately does NOT reset `retry_count` or `infra_failures` (verified by `TestResetTaskFull_PreservesRetryCount`) |
| `autoShardIfNoCommits` both paths | Timeout (retry_count from InfraFailures≥2) AND non-error zero-commits (retryCount≥2) |
| `maxReshardGeneration = 2` | `util.go:72`; cap-check escalates and refuses past 2 |
| `maxAskBranchConflicts = 3` | `pilot_rebase.go:38`; cap-check at lines 123 + 323 |
| Red-phase integrity | Strengthened (inverted to regression-check form) |
| Skip markers in scope | Zero |
| Out-of-scope | None |
| Test quality | 5/5 (`TestApplyMedicRequeue_AdversarialLLM` tests 3× cap-breach + Fix #3 partial UNIQUE collapse to 1 Open escalation) |
| **Verdict** | **ACCEPTED** |

### Fix #7 — Tighten ConvoyReview (merge `c94bfd6`)

| Check | Result |
|---|---|
| AUDIT IDs claimed closed | 006, 007, 029, 031, 032, 111, 113, 117, 120, 135, 136, 138, 161, 162 |
| AUDIT IDs verified closed | All 14 PASS |
| `convoy_review_max_findings` default | Dropped 5→2 (`convoy_review.go:103`) |
| Loop cap at 5 passes | Check runs BEFORE `runConvoyReviewLLM` (lines 241-261, 327) |
| Parse-failure cap = 2 | `convoyReviewParseFailureCap` at line 104; first failure retry + critic note; second failure `CreateEscalation` + `FailBounty` (NOT Completed) with `[CONVOY REVIEW PARSE FAILURE]` mail |
| Fingerprint dedup | SHA256 over sorted per-finding hashes (`findingSetFingerprint` at line 131); persisted to `last_findings_fingerprint` on Completed rows; repeat → escalate conflicted_loop (severity High) |
| Clean-pass gate | `hasPriorCleanPass` at line 175; post-clean new findings → severity Medium + operator mail (`[CONVOY REVIEW DRIFT]`) |
| Clean marker | `convoyReviewCleanMarker = "CLEAN"` stamped only on true clean; deferred-completion paths do NOT stamp |
| PR review thread cap = 2 | `pr_review_thread_depth_cap` at `pr_review_triage.go:104`; system prompt forbids `conflicted_loop` below cap |
| Captain hallucination filter | `filterHallucinatedRejections` at `captain.go:119`, called at line 154 |
| Skip markers in scope | Only 3 residual in audit_cost_loops_test.go and audit_medium_spotcheck_c_test.go — all AUDIT-137/099/100, tagged for other fixes (not Fix #7 scope) |
| Red-phase integrity | Strengthened (inverted to regression-check form) |
| Out-of-scope | None |
| Test quality | 5/5 (`convoy_review_fix7_test.go` 601 LOC; AST-based meta-tests in audit_test_quality_test.go) |
| **Verdict** | **ACCEPTED** |

### Fix #8a — Self-heal terminator signatures (merge `0d49877`)

| Check | Result |
|---|---|
| AUDIT IDs claimed closed | 013, 014, 022, 041 (+ P1) |
| Signatures | `UpdateBountyStatus(db,id,newStatus) error`, `FailBounty(db,id,msg) error`, `CreateEscalation(db,taskID,sev,msg) (int, error)` all verified at `tasks.go:196`, `tasks.go:290`, `escalation.go:47` |
| Hot-path callers check the error | Jedi Council, Medic (medic.go happy path), Medic CI (happy paths), Diplomat — all wrap with `if err := ...`. **WorktreeReset — partial regression — see Fix #8b** |
| Fallback FailBounty + operator mail on CreateEscalation failure | Present in Pilot, Astromech, Chancellor, Captain hot paths |
| Tests PASS | `TestPattern_P1`, `TestAUDIT_013/014/022/041` all PASS under `-race -count=3` |
| Red-phase integrity | Legitimate red→green conversion; static `NumOut()==1 && returnType.Implements(error)` assertion added |
| **Verdict** | **ACCEPTED** |

### Fix #8b — Remaining call-site migration (Campaign 3: `b9d1a7a`, `3ab456d`, `ad144a9`, `c614139`, `e824b78`)

| Check | Result |
|---|---|
| Claim | FINAL-STATUS: "Phase B (complete) migrated all 108 non-hot-path call sites across 18 files" |
| `deferral-comment(Fix #8b)` comment markers in production | 0 (as claimed) |
| `_ = store.*` or `_, _ = store.*` in production w/o comment marker | **~28** — violates CLAUDE.md's "only acceptable when paired with // deferral-comment(Fix #8b): propagate error" |
| **Bare-call `store.FailBounty(...)` / `store.UpdateBountyStatus(...)` without assignment or if-guard in production** | **8 sites** — all in CLAUDE.md-named hot-path files: |
| → `internal/agents/pilot_worktree_reset.go:78, 83, 91, 99, 111, 129` | 6 bare FailBounty/UpdateBountyStatus returns at failure-exits of `runWorktreeReset` (CLAUDE.md names WorktreeReset as a hot-path caller) |
| → `internal/agents/medic_ci.go:170` | Bare `store.UpdateBountyStatus(db, bounty.ID, "Completed")` on CI-breaker-open path |
| → `internal/agents/astromech.go:601` | Bare `store.FailBounty(db, bounty.ID, failMsg)` inside `autoShardIfNoCommits` (execution continues to `EmitEvent`+`SendMail` so log would be invisible) |
| Behavior on those sites | If terminator fails, task stays Locked; stale-lock detector recovers. Not cost-burn; silent-failure only |
| Campaign 3 tests PASS | TestFix8B_PRReviewTriage, _Auditor, _Investigator, _Inquisitor, _Librarian, _Pilot_*, _Captain_*, _ConvoyReview_*, _Commander_*, _Astromech*, _JediCouncil — all green |
| **Verdict** | **CONDITIONAL** — campaign was ~92% complete but declared done. Fix #8d sweep required to (a) error-check the 8 bare hot-path terminators, (b) add the comment markers to the ~28 `_ =` discards or migrate them. Does not re-open cost-burn class; stale-lock detector recovers. |

### Fix #8c — Schema/time/parser cleanup (merge `d82f8a3`, commit `320409e`)

| Check | Result |
|---|---|
| AUDIT IDs claimed closed | 077, 078, 080, 082, 143, 146, 147, 148 |
| `columnExists(db, table, column)` helper | Present at `schema.go:15`, gates destructive ALTERs (AUDIT-077) |
| `NowSQLite()` + `ParseSQLiteTime(s)` | Present in `internal/store/time.go`; adopted at `inquisitor.go:212` and `dogs.go:664` (AUDIT-146/147) |
| `rateLimitBackoffMaxShifts = 10` pre-loop clamp | Present at `estop.go:88,105-106` (AUDIT-148) |
| `TestSchemaParity` | PASSES (symmetric createSchema vs schema.sql column-set diff; AUDIT-080) |
| Post-ALTER backfill UPDATE | Present (AUDIT-078) |
| Tests PASS | All 8 AUDIT IDs PASS under `-race -count=3` |
| Skip markers in scope | Zero |
| **Verdict** | **ACCEPTED** |

### Fix #8.5 — LLM prompt boundary markers + strict JSON (Campaign 1 merge `2333d3f`)

| Check | Result |
|---|---|
| AUDIT IDs claimed closed | 030, 108, 109, 110, 114, 115, 116, 139 |
| `WrapUserContent` + `promptInjectionClause` | Defined in `llm_boundary.go:55-66`; used in all 6 agents (jedi_council, captain, medic, convoy_review, pr_review_triage, chancellor) |
| `strictJSONUnmarshal` on LLM responses | Every LLM-output `json.Unmarshal` routed through `strictJSONUnmarshal` (`llm_boundary.go:114`); plain `json.Unmarshal` remaining in the 6 agents parses fleet-internal data (bounty payloads, PlanJSON) only |
| `CouncilRuling.Approved *bool` | `types.go:45`; nil-check at `jedi_council.go:268` |
| Captain default-branch fail-closed | `runCaptainTask` default → `handleInfraFailure(..., "captain", ...)` at `captain.go:615`; "fail-closed" sentinel comment retained |
| Chancellor fail-closed both error paths | `store.FailBounty` + `[CHANCELLOR FAIL-CLOSED]` mail at chancellor.go:116-130 (Claude error) and 138-152 (parse error); NO `approveProposal(..., chancellorRuling{}, ...)` remaining (P12 sub-F enforces) |
| `SanitizeLLMPayload` signal-token denylist | All 8 tokens present in `llmSignalTokens` (llm_boundary.go:77-86); applied at all 6 documented ingress points |
| Reject, not strip | All callers route to `handleInfraFailure` / retry / escalate; no silent strip |
| Tests PASS | Pattern P12 A-F + TestAUDIT_030/108/109/110/114/115/116/139 all green; 4 fuzz targets clean (300k-488k execs/10s, new-interesting growth 16-37) |
| Skip markers in scope | Zero |
| Out-of-scope | None |
| Known residuals (per FINAL-STATUS) | Commander/Boot use plain `json.Unmarshal` but output routes through `validateTaskPlan` / post-parse enum-switch; legitimate scope deferral, not leak. Chancellor SEQUENCE/MERGE with empty required subfield still auto-approves (weaker fail-open than the closed Claude/parse paths) — accepted caveat, should be tracked |
| Test quality | 5/5 across 4 sampled tests (curated attack shapes including Cyrillic homoglyphs, BOM, null byte, nested depth, tag forgery) |
| **Verdict** | **ACCEPTED** |

### Fix #9 — Ref/path/URL validators + `--` separator (merge `15c391c`)

| Check | Result |
|---|---|
| AUDIT IDs claimed closed | 018, 019, 049, 050, 051, 098, 123, 140, 153, 154 (+ P10) |
| AUDIT IDs verified closed | All 10 + P10 PASS under `-race -count=5` |
| Validators present | `ValidateRef` (validators.go:88), `ValidateRepoPath` (180), `ValidateRemoteURL` (290), `ValidateGHRepoSpec` (406) |
| Store-layer regex | `validateRefName`/`validateRemoteURL` at `SetBranchNameTx`, `UpsertConvoyAskBranch`, convoy.go ask-branch setter, `AddRepo` |
| `--` separator | Pattern P10 test greps all `exec.Command("git"|"gh")` calls — green. Spot-check ~40 callers clean |
| Fuzz | `FuzzValidateRef`, `FuzzValidateRepoPath`, `FuzzValidateRemoteURL` all clean (1M+ execs each at 3s) |
| Symlink skips | Legitimate OS-capability skips (`t.Skipf("symlink unsupported")`), not AUDIT skips |
| Red-phase integrity | Strengthened (post-fix assertion "must contain Lstat+ModeSymlink"). One accepted loosening: AUDIT-057 accepts any of {capWriter, maxGHStdoutBytes, MaxBytesReader, io.MultiWriter, io.LimitReader} since Fix #10 chose capWriter |
| Out-of-scope | None; 22 files all validator/caller/store |
| CLAUDE.md consistency | "Shell-boundary validators (Fix #9)" section matches code |
| Test quality | 5/5 (validators_test.go + validators_fuzz_test.go + validators_integration_test.go) |
| **Verdict** | **ACCEPTED** |

### Fix #10 — Outbound-channel hardening (merge `78b2585`)

| Check | Result |
|---|---|
| AUDIT IDs claimed closed | 016, 017, 055, 056, 057 (+ P9) |
| AUDIT IDs verified closed | All 5 + P9 PASS under `-race -count=5` |
| `RedactSecrets` call sites | webhook.go:64, telemetry.go:147/151/199, escalation.go:48, fleet_mail.go:16, tasks.go:222/292/304/310, holocron.go:196, gh/gh.go:67/326 |
| `ValidateOutboundURL` | Config-write (`cmd/force/config.go:59`) + per-request (webhook.go:46, telemetry.go:68) + `CheckRedirect` (webhook.go:90-98, telemetry.go:85-90) — all three layers per CLAUDE.md |
| `maxGHStdoutBytes = 64 MiB` | `gh.go:30`; `ErrOverflow` classified to `ErrClassPermanent` at gh.go:776-777 |
| Fuzz | `FuzzRedactSecrets` 1.89M execs, 3 new interesting, no crash |
| OTLP race | Bonus fix — `TestEmitEvent_WithOTLPEndpoint` gets `WaitForOTLPDrain()` in deferred cleanup; folded into this PR |
| Skip markers in scope | Zero |
| Out-of-scope | None; 21 files all redaction/URL/gh scope |
| CLAUDE.md consistency | Matches |
| Test quality | 5/5 (`redact_test.go`, `outbound_url_test.go` 14-case table, fuzz) |
| **Verdict** | **ACCEPTED** |

### Campaign 2 — Scope deferrals (merge `acae291`)

| Check | Result |
|---|---|
| AUDIT IDs claimed closed | 011 (read-side), 025, 085, 149 |
| AUDIT IDs verified closed | All 4 PASS under `-race -count=5` |
| `Escalations.auto_resolve_count` | Column in createSchema + runMigrations + schema.sql; sweeper increments exactly once via `WHERE ... AND auto_resolve_count < 1` (escalation_sweeper.go:65-66, 125-126) |
| Operator re-open respected | `CloseEscalation`/`AckEscalation` do not touch counter — `TestDogEscalationSweeper_RespectsOperatorReopen` verifies |
| `Resolved` retired | Zero production references outside migration + docstring warning |
| Startup migration | `UPDATE Escalations SET status='Closed', acknowledged_at=IFNULL(...) WHERE status='Resolved'` — idempotent |
| `idx_bounty_convoy_status` + `convoy_id` column | Present; `grep -rn "payload LIKE.*convoy_id" --include="*.go"` excluding _test.go returns **0 hits** in production (CLAUDE.md rule satisfied) |
| Out-of-scope | 28 files; all trace to one of 4 AUDIT IDs |
| Test quality | 5/5 |
| **Verdict** | **ACCEPTED** |

---

## Pattern closure (P1–P12 minus P5)

| Pattern | Test status | Spot-check verdict |
|---|---|---|
| **P1** — silent failures | PASS under race. Three terminator signatures migrated (Fix #8a). AUDIT-013/014/022/041 closed. | **MOSTLY-CLOSED** — terminators return error, but ~28 `_ = store.*` in production are un-migrated without the required comment markers; AUDIT-090/091/094/095 (rows.Scan discards) still present in production despite "pattern-covered" allowlist claim. |
| **P2** — idempotency race | PASS under race-count=5 (50 goroutines, exactly 1 row). Fix #3 closed. | **CLOSED** — partial UNIQUE + `ON CONFLICT DO NOTHING RETURNING` genuinely fixes the class. |
| **P3** — payload LIKE dedup | PASS. Fix #3 + Campaign 2 closed. | **CLOSED** — `convoy_id` column + `idx_bounty_convoy_status`; `grep -rn "payload LIKE.*convoy_id"` in production returns 0. |
| **P4** — hot-table indexes missing | PASS. Fix #4 closed. | **CLOSED** — EXPLAIN QUERY PLAN at 10k rows confirms INDEX usage; schema parity enforced. |
| **P5** — NOT-APPLICABLE (no convoy-thrash dog to fix) | n/a | Dead. |
| **P6** — undocumented statuses / sweepers | PASS. Fix #5 + Campaign 2 closed (outer + A); B/C rewritten as regression traps. | **CLOSED** — stale-convoys dog respects Failed/Escalated; `Resolved` retired fleet-wide; AUDIT-083/084 remain open but are scoped-out (ConflictPending trap + AwaitingChancellorReview stale-lock — future fix). |
| **P7** — unguarded state transitions | **SKIPS** (both subtests). Fixes claimed "Fix #8 pattern-covered" in allowlist. | **NOT CLOSED** — `UpdateBountyStatusFrom(id, from, to)` referenced by both skips does not exist in the code. Allowlist annotation is false. Races: (a) cancel vs approve: 20/20 clobbers under prior test (b) ResetTask resurrects Completed. Data-integrity bug; bounded by Fix #3/#6 from compounding. |
| **P8** — dashboard | PASS. Fix #2 closed. | **CLOSED** — loopback bind, Origin allowlist, CSP headers, size cap, no innerHTML pipeline from attacker-controllable data. |
| **P9** — outbound secrets | PASS. Fix #10 closed. | **CLOSED** — RedactSecrets threaded through every channel; ValidateOutboundURL + CheckRedirect + gh stdout cap. |
| **P10** — shell injection | PASS. Fix #9 closed. | **CLOSED** — four validators + `--` separator invariant enforced by grep-based pattern test. |
| **P11** — e-stop ineffective | PASS under race, 3s budget honoured. Fix #1 closed. | **CLOSED** — Spawn loops guarded, dogs honour e-stop, heartbeat ctx-cancel verified. |
| **P12** — prompt injection | PASS. Fix #8.5 closed. | **CLOSED** — boundary markers, strict JSON, Captain/Chancellor fail-closed, sanitizer reject. Commander/Boot out-of-scope deferral is legitimate (downstream validation catches). |

---

## Common-cheat audit

| Cheat category | Severity | Where observed |
|---|---|---|
| **#1 Softened assertion** | Low | None found — sampled diff hunks show `t.Fatalf` count *grew* from red phase (63 removed, 399 added). Spot-checked 10 co-located removals of `!=` and additions of `>=` and all are legitimate (character ranges, cap-checks, duration lower bounds). |
| **#2 Skip rename** | Low | None found — every non-AUDIT `t.Skip` added in the campaign is a legitimate runtime skip (`git not found`, symlink-unsupported-on-platform, `-short` mode, umbrella-test comment). |
| **#3 Stub call-count loosened** | Low | None found — call-count assertions (`TestAuditMedium161/162` explicitly guard against this anti-pattern and were preserved). |
| **#4 Mechanism bypass / magic-string special-case** | Low | None found — audit tests run against real DB and real production code, not stub-instrumented paths. |
| **#5 Disable then replace** | Low | Test bodies were inverted (red → green), not deleted-and-recreated. |
| **#6 Pattern test downgrade to individual test** | **Medium** | Pattern P7 — *effective* downgrade via allowlist. Both subtests still `t.Skip` and the fix mechanism (`UpdateBountyStatusFrom`) was never written. Annotation "Fix #8 pattern-covered" is false. |
| **#7 Filler coverage** | Low | None found — +161 net new Test functions; sampled 10 non-trivial; all exercise real mechanisms. |
| **#8 Make target no-op** | Low | `make smoke` runs 5 real tests in 3.5s; `make fuzz` runs each `Fuzz*` for 30s (6 targets spot-checked at 3s produce 40k-1M execs with coverage growth); `make test-audit` walks the module and greps for real skip markers. All three targets genuine. |
| **#9 CLAUDE.md silently contradicted** | Low | None found — sub-agents spot-checked each invariant against code. |
| **#10 Phase 8a/8b/8c incomplete** | **Medium** | Confirmed — 8 unchecked bare terminator calls in CLAUDE.md-named hot-path files + ~28 `_ = store.*` in production without the required comment markers. Allowlist claims closure. |

**Count of cheat-bearing fixes: 2 — Fix #8b (cheat #10) and the P7 cross-cutting deferral (cheat #6).** Per the mission's severity calibration, 2 cheats of medium severity on non-cost-burn classes → CONDITIONAL-GO, not NO-GO.

---

## New test quality assessment (sampled 10 tests)

| Test | File | Score | Justification |
|---|---|---|---|
| `TestAUDIT_102_103_104_121_122_124_ProtectedBranchGuardsMissing` | `internal/git/audit_protected_branch_test.go` | 5 | Real git-init; 5 default-branch name cases; post-reject COUNT(*) assertion. |
| `TestPattern_P8_DashboardBindsAllInterfaces_ServesWildcardCORS` | `internal/dashboard/audit_pattern_p8_test.go` | 5 | Real httptest; 3 CDN hosts; CSP meta-tag check. |
| `TestPattern_P2_IdempotencyKeyRace` | `internal/store/audit_pattern_p2_test.go` | 5 | 50 goroutines, start-gate, count + same-id assertion. Deterministic under `-race -count=5`. |
| `TestHotTableIndexes_ClaimQueryUsesIndex_10kRows` | `internal/store/hot_table_indexes_test.go` | 5 | 10k-row seed + EXPLAIN QUERY PLAN + `USING INDEX` pattern match. |
| `TestSpendBurnPattern_TriggersAutoEstopInOneCycle` | `internal/agents/spend_cap_test.go` | 5 | Real DB, real dogSpendBurnWatch, idempotence via mail count. |
| `TestHeartbeatCancelsClaudeOnEstop` | `internal/agents/astromech_heartbeat_test.go` | 4 | Emulates heartbeat shape (not real goroutine); deterministic <500ms via 20ms ticker. |
| `TestApplyMedicRequeue_AdversarialLLM` | `internal/agents/medic_requeue_cap_test.go` | 5 | 3× cap-breach trials + Fix #3 partial UNIQUE collapses to 1 Open escalation. |
| `TestDogEscalationSweeper_RespectsOperatorReopen` | `internal/agents/escalation_sweeper_test.go` | 5 | Full Campaign 2 scenario — close + counter=1, operator re-open, sweeper MUST NOT re-close. |
| `TestRedactSecrets` + `FuzzRedactSecrets` | `internal/store/redact_test.go` | 5 | 5 tests + fuzz covering every GitHub PAT prefix, Bearer, URL basic auth, fine-grained, benign hot path. |
| `TestPRReviewTriage_InjectionPayload_DoesNotBypassBoundary` | `internal/agents/pr_review_triage_test.go` | 5 | 4 distinct injection shapes; LLM stub obeys so downstream defense is tested; DB-level assertion. |

**Average: 4.9 / 5.** No filler / decorative / passing-by-construction tests in the sample.

---

## FINAL-STATUS.md's "5 patterns audit missed"

| # | Pattern | Real or generic | Tracked? | Operator action before restart? |
|---|---|---|---|---|
| 1 | Schema-column-drift between createSchema and runMigrations | **Real** — `TestSchemaParity` now enforces; +a generated schema.sql or PRAGMA match test would ratchet further | Code: partial (column-set only, not type+default) | No — `TestSchemaParity` catches the common failure mode. |
| 2 | `_, _ = return` as a lint smell | **Real** — 108 call sites across the fleet; Fix #8b partial closure proves it | Not automated — a custom golangci-lint rule proposed | Yes if the operator wants to prevent recurrence (tracks toward Fix #8d). |
| 3 | Parallel worktree conflicts on shared docs | **Real but operational** — fix agent's own merge pain | No automation | No — affects future fix campaigns, not daemon safety. |
| 4 | Tests documenting pre-fix behavior without post-fix counterpart | **Real** — Fix #6's `TestApplyMedicRequeue_AdversarialLLM` red-phase collided with Fix #3's partial UNIQUE; required rewrite | Not enumerated | No. |
| 5 | Conflict-gate allowlists for in-flight work (fix dependency DAG) | **Real** — Fix #1's context threading forced 6 conflicts | Not automated | No. |
| *bonus* | RGR helper bug around inline interface types | **Real** — `extractFuncBody` latent bug flushed by Fix #0 | Fixed in `extractFuncBody` | No. |

All five (plus bonus) are evidence-based, not generic. The operator does not need to act on any before restart; (2) should seed a follow-up Fix #8d ticket.

---

## Forensic appendix

### A. Pattern P7 deferral forensic

`internal/store/audit_pattern_p7_test.go:31`
```go
func TestPattern_P7_ConcurrentCancelVsApproveRace(t *testing.T) {
    t.Skip("AUDIT-027/AUDIT-072: remove when UpdateBountyStatusFrom(id, from, to) guards state transitions (Fix #8/#5)")
    // Without skip, fails with:
    //   audit_pattern_p7_test.go:135: AUDIT-P7 (AUDIT-027, AUDIT-072): detected 20/20 clobbers
    //   where CancelTask succeeded but a later unguarded UpdateBountyStatus("Completed")
    //   overwrote 'Cancelled'. Stats: cancelWins=20, finalCancelled=0, finalCompleted=20.
    ...
```

`internal/store/audit_pattern_p7_test.go:174`
```go
func TestPattern_P7_ResetTaskResurrectsCompleted(t *testing.T) {
    t.Skip("AUDIT-026: remove when UpdateBountyStatusFrom(id, from, to) guards state transitions (Fix #8/#5)")
    // Without skip, fails with:
    //   audit_pattern_p7_test.go:195: AUDIT-P7 (AUDIT-026): ResetTask resurrected a Completed
    //   task to "Pending"...
```

`grep -rn "UpdateBountyStatusFrom" --include="*.go"` in the tree:
```
internal/audittools/audittools_test.go:77:  // need Fix #8's UpdateBountyStatusFrom variant.
internal/store/audit_pattern_p7_test.go:31: t.Skip("AUDIT-027/AUDIT-072: remove when UpdateBountyStatusFrom...")
internal/store/audit_pattern_p7_test.go:148:       "transition. Fix: UpdateBountyStatusFrom(db, id, from, to) returning "+
internal/store/audit_pattern_p7_test.go:174: t.Skip("AUDIT-026: remove when UpdateBountyStatusFrom...")
```

Function is never defined. Allowlist annotation "Fix #8 pattern-covered (P7)" on AUDIT-026/027/072 is
false — the class remains open.

### B. Fix #8b hot-path bare-call evidence

`internal/agents/pilot_worktree_reset.go:78-83, 91, 99, 111, 129`
```go
func runWorktreeReset(db *sql.DB, bounty *store.Bounty, logger interface{ Printf(string, ...any) }) {
    var p worktreeResetPayload
    if err := json.Unmarshal([]byte(bounty.Payload), &p); err != nil {
        store.FailBounty(db, bounty.ID, fmt.Sprintf("invalid payload: %v", err))  // L78 bare
        return
    }
    repo := store.GetRepo(db, p.Repo)
    if repo == nil || repo.LocalPath == "" {
        store.FailBounty(db, bounty.ID, fmt.Sprintf("repo %s not registered", p.Repo))  // L83 bare
        return
    }
    ...
    if err := igit.ValidateRef(p.TargetBranch); err != nil {
        store.FailBounty(db, bounty.ID, ...)  // L91 bare
        return
    }
    if out, err := exec.Command(...).CombinedOutput(); err != nil {
        store.FailBounty(db, bounty.ID, ...)  // L99 bare
        return
    }
    ...
    if len(worktreeRoots) == 0 {
        ...
        store.UpdateBountyStatus(db, bounty.ID, "Completed")  // L111 bare
        return
    }
    ...
    if len(failures) > 0 && wiped == 0 {
        store.FailBounty(db, bounty.ID, ...)  // L129 bare
        return
    }
```

Per CLAUDE.md "Fix #8 Phase A (signatures)": "Hot-path callers (Jedi Council, Medic, Medic CI,
Diplomat, **WorktreeReset**) check the error and either propagate or log a clear recovery hint."
These 6 calls do neither.

`internal/agents/medic_ci.go:170`
```go
if IsCIBreakerOpen(db, payload.Repo) {
    ...
    applyCITriageEnvironmental(db, agentName, pr, payload, ciTriageDecision{...}, logger)
    store.UpdateBountyStatus(db, bounty.ID, "Completed")  // bare — no if-guard, no log
    return
}
```
Immediately before this, the same function uses the guarded form at line 149 and 184 — the pattern is
known to the author of the file. Line 170 is an omission, not a style choice.

`internal/agents/astromech.go:601`
```go
newID := store.AddBounty(db, bounty.ID, "Decompose", bounty.Payload)
failMsg := fmt.Sprintf("Auto-sharded after repeated %s failures...", ...)
shardHistID := store.RecordTaskHistory(...)
store.StampHistoryMemoryIDs(...)
store.FailBounty(db, bounty.ID, failMsg)  // bare — if this fails, EmitEvent+SendMail still run
telemetry.EmitEvent(...)
store.SendMail(db, name, "operator", ...)
```
If FailBounty fails, the EmitEvent + SendMail cascade happens against a task whose status did not
actually transition. Silent-failure path.

### C. Pre-existing race in cmd/force

`cmd/force/testhelpers_test.go` at audit-baseline fd67620 already contained:
```go
func captureOutput(f func()) string {
    old := os.Stdout
    ...
    os.Stdout = w
    ...
    os.Stdout = old
```

The race under `go test -race` is between goroutines spawned by sibling `TestRunCommandCenter_*`
tests that each call `captureOutput`. Not introduced by any fix merge. Documented in
FINAL-STATUS.md as an accepted out-of-scope item. Test-infra only; no production impact.

### D. Allowlist "pattern-covered" false claims

From `internal/audittools/audittools_test.go`:
```go
"AUDIT-090": "Fix #8 pattern-covered (P1)",
"AUDIT-091": "Fix #8 pattern-covered (P1)",
"AUDIT-094": "Fix #8 pattern-covered (P1)",
"AUDIT-095": "Fix #8 pattern-covered (P1)",
```

Pattern P1 closure (Fix #8a) covered three *terminator* signatures (UpdateBountyStatus, FailBounty,
CreateEscalation). AUDIT-090/091/094/095 are about `rows.Scan` errors being silently discarded:

```go
// Example from captain.go:195 — rows.Scan ignored:
for rows.Next() {
    var id int
    rows.Scan(&id)   // error not checked
    ...
}
```

30+ such sites in production remain unchanged by Fix #8a. The allowlist "pattern-covered" claim is
overstated.

Similarly AUDIT-127/158/165 claim "Fix #8b lifecycle batch" coverage, but the fix mechanism is
`exec.CommandContext` adoption and only ~1 of ~100 `exec.Command` calls were migrated.

---

## Restart decision summary for operator

**Restart the daemon.** The $300 cost-burn vector is closed by Fix #1 (spend cap + e-stop), Fix #3
(idempotency), Fix #4 (indexes), Fix #6 (Medic cap), Fix #7 (ConvoyReview), Fix #10 (outbound), plus
Fix #8.5 (prompt injection). All of those fixes have real production-level closure, new test
coverage, and no cheat patterns.

Accept the four CONDITIONAL items G1-G4 in the verdict section. File a Fix #8d ticket to close:
- The 8 hot-path bare-terminator calls in `pilot_worktree_reset.go`, `medic_ci.go:170`,
  `astromech.go:601`.
- The `UpdateBountyStatusFrom(id, from, to)` helper + route ResetTask / CancelTask through it to
  close Pattern P7.
- Reclassify the overstated "pattern-covered" allowlist entries
  (AUDIT-090/091/094/095/127/158/165) with concrete follow-up fix references.
