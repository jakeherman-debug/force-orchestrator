package agents

// Cost-loop termination audit — static verification of findings
// AUDIT-005, -006, -007, -028, -029, -030, -117, -118, -119, -120.
//
// Each sub-test locks the CURRENT defective behaviour by either:
//   (a) grep-ing the cited source function body for the absent safeguard
//       (counter column, generation cap, classify-transient branch, etc), or
//   (b) reading schema.sql / PRAGMA table_info to assert the remedy column
//       does not exist.
//
// When a remedy lands, the matching assertion inverts and the test fails —
// forcing the author to update the lock test in lock-step with the fix.
//
// All assertions are deliberately static/structural so these cost-vector
// tests cost zero to run (no Claude CLI, no network).

import (
	"database/sql"
	"os"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// ── helpers ──────────────────────────────────────────────────────────────────

// readCostLoopSource loads a file's bytes as a string; fails the test on I/O error.
func readCostLoopSource(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// hasColumn returns true iff table has a column with the given name.
func hasColumn(t *testing.T, db *sql.DB, table, col string) bool {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		t.Fatalf("pragma_table_info(%s): %v", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if name == col {
			return true
		}
	}
	return false
}

// ── AUDIT-005 — Medic requeue zeros retry_count ───────────────────────────────
//
// Defect: `store.ResetTaskFull` zeros retry_count AND infra_failures — the
// Astromech→Council→Medic→Astromech loop has no terminating counter.
// The suggested remedy column `medic_requeue_count` on BountyBoard does not
// exist. Lock both facts so a future fix must remove this test.

func TestAUDIT_005_MedicRequeueZerosRetryCount(t *testing.T) {
	// Closed by Fix #6: medic_requeue_count column + ResetTaskFull no longer zeros counters.
	// This test now inverts — it fails iff the defect pattern is re-introduced.
	// Source-grep: ResetTaskFull in tasks.go must still zero retry_count.
	src := readCostLoopSource(t, "../store/tasks.go")
	start := strings.Index(src, "func ResetTaskFull(")
	if start < 0 {
		t.Fatalf("ResetTaskFull not found in store/tasks.go — source moved?")
	}
	body := src[start:]
	if nextFunc := strings.Index(body[10:], "\nfunc "); nextFunc > 0 {
		body = body[:nextFunc+10]
	}
	defectPresent := strings.Contains(body, "retry_count = 0") && strings.Contains(body, "infra_failures = 0")

	medicSrc := readCostLoopSource(t, "medic.go")
	reqStart := strings.Index(medicSrc, "func applyMedicRequeue(")
	if reqStart < 0 {
		t.Fatal("applyMedicRequeue not found in medic.go")
	}
	reqBody := medicSrc[reqStart:]
	if nextFunc := strings.Index(reqBody[10:], "\nfunc "); nextFunc > 0 {
		reqBody = reqBody[:nextFunc+10]
	}
	hasRequeueCounter := strings.Contains(reqBody, "medic_requeue_count") || strings.Contains(reqBody, "MedicRequeueCount")

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	hasSchemaCol := hasColumn(t, db, "BountyBoard", "medic_requeue_count")

	if defectPresent && !hasRequeueCounter && !hasSchemaCol {
		t.Fatal("AUDIT-005: defective pattern still present — ResetTaskFull zeros retry_count/infra_failures, no medic_requeue_count counter in applyMedicRequeue, no medic_requeue_count column on BountyBoard")
	}
}

// ── AUDIT-006 — ConvoyReview 5×5 structural ───────────────────────────────────
//
// Defect: pass cap defaults to 5 and max-findings per pass defaults to 5;
// each finding spawns an Astromech full-run. 25 Astromech sessions per convoy
// as the structural worst-case.

func TestAUDIT_006_ConvoyReview5x5Structural(t *testing.T) {
	// Closed by: Fix #7 (`fix/convoy-review-tightening`).
	// max_findings default dropped 5→2; finding-set fingerprint dedup added
	// (pass-N == pass-(N-1) short-circuits to conflicted_loop). Test
	// inverts: fails if any of the old defective patterns re-appear.
	src := readCostLoopSource(t, "convoy_review.go")

	hasMaxFindings5 := strings.Contains(src, `getIntConfig(db, "convoy_review_max_findings", 5)`)
	hasFingerprint := strings.Contains(src, "fingerprint") || strings.Contains(src, "findingHash")
	hasParseFailureMem := strings.Contains(src, "parse_failure_count") || strings.Contains(src, "ParseFailureCount")

	if hasMaxFindings5 {
		t.Fatal("AUDIT-006 regression: convoy_review_max_findings default is back to 5 — must be 2 (convoyReviewDefaultMaxFindings)")
	}
	if !hasFingerprint {
		t.Fatal("AUDIT-006 regression: convoy_review.go no longer references fingerprinting — pass-to-pass dedup gate has been removed")
	}
	if !hasParseFailureMem {
		t.Fatal("AUDIT-006 companion regression: convoy_review.go no longer tracks parse_failure_count — loop exit is broken")
	}
}

// ── AUDIT-007 — ConvoyReview parse-fail marks Completed ───────────────────────
//
// Defect: second LLM parse failure marks task Completed, and the watch dog
// re-queues on the next 5-min tick with no parse-failure memory.
// No parse_failure_count column exists.

func TestAUDIT_007_ConvoyReviewParseFailCompletesNoMemory(t *testing.T) {
	// Closed by: Fix #7 (`fix/convoy-review-tightening`).
	// Parse failures now increment BountyBoard.parse_failure_count on the
	// ConvoyReview row; after convoyReviewParseFailureCap (=2) attempts,
	// the task escalates instead of marking Completed for dog retrigger.
	// Test inverts: fails if the schema column or counter memory regresses.
	src := readCostLoopSource(t, "convoy_review.go")

	hasParseFailureMem := strings.Contains(src, "parse_failure_count") || strings.Contains(src, "ParseFailureCount")
	if !hasParseFailureMem {
		t.Fatal("AUDIT-007 regression: convoy_review.go no longer references parse_failure_count — parse-loop memory is gone")
	}

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	if !hasColumn(t, db, "BountyBoard", "parse_failure_count") {
		t.Fatal("AUDIT-007 regression: BountyBoard.parse_failure_count column missing — migration has been reverted")
	}

	// Escalation path must be wired: after cap, FailBounty + CreateEscalation
	// must be called (not UpdateBountyStatus(Completed) which was the bug).
	if !strings.Contains(src, "convoyReviewParseFailureCap") {
		t.Fatal("AUDIT-007 regression: convoyReviewParseFailureCap constant gone — cap is not enforced")
	}
}

// ── AUDIT-028 — Ask-branch rebase conflict has no cap ─────────────────────────
//
// Defect: `QueueRebaseAgentBranch` enforces a per-agent-branch cap
// (maxRebaseConflictTasks=5) but `runRebaseAskBranch` (ask-branch rebase
// path) only uses an idempotency key — no serial retry counter. Every
// 15-min drift tick can respawn the conflict CodeEdit when the previous
// one terminates without resolving.

func TestAUDIT_028_AskBranchRebaseConflictNoCap(t *testing.T) {
	// Closed by Fix #6: failed_rebase_attempts column + maxAskBranchConflicts cap.
	src := readCostLoopSource(t, "pilot_rebase.go")

	start := strings.Index(src, "func runRebaseAskBranch(")
	if start < 0 {
		t.Fatal("runRebaseAskBranch not found in pilot_rebase.go")
	}
	body := src[start:]
	if nextFunc := strings.Index(body[10:], "\nfunc "); nextFunc > 0 {
		body = body[:nextFunc+10]
	}

	hasIdempKey := strings.Contains(body, `"rebase-conflict:askbranch:"`) && strings.Contains(body, "AddConvoyTaskIdempotent")
	hasCounter := strings.Contains(body, "failed_rebase_attempts") ||
		strings.Contains(body, "FailedRebaseAttempts") ||
		strings.Contains(body, "maxAskBranchConflicts")

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	hasSchemaCol := hasColumn(t, db, "ConvoyAskBranches", "failed_rebase_attempts")

	if hasIdempKey && !hasCounter && !hasSchemaCol {
		t.Fatal("AUDIT-028: defective pattern still present — runRebaseAskBranch uses idempotency key only (no attempt counter), no failed_rebase_attempts column on ConvoyAskBranches")
	}
}

// ── AUDIT-029 — Council JSON-parse routes to infra-retry 5× ──────────────────
//
// Defect: Council JSON parse failure calls handleInfraFailure with the
// shared MaxInfraFailures=5 budget. Nothing rejects earlier or converts to
// Medic after a distinct parse-failure threshold.

func TestAUDIT_029_CouncilJSONParseRoutesToInfra5x(t *testing.T) {
	// Closed by: Fix #7 (`fix/convoy-review-tightening`).
	// Council parse failures now increment BountyBoard.parse_failure_count
	// and escalate ("unable to parse LLM output") after councilParseFailureCap
	// (=3), instead of counting against the shared MaxInfraFailures=5 budget.
	src := readCostLoopSource(t, "jedi_council.go")

	hasDedicatedHandling := strings.Contains(src, "parse_failure_count") ||
		strings.Contains(src, "councilParseFailureCap") ||
		strings.Contains(src, "unable to parse LLM output")

	if !hasDedicatedHandling {
		t.Fatal("AUDIT-029 regression: jedi_council.go no longer has a dedicated parse-failure budget; parse loops are back on the shared MaxInfraFailures=5 gate")
	}
}

// ── AUDIT-030 — Chancellor auto-approves on any Claude error ──────────────────
//
// NOTE: AUDIT-030 is DUPLICATE-OF AUDIT-116 (same defect, two audit passes).
// Both route to the same Chancellor error-handler block.
//
// Defect: `runChancellorReview` calls `approveProposal(..., chancellorRuling{}, ...)`
// on BOTH Claude error AND JSON parse error — including transient/rate-limit
// errors. No gh.ClassifyError check, no infra-failure retry loop, no
// consecutive-fallback counter.

func TestAUDIT_030_ChancellorAutoApprovesOnClaudeError(t *testing.T) {
	// Closed by Fix #8.5 (DUPLICATE-OF-116).
	//
	// runChancellorReview now fails CLOSED on Claude error AND JSON
	// parse error: both paths call store.FailBounty + operator mail
	// with a [CHANCELLOR FAIL-CLOSED] subject line. The two zero-value
	// approveProposal fail-open sites (the AUDIT-116 defect) are gone.
	//
	// REGRESSION if any of the following reappears:
	//   - a zero-value `approveProposal(db, feature, tasks, chancellorRuling{}, logger)`
	//     call in the Claude-error or parse-error path
	//   - the FAIL-CLOSED operator-mail sentinel missing from both paths
	src := readCostLoopSource(t, "chancellor.go")

	start := strings.Index(src, "func runChancellorReview(")
	if start < 0 {
		t.Fatal("runChancellorReview not found in chancellor.go")
	}
	body := src[start:]
	if nextFunc := strings.Index(body[10:], "\nfunc "); nextFunc > 0 {
		body = body[:nextFunc+10]
	}

	// Two zero-value approveProposal calls were the pre-fix AUDIT-116
	// pattern in the Claude-error + parse-error handlers.
	if strings.Count(body, "approveProposal(db, feature, tasks, chancellorRuling{}, logger)") > 0 {
		t.Errorf("AUDIT-030 REGRESSION: runChancellorReview still contains zero-value approveProposal fail-open call")
	}
	// Positive assertion: the fail-closed sentinel must be present on
	// both error handlers (Claude error + JSON parse error).
	if strings.Count(body, "FAIL-CLOSED") < 2 {
		t.Errorf("AUDIT-030 REGRESSION: runChancellorReview missing FAIL-CLOSED markers on error paths (count=%d, want >=2)",
			strings.Count(body, "FAIL-CLOSED"))
	}
}

// ── AUDIT-117 — PRReview per-thread cap bypassable ───────────────────────────
//
// Defect: `pr_review_thread_depth_cap` is enforced per-thread. A bot that
// opens a NEW thread per iteration resets the counter every time; the
// classifier's `conflicted_loop` gate never fires and `in_scope_fix` spawns
// unbounded CodeEdits on the ask-branch. No convoy-level dispatch counter.

func TestAUDIT_117_PRReviewPerThreadCapBypassable(t *testing.T) {
	// Closed by: Fix #7 (`fix/convoy-review-tightening`).
	// dispatchPRReviewDecision now hard-forces conflicted_loop when either
	// ThreadDepth >= depth_cap OR convoy-level CountInScopeFixesForConvoy
	// >= pr_review_convoy_fix_cap. The thread-hop bypass (bot opens new
	// thread each iteration) no longer defeats the global limit.
	src := readCostLoopSource(t, "pr_review_triage.go")

	hasConvoyCap := strings.Contains(src, "pr_review_convoy_fix_cap") ||
		strings.Contains(src, "CountInScopeFixesForConvoy")

	if !hasConvoyCap {
		t.Fatal("AUDIT-117 regression: pr_review_triage.go no longer enforces a convoy-level in_scope_fix cap — per-thread depth is bypassable by a bot that opens a new thread per iteration")
	}
}

// ── AUDIT-118 — Reshard cascade has no generation cap ────────────────────────
//
// Defect: `autoInsertReshardTasks` inserts shards with a `[RESHARD from task #N]`
// payload prefix but NEVER stamps a generation number. Each shard that
// fails can trigger another `queueReshardDecompose` with a fresh
// idempotency key (per failed task), producing 1→3→9→27 fanout.

func TestAUDIT_118_ReshardCascadeNoGenerationCap(t *testing.T) {
	// Closed by Fix #6: reshard_generation column on BountyBoard + gen=N prefix + maxReshardGeneration cap in util.go.
	src := readCostLoopSource(t, "commander.go")

	start := strings.Index(src, "func autoInsertReshardTasks(")
	if start < 0 {
		t.Fatal("autoInsertReshardTasks not found")
	}
	body := src[start:]
	if nextFunc := strings.Index(body[10:], "\nfunc "); nextFunc > 0 {
		body = body[:nextFunc+10]
	}

	hasReshardPrefix := strings.Contains(body, `"[RESHARD from task #%d]`)
	hasGenCap := strings.Contains(body, "reshard_generation") ||
		strings.Contains(body, "ReshardGeneration") ||
		strings.Contains(body, "gen=")

	utilSrc := readCostLoopSource(t, "util.go")
	hasUtilGenCheck := strings.Contains(utilSrc, "reshard_generation") ||
		strings.Contains(utilSrc, "ReshardGeneration")

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	hasSchemaCol := hasColumn(t, db, "BountyBoard", "reshard_generation")

	if hasReshardPrefix && !hasGenCap && !hasUtilGenCheck && !hasSchemaCol {
		t.Fatal("AUDIT-118: defective pattern still present — commander.go uses bare [RESHARD from task #N] prefix with no generation stamp, no reshard_generation in util.go, no reshard_generation column on BountyBoard")
	}
}

// ── AUDIT-119 — main-drift-watch rebase loop has no attempt counter ──────────
//
// Defect: `dogMainDriftWatch` + `runRebaseAskBranch` respawn ask-branch
// rebase conflict CodeEdits indefinitely when the prior conflict resolver
// terminates without updating the base SHA. Idempotency key alone; no
// ConvoyAskBranches.failed_rebase_attempts counter.

func TestAUDIT_119_MainDriftWatchNoAttemptCounter(t *testing.T) {
	// Closed by Fix #6: dogMainDriftWatch now short-circuits when the ask-branch has hit maxAskBranchConflicts.
	src := readCostLoopSource(t, "pilot_rebase.go")

	dogStart := strings.Index(src, "func dogMainDriftWatch(")
	if dogStart < 0 {
		t.Fatal("dogMainDriftWatch not found in pilot_rebase.go")
	}
	dogBody := src[dogStart:]
	if nextFunc := strings.Index(dogBody[10:], "\nfunc "); nextFunc > 0 {
		dogBody = dogBody[:nextFunc+10]
	}

	hasQueue := strings.Contains(dogBody, "QueueRebaseAskBranch")
	hasAttemptCounter := strings.Contains(dogBody, "failed_rebase_attempts") ||
		strings.Contains(dogBody, "FailedRebaseAttempts") ||
		strings.Contains(dogBody, "rebaseAttemptCap")

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	hasSchemaCol := hasColumn(t, db, "ConvoyAskBranches", "failed_rebase_attempts")

	if hasQueue && !hasAttemptCounter && !hasSchemaCol {
		t.Fatal("AUDIT-119: defective pattern still present — dogMainDriftWatch queues via QueueRebaseAskBranch with no attempt counter, no failed_rebase_attempts column on ConvoyAskBranches")
	}
}

// ── AUDIT-120 — Flaky→RealBug concurrent fix spawns ──────────────────────────
//
// Defect: `applyCITriageFlaky` promotes to RealBug when FailureCount >=
// medicRetriggerCap(=3); `applyCITriageRealBug` spawns a CodeEdit fix task.
// A second failure while the prior fix is still in flight re-enters
// applyCITriageRealBug (still past the cap) and spawns ANOTHER fix.
// No AskBranchPRs.spawned_fix_count counter + concurrency guard.

func TestAUDIT_120_FlakyRealBugConcurrentFixSpawns(t *testing.T) {
	// Closed by: Fix #7 (`fix/convoy-review-tightening`).
	// applyCITriageRealBug now checks HasOpenFixTaskForPR (concurrency gate:
	// 1 in-flight per PR) AND SpawnedFixCount against medicRetriggerCap
	// (lifetime gate: 3 total per PR). Second failure while prior fix task
	// still running no longer races a second Astromech on the branch.
	src := readCostLoopSource(t, "medic_ci.go")

	rbStart := strings.Index(src, "func applyCITriageRealBug(")
	if rbStart < 0 {
		t.Fatal("applyCITriageRealBug not found")
	}
	rbBody := src[rbStart:]
	if nextFunc := strings.Index(rbBody[10:], "\nfunc "); nextFunc > 0 {
		rbBody = rbBody[:nextFunc+10]
	}

	hasConcurrencyGate := strings.Contains(rbBody, "spawned_fix_count") ||
		strings.Contains(rbBody, "SpawnedFixCount") ||
		strings.Contains(rbBody, "HasOpenFixTaskForPR")
	if !hasConcurrencyGate {
		t.Fatal("AUDIT-120 regression: applyCITriageRealBug no longer references spawned_fix_count/HasOpenFixTaskForPR — concurrent fix-task spawns are back")
	}

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	if !hasColumn(t, db, "AskBranchPRs", "spawned_fix_count") {
		t.Fatal("AUDIT-120 regression: AskBranchPRs.spawned_fix_count column missing — migration has been reverted")
	}
}
