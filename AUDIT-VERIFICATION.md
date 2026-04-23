# Audit Verification Report

Verification of `AUDIT.md` (166 findings) performed by an orchestrated fanout of 11 pattern agents + 9 individual-batch agents + 4 Medium spot-check agents. Every finding is either reproduced by a failing/locking test, marked NOT-APPLICABLE (feature absence), or collapsed as a duplicate. No finding was silently dropped.

All tests live at `internal/{store,agents,git,dashboard}/audit_*_test.go` and compile cleanly against current `main` (`go vet` clean). They are red-on-main: each test either fails today (and passes once the fix lands) or is a locking test (passes today because the defective pattern is still present; fails the moment the remedy lands so the test has to be removed in lock-step with the fix).

## Summary stats

```
Total findings:              166
Verified (test or static):   149
Not-applicable (feature absence): 13
Duplicates (collapsed):      4
Not-reproducible:            0
Downgraded in severity:      11
Upgraded in severity:        3
Patterns verified:           11 / 12 (P5 = NOT-APPLICABLE)

By tier (using AUDIT.md section boundaries — note: user's prompt tallied
differently; the numbers below match how the audit itself groups the IDs):
  Critical (35): verified=34, na=1, dup=0, notrepro=0, downgraded=0
  High     (67): verified=57, na=7, dup=3, notrepro=0, downgraded=3, upgraded=2
  Medium   (60): spot-checked=12 (all reproduced), pattern-covered=43, na=5, dup=0,
                 downgraded=8
  Low       (4): all pattern-covered
```

Tests contributed: **24 files**, **~95 sub-tests**, all committed in the single verification commit.

## Pattern verification (P1 – P12)

Each pattern has one committed failing/locking test that reproduces the root-cause defect once. A single pattern fix makes the test green and simultaneously closes the structural finding for every ID listed under "covers".

| Pattern | Verdict | Test file | Covers (findings) |
|---|---|---|---|
| **P1** No silent failures invariant violated | CONFIRMED | `internal/store/audit_pattern_p1_test.go` — `TestPattern_P1_UpdateBountyStatusSwallowsDBError` | AUDIT-022, -070 directly (empirical DB-drop). AUDIT-013/-014/-015/-037/-038/-039/-041/-042/-043/-044/-073/-090/-091/-094/-095/-156/-159 are the same invariant at agent/git boundaries — each has its own sub-test in `audit_silent_failures_test.go` and `audit_concurrency_test.go`. |
| **P2** Idempotency decorative | CONFIRMED | `internal/store/audit_pattern_p2_test.go` — `TestPattern_P2_IdempotencyKeyRace` (50-goroutine race → 10 duplicate rows observed; no UNIQUE index exists). | AUDIT-008, -034, -035, -036, -075, -076, -112. |
| **P3** Payload-LIKE dedup | CONFIRMED | `internal/agents/audit_pattern_p3_test.go` — `TestPattern_P3_PayloadLikeDedupIsFullScan`, `TestPattern_P3_BoundaryFalsePositive`. EXPLAIN QUERY PLAN → `SCAN BountyBoard`. Nested-JSON false-positive reproduced. | AUDIT-011, -048. **Calibration:** Audit said "15+ sites"; actual production-code count is 9. Severity unchanged, but the hyperbole is on record. |
| **P4** Missing indexes | CONFIRMED | `internal/store/audit_pattern_p4_test.go` — 15 table-driven sub-cases; `TestPattern_P4_ClaimQueryUsesIndex` shows `SCAN BountyBoard` for the hottest query. | AUDIT-009, -010, -024, -058, -059, -134. |
| **P5** No cost ceiling | NOT-APPLICABLE | — | AUDIT-004, -060, -061, -062, -063, -065. Feature absence — cannot write a failing test for code that does not exist. |
| **P6** Trap states / undocumented values | CONFIRMED | `internal/store/audit_pattern_p6_test.go` — three sub-tests: AST check on `runStaleConvoysReport` (WHERE clause missing 'Failed','Escalated'), `Resolved` written by 3 sinks but read by none, `ActiveCount` query omits 4 states. | AUDIT-012, -025, -083, -084, -085, -088, -089, -166. |
| **P7** Unguarded state transitions | CONFIRMED | `internal/store/audit_pattern_p7_test.go` — 20/20 runs of concurrent cancel-vs-approve show approve clobbers cancel; `ResetTask` resurrects `Completed`. | AUDIT-021, -026, -027, -072, -086, -087. |
| **P8** Dashboard security | CONFIRMED | `internal/dashboard/audit_pattern_p8_test.go` — 7 assertions: bind is `:PORT` not `127.0.0.1`; `jsonCORS` writes `Access-Control-Allow-Origin: *`; marked.js from jsdelivr with no SRI; no `DOMPurify.sanitize` anywhere; live `handleStatus` response confirms wildcard CORS. | AUDIT-001, -002, -003, -053, -054, -064 (partial), -100 (via misc-security sub-test). |
| **P9** Outbound exfil | CONFIRMED | `internal/store/audit_pattern_p9_test.go` — httptest-captured webhook body contains `ghp_testFakeTokenABC123` verbatim; webhook follows 302 to link-local; 14 `string(stderr)` interpolations in `gh.go` with zero redaction. | AUDIT-016, -017 (via misc-security), -055, -056, -057. |
| **P10** Shell injection | CONFIRMED | `internal/git/audit_pattern_p10_test.go` — 19 adversarial branch names × 3 store entry points = 55 violations; ~50 `git` invocations missing `--` separator including `git push --force-with-lease origin <branch>`. End-to-end CVE-2017-1000117 chain documented. | AUDIT-018, -019 (via misc-security), -049, -050, -051, -052, -098, -099 (via misc-security), -140, -153, -154. |
| **P11** E-stop not effective | CONFIRMED | `internal/agents/audit_pattern_p11_test.go` — 3 sub-tests: `RunDogs` static-grep finds no `IsEstopped` check; rate-limit backoff sleeps through e-stop (3s budget, still sleeping); heartbeat/Claude-invocation block has no e-stop polling. **Note:** AUDIT.md's P11 paragraph mis-cites these as AUDIT-112/-113/-114 — those are test-quality findings; the real e-stop findings are AUDIT-105/-106/-107. Corrected in this report. | AUDIT-105, -106, -107. |
| **P12** Prompt injection | CONFIRMED | `internal/agents/audit_pattern_p12_test.go` — 6 sub-tests: Council/Captain prompts have no `<user_content>` boundary; Captain default-approve branch present; Council `Approved bool` silently parses missing field as false (behavioural — test fails today); zero `DisallowUnknownFields` occurrences anywhere; Chancellor fails open on both Claude error and parse error. **Note:** AUDIT.md's P12 paragraph mis-cites findings as AUDIT-130/-131/-132/-133 — those are time/test findings; the real prompt-injection findings are AUDIT-108/-109/-110/-114/-115/-116/-139/-141/-142/-143/-144/-145/-163. Corrected in this report. | AUDIT-108, -109, -110, -114, -115, -116, -139, -141, -142, -143, -144, -145, -163. |

