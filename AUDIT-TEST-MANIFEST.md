# AUDIT Test Manifest

Red-phase tests committed per finding. Each test is gated by `t.Skip("AUDIT-NNN: remove when <fix> lands")` until the fix lands. Fix PR removes the skip; test goes Green; test stays as permanent regression protection.

Every test has a `// Without skip, fails with: ...` comment block directly below the skip line showing the exact failure message captured by removing the skip and running `go test`.

**Total tests committed:** 110 (11 pattern + 99 individual, across 24 files). Current `go test ./... -tags sqlite_fts5` status: **green** (all AUDIT tests skipped).

## Patterns (11 + P5 NOT-APPLICABLE)

| Pattern | Test file | Test name(s) | Fix removes skip |
|---|---|---|---|
| P1 | `internal/store/audit_pattern_p1_test.go` | `TestPattern_P1_UpdateBountyStatusSwallowsDBError` | Fix #8 (no silent failures) |
| P2 | `internal/store/audit_pattern_p2_test.go` | `TestPattern_P2_IdempotencyKeyRace`, `TestPattern_P2_NoUniqueIndex_Static` | Fix #3 (partial UNIQUE idempotency_key) |
| P3 | `internal/agents/audit_pattern_p3_test.go` | `TestPattern_P3_PayloadLikeDedupIsFullScan`, `TestPattern_P3_BoundaryFalsePositive` | Fix #3/#4 (structured convoy_id + index) |
| P4 | `internal/store/audit_pattern_p4_test.go` | `TestPattern_P4_HotTablesMissingIndexes`, `TestPattern_P4_ClaimQueryUsesIndex` | Fix #4 (hot-table indexes) | Closed by: Fix #4 (`fix/hot-table-indexes`) |
| P5 | `internal/agents/spend_cap_test.go` | `TestSpendCap_*`, `TestSpendBurnPattern_*` — feature now exists | Closed by: Fix #1 |
| P6 | `internal/store/audit_pattern_p6_test.go` | `TestPattern_P6_UndocumentedStatusValues` (+ 3 subtests) | Fix #5 (state machine sweepers + Resolved normalization) — **Closed by: Fix #5 (outer + A subtest); B remains pending Fix #5 AUDIT-025 follow-up; C remains pending AUDIT-085** |
| P7 | `internal/store/audit_pattern_p7_test.go` | `TestPattern_P7_ConcurrentCancelVsApproveRace`, `TestPattern_P7_ResetTaskResurrectsCompleted` | Fix #8/#5 (UpdateBountyStatusFrom) |
| P8 | `internal/dashboard/audit_pattern_p8_test.go` | `TestPattern_P8_DashboardBindsAllInterfaces_ServesWildcardCORS` | Fix #2 (dashboard hardening) | Closed by: Fix #2 (`fix/dashboard-hardening`) |
| P9 | `internal/store/audit_pattern_p9_test.go` | `TestPattern_P9_SecretLeaksInOutboundChannels` (+ 3 subtests) | Fix #10 (RedactSecrets + webhook allow-list) | Closed by: Fix #10 (`fix/redact-and-outbound`) |
| P10 | `internal/git/audit_pattern_p10_test.go` | `TestPattern_P10_BranchValidatorsMissing`, `TestPattern_P10_GitInvocationsLackDashDashSeparator` | Fix #9 (validRef + `--` separator) | Closed by: Fix #9 (`fix/ref-path-validators`) |
| P11 | `internal/agents/audit_pattern_p11_test.go` | `TestPattern_P11_EstopDoesNotStopTheWorld` (+ 3 subtests A/B/C for AUDIT-105/106/107) | Fix #1 (effective e-stop) |
| P12 | `internal/agents/audit_pattern_p12_test.go` | `TestPattern_P12_PromptInjectionSurface` (+ 6 subtests A-F) | Fix #8.5 (LLM boundary markers + DisallowUnknownFields) |

## Criticals (35 — 34 verified, 1 NOT-APPLICABLE)

