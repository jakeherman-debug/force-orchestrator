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
	// Source-grep: ResetTaskFull in tasks.go must still zero retry_count.
	src := readCostLoopSource(t, "../store/tasks.go")
	// The body of ResetTaskFull sits between the function marker and the next func.
	start := strings.Index(src, "func ResetTaskFull(")
	if start < 0 {
		t.Fatalf("ResetTaskFull not found in store/tasks.go — source moved?")
	}
	body := src[start:]
	if nextFunc := strings.Index(body[10:], "\nfunc "); nextFunc > 0 {
		body = body[:nextFunc+10]
	}
	if !strings.Contains(body, "retry_count = 0") {
		t.Errorf("ResetTaskFull no longer zeros retry_count — if a counter-preserving path replaced it, delete this lock test")
	}
	if !strings.Contains(body, "infra_failures = 0") {
		t.Errorf("ResetTaskFull no longer zeros infra_failures — lock test may be stale")
	}

	// Source-grep: applyMedicRequeue calls ResetTaskFull with no pre-counter check.
	medicSrc := readCostLoopSource(t, "medic.go")
	reqStart := strings.Index(medicSrc, "func applyMedicRequeue(")
	if reqStart < 0 {
		t.Fatal("applyMedicRequeue not found in medic.go")
	}
	reqBody := medicSrc[reqStart:]
	if nextFunc := strings.Index(reqBody[10:], "\nfunc "); nextFunc > 0 {
		reqBody = reqBody[:nextFunc+10]
	}
	if !strings.Contains(reqBody, "store.ResetTaskFull(db, parent.ID)") {
		t.Errorf("applyMedicRequeue no longer calls ResetTaskFull — lock test may be stale")
	}
	if strings.Contains(reqBody, "medic_requeue_count") || strings.Contains(reqBody, "MedicRequeueCount") {
		t.Errorf("applyMedicRequeue appears to reference a medic_requeue_count cap — remove this lock test; the AUDIT-005 defect is fixed")
	}

	// Schema check: no medic_requeue_count column on BountyBoard.
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	if hasColumn(t, db, "BountyBoard", "medic_requeue_count") {
		t.Errorf("BountyBoard now has medic_requeue_count — AUDIT-005 appears fixed, remove this lock test")
	}
}

// ── AUDIT-006 — ConvoyReview 5×5 structural ───────────────────────────────────
//
// Defect: pass cap defaults to 5 and max-findings per pass defaults to 5;
// each finding spawns an Astromech full-run. 25 Astromech sessions per convoy
// as the structural worst-case.

func TestAUDIT_006_ConvoyReview5x5Structural(t *testing.T) {
	src := readCostLoopSource(t, "convoy_review.go")

	if !strings.Contains(src, `const maxPasses = 5`) {
		t.Errorf("convoy_review.go: expected `const maxPasses = 5` — remedy may have tightened the cap; remove this lock test")
	}
	if !strings.Contains(src, `getIntConfig(db, "convoy_review_max_findings", 5)`) {
		t.Errorf("convoy_review.go: expected default convoy_review_max_findings=5; remedy may have tightened; remove this lock test")
	}

	// Each finding spawns a CodeEdit (full Astromech run). Lock the spawn site.
	if !strings.Contains(src, `store.AddConvoyTask(db, bounty.ID, repo, taskPayload,`) {
		t.Errorf("convoy_review.go: fix-task spawn pattern moved — lock test stale")
	}
	// No fingerprinting / short-circuit on repeated findings.
	if strings.Contains(src, "fingerprint") || strings.Contains(src, "findingHash") {
		t.Errorf("convoy_review.go: fingerprint-based dedup detected — AUDIT-006 remedy landed; remove this lock test")
	}
}

// ── AUDIT-007 — ConvoyReview parse-fail marks Completed ───────────────────────
//
// Defect: second LLM parse failure marks task Completed, and the watch dog
// re-queues on the next 5-min tick with no parse-failure memory.
// No parse_failure_count column exists.