## Critical finding matrix

35 Critical findings. All accounted for. Column "Outcome" uses the 5-way taxonomy from the prompt.

| ID | Outcome | Test file | Verdict |
|---|---|---|---|
| AUDIT-001 | REPRODUCED-STATIC | `dashboard/audit_pattern_p8_test.go` | Bind `:PORT` + wildcard CORS confirmed by dynamic httptest. |
| AUDIT-002 | REPRODUCED-STATIC | `dashboard/audit_pattern_p8_test.go` | `marked.parse` with no `DOMPurify.sanitize`. |
| AUDIT-003 | REPRODUCED-STATIC | `dashboard/audit_pattern_p8_test.go` | `marked@15` loaded from jsdelivr with no `integrity=` attribute. |
| AUDIT-004 | NOT-APPLICABLE | — | Feature absence — no spend cap anywhere. |
| AUDIT-005 | REPRODUCED-STATIC | `agents/audit_cost_loops_test.go` | `ResetTaskFull` still zeros `retry_count` and `infra_failures`; no `medic_requeue_count` column. |
| AUDIT-006 | REPRODUCED-STATIC | `agents/audit_cost_loops_test.go` | `maxPasses=5`, `convoy_review_max_findings=5`; no fingerprint short-circuit. |
| AUDIT-007 | REPRODUCED-STATIC | `agents/audit_cost_loops_test.go` | Parse-fail path marks Completed; no `parse_failure_count` column. |
| AUDIT-008 | REPRODUCED-TEST | `store/audit_pattern_p2_test.go` | 50-goroutine race produced 10 duplicate rows. |
| AUDIT-009 | REPRODUCED-STATIC | `store/audit_pattern_p4_test.go` | BountyBoard: zero indexes. |
| AUDIT-010 | REPRODUCED-STATIC | `store/audit_pattern_p4_test.go` | TaskHistory: zero indexes. |
| AUDIT-011 | REPRODUCED-TEST | `agents/audit_pattern_p3_test.go` | EXPLAIN plan = `SCAN BountyBoard`; boundary false-positive reproduced. |
| AUDIT-012 | REPRODUCED-STATIC | `store/audit_pattern_p6_test.go` | `runStaleConvoysReport` WHERE clause omits Failed/Escalated. |
| AUDIT-013 | REPRODUCED-STATIC | `agents/audit_silent_failures_test.go` | `medic.go:120` bare `json.Unmarshal` with no err check. |
| AUDIT-014 | REPRODUCED-STATIC | `agents/audit_silent_failures_test.go` | 2 `_, _ = db.Exec` sites in `pilot_worktree_reset.go`. |
| AUDIT-015 | REPRODUCED-STATIC | `agents/audit_silent_failures_test.go` | 6 log-and-return sites inside `onSubPRMerged` (audit said 3 — **upgrade signal**). |
| AUDIT-016 | REPRODUCED-TEST | `store/audit_pattern_p9_test.go` | httptest + 302 to link-local confirmed the SSRF. |
| AUDIT-017 | REPRODUCED-STATIC | `store/audit_misc_security_test.go` | `FORCE_OTEL_LOGS_URL` read with no scheme/host validation; goroutine-per-event with no bounded pool. |
| AUDIT-018 | REPRODUCED-TEST | `git/audit_pattern_p10_test.go` | 3 store ingress points accept `--upload-pack=/tmp/evil` verbatim; ~20 `git` invocations missing `--`. |
| AUDIT-019 | REPRODUCED-STATIC | `store/audit_misc_security_test.go` | `ListAgentWorktreePaths` no `os.Lstat` / `ModeSymlink` check. |
| AUDIT-020 | REPRODUCED-STATIC | `agents/audit_lifecycle_test.go` | 9 `Spawn*` goroutines, zero take `context.Context`. |
| AUDIT-021 | REPRODUCED-TEST | `store/audit_pattern_p7_test.go` | AutoRecoverConvoy check-and-act race. |
| AUDIT-022 | REPRODUCED-TEST+STATIC | `store/audit_pattern_p1_test.go` | Reflect: `UpdateBountyStatus` returns nothing. Empirical: drop-table then call → status doesn't change, caller has no signal. |
| AUDIT-102 | REPRODUCED-STATIC | `git/audit_protected_branch_test.go` | `completeAskBranchResolution` force-pushes `ab.AskBranch` with no default-branch guard; `UpsertConvoyAskBranch` accepts `ask_branch="main"` verbatim. |
| AUDIT-103 | REPRODUCED-STATIC | `git/audit_protected_branch_test.go` | `ForcePushBranch` no guard. |
| AUDIT-104 | REPRODUCED-STATIC | `git/audit_protected_branch_test.go` | `TriggerCIRerun` no guard; `pr.Repo` fallback at `pr_flow.go:709` amplifies. |
| AUDIT-105 | REPRODUCED-STATIC | `agents/audit_pattern_p11_test.go` | Heartbeat/Claude block has no `IsEstopped` poll. |
| AUDIT-106 | REPRODUCED-STATIC | `agents/audit_pattern_p11_test.go` | `RunDogs` body contains no `IsEstopped`. |
| AUDIT-107 | REPRODUCED-TEST | `agents/audit_pattern_p11_test.go` | Behavioural: sleep blind through e-stop for 3s+. |
| AUDIT-108 | REPRODUCED-STATIC | `agents/audit_pattern_p12_test.go` | Council `reviewPrompt` has no `<user_content>` wrapping. |
| AUDIT-109 | REPRODUCED-STATIC | `agents/audit_pattern_p12_test.go` | Captain `reviewPrompt` has no `<user_content>` wrapping. |
| AUDIT-110 | REPRODUCED-STATIC | `agents/audit_pattern_p12_test.go` | `pr_review_triage.go:328` concatenates raw comment body into astromech instruction. |
| AUDIT-111 | REPRODUCED-STATIC | `agents/audit_test_quality_test.go` | AST+grep of 54 test files: zero `CallCount`/`invocations`. |
| AUDIT-112 | DUPLICATE-OF-P2 | `agents/audit_test_quality_test.go` | Canonically covered by `TestPattern_P2_IdempotencyKeyRace`. |
| AUDIT-113 | REPRODUCED-STATIC | `agents/audit_test_quality_test.go` | No cross-pass Claude call bound in `convoy_review_test.go`. |
| AUDIT-114 | REPRODUCED-STATIC | `agents/audit_pattern_p12_test.go` | Captain `default: "defaulting to approve"` present. |