| ID | Test file | Test name | Kind | Fix plan |
|---|---|---|---|---|
| AUDIT-001 | `internal/dashboard/audit_pattern_p8_test.go` | `TestPattern_P8_DashboardBindsAllInterfaces_ServesWildcardCORS` | static | Fix #2 | Closed by: Fix #2 (`fix/dashboard-hardening`) |
| AUDIT-002 | `internal/dashboard/audit_pattern_p8_test.go` | same | static | Fix #2 | Closed by: Fix #2 |
| AUDIT-003 | `internal/dashboard/audit_pattern_p8_test.go` | same | static | Fix #2 | Closed by: Fix #2 |
| AUDIT-004 | `internal/agents/spend_cap_test.go` | `TestSpendCap_*`, `TestDogSpendBurnWatch_*` | unit+integration+feature | Fix #1 | Closed by: Fix #1 (`fix/spend-cap-and-estop`) |
| AUDIT-005 | `internal/agents/audit_cost_loops_test.go` | `TestAUDIT_005_MedicRequeueZerosRetryCount` | static | Fix #6 | Closed by: Fix #6 (`fix/medic-requeue-cap`) |
| AUDIT-006 | `internal/agents/audit_cost_loops_test.go` | `TestAUDIT_006_ConvoyReview5x5Structural` | static | Fix #7 | Closed by: Fix #7 (`fix/convoy-review-tightening`) |
| AUDIT-007 | `internal/agents/audit_cost_loops_test.go` | `TestAUDIT_007_ConvoyReviewParseFailCompletesNoMemory` | static | Fix #7 | Closed by: Fix #7 |
| AUDIT-008 | `internal/store/audit_pattern_p2_test.go` | `TestPattern_P2_IdempotencyKeyRace` | race (50 goroutines) | Fix #3 | Closed by: Fix #3 (`fix/idempotency-unique`) |
| AUDIT-009 | `internal/store/audit_pattern_p4_test.go` | `TestPattern_P4_HotTablesMissingIndexes` | static (PRAGMA) | Fix #4 | Closed by: Fix #4 |
| AUDIT-010 | `internal/store/audit_pattern_p4_test.go` | same | static | Fix #4 | Closed by: Fix #4 |
| AUDIT-011 | `internal/agents/audit_pattern_p3_test.go` | `TestPattern_P3_PayloadLikeDedupIsFullScan` | static (EXPLAIN QUERY PLAN) | Fix #3/#4 | Write-side closed by: Fix #3 (Queue* helpers → idempotency_key + idx_bounty_idem). Read-side (`TestPattern_P3_BoundaryFalsePositive`) remains for Fix #4. |
| AUDIT-012 | `internal/store/audit_pattern_p6_test.go` | `TestPattern_P6_UndocumentedStatusValues/A_*` | static (AST grep) | Fix #5 | Closed by: Fix #5 |
| AUDIT-013 | `internal/agents/audit_silent_failures_test.go` | `TestAUDIT_013_MedicPayloadJSONSwallow` | static | Fix #8 |
| AUDIT-014 | `internal/agents/audit_silent_failures_test.go` | `TestAUDIT_014_WorktreeResetParentRequeueSilent` | static | Fix #8 |
| AUDIT-015 | `internal/agents/audit_silent_failures_test.go` | `TestAUDIT_015_OnSubPRMergedMidTxLogAndReturn` | static | Fix #8 |
| AUDIT-016 | `internal/store/audit_pattern_p9_test.go` | `TestPattern_P9_SecretLeaksInOutboundChannels/A_*` | behavioral (httptest) | Fix #10 | Closed by: Fix #10 (`fix/redact-and-outbound`) |
| AUDIT-017 | `internal/store/audit_misc_security_test.go` | `TestAUDIT_MiscSecurity/AUDIT_017_*` | static | Fix #10 | Closed by: Fix #10 |
| AUDIT-018 | `internal/git/audit_pattern_p10_test.go` | `TestPattern_P10_BranchValidatorsMissing` | behavioral | Fix #9 | Closed by: Fix #9 (`fix/ref-path-validators`) |
| AUDIT-019 | `internal/store/audit_misc_security_test.go` | `TestAUDIT_MiscSecurity/AUDIT_019_*` | static | Fix #9 | Closed by: Fix #9 |
| AUDIT-020 | `internal/agents/audit_lifecycle_test.go` | `TestAUDIT_020_*` | static | Fix #1 | Closed by: Fix #1 |
| AUDIT-021 | `internal/store/audit_pattern_p7_test.go` | `TestPattern_P7_ConcurrentCancelVsApproveRace` | race (20 trials, 20/20 clobbers) | Fix #8 |
| AUDIT-022 | `internal/store/audit_pattern_p1_test.go` | `TestPattern_P1_UpdateBountyStatusSwallowsDBError` | behavioral+static | Fix #8 |
| AUDIT-102 | `internal/git/audit_protected_branch_test.go` | `TestAUDIT_102_103_104_121_122_124_ProtectedBranchGuardsMissing/AUDIT-102/*` | static | Fix #0 | Closed by: Fix #0 (`fix/protected-branch-guard`) |
| AUDIT-103 | same | `.../AUDIT-103/ForcePushBranch` | static | Fix #0 | Closed by: Fix #0 |
| AUDIT-104 | same | `.../AUDIT-104/TriggerCIRerun` | static | Fix #0 | Closed by: Fix #0 |
| AUDIT-105 | `internal/agents/audit_pattern_p11_test.go` | `TestPattern_P11_EstopDoesNotStopTheWorld/AUDIT-105_*` | static | Fix #1 | Closed by: Fix #1 |
| AUDIT-106 | same | `.../AUDIT-106_*` | static | Fix #1 | Closed by: Fix #1 |
| AUDIT-107 | same | `.../AUDIT-107_*` | behavioral (3s budget) | Fix #1 | Closed by: Fix #1 |
| AUDIT-108 | `internal/agents/audit_pattern_p12_test.go` | `TestPattern_P12_PromptInjectionSurface/A_*` | static | Fix #8.5 |
| AUDIT-109 | same | `.../B_*` | static | Fix #8.5 |
| AUDIT-110 | same | (covered by A/B pattern — same boundary) | static | Fix #8.5 |
| AUDIT-111 | `internal/agents/audit_test_quality_test.go` | `TestAuditTestQualityMetaFindings/AUDIT_111_*` | static (AST) | Fix #7 companion | Closed by: Fix #7 |
| AUDIT-112 | `internal/agents/audit_test_quality_test.go` | `TestAuditTestQualityMetaFindings/AUDIT_112_*` | DUPLICATE-OF-P2 | Fix #3 | Closed by: Fix #3 (concurrency coverage added in `internal/store/tasks_idempotent_test.go::TestAddConvoyTaskIdempotent_ConcurrentCallers` — 50-goroutine race, `-race -count=50` clean) |
| AUDIT-113 | `internal/agents/audit_test_quality_test.go` | `TestAuditTestQualityMetaFindings/AUDIT_113_*` | static | Fix #7 | Closed by: Fix #7 |
| AUDIT-114 | `internal/agents/audit_pattern_p12_test.go` | `TestPattern_P12_PromptInjectionSurface/C_*` | static | Fix #8.5 |

