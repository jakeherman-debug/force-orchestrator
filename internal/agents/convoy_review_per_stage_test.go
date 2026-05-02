package agents

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// ── D5.5 P2 β — ConvoyReview per-stage scoping + per-stage Senate hook ────
//
// These tests pin the contract of slice β:
//
//  1. Single-mode convoys behave exactly as before (every ask-branch on the
//     convoy is in scope; no per-stage Senate hook fires).
//  2. Staged convoys scope the review to the currently in-flight stage's
//     ask-branches only (cross-stage ask-branches are ignored).
//  3. Stage N (N > 1) reviews against a base SHA recorded at stage-open
//     time, NOT the convoy-creation-time main HEAD. The diff base column
//     `ask_branch_base_sha` is the post-stage-(N-1) main commit by
//     construction in strict mode.
//  4. Each stage's DraftPROpen ConvoyReview fires exactly one SenateReview
//     task scoped to that stage (convoy_id + stage_id payload, no
//     feature_id).
//
// Test infrastructure: in-memory SQLite, the existing seedDraftPROpenConvoy
// helper (single-mode baseline), and a small per-stage seed helper
// (seedStagedConvoyForReview). Stub LLM via stubConvoyReviewLLM so the
// review path runs end-to-end without spending budget.

// seedStagedConvoyForReview creates a 2-stage convoy in staging_mode='staged'
// with one ask-branch per stage on DIFFERENT repos (so the
// UNIQUE(convoy_id, repo) constraint on ConvoyAskBranches doesn't bite).
// Returns (convoyID, stageOneID, stageTwoID).
//
// Stage 1 carries a "force/ask-1-stage1" branch on repo "api"; stage 2 a
// "force/ask-1-stage2" branch on "web" with a distinct base SHA. The
// distinct branch names + repos let the prompt-shape assertions
// distinguish "stage 1's diff is in scope" from "stage 2's diff leaked
// in" — which is the contract slice β is about.
func seedStagedConvoyForReview(t *testing.T, db *sql.DB) (convoyID, stageOneID, stageTwoID int) {
	t.Helper()
	store.AddRepo(db, "api", "/tmp/api", "")
	_ = store.SetRepoRemoteInfo(db, "api", "https://github.com/acme/api.git", "main")
	store.AddRepo(db, "web", "/tmp/web", "")
	_ = store.SetRepoRemoteInfo(db, "web", "https://github.com/acme/web.git", "main")

	// Insert a staged convoy directly so we control the staging_mode field
	// without going through CreateConvoy (which auto-seeds a stage 1 row;
	// we'd then have to either reuse that row or work around the unique
	// constraint on (convoy_id, stage_num)).
	res, err := db.Exec(`INSERT INTO Convoys (name, status, staging_mode, staging_strategy)
		VALUES ('staged-convoy', 'DraftPROpen', 'staged', 'strict')`)
	if err != nil {
		t.Fatalf("insert staged convoy: %v", err)
	}
	id, _ := res.LastInsertId()
	convoyID = int(id)

	// Stage 1 — currently in flight (Open). Stage 2 — Pending (queued behind).
	stageOneID, err = store.CreateStage(db, convoyID, 1, "stage 1: add nullable column", "", "")
	if err != nil {
		t.Fatalf("CreateStage(1): %v", err)
	}
	if err := store.AdvanceStage(db, stageOneID, store.StageStatusOpen); err != nil {
		t.Fatalf("AdvanceStage(1, Open): %v", err)
	}
	stageTwoID, err = store.CreateStage(db, convoyID, 2, "stage 2: dual-write", "", "")
	if err != nil {
		t.Fatalf("CreateStage(2): %v", err)
	}

	// Stage 1 ask-branch on repo `api` (base = pre-stage-1 main).
	_, err = db.Exec(`INSERT INTO ConvoyAskBranches
		(convoy_id, repo, ask_branch, ask_branch_base_sha, stage_id, draft_pr_number, draft_pr_state, draft_pr_url)
		VALUES (?, 'api', 'force/ask-1-stage1', 'sha-pre-stage1', ?, 101, 'Open', 'https://gh/p/101')`,
		convoyID, stageOneID)
	if err != nil {
		t.Fatalf("insert stage-1 ask-branch: %v", err)
	}
	// Stage 2 ask-branch on repo `web` (base = post-stage-1 / pre-stage-2
	// main; in strict mode this is the merge-commit-of-stage-1 SHA).
	_, err = db.Exec(`INSERT INTO ConvoyAskBranches
		(convoy_id, repo, ask_branch, ask_branch_base_sha, stage_id, draft_pr_number, draft_pr_state, draft_pr_url)
		VALUES (?, 'web', 'force/ask-1-stage2', 'sha-merge-of-stage1', ?, 0, '', '')`,
		convoyID, stageTwoID)
	if err != nil {
		t.Fatalf("insert stage-2 ask-branch: %v", err)
	}

	// Pin the adversarial sampling rate to 0 so test assertions about
	// CallCount aren't shaken by the production-default 0.1 ramp.
	db.Exec(`INSERT OR REPLACE INTO SystemConfig (key, value) VALUES ('adversarial_pairing_rate', '0')`)
	return convoyID, stageOneID, stageTwoID
}