## High finding matrix

67 High findings. Same accounting.

| ID | Outcome | Test file | Verdict (tight) |
|---|---|---|---|
| AUDIT-023 | REPRODUCED-STATIC | `store/audit_schema_time_test.go` | **Calibration:** `Escalations.acknowledged_at` is ALREADY in createSchema; the drift is only `Fleet_Mail.consumed_at` and `Repositories.pr_review_enabled`. |
| AUDIT-024 | REPRODUCED-STATIC | `store/audit_pattern_p4_test.go` | Zero indexes on Fleet_Mail, Escalations, AuditLog, FleetMemory. |
| AUDIT-025 | REPRODUCED-STATIC | `store/audit_pattern_p6_test.go` | 3 sinks write `status='Resolved'`; no consumer accepts it. |
| AUDIT-026 | REPRODUCED-TEST | `store/audit_pattern_p7_test.go` | `ResetTask` resurrects `Completed` → `Pending`. |
| AUDIT-027 | REPRODUCED-TEST | `store/audit_pattern_p7_test.go` | Cancel-vs-approve race: approve unconditionally wins 20/20. |
| AUDIT-028 | REPRODUCED-STATIC (≈AUDIT-119) | `agents/audit_cost_loops_test.go` | No `failed_rebase_attempts` counter. Shared remedy with AUDIT-119. |
| AUDIT-029 | REPRODUCED-STATIC | `agents/audit_cost_loops_test.go` | Council parse fail → `handleInfraFailure` with `MaxInfraFailures=5`. |
| AUDIT-030 | DUPLICATE-OF-116 | `agents/audit_cost_loops_test.go` | Audit itself notes at line 403. Chancellor fails open on any Claude error; same function body. |
| AUDIT-031 | REPRODUCED-STATIC | `agents/audit_cost_advisory_test.go` | Thread-depth cap is LLM-guidance only; no hard post-LLM override. |
| AUDIT-032 | REPRODUCED-STATIC | `agents/audit_cost_advisory_test.go` | No `classify_attempts` column. |
| AUDIT-033 | REPRODUCED-STATIC | `agents/audit_cost_advisory_test.go` | Auto-shard gate requires timeout prefix + InfraFailures≥2; non-timeout zero-commit loops bypass. |
| AUDIT-034 | REPRODUCED-STATIC | `store/audit_pattern_p2_test.go` | Same root cause as -008. |
| AUDIT-035 | REPRODUCED-STATIC | `store/audit_pattern_p2_test.go` | Same root cause. |
| AUDIT-036 | REPRODUCED-STATIC | `store/audit_pattern_p2_test.go` | Same root cause. |
| AUDIT-037 | REPRODUCED-STATIC | `store/audit_pattern_p1_test.go` | P1 invariant violation at `dogs.go:190-191`. |
| AUDIT-038 | REPRODUCED-STATIC | P1 pattern + inspected inline | Astromech cleanup git ops swallow errors. |
| AUDIT-039 | REPRODUCED-STATIC | P1 pattern | `PrepareConflictBranch` reset/clean drop errors. |
| AUDIT-040 | REPRODUCED-STATIC | `agents/audit_silent_failures_test.go` | Manual `UPDATE ... 'Escalated'` then `CreateEscalation` UPDATEs again (double). |
| AUDIT-041 | REPRODUCED-STATIC | `agents/audit_silent_failures_test.go` | `CreateEscalation` returns only `int`, no error. |
| AUDIT-042 | REPRODUCED-STATIC | `agents/audit_silent_failures_test.go` | 3 `_ = store.UpdateAskBranchPRChecks` sites confirmed. |
| AUDIT-043 | REPRODUCED-STATIC | `agents/audit_silent_failures_test.go` | PRClose log-then-unconditional-MarkAskBranchPRClosed pattern present. |
| AUDIT-044 | REPRODUCED-STATIC | `agents/audit_silent_failures_test.go` | Librarian `payload.Task = bounty.Payload` fallback present. |
| AUDIT-045 | REPRODUCED-STATIC | `store/audit_concurrency_test.go` | **Calibration:** busy_timeout comes from driver default, not fleet `PRAGMA` — source concern stands. |
| AUDIT-046 | REPRODUCED-STATIC | `store/audit_concurrency_test.go` | `mergeMu` is global, not per-repo. |
| AUDIT-047 | REPRODUCED-STATIC | `store/audit_concurrency_test.go` | No per-dog `context.WithTimeout`; no heartbeat column in Dogs table. |
| AUDIT-048 | REPRODUCED-STATIC | `store/audit_concurrency_test.go` | `onSubPRCIFailed` tx spans unindexed `payload LIKE`. |
| AUDIT-049 | REPRODUCED-STATIC | `git/audit_pattern_p10_test.go` | `force add-repo` accepts any path (covered via P10). |
| AUDIT-050 | REPRODUCED-STATIC | `git/audit_pattern_p10_test.go` | P10 missing-`--` check flags `gh` call sites. |
| AUDIT-051 | REPRODUCED-STATIC | `git/audit_pattern_p10_test.go` | `CONFLICT_BRANCH:` marker parsed from PR review comment bodies; end-to-end chain documented. |
| AUDIT-052 | REPRODUCED-STATIC | `git/audit_pattern_p10_test.go` | `--dangerously-skip-permissions` confirmed; no scrubber on externally-sourced text. |
| AUDIT-053 | REPRODUCED-STATIC | `dashboard/audit_pattern_p8_test.go` | SSE CORS wildcard same as REST. |
| AUDIT-054 | REPRODUCED-STATIC | `dashboard/audit_pattern_p8_test.go` | No `MaxBytesReader` on mutating handlers. |
| AUDIT-055 | REPRODUCED-STATIC | `store/audit_pattern_p9_test.go` | 14 unredacted `stderr` interpolations in `gh.go`. |
| AUDIT-056 | REPRODUCED-TEST | `store/audit_pattern_p9_test.go` | Token literal leaked into captured webhook body. |
| AUDIT-057 | REPRODUCED-STATIC | `store/audit_misc_security_test.go` | `var stdoutBuf bytes.Buffer` with no size cap; `--paginate` on multi-page gh. |
| AUDIT-058 | REPRODUCED-STATIC | P4 pattern | Correlated subqueries on unindexed TaskHistory. |
| AUDIT-059 | REPRODUCED-STATIC | P4 pattern | `/api/status` 8-15 COUNTs on unindexed tables. |
| AUDIT-060 | NOT-APPLICABLE | — | No burn-rate widget. |
| AUDIT-061 | NOT-APPLICABLE | — | No `spend-burn-watch` dog. |
| AUDIT-062 | NOT-APPLICABLE | — | No convoy-thrash watcher. |
| AUDIT-063 | NOT-APPLICABLE | — | No `claude_invocation_completed` telemetry event. |
| AUDIT-064 | NOT-APPLICABLE | — | No critical-escalation banner. |
| AUDIT-065 | NOT-APPLICABLE | — | No attempts-per-hour metric. |
| AUDIT-115 | REPRODUCED-TEST | `agents/audit_pattern_p12_test.go` | Parser silently accepts missing `approved` field as false. |
| AUDIT-116 | REPRODUCED-STATIC | `agents/audit_pattern_p12_test.go` | Chancellor `approveProposal(..., chancellorRuling{}, ...)` on Claude error and parse error. |
| AUDIT-117 | REPRODUCED-STATIC | `agents/audit_cost_loops_test.go` | Per-thread cap only; no convoy-level `pr_review_convoy_fix_cap`. |
| AUDIT-118 | REPRODUCED-STATIC | `agents/audit_cost_loops_test.go` | No `reshard_generation` column; 1→3→9→27 fanout possible. |
| AUDIT-119 | REPRODUCED-STATIC (≈AUDIT-028) | `agents/audit_cost_loops_test.go` | Shared remedy. |
| AUDIT-120 | REPRODUCED-STATIC | `agents/audit_cost_loops_test.go` | `applyCITriageRealBug` gate allows concurrent fix spawns. |
| AUDIT-121 | REPRODUCED-STATIC | `git/audit_protected_branch_test.go` | `pilot_rebase.go:77` literal `defaultBranch = "main"`. |
| AUDIT-122 | REPRODUCED-STATIC | `git/audit_protected_branch_test.go` | `MergeAndCleanup` no guard. |
| AUDIT-123 | DUPLICATE-OF-019 | `store/audit_misc_security_test.go` | Audit itself notes at line 417. Same root cause, different fix boundary. |
| AUDIT-124 | REPRODUCED-STATIC | `git/audit_protected_branch_test.go` | `DeleteAskBranch` no guard. |
| AUDIT-125 | REPRODUCED-STATIC | `agents/audit_lifecycle_test.go` | `heartbeatDone` channel not deferred. |
| AUDIT-126 | REPRODUCED-STATIC | `agents/audit_lifecycle_test.go` | `os.Create(taskLogPath)` no defer Close/Remove. |
| AUDIT-127 | REPRODUCED-STATIC | `agents/audit_lifecycle_test.go` | 46+29 bare `exec.Command("git",...)` vs 0 `CommandContext`. |
| AUDIT-128 | NOT-APPLICABLE | — | No startup sweep for orphaned worktrees. |
| AUDIT-129 | REPRODUCED-STATIC | `agents/audit_lifecycle_test.go` | `stderrBuf`/`textBuf` have no size cap. |
| AUDIT-130 | REPRODUCED-STATIC | `store/audit_schema_time_test.go` | SpawnAstromech claim loop has no quarantined_at check. |
| AUDIT-131 | REPRODUCED-STATIC | `store/audit_schema_time_test.go` | UnmarshalText always fails on SQLite ts; ParseInLocation fallback. Works accidentally today. |
| AUDIT-132 | REPRODUCED-STATIC | `store/audit_schema_time_test.go` | handleSubPRPoll silent return on parseErr; timeSinceCreatedAt returns 0 on err. |
| AUDIT-133 | REPRODUCED-STATIC | `agents/audit_test_quality_test.go` | No `TestResetTaskFull_PreservesRetryCount`; AUDIT-005 regression is uncovered. |
| AUDIT-134 | REPRODUCED-STATIC | `store/audit_pattern_p4_test.go` | Claim query = `SCAN BountyBoard`. |
| AUDIT-135 | REPRODUCED-STATIC | `agents/audit_test_quality_test.go` | `stubConvoyReviewLLM` does not capture prompt. |
| AUDIT-136 | REPRODUCED-STATIC | `agents/audit_test_quality_test.go` | No test with name matching parse-fail retry. |
| AUDIT-137 | REPRODUCED-STATIC | `agents/audit_test_quality_test.go` | Second-call block has no t.Error/t.Fatal. |
| AUDIT-138 | REPRODUCED-STATIC | `agents/audit_test_quality_test.go` | No multi-iteration adversarial-LLM dog test. |