## Highs (67 — 57 verified, 7 NOT-APPLICABLE, 3 duplicates)

| ID | Test file | Test name / sub-test | Kind | Fix plan |
|---|---|---|---|---|
| AUDIT-023 | `internal/store/audit_schema_time_test.go` | `TestAUDIT_023_createSchema_drift` | static (PRAGMA) | Fix #4 companion | Closed by: Fix #4 |
| AUDIT-024 | `internal/store/audit_pattern_p4_test.go` | `TestPattern_P4_HotTablesMissingIndexes` | static | Fix #4 | Closed by: Fix #4 |
| AUDIT-025 | `internal/store/audit_pattern_p6_test.go` | `.../B_*` | static grep | Fix #5 | **Still pending** — requires Resolved→Closed normalization pass, not covered by Fix #5's stale-convoys change. Sub-test B skip remains. |
| AUDIT-026 | `internal/store/audit_pattern_p7_test.go` | `TestPattern_P7_ResetTaskResurrectsCompleted` | behavioral | Fix #8 |
| AUDIT-027 | `internal/store/audit_pattern_p7_test.go` | `TestPattern_P7_ConcurrentCancelVsApproveRace` | race | Fix #8 |
| AUDIT-028 | `internal/agents/audit_cost_loops_test.go` | `TestAUDIT_028_AskBranchRebaseConflictNoCap` | static (≈AUDIT-119) | Fix #6/#7 | Closed by: Fix #6 |
| AUDIT-029 | `internal/agents/audit_cost_loops_test.go` | `TestAUDIT_029_CouncilJSONParseRoutesToInfra5x` | static | Fix #7 | Closed by: Fix #7 |
| AUDIT-030 | `internal/agents/audit_cost_loops_test.go` | `TestAUDIT_030_ChancellorAutoApprovesOnClaudeError` | DUPLICATE-OF-116 | Fix #8 |
| AUDIT-031 | `internal/agents/audit_cost_advisory_test.go` | `TestAUDIT_CostAdvisory/TestAUDIT_031_*` | static | Fix #7 | Closed by: Fix #7 |
| AUDIT-032 | `internal/agents/audit_cost_advisory_test.go` | `.../TestAUDIT_032_*` | static (PRAGMA) | Fix #7 | Closed by: Fix #7 |
| AUDIT-033 | `internal/agents/audit_cost_advisory_test.go` | `.../TestAUDIT_033_*` | static | Fix #7 | Closed by: Fix #6 |
| AUDIT-034 | `internal/store/audit_pattern_p2_test.go` | pattern coverage (partial UNIQUE) | race | Fix #3 | Closed by: Fix #3 (partial UNIQUE on `Escalations(task_id) WHERE status='Open'` + ON CONFLICT DO UPDATE merge in `CreateEscalation`; race test at `internal/agents/escalation_race_test.go`) |
| AUDIT-035 | same | pattern coverage | race | Fix #3 | Closed by: Fix #3 (all Queue* helpers migrated to canonical `store.AddIdempotentTask` keys: `convoy-review:<id>`, `worktree-reset:<parent>`, `rebase-agent:<sub_pr_row_id>`, `create-askbranch:<convoyID>`, `rebase-askbranch:<convoy>:<repo>`, `pr-review-triage:<convoyID>`, `ci-failure-triage:<sub_pr_row_id>`) |
| AUDIT-036 | same | pattern coverage | race | Fix #3 | Closed by: Fix #3 (partial UNIQUE on `FeatureBlockers(blocked_convoy_id, blocking_feature_id) WHERE resolved_at IS NULL` + ON CONFLICT DO NOTHING in `CreateFeatureBlocker`) |
| AUDIT-037 | `internal/store/audit_pattern_p1_test.go` | P1 invariant | pattern | Fix #8 |
| AUDIT-038 | same | P1 pattern | pattern | Fix #8 |
| AUDIT-039 | same | P1 pattern | pattern | Fix #8 |
| AUDIT-040 | `internal/agents/audit_silent_failures_test.go` | `TestAUDIT_040_EscalateCITriageDoubleUPDATE` | static | Fix #8 |
| AUDIT-041 | `internal/agents/audit_silent_failures_test.go` | `TestAUDIT_041_CreateEscalationNoErrorReturn` | static (AST) | Fix #8 |
| AUDIT-042 | `internal/agents/audit_silent_failures_test.go` | `TestAUDIT_042_UpdateAskBranchPRChecksDiscarded` | static grep | Fix #8 |
| AUDIT-043 | `internal/agents/audit_silent_failures_test.go` | `TestAUDIT_043_PRCloseUnconditionalMarkClosed` | static | Fix #8 |
| AUDIT-044 | `internal/agents/audit_silent_failures_test.go` | `TestAUDIT_044_LibrarianSilentFallback` | static | Fix #8 |
| AUDIT-045 | `internal/store/audit_concurrency_test.go` | `TestAUDIT_Concurrency/AUDIT_045_*` | static | Fix #4/#8 |
| AUDIT-046 | `internal/store/audit_concurrency_test.go` | `.../AUDIT_046_*` | static | Fix #8 |
| AUDIT-047 | `internal/store/audit_concurrency_test.go` | `.../AUDIT_047_*` | static | Fix #8 |
| AUDIT-048 | `internal/store/audit_concurrency_test.go` | `.../AUDIT_048_*` | static | Fix #3/#4 | Closed by: Fix #3 (`QueueCIFailureTriageTx` routes through `store.AddIdempotentTaskTx` + `idx_bounty_idem`; `onSubPRCIFailed` no longer runs `tx.QueryRow(... payload LIKE)` inside the tx) |
| AUDIT-049 | `internal/git/audit_pattern_p10_test.go` | P10 branch-validator coverage | static | Fix #9 | Closed by: Fix #9 |
| AUDIT-050 | same | P10 `--` separator coverage | static | Fix #9 | Closed by: Fix #9 |
| AUDIT-051 | same | P10 end-to-end chain | static | Fix #9 | Closed by: Fix #9 |
| AUDIT-052 | same | P10 `--dangerously-skip-permissions` | static | Fix #9 | (pattern covers; operator sandboxing deferred) |
| AUDIT-053 | `internal/dashboard/audit_pattern_p8_test.go` | P8 | static | Fix #2 | Closed by: Fix #2 (`fix/dashboard-hardening`) |
| AUDIT-054 | `internal/dashboard/audit_pattern_p8_test.go` | P8 | static | Fix #2 | Closed by: Fix #2 |
| AUDIT-055 | `internal/store/audit_pattern_p9_test.go` | `.../C_GhStderrNotRedacted` | static grep | Fix #10 | Closed by: Fix #10 |
| AUDIT-056 | `internal/store/audit_pattern_p9_test.go` | `.../B_WebhookBodyLeaksTokens` | behavioral (httptest) | Fix #10 | Closed by: Fix #10 |
| AUDIT-057 | `internal/store/audit_misc_security_test.go` | `.../AUDIT_057_*` | static | Fix #10 | Closed by: Fix #10 |
| AUDIT-058 | `internal/store/audit_pattern_p4_test.go` | pattern coverage | static | Fix #4 | Closed by: Fix #4 |
| AUDIT-059 | `internal/store/audit_pattern_p4_test.go` | pattern coverage | static | Fix #4 | Closed by: Fix #4 |
| AUDIT-060 | `internal/dashboard/spend_cap_api_test.go` | `TestAPIStatus_ExposesHourlySpend` | acceptance | Fix #1 | Closed by: Fix #1 |
| AUDIT-061 | `internal/agents/spend_cap_test.go` | `TestDogSpendBurnWatch_AutoEstopsAtHardCap`, `TestSpendBurnPattern_TriggersAutoEstopInOneCycle` | feature+integration | Fix #1 | Closed by: Fix #1 |
| AUDIT-062 | — | NOT-APPLICABLE (no thrash dog) | — | Fix #1 |
| AUDIT-063 | — | NOT-APPLICABLE (no claude event) | — | Fix #1 |
| AUDIT-064 | `internal/dashboard/security_test.go` | `TestFix2_HighEscalationBanner_Present` | static | Fix #2 | Closed by: Fix #2 (`fix/dashboard-hardening`) — banner now exists, formerly NOT-APPLICABLE (feature absence) |
| AUDIT-065 | `internal/dashboard/spend_cap_api_test.go` | `TestAPIStatus_ExposesHourlySpend` (AttemptsLastHour) | acceptance | Fix #1 | Closed by: Fix #1 |
| AUDIT-115 | `internal/agents/audit_pattern_p12_test.go` | `.../D_MissingApprovedField*` | behavioral | Fix #8.5 |
| AUDIT-116 | `internal/agents/audit_pattern_p12_test.go` | `.../F_ChancellorFailsOpen*` | static | Fix #8.5 |
| AUDIT-117 | `internal/agents/audit_cost_loops_test.go` | `TestAUDIT_117_PRReviewPerThreadCapBypassable` | static | Fix #7 | Closed by: Fix #7 |
| AUDIT-118 | `internal/agents/audit_cost_loops_test.go` | `TestAUDIT_118_ReshardCascadeNoGenerationCap` | static | Fix #6 | Closed by: Fix #6 |
| AUDIT-119 | `internal/agents/audit_cost_loops_test.go` | `TestAUDIT_119_MainDriftWatchNoAttemptCounter` | static (≈AUDIT-028) | Fix #6 | Closed by: Fix #6 |
| AUDIT-120 | `internal/agents/audit_cost_loops_test.go` | `TestAUDIT_120_FlakyRealBugConcurrentFixSpawns` | static | Fix #7 | Closed by: Fix #7 |
| AUDIT-121 | `internal/git/audit_protected_branch_test.go` | `.../AUDIT-121/HardcodedMainFallback` | static | Fix #0 | Closed by: Fix #0 |
| AUDIT-122 | `internal/git/audit_protected_branch_test.go` | `.../AUDIT-122/MergeAndCleanup` | static | Fix #0 | Closed by: Fix #0 |
| AUDIT-123 | `internal/store/audit_misc_security_test.go` | `.../AUDIT_123_*` | DUPLICATE-OF-019 | Fix #9 | Closed by: Fix #9 |
| AUDIT-124 | `internal/git/audit_protected_branch_test.go` | `.../AUDIT-124/DeleteAskBranch` | static | Fix #0 | Closed by: Fix #0 |
| AUDIT-125 | `internal/agents/audit_lifecycle_test.go` | `TestAUDIT_125_*` | static | Fix #8 |
| AUDIT-126 | `internal/agents/audit_lifecycle_test.go` | `TestAUDIT_126_*` | static | Fix #8 |
| AUDIT-127 | `internal/agents/audit_lifecycle_test.go` | `TestAUDIT_127_*` | static | Fix #8 |
| AUDIT-128 | — | NOT-APPLICABLE (no orphan sweep) | — | Fix #8 |
| AUDIT-129 | `internal/agents/audit_lifecycle_test.go` | `TestAUDIT_129_*` | static | Fix #8 |
| AUDIT-130 | `internal/store/audit_schema_time_test.go` | `TestAUDIT_130_*` | static | Fix #8 |
| AUDIT-131 | `internal/store/audit_schema_time_test.go` | `TestAUDIT_131_*` | static | Fix #8 |
| AUDIT-132 | `internal/store/audit_schema_time_test.go` | `TestAUDIT_132_*` | static | Fix #8 |
| AUDIT-133 | `internal/agents/audit_test_quality_test.go` | `.../AUDIT_133_*` | static | Fix #6 | Closed by: Fix #6 |
| AUDIT-134 | `internal/store/audit_pattern_p4_test.go` | `TestPattern_P4_ClaimQueryUsesIndex` | static (EXPLAIN) | Fix #4 | Closed by: Fix #4 |
| AUDIT-135 | `internal/agents/audit_test_quality_test.go` | `.../AUDIT_135_*` | static | Fix #7 | Closed by: Fix #7 |
| AUDIT-136 | `internal/agents/audit_test_quality_test.go` | `.../AUDIT_136_*` | static | Fix #7 | Closed by: Fix #7 |
| AUDIT-137 | `internal/agents/audit_test_quality_test.go` | `.../AUDIT_137_*` | static | Fix #8 |
| AUDIT-138 | `internal/agents/audit_test_quality_test.go` | `.../AUDIT_138_*` | static | Fix #7 | Closed by: Fix #7 |

