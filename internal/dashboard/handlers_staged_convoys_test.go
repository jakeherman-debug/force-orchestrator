// D5.5 P4 — staged-convoys dashboard handler tests.
//
// Each test uses an in-memory SQLite (store.InitHolocronDSN(":memory:"))
// and dispatches against handleConvoysSubroutes — i.e. the routing layer
// the dashboard mux exposes — so we exercise the full path-parsing and
// dispatch shape, not just the inner handler functions.

package dashboard

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// ── seeds ──────────────────────────────────────────────────────────────────

// sscNewStagedConvoy creates a 3-stage convoy (soak / operator_confirm /
// terminal-no-gate) for the dashboard tests. Returns the convoy id and the
// per-stage row IDs so tests can manipulate stage state directly.
func sscNewStagedConvoy(t *testing.T, db *sql.DB) (convoyID int, stageIDs []int) {
	t.Helper()
	stages := []store.StagedStageSpec{
		{StageNum: 1, Intent: "phase-one-canary", GateType: "soak_minutes", GateConfigJSON: `{"minutes":60}`},
		{StageNum: 2, Intent: "phase-two-rest", GateType: "operator_confirm", GateConfigJSON: `{"prompt":"deploy looks healthy?"}`},
		{StageNum: 3, Intent: "phase-three-cleanup", GateType: "", GateConfigJSON: `{}`},
	}
	cid, sids, err := store.CreateStagedConvoy(db, "staged-convoy",
		store.StagingStrategyStrict, stages)
	if err != nil {
		t.Fatalf("CreateStagedConvoy: %v", err)
	}
	return cid, sids
}

// sscDispatch wraps a request through handleConvoysSubroutes, exercising
// the full path-parsing and method-routing layer the SPA hits. body=""
// means no body (tests use this for GETs); a non-empty body is sent with
// Content-Type: application/json.
func sscDispatch(t *testing.T, db *sql.DB, method, target string, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, target, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, target, nil)
	}
	rec := httptest.NewRecorder()
	handleConvoysSubroutes(db)(rec, req)
	return rec
}

// ── List stages ────────────────────────────────────────────────────────────

func TestListStagesHandler_StagedConvoy_ReturnsAllStages(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := sscNewStagedConvoy(t, db)

	rec := sscDispatch(t, db, http.MethodGet, urlStages(cid), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Stages []StageRowResponse `json:"stages"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v body=%s", err, rec.Body.String())
	}
	if len(resp.Stages) != 3 {
		t.Fatalf("expected 3 stages, got %d", len(resp.Stages))
	}
	// Stages are emitted in stage_num order.
	for i, s := range resp.Stages {
		if s.StageNum != i+1 {
			t.Errorf("stage[%d].stage_num = %d, want %d", i, s.StageNum, i+1)
		}
	}
	// Stage 1 + 2 carry gate types; stage 3 (terminal) gets a NULL gate.
	if resp.Stages[0].GateType == nil || *resp.Stages[0].GateType != "soak_minutes" {
		t.Errorf("stage 1 gate_type = %v, want soak_minutes", resp.Stages[0].GateType)
	}
	if resp.Stages[2].GateType != nil {
		t.Errorf("stage 3 (terminal) should have null gate_type, got %v", resp.Stages[2].GateType)
	}
	// gate_evaluation_status derived from status — Open/Pending → "n/a".
	if got := resp.Stages[0].GateEvaluationStatus; got != "n/a" {
		t.Errorf("stage 1 (Open) gate_evaluation_status = %q, want n/a", got)
	}
}

func TestListStagesHandler_SingleModeConvoy_ReturnsOneStage(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	cid, err := store.CreateConvoy(db, "single-mode")
	if err != nil {
		t.Fatalf("CreateConvoy: %v", err)
	}

	rec := sscDispatch(t, db, http.MethodGet, urlStages(cid), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Stages []StageRowResponse `json:"stages"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Stages) != 1 {
		t.Fatalf("expected 1 stage for single-mode convoy, got %d", len(resp.Stages))
	}
	if resp.Stages[0].GateType != nil {
		t.Errorf("single-mode stage 1 gate_type should be null, got %v", resp.Stages[0].GateType)
	}
}