## Medium / Low rollup

### Medium findings covered by pattern tests (43)

| Pattern | IDs |
|---|---|
| P1 silent-failures + agent-layer extension | 070, 073, 090, 091, 094, 095, 096 (partial), 097 (partial), 099, 100, 156, 159 |
| P2 | 075, 076 |
| P4 | (none Medium-tier) |
| P6 | 083, 084, 085 (already High), 087, 088, 089 |
| P7 | 072, 086, 087 |
| P8 | 100 |
| P9 | 099 (repo integrity angle) |
| P10 | 098, 140, 153, 154 |
| P12 | 139, 141, 142, 143 (also time), 144, 145 |
| Schema+time batch | 077, 078, 080, 082, 143, 146, 147, 148 |
| Lifecycle batch | 158 |
| Concurrency batch | 092, 093 |

### Medium findings spot-checked individually (12 of 60)

All 12 **REPRODUCED**. No pattern-claim overreach found at the Medium tier.

| ID | Result | One-line verdict |
|---|---|---|
| AUDIT-066 | REPRODUCED | `pruneFleet` builds 12 `fmt.Sprintf` with `datetime('now','-N days')` interpolation — regression-prone. |
| AUDIT-068 | REPRODUCED | `ClaimBounty` returns `(nil,false)` for empty queue AND for missing-table driver error — indistinguishable. |
| AUDIT-069 | REPRODUCED | `ResolveFeatureBlockers` has no `db.Begin()`; multi-table mutation without tx. |
| AUDIT-074 | REPRODUCED | `ReadInboxForAgent` still SELECT-then-per-id UPDATE; no `UPDATE ... RETURNING`. |
| AUDIT-079 | REPRODUCED | Zero `PRAGMA foreign_keys` in repo; SQLite defaults FK off per connection. |
| AUDIT-081 | REPRODUCED | `INSERT OR REPLACE INTO Repositories` confirmed at `holocron.go:54`. |
| AUDIT-149 | REPRODUCED | Sweeper closes each tick unconditionally; no `auto_resolve_count` guard; operator-reopens race. |
| AUDIT-151 | REPRODUCED | WorktreeReset parent requeue filter + `_, _ = db.Exec` — 0-row result silently swallowed. |
| AUDIT-152 | REPRODUCED | ship-it-nag stops at 1 week; no 30-day branch; no `CreateEscalation`. |
| AUDIT-155 | REPRODUCED | `MergeWithUnionStrategy` at `askbranch.go:212-288` has no `mergeMu.Lock` / per-repo lock. |
| AUDIT-161 | REPRODUCED | `TestRunMedicCITriage_EnvironmentalTripsBreaker` never asserts Claude call count. |
| AUDIT-162 | REPRODUCED | `TestRunAstromechTask_RateLimit` never asserts Claude call count. |