// runConvoyReviewWithStub seeds a ConvoyReview bounty row, runs the handler,
// and returns the bounty ID. Centralises the boilerplate so each test reads
// just the assertions.
func runConvoyReviewWithStub(t *testing.T, db *sql.DB, convoyID int, bountyID int64, llmResult convoyReviewResult) {
	t.Helper()
	stubConvoyReviewLLM(t, llmResult)
	payload, _ := json.Marshal(convoyReviewPayload{ConvoyID: convoyID})
	bounty := &store.Bounty{ID: int(bountyID), Type: "ConvoyReview", Payload: string(payload)}
	if _, err := db.Exec(`INSERT INTO BountyBoard (id, parent_id, target_repo, type, status, payload, convoy_id, priority, created_at)
		VALUES (?, 0, '', 'ConvoyReview', 'Locked', ?, ?, 5, datetime('now'))`,
		bountyID, string(payload), convoyID); err != nil {
		t.Fatalf("seed ConvoyReview bounty: %v", err)
	}
	runConvoyReview(context.Background(), db, "Diplomat-1", bounty,
		mustLoadCapProfile(t, "convoy-review"), testLogger{})
}

// TestConvoyReview_SingleMode_UnchangedBehavior — slice β contract #1.
//
// A single-stage (legacy) convoy reviews ALL its ask-branches regardless
// of their stage_id, and the per-stage Senate hook does NOT fire (the
// legacy Feature-scoped Senate hook covers single-mode convoys).
func TestConvoyReview_SingleMode_UnchangedBehavior(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := seedDraftPROpenConvoy(t, db)
	// Sanity: convoy is single-mode by default.
	c := store.GetConvoy(db, convoyID)
	if c == nil {
		t.Fatal("GetConvoy: nil")
	}
	if c.StagingMode != store.StagingModeSingle {
		t.Fatalf("seedDraftPROpenConvoy created %q convoy, want %q",
			c.StagingMode, store.StagingModeSingle)
	}

	stub := stubConvoyReviewLLM(t, convoyReviewResult{Status: "clean", Findings: nil})
	payload, _ := json.Marshal(convoyReviewPayload{ConvoyID: convoyID})
	bounty := &store.Bounty{ID: 9001, Type: "ConvoyReview", Payload: string(payload)}
	db.Exec(`INSERT INTO BountyBoard (id, parent_id, target_repo, type, status, payload, convoy_id, priority, created_at)
		VALUES (9001, 0, '', 'ConvoyReview', 'Locked', ?, ?, 5, datetime('now'))`,
		string(payload), convoyID)

	runConvoyReview(context.Background(), db, "Diplomat-1", bounty,
		mustLoadCapProfile(t, "convoy-review"), testLogger{})

	// LLM call MUST have run (one prompt captured).
	if got := stub.CallCount(); got != 1 {
		t.Fatalf("LLM CallCount = %d, want 1", got)
	}
	// The prompt must NOT carry a "Stage N intent:" prefix — single-mode
	// convoys skip the per-stage prefix.
	if strings.Contains(stub.LastPrompt(), "Stage 1 intent:") {
		t.Errorf("single-mode prompt unexpectedly carries stage prefix:\n%s", stub.LastPrompt())
	}

	// Per-stage Senate hook MUST NOT have queued anything for single-mode.
	var senateRows int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard
		WHERE type = 'SenateReview' AND convoy_id = ?`, convoyID).Scan(&senateRows)
	if senateRows != 0 {
		t.Errorf("single-mode convoy queued %d SenateReview row(s); want 0 (hook should not fire)",
			senateRows)
	}

	// Original ConvoyReview bounty completed cleanly.
	var status string
	db.QueryRow(`SELECT status FROM BountyBoard WHERE id = 9001`).Scan(&status)
	if status != "Completed" {
		t.Errorf("ConvoyReview status = %q, want Completed", status)
	}
}

// TestConvoyReview_StagedMode_ScopedToCurrentStage — slice β contract #2.
//
// A staged convoy with stage 1 Open and stage 2 Pending: the review walks
// only stage 1's ask-branches. The prompt's diff block must reference the
// stage-1 branch and NOT the stage-2 branch.
func TestConvoyReview_StagedMode_ScopedToCurrentStage(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, stageOneID, stageTwoID := seedStagedConvoyForReview(t, db)

	stub := stubConvoyReviewLLM(t, convoyReviewResult{Status: "clean", Findings: nil})
	payload, _ := json.Marshal(convoyReviewPayload{ConvoyID: convoyID})
	bounty := &store.Bounty{ID: 9100, Type: "ConvoyReview", Payload: string(payload)}
	db.Exec(`INSERT INTO BountyBoard (id, parent_id, target_repo, type, status, payload, convoy_id, priority, created_at)
		VALUES (9100, 0, '', 'ConvoyReview', 'Locked', ?, ?, 5, datetime('now'))`,
		string(payload), convoyID)

	runConvoyReview(context.Background(), db, "Diplomat-1", bounty,
		mustLoadCapProfile(t, "convoy-review"), testLogger{})

	if got := stub.CallCount(); got != 1 {
		t.Fatalf("LLM CallCount = %d, want 1", got)
	}
	prompt := stub.LastPrompt()

	// Stage prefix is present (stage 1 intent).
	if !strings.Contains(prompt, "Stage 1 intent: stage 1: add nullable column") {
		t.Errorf("prompt missing stage 1 intent prefix:\n%s", prompt)
	}
	// Stage 1 ask-branch is in the diff block; stage 2 is NOT.
	if !strings.Contains(prompt, "force/ask-1-stage1") {
		t.Errorf("prompt missing stage-1 ask-branch reference:\n%s", prompt)
	}
	if strings.Contains(prompt, "force/ask-1-stage2") {
		t.Errorf("prompt unexpectedly references stage-2 ask-branch (cross-stage leak):\n%s", prompt)
	}
	// Stage prefix references stage 1 specifically — stage 2's intent
	// should not appear in the prompt body anywhere.
	if strings.Contains(prompt, "stage 2: dual-write") {
		t.Errorf("prompt unexpectedly references stage-2 intent (cross-stage leak):\n%s", prompt)
	}

	// Sanity that the seed actually stamped distinct stage_ids — guards
	// against test-fixture drift breaking the scoping assertion silently.
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM ConvoyAskBranches WHERE convoy_id = ? AND stage_id = ?`,
		convoyID, stageOneID).Scan(&n)
	if n != 1 {
		t.Errorf("expected 1 ConvoyAskBranches row pinned to stage 1, got %d", n)
	}
	db.QueryRow(`SELECT COUNT(*) FROM ConvoyAskBranches WHERE convoy_id = ? AND stage_id = ?`,
		convoyID, stageTwoID).Scan(&n)
	if n != 1 {
		t.Errorf("expected 1 ConvoyAskBranches row pinned to stage 2, got %d", n)
	}
}