## Medium spot-checks (12 of 60 individually tested)

| ID | Test file | Test name | Kind | Fix plan |
|---|---|---|---|---|
| AUDIT-066 | `internal/store/audit_medium_spotcheck_a_test.go` | `TestAUDIT_066_PruneFleetUnparameterizedInterval` | static grep | Fix #8 companion |
| AUDIT-068 | `internal/store/audit_medium_spotcheck_a_test.go` | `TestAUDIT_068_ClaimBountyConflatesErrNoRowsWithRealErrors` | behavioral | Fix #8 |
| AUDIT-069 | `internal/store/audit_medium_spotcheck_a_test.go` | `TestAUDIT_069_ResolveFeatureBlockersNoTransaction` | static | Fix #8 |
| AUDIT-074 | `internal/store/audit_medium_spotcheck_b_test.go` | `TestAUDIT_MediumSpotcheckB/AUDIT_074_*` | static | Fix #3 | Closed by: Fix #3 (`ReadInboxForAgent` rewritten as single-statement `UPDATE ... RETURNING` — no SELECT-then-per-id UPDATE race window) |
| AUDIT-079 | `internal/store/audit_medium_spotcheck_b_test.go` | `.../AUDIT_079_*` | static grep + live PRAGMA | Fix #4 companion | Closed by: Fix #4 |
| AUDIT-081 | `internal/store/audit_medium_spotcheck_b_test.go` | `.../AUDIT_081_*` | static grep + behavioural | Fix #4 companion | Closed by: Fix #4 |
| AUDIT-149 | `internal/agents/audit_medium_spotcheck_c_test.go` | `TestAuditMediumSpotcheckC/TestAUDIT_149_*` | static | Fix #5 | **Still pending** — requires `Escalations.auto_resolve_count` column + sweeper gate, not covered by Fix #5's stale-convoys change. |
| AUDIT-151 | `internal/agents/audit_medium_spotcheck_c_test.go` | `.../TestAUDIT_151_*` | static | Fix #8 |
| AUDIT-152 | `internal/agents/audit_medium_spotcheck_c_test.go` | `.../TestAUDIT_152_*` | static | Fix #1 | Closed by: Fix #1 |
| AUDIT-155 | `internal/agents/audit_medium_spotcheck_d_test.go` | `TestAuditMedium155_UnionMergeNoRepoLock` | static | Fix #8 |
| AUDIT-161 | `internal/agents/audit_medium_spotcheck_d_test.go` | `TestAuditMedium161_EnvBreakerTestNoCallCountAssert` | static (AST) | Fix #7 companion | Closed by: Fix #7 |
| AUDIT-162 | `internal/agents/audit_medium_spotcheck_d_test.go` | `TestAuditMedium162_RateLimitTestNoCallCountAssert` | static (AST) | Fix #7 companion | Closed by: Fix #7 |