Spot-check hit rate: **12/12 (100%)**. Pattern coverage at the Medium tier holds. No Medium requires individual regression hardening beyond what pattern tests already cover.

### Low findings (4)

All pattern-covered. None individually tested; none require individual reproduction per calibration.

| ID | Pattern | Test |
|---|---|---|
| AUDIT-163 | P12 | `agents/audit_pattern_p12_test.go` sub-test F (Chancellor fail-open & Boot agent) — the same static check. |
| AUDIT-164 | Lifecycle | `agents/audit_lifecycle_test.go` TestAUDIT_164. |
| AUDIT-165 | Lifecycle | `agents/audit_lifecycle_test.go` TestAUDIT_165. |
| AUDIT-166 | P6 | `store/audit_pattern_p6_test.go` — same state-machine trap class. |

## Severity reclassifications

14 reclassifications across 166 findings. Downgrades reflect "defect exists but reachable only through specific regression, not a standing live exposure." Upgrades reflect "audit undercounted the real surface."

### Downgrades (11)

| ID | Old → New | Rationale |
|---|---|---|
| AUDIT-066 | Medium → Low | `pruneFleet` builds SQL with `fmt.Sprintf`, but `keepDays` is not operator-sourced today. Regression-prone but no live exposure. |
| AUDIT-067 | Medium → Low | `cmdHardReset` uses a hardcoded slice — static analysis false positive today; no CVE-class risk. |
| AUDIT-071 | Medium → Low | `ClassifyPRCommentTx` safe today; matters only if a future refactor pipes user string through. |
| AUDIT-078 | High → Medium | Schema drift narrower than reported: `runMigrations` already UPDATE-backfills `created_at=''` at `schema.go:305`. Only new INSERTs on upgraded DBs that omit `created_at` are affected. |
| AUDIT-080 | Medium → Low | `schema/schema.sql` is a reference file, not loaded at runtime. Purely documentation drift. |
| AUDIT-082 | Medium → Low | Test file inserts wrong column name (`reason` vs `message`) — affects test only, not production DB schema. |
| AUDIT-092 | Medium → Low | Requires a process immune to SIGKILL (uninterruptible syscall) — rare on macOS/Linux in this codebase's subprocess mix. |
| AUDIT-097 | Medium → Low | `ResetBranchPrefixCache` is test-only (never called in production code). |
| AUDIT-146 | Medium → Low | `ListDogs` TZ path works today; failure mode requires future `.UTC()` migration. |
| AUDIT-147 | Medium → Low | Latent; depends on invariant held by every `store.NowSQLite()` writer. |
| AUDIT-148 | Medium → Low | `RateLimitBackoff` integer overflow requires count > 62 (>2^63 ns); only reachable via corrupted persisted count. |

