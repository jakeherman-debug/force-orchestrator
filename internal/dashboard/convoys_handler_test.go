package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"force-orchestrator/internal/store"
)

// TestHandleConvoys_EmitsPRFlowFields verifies the JSON payload includes the
// ask_branches and sub_pr_rollup fields for convoys that have gone through
// the PR flow.
func TestHandleConvoys_EmitsPRFlowFields(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := store.CreateConvoy(db, "[1] pr-flow-test")
	tid, _ := store.AddConvoyTask(db, 0, "api", "t", cid, 0, "Pending")
	prID, _ := store.CreateAskBranchPR(db, tid, cid, "api", "https://gh/pull/1", 1)
	_ = store.MarkAskBranchPRMerged(db, prID)
	_ = store.UpsertConvoyAskBranch(db, cid, "api", "force/ask-1-pr-flow-test", "sha")
	_ = store.SetConvoyAskBranchDraftPR(db, cid, "api", "https://gh/pull/100", 100, "Open")

	handler := handleConvoys(db)
	req := httptest.NewRequest(http.MethodGet, "/api/convoys", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var convoys []DashboardConvoy
	if err := json.Unmarshal(rec.Body.Bytes(), &convoys); err != nil {
		t.Fatalf("parse: %v (body=%s)", err, rec.Body.String())
	}
	if len(convoys) != 1 {
		t.Fatalf("expected 1 convoy, got %d", len(convoys))
	}
	c := convoys[0]
	if len(c.AskBranches) != 1 {
		t.Fatalf("expected 1 ask-branch entry, got %d", len(c.AskBranches))
	}
	ab := c.AskBranches[0]
	if ab.Repo != "api" || ab.AskBranch != "force/ask-1-pr-flow-test" {
		t.Errorf("ask-branch payload wrong: %+v", ab)
	}
	if ab.DraftPRNumber != 100 || ab.DraftPRState != "Open" {
		t.Errorf("draft PR fields wrong: %+v", ab)
	}
	if c.SubPRRollup == nil {
		t.Fatal("SubPRRollup should be populated for PR-flow convoy")
	}
	if c.SubPRRollup.Total != 1 || c.SubPRRollup.Merged != 1 {
		t.Errorf("rollup numbers wrong: %+v", c.SubPRRollup)
	}
}

// TestHandleConvoys_LegacyConvoyHasNoPRFields verifies legacy convoys (no
// ConvoyAskBranch rows) emit nil AskBranches + SubPRRollup.
func TestHandleConvoys_LegacyConvoyHasNoPRFields(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	_, _ = store.CreateConvoy(db, "[1] legacy")

	handler := handleConvoys(db)
	req := httptest.NewRequest(http.MethodGet, "/api/convoys", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	var convoys []DashboardConvoy
	_ = json.Unmarshal(rec.Body.Bytes(), &convoys)
	if len(convoys) != 1 {
		t.Fatalf("expected 1 convoy, got %d", len(convoys))
	}
	c := convoys[0]
	if len(c.AskBranches) != 0 {
		t.Errorf("legacy convoy should have no ask-branches, got %d", len(c.AskBranches))
	}
	if c.SubPRRollup != nil {
		t.Errorf("legacy convoy should have nil SubPRRollup, got %+v", c.SubPRRollup)
	}
}