## Medium findings pattern-covered (48 of 60 — no individual test; pattern fix closes them)

These are Medium findings where the pattern test in the table above structurally covers the root cause. The fix for the parent pattern closes these simultaneously. Listed for the operator's reference when the pattern test flips green.

- **P1 (silent failures) covers:** AUDIT-070, -073, -090, -091, -094, -095, -099, -100, -156, -159
- **P2 (idempotency) covers:** AUDIT-075, -076
- **P6 (state machine) covers:** AUDIT-083, -084, -087 — AUDIT-087 (convoy UPDATE source-status guard) *Closed by: Fix #5* via the new `AND status = 'Active'` clause on the mark-Completed / mark-Failed UPDATEs in `runStaleConvoysReport`. AUDIT-083 and -084 remain open (ConflictPending trap state + AwaitingChancellorReview stale-lock flow) — they need their own dog passes.
- **P7 (unguarded transitions) covers:** AUDIT-072, -086
- **P10 (shell injection) covers:** AUDIT-098, -099, -140, -153, -154 — Closed by: Fix #9 (`fix/ref-path-validators`). AUDIT-099 (.git/info/attributes atomic rewrite) still needs a signal handler; kept open for Fix #10.
- **P12 (prompt injection) covers:** AUDIT-139, -141, -142, -143 (also time), -144, -145
- **Concurrency batch covers:** AUDIT-092, -093, -096, -097
- **Schema+time batch covers:** AUDIT-077, -078, -080, -082, -143, -146, -147, -148
- **Lifecycle batch covers:** AUDIT-158, -164, -165