// TestConvoyReview_StagedMode_StageN_BaseIsPriorMerge — slice β contract #3.
//
// When the in-flight stage is stage 2, the ask-branch's recorded
// `ask_branch_base_sha` must be the post-stage-1 merge commit, not the
// pre-stage-1 main HEAD. The prompt's "(vs base ...)" marker exposes the
// truncated base SHA we used for the diff command.
func TestConvoyReview_StagedMode_StageN_BaseIsPriorMerge(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, stageOneID, stageTwoID := seedStagedConvoyForReview(t, db)

	// Promote stage 1 to Verified and stage 2 to Open so stage 2 is the
	// in-flight stage. AdvanceStage enforces the linear progression so we
	// have to step through each intermediate.
	for _, status := range []string{
		store.StageStatusAllPRsMerged,
		store.StageStatusAwaitingGate,
		store.StageStatusGatePassed,
		store.StageStatusVerified,
	} {
		if err := store.AdvanceStage(db, stageOneID, status); err != nil {
			t.Fatalf("AdvanceStage(stage 1 → %s): %v", status, err)
		}
	}
	if err := store.AdvanceStage(db, stageTwoID, store.StageStatusOpen); err != nil {
		t.Fatalf("AdvanceStage(stage 2 → Open): %v", err)
	}
	// Mark stage 2's draft PR Open so it's reviewable.
	db.Exec(`UPDATE ConvoyAskBranches SET draft_pr_number = 102, draft_pr_state = 'Open',
		draft_pr_url = 'https://gh/p/102'
		WHERE stage_id = ?`, stageTwoID)

	stub := stubConvoyReviewLLM(t, convoyReviewResult{Status: "clean", Findings: nil})
	payload, _ := json.Marshal(convoyReviewPayload{ConvoyID: convoyID})
	bounty := &store.Bounty{ID: 9200, Type: "ConvoyReview", Payload: string(payload)}
	db.Exec(`INSERT INTO BountyBoard (id, parent_id, target_repo, type, status, payload, convoy_id, priority, created_at)
		VALUES (9200, 0, '', 'ConvoyReview', 'Locked', ?, ?, 5, datetime('now'))`,
		string(payload), convoyID)

	runConvoyReview(context.Background(), db, "Diplomat-1", bounty,
		mustLoadCapProfile(t, "convoy-review"), testLogger{})

	if got := stub.CallCount(); got != 1 {
		t.Fatalf("LLM CallCount = %d, want 1", got)
	}
	prompt := stub.LastPrompt()

	// Stage 2 intent is in the prefix.
	if !strings.Contains(prompt, "Stage 2 intent: stage 2: dual-write") {
		t.Errorf("prompt missing stage 2 intent prefix:\n%s", prompt)
	}
	// The diff block's base marker is the truncated stage-2 base SHA
	// ("sha-merge-of-stage1" → first 12 chars). The pre-stage-1 SHA
	// MUST NOT appear in any base marker — that would mean stage 2's
	// review accidentally diffed against the convoy-creation-time main.
	if !strings.Contains(prompt, "sha-merge-of") {
		t.Errorf("prompt missing stage-2 base SHA marker (sha-merge-of...):\n%s", prompt)
	}
	if strings.Contains(prompt, "sha-pre-stage") {
		t.Errorf("prompt unexpectedly references pre-stage-1 base SHA — stage 2 reviewed against wrong base:\n%s", prompt)
	}
	// Only the stage-2 ask-branch should appear; stage-1 was Verified so
	// its branch is out of scope for stage-2's review.
	if strings.Contains(prompt, "force/ask-1-stage1") {
		t.Errorf("prompt unexpectedly references stage-1 ask-branch (cross-stage leak after stage 1 verified):\n%s", prompt)
	}
	if !strings.Contains(prompt, "force/ask-1-stage2") {
		t.Errorf("prompt missing stage-2 ask-branch reference:\n%s", prompt)
	}
}