func TestListStagesHandler_NonexistentConvoy_404(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	rec := sscDispatch(t, db, http.MethodGet, urlStages(9999), "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 for missing convoy, got %d", rec.Code)
	}
}

// ── Stage detail ───────────────────────────────────────────────────────────

func TestStageDetailHandler_HappyPath(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	cid, sids := sscNewStagedConvoy(t, db)

	// Seed an ask-branch + sub-PR for stage 1 so the detail endpoint
	// has real ask_branches / prs to surface.
	if err := store.UpsertConvoyAskBranch(db, cid, "repo-alpha", "force/ask-1-staged", "deadbeef"); err != nil {
		t.Fatalf("UpsertConvoyAskBranch: %v", err)
	}
	if _, err := db.Exec(`UPDATE ConvoyAskBranches SET stage_id = ? WHERE convoy_id = ? AND repo = ?`,
		sids[0], cid, "repo-alpha"); err != nil {
		t.Fatalf("set stage_id: %v", err)
	}
	if _, err := store.CreateAskBranchPR(db, 1234, cid, "repo-alpha", "https://gh/pull/77", 77); err != nil {
		t.Fatalf("CreateAskBranchPR: %v", err)
	}

	rec := sscDispatch(t, db, http.MethodGet, urlStage(cid, 1), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var resp StageDetailResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v body=%s", err, rec.Body.String())
	}
	if resp.Stage.StageNum != 1 {
		t.Errorf("stage_num = %d, want 1", resp.Stage.StageNum)
	}
	if len(resp.AskBranches) != 1 {
		t.Fatalf("expected 1 ask-branch, got %d", len(resp.AskBranches))
	}
	if resp.AskBranches[0].Repo != "repo-alpha" {
		t.Errorf("ask-branch repo = %q, want repo-alpha", resp.AskBranches[0].Repo)
	}
	if len(resp.AskBranches[0].PRs) != 1 || resp.AskBranches[0].PRs[0].PRNumber != 77 {
		t.Errorf("PRs not surfaced: %+v", resp.AskBranches[0].PRs)
	}
	if resp.AuditLog == nil {
		t.Error("AuditLog should be non-nil (empty slice on no rows)")
	}
}

func TestStageDetailHandler_StageNumOutOfRange_404(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	cid, _ := sscNewStagedConvoy(t, db)

	rec := sscDispatch(t, db, http.MethodGet, urlStage(cid, 99), "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 for missing stage_num, got %d", rec.Code)
	}
}

// ── Advance ────────────────────────────────────────────────────────────────