## Low findings (4 of 4 pattern-covered)

| ID | Covered by |
|---|---|
| AUDIT-163 | P12 — `audit_pattern_p12_test.go` |
| AUDIT-164 | Lifecycle batch — `audit_lifecycle_test.go` (TestAUDIT_164) |
| AUDIT-165 | Lifecycle batch — `audit_lifecycle_test.go` (TestAUDIT_165) |
| AUDIT-166 | P6 — `audit_pattern_p6_test.go` — remains open (ReleaseInFlightTasks needs its own fix; stale-convoys dog does not touch `locked_at`) |

## Not committed (NOT-APPLICABLE / DUPLICATE)

| ID | Category | Canonical test (if duplicate) |
|---|---|---|
| AUDIT-004 | Closed by Fix #1 — test now committed | `internal/agents/spend_cap_test.go` |
| AUDIT-060 | Closed by Fix #1 — test now committed | `internal/dashboard/spend_cap_api_test.go` |
| AUDIT-061 | Closed by Fix #1 — test now committed | `internal/agents/spend_cap_test.go` |
| AUDIT-062 | NOT-APPLICABLE (no convoy-thrash dog) | — |
| AUDIT-063 | NOT-APPLICABLE (no claude_invocation_completed event) | — |
| AUDIT-064 | PROMOTED to individual coverage by Fix #2 | see `TestFix2_HighEscalationBanner_Present` |
| AUDIT-065 | Closed by Fix #1 — test now committed | `internal/dashboard/spend_cap_api_test.go` |
| AUDIT-128 | NOT-APPLICABLE (no orphan-worktree sweep) | — |
| AUDIT-019 | Canonical test exists | `TestAUDIT_MiscSecurity/AUDIT_019_worktree_symlink_follow` |
| AUDIT-123 | DUPLICATE-OF-019 | Same test as -019 |
| AUDIT-030 | DUPLICATE-OF-116 | `TestPattern_P12_PromptInjectionSurface/F_ChancellorFailsOpen*` |
| AUDIT-116 | Canonical test exists | Same test as above |
| AUDIT-112 | DUPLICATE-OF-P2 | `TestPattern_P2_IdempotencyKeyRace` |