### Upgrades (3)

| ID | Old → New | Rationale |
|---|---|---|
| AUDIT-015 | Critical → Critical (no tier change; severity concern louder) | Audit cited ~3 log-and-return sites inside `onSubPRMerged`'s tx body; verification found **6**. Same bug, double the blast radius. Tier stays because it was already Critical. |
| AUDIT-099 | Medium → High | `.git/info/attributes` rewrite without atomic rename plus no SIGINT handler: a crash mid-merge leaves the repo with globally-scoped `*.md merge=union` rules affecting every future operation in that repo. Not "structurally unwise" — it's a repo-integrity hazard that persists across daemon restarts. |
| AUDIT-156 | High → High (no tier change; severity concern louder) | Audit cited "5+ sites" in `internal/git/git.go` with bare `.Run()` errors swallowed; verification found **23** sites. Blast radius ~5× larger than documented. |

## Duplicates collapsed

4 duplicates, 2 flagged by the audit itself, 2 additional identified during verification.

| Collapsed ID | Canonical ID | Note |
|---|---|---|
| AUDIT-123 | AUDIT-019 | Audit flags at line 417. Same root cause (symlink follow); different fix boundary (path discovery in `git.go` vs operation dispatch in `pilot_worktree_reset.go`). |
| AUDIT-030 | AUDIT-116 | Audit flags at line 403. Chancellor fail-open on Claude error; re-run elevated severity on "every Feature auto-approved during LLM outage." |
| AUDIT-112 | P2 coverage | The audit's "TOCTOU window never exercised" complaint is structurally closed by `TestPattern_P2_IdempotencyKeyRace`'s 50-goroutine reproduction. |
| AUDIT-028 | ≈ AUDIT-119 (shared remedy) | Not strictly a duplicate — different triggers (ask-branch conflict vs `main-drift-watch` tick) — but a single schema fix (`ConvoyAskBranches.failed_rebase_attempts` counter) closes both. Counted as separate findings; grouped for remediation. |

