package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"force-orchestrator/internal/store"
)

func TestHandleShipSummary_NotFound(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	handler := handleConvoysSubroutes(db)
	req := httptest.NewRequest(http.MethodGet, "/api/convoys/999/ship-summary", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleShipSummary_WrongStatus(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := store.CreateConvoy(db, "[1] wrong-status-test")
	// Status defaults to Active — not DraftPROpen

	handler := handleConvoysSubroutes(db)
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/convoys/%d/ship-summary", cid), nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleShipSummary_OK(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := store.CreateConvoy(db, "[1] ship-summary-ok")
	_ = store.SetConvoyStatus(db, cid, "DraftPROpen")

	// Two tasks: one completed, one pending
	t1, _ := store.AddConvoyTask(db, 0, "api", "task-one", cid, 0, "Completed")
	t2, _ := store.AddConvoyTask(db, 0, "api", "task-two", cid, 0, "Pending")

	// Ask-branch with draft PR
	_ = store.UpsertConvoyAskBranch(db, cid, "api", "force/ask-1-ship-summary-ok", "abc123")
	_ = store.SetConvoyAskBranchDraftPR(db, cid, "api", "https://github.com/acme/api/pull/42", 42, "Open")

	// Sub-PRs: one merged (checks pending), one open (checks success)
	pr1, _ := store.CreateAskBranchPR(db, t1, cid, "api", "https://github.com/acme/api/pull/1", 1)
	pr2, _ := store.CreateAskBranchPR(db, t2, cid, "api", "https://github.com/acme/api/pull/2", 2)
	_ = store.MarkAskBranchPRMerged(db, pr1)
	_ = store.UpdateAskBranchPRChecks(db, pr2, "Success")

	handler := handleConvoysSubroutes(db)
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/convoys/%d/ship-summary", cid), nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp ShipSummaryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v (body=%s)", err, rec.Body.String())
	}

	if resp.ConvoyID != cid {
		t.Errorf("convoy_id: got %d, want %d", resp.ConvoyID, cid)
	}
	if resp.ConvoyStatus != "DraftPROpen" {
		t.Errorf("convoy_status: got %q, want DraftPROpen", resp.ConvoyStatus)
	}
	if len(resp.AskBranches) != 1 {
		t.Fatalf("ask_branches: got %d, want 1", len(resp.AskBranches))
	}
	ab := resp.AskBranches[0]
	if ab.Repo != "api" || ab.DraftPRNumber != 42 || ab.DraftPRState != "Open" {
		t.Errorf("ask_branch fields wrong: %+v", ab)
	}
	if ab.AskBranch != "force/ask-1-ship-summary-ok" {
		t.Errorf("ask_branch name wrong: %q", ab.AskBranch)
	}
	// pr1 is Merged; pr2 is Open with ChecksSuccess
	if resp.SubPRRollup.Merged != 1 || resp.SubPRRollup.Open != 1 {
		t.Errorf("sub_pr_rollup state wrong: %+v", resp.SubPRRollup)
	}
	if resp.SubPRRollup.CISuccess != 1 {
		t.Errorf("sub_pr_rollup ci_success wrong: %+v", resp.SubPRRollup)
	}
	if resp.TaskStats.Total != 2 || resp.TaskStats.Completed != 1 {
		t.Errorf("task_stats wrong: %+v", resp.TaskStats)
	}
}