## Verification invariants

- `go test ./... -tags sqlite_fts5` is **green**: all AUDIT tests skip cleanly; no package fails.
- Every `t.Skip(...)` line is followed by a `// Without skip, fails with:` comment block with 2-5 lines of the captured failure.
- Every test fails without the skip (RGR red-phase). Removing a `t.Skip` line will produce a clear `AUDIT-NNN: <defect> still present` style message pointing at the specific defect the fix must close.
- When a fix lands, the fix PR's one-line change is deleting the matching `t.Skip("AUDIT-NNN: ...")` line. The test then turns green and stays as permanent regression protection.

## Fail-rate notes

- **Concurrency / race tests** (P2 and P7): verified under `-race -count=5` — both are deterministic (start-gate + goroutine sequencing). `TestPattern_P7_ConcurrentCancelVsApproveRace` shows 20/20 clobbers in every run; `TestPattern_P2_IdempotencyKeyRace` produces 5-30 duplicate rows (non-zero is what fails the assertion).
- **Behavioral tests** with time budgets (P11 rate-limit sleep): 3-second wall-clock budget, runs in ~3.01s. Pass/fail deterministic against fleet code that either checks e-stop mid-sleep or doesn't.
- **Static tests** (majority): deterministic; either the cited source has the defect pattern (fails on main) or doesn't (passes; skip can be removed).

## How a fix PR removes tests

1. Fix PR lands the remedy (e.g. partial UNIQUE index on `BountyBoard.idempotency_key`).
2. Fix PR removes the matching `t.Skip("AUDIT-008: ...")` line from `TestPattern_P2_IdempotencyKeyRace`.
3. CI runs the test; it now passes because the defect is closed.
4. The test stays as permanent regression protection — any future code path that re-introduces the race (accidental SELECT-then-INSERT somewhere) will re-fail the test.
5. If multiple AUDIT-NNN tests share a single fix (e.g. Fix #8 closes ~20 silent-failure tests), the fix PR removes all their skip lines in one commit.