func TestAdvanceStageHandler_HappyPath_WritesSystemConfigAndAuditLog(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	cid, _ := sscNewStagedConvoy(t, db)
	// Move stage 1 to AwaitingGate so the advance is meaningful.
	if _, err := db.Exec(`UPDATE ConvoyStages SET status='AwaitingGate', all_prs_merged_at=datetime('now') WHERE convoy_id=? AND stage_num=1`, cid); err != nil {
		t.Fatalf("set AwaitingGate: %v", err)
	}

	rec := sscDispatch(t, db, http.MethodPost, urlAdvance(cid, 1),
		`{"operator":"jake","reason":"canary metrics look clean"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	// SystemConfig key should be set with operator name + RFC3339 timestamp.
	got := store.GetConfig(db, "stage_advance_"+strconv.Itoa(cid)+"_1", "")
	if !strings.HasPrefix(got, "jake:") {
		t.Errorf("SystemConfig.stage_advance_*_1 = %q, want prefix \"jake:\"", got)
	}

	// AuditLog row landed.
	logs, err := store.ListStageAuditLog(db, cid, 1)
	if err != nil {
		t.Fatalf("ListStageAuditLog: %v", err)
	}
	if len(logs) != 1 || logs[0].Action != store.AuditActionStageAdvance || logs[0].Actor != "jake" {
		t.Errorf("audit log mismatch: %+v", logs)
	}
	if !strings.Contains(logs[0].Detail, "canary metrics look clean") {
		t.Errorf("audit detail missing reason: %q", logs[0].Detail)
	}
}

func TestAdvanceStageHandler_MissingReason_400(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	cid, _ := sscNewStagedConvoy(t, db)

	rec := sscDispatch(t, db, http.MethodPost, urlAdvance(cid, 1),
		`{"operator":"jake","reason":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 on missing reason, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAdvanceStageHandler_MissingOperator_400(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	cid, _ := sscNewStagedConvoy(t, db)

	rec := sscDispatch(t, db, http.MethodPost, urlAdvance(cid, 1),
		`{"operator":"","reason":"because"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 on missing operator, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAdvanceStageHandler_Idempotent_OverwritesTimestamp(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	cid, _ := sscNewStagedConvoy(t, db)
	if _, err := db.Exec(`UPDATE ConvoyStages SET status='AwaitingGate', all_prs_merged_at=datetime('now') WHERE convoy_id=? AND stage_num=1`, cid); err != nil {
		t.Fatalf("set AwaitingGate: %v", err)
	}

	rec1 := sscDispatch(t, db, http.MethodPost, urlAdvance(cid, 1),
		`{"operator":"alice","reason":"first click"}`)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first advance: got %d", rec1.Code)
	}
	first := store.GetConfig(db, "stage_advance_"+strconv.Itoa(cid)+"_1", "")
	rec2 := sscDispatch(t, db, http.MethodPost, urlAdvance(cid, 1),
		`{"operator":"bob","reason":"second click"}`)
	if rec2.Code != http.StatusOK {
		t.Fatalf("second advance: got %d", rec2.Code)
	}
	second := store.GetConfig(db, "stage_advance_"+strconv.Itoa(cid)+"_1", "")
	if !strings.HasPrefix(second, "bob:") {
		t.Errorf("second advance should overwrite key (expected prefix \"bob:\"), got %q", second)
	}
	if first == second {
		t.Errorf("expected first vs second SystemConfig values to differ; both = %q", first)
	}

	// Both audit rows landed (one per click).
	logs, err := store.ListStageAuditLog(db, cid, 1)
	if err != nil {
		t.Fatalf("ListStageAuditLog: %v", err)
	}
	if len(logs) != 2 {
		t.Errorf("expected 2 audit rows after 2 advances, got %d", len(logs))
	}
}

// ── Abort ──────────────────────────────────────────────────────────────────

func TestAbortStageHandler_HappyPath_TransitionsToFailed(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	cid, sids := sscNewStagedConvoy(t, db)

	rec := sscDispatch(t, db, http.MethodPost, urlAbort(cid, 1),
		`{"operator":"jake","reason":"hotfix priority — drop this convoy"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	stage, err := store.GetStage(db, sids[0])
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	if stage.Status != store.StageStatusFailed {
		t.Errorf("stage status = %q, want Failed", stage.Status)
	}
	logs, _ := store.ListStageAuditLog(db, cid, 1)
	if len(logs) != 1 || logs[0].Action != store.AuditActionStageAbort {
		t.Errorf("audit log: expected 1 stage_abort row, got %+v", logs)
	}
}

func TestAbortStageHandler_AlreadyTerminal_400(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	cid, sids := sscNewStagedConvoy(t, db)
	// Force stage 1 to Failed via the store so the handler's terminal-check fires.
	if err := store.AdvanceStage(db, sids[0], store.StageStatusFailed); err != nil {
		t.Fatalf("seed Failed: %v", err)
	}

	rec := sscDispatch(t, db, http.MethodPost, urlAbort(cid, 1),
		`{"operator":"jake","reason":"retry"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for terminal stage, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// ── URL helpers ────────────────────────────────────────────────────────────

func urlStages(cid int) string {
	return "/api/convoys/" + strconv.Itoa(cid) + "/stages"
}
func urlStage(cid, stageNum int) string {
	return "/api/convoys/" + strconv.Itoa(cid) + "/stages/" + strconv.Itoa(stageNum)
}
func urlAdvance(cid, stageNum int) string {
	return urlStage(cid, stageNum) + "/advance"
}
func urlAbort(cid, stageNum int) string {
	return urlStage(cid, stageNum) + "/abort"
}