func TestAUDIT_007_ConvoyReviewParseFailCompletesNoMemory(t *testing.T) {
	src := readCostLoopSource(t, "convoy_review.go")

	// Lock the exact "Completed on second parse fail" branch.
	if !strings.Contains(src, `second parse failed`) ||
		!strings.Contains(src, `store.UpdateBountyStatus(db, bounty.ID, "Completed")`) {
		t.Errorf("convoy_review.go: second-parse-failure path changed; lock test stale")
	}
	// No parse-failure memory anywhere in the file.
	if strings.Contains(src, "parse_failure_count") || strings.Contains(src, "ParseFailureCount") {
		t.Errorf("convoy_review.go: parse_failure_count reference detected — AUDIT-007 fix landed; remove this lock test")
	}

	// Schema check: no parse_failure_count column on BountyBoard or Convoys.
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	if hasColumn(t, db, "BountyBoard", "parse_failure_count") ||
		hasColumn(t, db, "Convoys", "parse_failure_count") {
		t.Errorf("parse_failure_count column exists — AUDIT-007 appears fixed; remove this lock test")
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
	src := readCostLoopSource(t, "pilot_rebase.go")

	start := strings.Index(src, "func runRebaseAskBranch(")
	if start < 0 {
		t.Fatal("runRebaseAskBranch not found in pilot_rebase.go")
	}
	body := src[start:]
	if nextFunc := strings.Index(body[10:], "\nfunc "); nextFunc > 0 {
		body = body[:nextFunc+10]
	}

	// Must still spawn via idempotency key only (no counter).
	if !strings.Contains(body, `"rebase-conflict:askbranch:"`) {
		t.Errorf("ask-branch conflict spawn moved; lock test stale")
	}
	if !strings.Contains(body, "AddConvoyTaskIdempotent") {
		t.Errorf("ask-branch conflict no longer uses AddConvoyTaskIdempotent; lock test stale")
	}
	// Expect the absence of any per-convoy / per-ask-branch attempt counter.
	if strings.Contains(body, "failed_rebase_attempts") ||
		strings.Contains(body, "FailedRebaseAttempts") ||
		strings.Contains(body, "maxAskBranchConflicts") {
		t.Errorf("runRebaseAskBranch now has a conflict-attempt counter — AUDIT-028 fix landed; remove this lock test")
	}

	// Schema check: no failed_rebase_attempts column on ConvoyAskBranches.
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	if hasColumn(t, db, "ConvoyAskBranches", "failed_rebase_attempts") {
		t.Errorf("ConvoyAskBranches.failed_rebase_attempts exists — AUDIT-028/AUDIT-119 fix landed")
	}
}

// ── AUDIT-029 — Council JSON-parse routes to infra-retry 5× ──────────────────
//
// Defect: Council JSON parse failure calls handleInfraFailure with the
// shared MaxInfraFailures=5 budget. Nothing rejects earlier or converts to
// Medic after a distinct parse-failure threshold.

func TestAUDIT_029_CouncilJSONParseRoutesToInfra5x(t *testing.T) {
	src := readCostLoopSource(t, "jedi_council.go")

	// Lock the exact parse-fail branch.
	if !strings.Contains(src, "council JSON parse failed") {
		t.Errorf("jedi_council.go: JSON-parse-failure log line moved; lock test stale")
	}
	if !strings.Contains(src, `handleInfraFailure(db, agentName, "council", b, sessionID, msg, "AwaitingCouncilReview", true, logger)`) {
		t.Errorf("jedi_council.go: parse-fail no longer routes to handleInfraFailure(...,AwaitingCouncilReview,true) — lock test stale")
	}
	// Expect NO dedicated parse-failure counter / earlier-reject path.
	if strings.Contains(src, "parse_failure_count") ||
		strings.Contains(src, "councilParseFailureCap") ||
		strings.Contains(src, "unable to parse LLM output") {
		t.Errorf("jedi_council.go: dedicated parse-fail handling present — AUDIT-029 fix landed; remove lock test")
	}

	// Confirm MaxInfraFailures is still the single 5-budget gate.
	if MaxInfraFailures != 5 {
		t.Errorf("MaxInfraFailures=%d; expected 5 for AUDIT-029 structural finding", MaxInfraFailures)
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
	// DUPLICATE-OF: AUDIT-116 (same function body, identical defect).
	src := readCostLoopSource(t, "chancellor.go")

	start := strings.Index(src, "func runChancellorReview(")
	if start < 0 {
		t.Fatal("runChancellorReview not found in chancellor.go")
	}
	body := src[start:]
	if nextFunc := strings.Index(body[10:], "\nfunc "); nextFunc > 0 {
		body = body[:nextFunc+10]
	}

	// Both error branches must still call approveProposal with zero ruling.
	if !strings.Contains(body, "auto-approving") {
		t.Errorf("chancellor.go: auto-approve log line moved; lock test stale")
	}
	if strings.Count(body, "approveProposal(db, feature, tasks, chancellorRuling{}, logger)") < 2 {
		t.Errorf("chancellor.go: expected TWO auto-approve-on-error call sites (Claude error + parse error); AUDIT-030 fix may have landed")
	}

	// No ClassifyError / infra-failure retry on the error path.
	if strings.Contains(body, "gh.ClassifyError") ||
		strings.Contains(body, "ShouldRetry") ||
		strings.Contains(body, "handleInfraFailure") {
		t.Errorf("chancellor.go: error classification or infra-failure retry detected — AUDIT-030/AUDIT-116 fix landed; remove lock test")
	}
	// No consecutive-fallback counter in SystemConfig.
	if strings.Contains(body, "chancellor_auto_approve_fallbacks") ||
		strings.Contains(body, "AwaitingOperatorReview") {
		t.Errorf("chancellor.go: consecutive-fallback counter detected; AUDIT-116 remedy present")
	}
}

// ── AUDIT-117 — PRReview per-thread cap bypassable ───────────────────────────
//
// Defect: `pr_review_thread_depth_cap` is enforced per-thread. A bot that
// opens a NEW thread per iteration resets the counter every time; the
// classifier's `conflicted_loop` gate never fires and `in_scope_fix` spawns
// unbounded CodeEdits on the ask-branch. No convoy-level dispatch counter.

func TestAUDIT_117_PRReviewPerThreadCapBypassable(t *testing.T) {
	src := readCostLoopSource(t, "pr_review_triage.go")

	// Confirm the depth cap is read per-comment (per-thread state).
	if !strings.Contains(src, `getIntConfig(db, "pr_review_thread_depth_cap", 2)`) {
		t.Errorf("pr_review_triage.go: per-thread depth cap read moved; lock test stale")
	}
	if !strings.Contains(src, "c.ThreadDepth") {
		t.Errorf("pr_review_triage.go: classifier no longer references per-thread depth; lock test stale")
	}
	// No convoy-level dispatch cap.
	if strings.Contains(src, "pr_review_convoy_fix_cap") ||
		strings.Contains(src, "convoyFixCount") ||
		strings.Contains(src, "ConvoyFixDispatchCount") {
		t.Errorf("pr_review_triage.go: convoy-level fix cap detected — AUDIT-117 remedy landed; remove lock test")
	}
	// dispatchInScope must not consult any convoy-level counter before spawning.
	inScopeStart := strings.Index(src, "func dispatchInScope(")
	if inScopeStart < 0 {
		t.Fatal("dispatchInScope not found")
	}
	inScopeBody := src[inScopeStart:]
	if nextFunc := strings.Index(inScopeBody[10:], "\nfunc "); nextFunc > 0 {
		inScopeBody = inScopeBody[:nextFunc+10]
	}
	if strings.Contains(inScopeBody, "CountInScopeFixesForConvoy") ||
		strings.Contains(inScopeBody, "convoy_fix_cap") {
		t.Errorf("dispatchInScope now gates on convoy-level count — AUDIT-117 fix landed")
	}
}

// ── AUDIT-118 — Reshard cascade has no generation cap ────────────────────────
//
// Defect: `autoInsertReshardTasks` inserts shards with a `[RESHARD from task #N]`
// payload prefix but NEVER stamps a generation number. Each shard that
// fails can trigger another `queueReshardDecompose` with a fresh
// idempotency key (per failed task), producing 1→3→9→27 fanout.

func TestAUDIT_118_ReshardCascadeNoGenerationCap(t *testing.T) {
	src := readCostLoopSource(t, "commander.go")

	start := strings.Index(src, "func autoInsertReshardTasks(")
	if start < 0 {
		t.Fatal("autoInsertReshardTasks not found")
	}
	body := src[start:]
	if nextFunc := strings.Index(body[10:], "\nfunc "); nextFunc > 0 {
		body = body[:nextFunc+10]
	}
	// Must still use the bare [RESHARD from task #N] prefix with no generation.
	if !strings.Contains(body, `"[RESHARD from task #%d]`) {
		t.Errorf("commander.go: reshard prefix format moved; lock test stale")
	}
	if strings.Contains(body, "reshard_generation") ||
		strings.Contains(body, "ReshardGeneration") ||
		strings.Contains(body, "gen=") {
		t.Errorf("commander.go: generation cap detected — AUDIT-118 fix landed; remove lock test")
	}

	// util.go's queueReshardDecompose must not check generation either.
	utilSrc := readCostLoopSource(t, "util.go")
	if strings.Contains(utilSrc, "reshard_generation") ||
		strings.Contains(utilSrc, "ReshardGeneration") {
		t.Errorf("util.go: reshard generation check present — AUDIT-118 fix landed")
	}

	// Schema: no reshard_generation column on BountyBoard.
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	if hasColumn(t, db, "BountyBoard", "reshard_generation") {
		t.Errorf("BountyBoard.reshard_generation exists — AUDIT-118 fix landed")
	}

	// Behavioural-adjacent: fabricate two Decompose tasks with the INFRA_FAILURE_RESHARD
	// prefix to confirm they are NOT distinguishable by generation — the schema
	// has no column to tell generation 1 from generation 2.
	if _, err := db.Exec(`INSERT INTO BountyBoard (type, status, payload) VALUES
		('Decompose','Pending','[INFRA_FAILURE_RESHARD task 1] shard A'),
		('Decompose','Pending','[INFRA_FAILURE_RESHARD task 2] shard B')`); err != nil {
		t.Fatalf("seed rows: %v", err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE payload LIKE '[INFRA_FAILURE_RESHARD%'`).Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 reshard rows, got %d", count)
	}
	// They are indistinguishable — no column to filter on.
}

// ── AUDIT-119 — main-drift-watch rebase loop has no attempt counter ──────────
//
// Defect: `dogMainDriftWatch` + `runRebaseAskBranch` respawn ask-branch
// rebase conflict CodeEdits indefinitely when the prior conflict resolver
// terminates without updating the base SHA. Idempotency key alone; no
// ConvoyAskBranches.failed_rebase_attempts counter.

func TestAUDIT_119_MainDriftWatchNoAttemptCounter(t *testing.T) {
	src := readCostLoopSource(t, "pilot_rebase.go")

	dogStart := strings.Index(src, "func dogMainDriftWatch(")
	if dogStart < 0 {
		t.Fatal("dogMainDriftWatch not found in pilot_rebase.go")
	}
	dogBody := src[dogStart:]
	if nextFunc := strings.Index(dogBody[10:], "\nfunc "); nextFunc > 0 {
		dogBody = dogBody[:nextFunc+10]
	}
	// Must still just compare SHAs + queue — no attempt counter.
	if !strings.Contains(dogBody, "QueueRebaseAskBranch") {
		t.Errorf("dogMainDriftWatch: queue call moved; lock test stale")
	}
	if strings.Contains(dogBody, "failed_rebase_attempts") ||
		strings.Contains(dogBody, "FailedRebaseAttempts") ||
		strings.Contains(dogBody, "rebaseAttemptCap") {
		t.Errorf("dogMainDriftWatch: attempt counter detected — AUDIT-119 fix landed; remove lock test")
	}

	// Schema: no failed_rebase_attempts column (shared remedy with AUDIT-028).
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	if hasColumn(t, db, "ConvoyAskBranches", "failed_rebase_attempts") {
		t.Errorf("ConvoyAskBranches.failed_rebase_attempts exists — AUDIT-119 fix landed")
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
	src := readCostLoopSource(t, "medic_ci.go")

	// Lock the promotion branch structure.
	rbStart := strings.Index(src, "func applyCITriageRealBug(")
	if rbStart < 0 {
		t.Fatal("applyCITriageRealBug not found")
	}
	rbBody := src[rbStart:]
	if nextFunc := strings.Index(rbBody[10:], "\nfunc "); nextFunc > 0 {
		rbBody = rbBody[:nextFunc+10]
	}
	if !strings.Contains(rbBody, "FailureCount >= medicRetriggerCap") {
		t.Errorf("applyCITriageRealBug: FailureCount >= cap check moved; lock test stale")
	}
	if !strings.Contains(rbBody, "store.AddConvoyTask(db, payload.TaskID, payload.Repo, fixPayload") {
		t.Errorf("applyCITriageRealBug: fix-task spawn moved; lock test stale")
	}
	// No concurrency gate / spawned-fix counter.
	if strings.Contains(rbBody, "spawned_fix_count") ||
		strings.Contains(rbBody, "SpawnedFixCount") ||
		strings.Contains(rbBody, "HasOpenFixTaskForPR") {
		t.Errorf("applyCITriageRealBug: concurrency guard detected — AUDIT-120 fix landed; remove lock test")
	}

	// Schema: no spawned_fix_count column on AskBranchPRs.
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	if hasColumn(t, db, "AskBranchPRs", "spawned_fix_count") {
		t.Errorf("AskBranchPRs.spawned_fix_count exists — AUDIT-120 fix landed; remove lock test")
	}
}