// TestConvoyReview_StagedMode_PerStageSenateHook_Fires — slice β contract #4.
//
// Each ConvoyReview pass on a staged convoy queues exactly one stage-scoped
// SenateReview task (payload carries convoy_id + stage_id; no feature_id).
// Re-running ConvoyReview for the same stage is idempotent — only one
// SenateReview row exists.
func TestConvoyReview_StagedMode_PerStageSenateHook_Fires(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, stageOneID, _ := seedStagedConvoyForReview(t, db)

	stubConvoyReviewLLM(t, convoyReviewResult{Status: "clean", Findings: nil})
	payload, _ := json.Marshal(convoyReviewPayload{ConvoyID: convoyID})
	bounty := &store.Bounty{ID: 9300, Type: "ConvoyReview", Payload: string(payload)}
	db.Exec(`INSERT INTO BountyBoard (id, parent_id, target_repo, type, status, payload, convoy_id, priority, created_at)
		VALUES (9300, 0, '', 'ConvoyReview', 'Locked', ?, ?, 5, datetime('now'))`,
		string(payload), convoyID)

	runConvoyReview(context.Background(), db, "Diplomat-1", bounty,
		mustLoadCapProfile(t, "convoy-review"), testLogger{})

	// Exactly one SenateReview row was queued, scoped to (convoyID, stageOneID).
	rows, err := db.Query(`SELECT id, payload, status FROM BountyBoard
		WHERE type = 'SenateReview' AND convoy_id = ? ORDER BY id ASC`, convoyID)
	if err != nil {
		t.Fatalf("query SenateReview rows: %v", err)
	}
	defer rows.Close()
	count := 0
	var firstPayload, firstStatus string
	for rows.Next() {
		var id int
		var p, s string
		if err := rows.Scan(&id, &p, &s); err != nil {
			t.Fatalf("scan SenateReview row: %v", err)
		}
		if count == 0 {
			firstPayload = p
			firstStatus = s
		}
		count++
	}
	if count != 1 {
		t.Fatalf("expected 1 stage-scoped SenateReview, got %d", count)
	}
	if firstStatus != "Pending" {
		t.Errorf("SenateReview status = %q, want Pending", firstStatus)
	}
	// Payload carries stage_id pointing at stage 1 and convoy_id at the convoy.
	var p struct {
		ConvoyID  int `json:"convoy_id"`
		StageID   int `json:"stage_id"`
		FeatureID int `json:"feature_id"`
	}
	if err := json.Unmarshal([]byte(firstPayload), &p); err != nil {
		t.Fatalf("parse SenateReview payload: %v (raw=%s)", err, firstPayload)
	}
	if p.ConvoyID != convoyID {
		t.Errorf("payload convoy_id = %d, want %d", p.ConvoyID, convoyID)
	}
	if p.StageID != stageOneID {
		t.Errorf("payload stage_id = %d, want %d", p.StageID, stageOneID)
	}
	if p.FeatureID != 0 {
		t.Errorf("payload feature_id = %d, want 0 (stage-scoped review never carries feature_id)", p.FeatureID)
	}

	// Re-run ConvoyReview a second time (simulating the dog requeueing
	// after a fix-task batch completes). The Senate hook must remain
	// idempotent — still exactly one row, not two. Use a far-out bounty
	// id so we don't collide with whatever auto-incremented row IDs the
	// first pass + its SenateReview spawn left behind.
	runConvoyReviewWithStub(t, db, convoyID, 99301, convoyReviewResult{Status: "clean"})
	var dedupCount int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type = 'SenateReview' AND convoy_id = ?`,
		convoyID).Scan(&dedupCount)
	if dedupCount != 1 {
		t.Errorf("Senate hook re-fired: SenateReview count = %d, want 1 (idempotent)", dedupCount)
	}
}

// TestConvoyReview_StagedMode_PerStageSenateHook_FiresOnNeedsWork — when the
// LLM returns needs_work and fix tasks spawn, the per-stage Senate hook
// still fires. The hook is verdict-independent.
func TestConvoyReview_StagedMode_PerStageSenateHook_FiresOnNeedsWork(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, stageOneID, _ := seedStagedConvoyForReview(t, db)

	stubConvoyReviewLLM(t, convoyReviewResult{
		Status: "needs_work",
		Findings: []convoyReviewFinding{
			{Type: "gap", Description: "missing X", Fix: "add X to api", Repo: "api"},
		},
	})
	payload, _ := json.Marshal(convoyReviewPayload{ConvoyID: convoyID})
	bounty := &store.Bounty{ID: 9400, Type: "ConvoyReview", Payload: string(payload)}
	db.Exec(`INSERT INTO BountyBoard (id, parent_id, target_repo, type, status, payload, convoy_id, priority, created_at)
		VALUES (9400, 0, '', 'ConvoyReview', 'Locked', ?, ?, 5, datetime('now'))`,
		string(payload), convoyID)

	runConvoyReview(context.Background(), db, "Diplomat-1", bounty,
		mustLoadCapProfile(t, "convoy-review"), testLogger{})

	// One fix task was spawned (the LLM finding).
	var fixCount int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE parent_id = 9400 AND type = 'CodeEdit'`).Scan(&fixCount)
	if fixCount != 1 {
		t.Errorf("expected 1 fix task on needs_work, got %d", fixCount)
	}

	// And the per-stage Senate hook fired exactly once.
	var senateCount int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type = 'SenateReview' AND convoy_id = ?`,
		convoyID).Scan(&senateCount)
	if senateCount != 1 {
		t.Errorf("Senate hook count = %d, want 1 (must fire on needs_work too)", senateCount)
	}
	// Payload pinned to stage 1.
	var stageID int
	db.QueryRow(`SELECT json_extract(payload, '$.stage_id') FROM BountyBoard
		WHERE type = 'SenateReview' AND convoy_id = ?`, convoyID).Scan(&stageID)
	if stageID != stageOneID {
		t.Errorf("Senate row payload stage_id = %d, want %d", stageID, stageOneID)
	}
}

// TestQueueStageSenateReview_Idempotent locks the dedup contract directly:
// two consecutive calls land exactly one row.
func TestQueueStageSenateReview_Idempotent(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, _ := store.CreateConvoy(db, "dedup-test")
	// CreateConvoy auto-seeds stage 1, so target stage 2 here.
	stageID, err := store.CreateStage(db, convoyID, 2, "intent", "", "")
	if err != nil {
		t.Fatalf("CreateStage: %v", err)
	}

	id1, err := store.QueueStageSenateReview(db, convoyID, stageID)
	if err != nil || id1 == 0 {
		t.Fatalf("first queue: id=%d err=%v", id1, err)
	}
	id2, err := store.QueueStageSenateReview(db, convoyID, stageID)
	if err != nil {
		t.Fatalf("second queue: %v", err)
	}
	if id2 != 0 {
		t.Errorf("expected dedup (id=0) on second queue, got id=%d", id2)
	}
	// Validation: bad inputs reject.
	if _, err := store.QueueStageSenateReview(db, 0, stageID); err == nil {
		t.Errorf("expected error on convoyID=0")
	}
	if _, err := store.QueueStageSenateReview(db, convoyID, 0); err == nil {
		t.Errorf("expected error on stageID=0")
	}
}

// TestRunStageScopedSenateReview_CompletesOnGoodPayload drives the Senate
// claim handler through a stage-scoped task and asserts it completes
// without firing the Feature-scoped path.
func TestRunStageScopedSenateReview_CompletesOnGoodPayload(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, _ := store.CreateConvoy(db, "stage-handler-test")
	// CreateConvoy auto-seeds stage 1, so add a fresh stage 2 here.
	stageID, err := store.CreateStage(db, convoyID, 2, "test stage intent", "", "")
	if err != nil {
		t.Fatalf("CreateStage: %v", err)
	}
	taskID, err := store.QueueStageSenateReview(db, convoyID, stageID)
	if err != nil || taskID == 0 {
		t.Fatalf("queue: %v %d", err, taskID)
	}

	// Load the bounty for the handler.
	var b store.Bounty
	var pl, status sql.NullString
	if err := db.QueryRow(`SELECT id, payload, status FROM BountyBoard WHERE id = ?`,
		taskID).Scan(&b.ID, &pl, &status); err != nil {
		t.Fatalf("load bounty: %v", err)
	}
	b.Payload = pl.String
	b.Type = "SenateReview"

	var p senateReviewPayload
	if err := json.Unmarshal([]byte(b.Payload), &p); err != nil {
		t.Fatalf("parse payload: %v", err)
	}

	runStageScopedSenateReview(context.Background(), db, "Senate-test", &b, p, testLogger{})

	var got string
	db.QueryRow(`SELECT status FROM BountyBoard WHERE id = ?`, taskID).Scan(&got)
	if got != "Completed" {
		t.Errorf("status = %q, want Completed", got)
	}
	// Audit row recorded.
	var auditCount int
	db.QueryRow(`SELECT COUNT(*) FROM AuditLog WHERE action = 'stage-senate-review' AND task_id = ?`,
		taskID).Scan(&auditCount)
	if auditCount != 1 {
		t.Errorf("expected 1 stage-senate-review audit row, got %d", auditCount)
	}
}