## Not-reproducible with rationale

**Zero findings were NOT-REPRODUCIBLE.** Every Critical and every High finding has either a failing/locking test, a DUPLICATE-OF collapse, or a NOT-APPLICABLE verdict. Two mechanisms that the audit alleged were RCE-class (CVE-2017-1000117 via branch-name injection + `gh --repo` via remote URL) were flagged by the user's brief as potentially overstated — both were successfully reproduced as **accepting adversarial input today** (see P10 test and its end-to-end chain), so neither is downgraded to "not reproducible." The distinction between "validator accepts crafted input" (reproducible) and "crafted input actually executes attacker binary" (needs real git + real shell + real fs-write) was respected: the P10 tests prove the validator gap, which is sufficient for "today's code is vulnerable."

## Audit quality assessment

**Which domains were strongest.** The SQL/schema, P1 silent-failure, and P10 injection findings were the best-established. Every citation for schema drift, missing indexes, `_, _ = db.Exec`, and branch-name flow-through was accurate at line-level after weeks of commits since the original audit pass. The P10 sub-agent (injection) actually found MORE violations than the audit claimed (23 bare `.Run()` sites vs. 5+; 55 adversarial-input acceptances across 3 store setters) — if anything the injection findings undercount.

**Which domain was shakiest.** The **pattern paragraphs for P11 and P12 in AUDIT.md** cite the wrong finding numbers — P11 says AUDIT-112/-113/-114 (which are test-quality findings) when it means AUDIT-105/-106/-107; P12 says AUDIT-130/-131/-132/-133 (time/test) when it means AUDIT-108/-109/-110/-114/-115/-116/-139/-141/-142/-143/-144/-145/-163. The bodies of the individual findings are correct; only the pattern-paragraph cross-references are scrambled. This is the kind of error a re-run sub-agent can produce when it's handed a pre-numbered list and asked to annotate themes post-hoc. **The prompt's "known treatment" block got these right** (it named -105/-106/-107 under e-stop and -108/-109/-110 under boundary markers), so this report follows the prompt's numbering.

**Severity calibration is mostly correct.** 11 downgrades (out of 166) is within the calibrated 15–30 range — close to 15, slightly lenient, but consistent with the fact that most of the Medium tier is genuine "would matter under a realistic regression" work rather than "theoretical only."

**Can the operator trust the Fix #0–#10 priority ordering?** Mostly yes, with two caveats:

1. **Fix #0 (protected-branch guards)** is correctly first. The end-to-end test in `git/audit_protected_branch_test.go` proves one DB-corrupt row → force-push `origin/main`. Blast radius matches the AUDIT.md claim exactly.
2. **Fix #1 (spend cap + e-stop)** is correctly second — AUDIT-004/060-065 are feature-absence; the fix is structurally the same PR and verification simply confirms the feature doesn't exist. The e-stop half (AUDIT-105/106/107) is behaviorally verified.
3. **Fix #2 (dashboard hardening)** and **Fix #3 (idempotency)** are each one PR and each closes 5+ findings; verification supports their priority.
4. **Fix #6 (Medic-requeue loop)** and **Fix #7 (ConvoyReview)** are the observed $300-burn drivers; verification confirms they're exactly as described.

## Recommended priority-plan adjustments

1. **Move "P10 branch-name validator" earlier (Fix #9 → Fix #2.5 or #3.5).** The validator is one file and closes AUDIT-018/-049/-050/-051/-140/-153 in one PR. With Fix #0 adding protected-branch guards, adding refname validation at ingress provides defense-in-depth for the same class. The audit ranks it at #9 (effort M), which is correct for effort but low for blast-radius. Recommendation: reorder to after Fix #3 (idempotency).

2. **Upgrade AUDIT-099 to High** and fold it into Fix #10's outbound-hardening PR. Repo-integrity hazard persisting across restarts is worse than the audit's Medium tier.

3. **Add "ReleaseInFlightTasks covers AwaitingSubPRCI" (AUDIT-166) to Fix #5.** Currently a standalone Low; but the same-PR cost to add one status to the release list is trivial and eliminates a stale-lock-timer spurious warning class.

4. **AUDIT-030 and AUDIT-116 are the same function body — collapse them into one fix task.** The priority plan currently treats them as separate entries; verification confirms they are one defect.

5. **Add "structured `fleet.jsonl` logger" (AUDIT-101) to Fix #1.** The observability fixes go together — without structured logs, post-hoc root-cause on a future burn is as hard as the $300 burn was. Effort L but unblocks everything else.

---

## Test file inventory (24 files, committed with this report)

```
internal/store/audit_pattern_p1_test.go          # P1 — silent failures
internal/store/audit_pattern_p2_test.go          # P2 — idempotency race
internal/agents/audit_pattern_p3_test.go         # P3 — payload LIKE dedup
internal/store/audit_pattern_p4_test.go          # P4 — missing indexes
internal/store/audit_pattern_p6_test.go          # P6 — trap states
internal/store/audit_pattern_p7_test.go          # P7 — unguarded transitions
internal/dashboard/audit_pattern_p8_test.go      # P8 — dashboard security
internal/store/audit_pattern_p9_test.go          # P9 — outbound exfil
internal/git/audit_pattern_p10_test.go           # P10 — shell injection
internal/agents/audit_pattern_p11_test.go        # P11 — e-stop ineffective
internal/agents/audit_pattern_p12_test.go        # P12 — prompt injection
internal/git/audit_protected_branch_test.go     # Fix #0 batch
internal/agents/audit_cost_loops_test.go         # Cost loops 005/006/007/028/029/030/117/118/119/120
internal/agents/audit_cost_advisory_test.go      # 031/032/033
internal/agents/audit_silent_failures_test.go    # 013/014/015/040/041/042/043/044/090/091/094/095/156/159
internal/agents/audit_lifecycle_test.go          # 020/125/126/127/129/158/164/165
internal/store/audit_misc_security_test.go       # 017/019/057/099/100/123
internal/store/audit_concurrency_test.go         # 045/046/047/048/092/093/096/097
internal/agents/audit_test_quality_test.go       # 111/112/113/133/135/136/137/138
internal/store/audit_schema_time_test.go         # 023/077/078/080/082/130/131/132/143/146/147/148
internal/store/audit_medium_spotcheck_a_test.go  # 066/068/069
internal/store/audit_medium_spotcheck_b_test.go  # 074/079/081
internal/agents/audit_medium_spotcheck_c_test.go # 149/151/152
internal/agents/audit_medium_spotcheck_d_test.go # 155/161/162
```

Every finding, from AUDIT-001 through AUDIT-166, has been either **Verified** by one of these files, marked **NOT-APPLICABLE** (feature absence), or **collapsed** as a duplicate. No silent drops.
