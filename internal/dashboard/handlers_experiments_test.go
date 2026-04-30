package dashboard

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"force-orchestrator/internal/experiments"
	"force-orchestrator/internal/holdout"
	"force-orchestrator/internal/store"
)

const dashTestManifest = `
name: dash-shakedown
hypothesis: dashboard surface smoke test
subject_agent: captain
assignment_unit: task
stakes_tier: low
analysis_framework_version: "2026-04-29"
treatments:
  - arm_label: control
    prompt_template_ref: captain/default@HEAD
    target_cell_weight: 0.5
  - arm_label: treatment
    prompt_template_ref: captain/treatmentA@HEAD
    target_cell_weight: 0.5
metrics:
  - metric_name: captain_rejection_rate
    metric_version: "2026-04-23"
    direction: lower_is_better
    is_primary: true
`

func openDashTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db := store.InitHolocronDSN(":memory:")
	if db == nil {
		t.Fatalf("InitHolocronDSN returned nil")
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestExperimentsHandler_List_EmptyResponse — no experiments → empty
// list, count=0, status_filter='all'.
func TestExperimentsHandler_List_EmptyResponse(t *testing.T) {
	db := openDashTestDB(t)
	r := httptest.NewRequest(http.MethodGet, "/api/experiments", nil)
	w := httptest.NewRecorder()
	handleExperimentsList(db)(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Experiments  []experimentSummary `json:"experiments"`
		Count        int                 `json:"count"`
		StatusFilter string              `json:"status_filter"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Count != 0 {
		t.Errorf("count: got %d, want 0", resp.Count)
	}
	if resp.StatusFilter != "all" {
		t.Errorf("status_filter: got %q, want all", resp.StatusFilter)
	}
}

// TestExperimentsHandler_List_FiltersByStatus — seed two experiments
// (one authored, one running) and assert the status filter narrows.
func TestExperimentsHandler_List_FiltersByStatus(t *testing.T) {
	db := openDashTestDB(t)
	ctx := context.Background()
	id1, _ := experiments.AuthorFromBytes(ctx, db, []byte(dashTestManifest))
	id2, _ := experiments.AuthorFromBytes(ctx, db, []byte(dashTestManifest))
	if err := experiments.Ratify(ctx, db, id2, "op@x.com"); err != nil {
		t.Fatalf("ratify: %v", err)
	}

	for _, c := range []struct {
		filter      string
		wantCount   int
		wantContain []int
	}{
		{"all", 2, []int{id1, id2}},
		{"authored", 1, []int{id1}},
		{"running", 1, []int{id2}},
	} {
		r := httptest.NewRequest(http.MethodGet, "/api/experiments?status="+c.filter, nil)
		w := httptest.NewRecorder()
		handleExperimentsList(db)(w, r)
		if w.Code != http.StatusOK {
			t.Errorf("filter=%s: status %d body=%s", c.filter, w.Code, w.Body.String())
			continue
		}
		var resp struct {
			Experiments []experimentSummary `json:"experiments"`
			Count       int                 `json:"count"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Errorf("filter=%s: unmarshal: %v", c.filter, err)
			continue
		}
		if resp.Count != c.wantCount {
			t.Errorf("filter=%s: count got %d, want %d (rows=%v)", c.filter, resp.Count, c.wantCount, resp.Experiments)
		}
		got := map[int]bool{}
		for _, e := range resp.Experiments {
			got[e.ID] = true
		}
		for _, want := range c.wantContain {
			if !got[want] {
				t.Errorf("filter=%s: missing experiment id %d", c.filter, want)
			}
		}
	}
}

// TestExperimentsHandler_Detail_HappyPath — single-experiment view
// returns the full record + per-arm enrollment + outcome (when
// terminated).
func TestExperimentsHandler_Detail_HappyPath(t *testing.T) {
	db := openDashTestDB(t)
	ctx := context.Background()
	id, err := experiments.AuthorFromBytes(ctx, db, []byte(dashTestManifest))
	if err != nil {
		t.Fatalf("author: %v", err)
	}
	if err := experiments.Ratify(ctx, db, id, "op@x.com"); err != nil {
		t.Fatalf("ratify: %v", err)
	}

	r := httptest.NewRequest(http.MethodGet, "/api/experiments/"+itoa(id), nil)
	w := httptest.NewRecorder()
	handleExperimentDetail(db)(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	var d experimentDetail
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if d.ID != id {
		t.Errorf("id: got %d, want %d", d.ID, id)
	}
	if d.Hypothesis == "" {
		t.Errorf("hypothesis should be populated")
	}
	if len(d.Treatments) != 2 {
		t.Errorf("treatments: got %d, want 2", len(d.Treatments))
	}
	if len(d.Metrics) != 1 {
		t.Errorf("metrics: got %d, want 1", len(d.Metrics))
	}
	if !d.Metrics[0].IsPrimary {
		t.Errorf("expected primary metric")
	}
}

// TestExperimentsHandler_Detail_404OnUnknownID — bad id returns 404.
func TestExperimentsHandler_Detail_404OnUnknownID(t *testing.T) {
	db := openDashTestDB(t)
	r := httptest.NewRequest(http.MethodGet, "/api/experiments/99999", nil)
	w := httptest.NewRecorder()
	handleExperimentDetail(db)(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404 body=%s", w.Code, w.Body.String())
	}
}

// TestExperimentsHandler_Detail_400OnBadID — non-numeric or missing
// id returns 400 (path-shape error).
func TestExperimentsHandler_Detail_400OnBadID(t *testing.T) {
	db := openDashTestDB(t)
	for _, p := range []string{"/api/experiments/", "/api/experiments/abc", "/api/experiments/1/foo"} {
		r := httptest.NewRequest(http.MethodGet, p, nil)
		w := httptest.NewRecorder()
		handleExperimentDetail(db)(w, r)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400 body=%s", p, w.Code, w.Body.String())
		}
	}
}

// TestExperimentsHandler_Subroutes_RejectsNonGET — POST/PATCH return
// 405. Operator mutations land in Phase 6.
func TestExperimentsHandler_Subroutes_RejectsNonGET(t *testing.T) {
	db := openDashTestDB(t)
	for _, m := range []string{http.MethodPost, http.MethodDelete, http.MethodPatch} {
		r := httptest.NewRequest(m, "/api/experiments/1", nil)
		w := httptest.NewRecorder()
		handleExperimentsSubroutes(db)(w, r)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: got %d, want 405", m, w.Code)
		}
	}
}

// TestFleetProgressHandler_Shape — the response includes the holdout
// row metadata and three windows (24h, 7d, 30d).
func TestFleetProgressHandler_Shape(t *testing.T) {
	db := openDashTestDB(t)
	ctx := context.Background()
	if _, err := holdout.MintBaseline2026(ctx, db); err != nil {
		t.Fatalf("mint: %v", err)
	}
	r := httptest.NewRequest(http.MethodGet, "/api/fleet-progress", nil)
	w := httptest.NewRecorder()
	handleFleetProgress(db)(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	var resp fleetProgressResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.HoldoutName != holdout.BaselineHoldoutName {
		t.Errorf("name: got %q, want %q", resp.HoldoutName, holdout.BaselineHoldoutName)
	}
	if len(resp.Windows) != 3 {
		t.Errorf("windows: got %d, want 3", len(resp.Windows))
	}
	wantLabels := []string{"24h", "7d", "30d"}
	for i, w := range resp.Windows {
		if w.Label != wantLabels[i] {
			t.Errorf("window[%d].Label: got %q, want %q", i, w.Label, wantLabels[i])
		}
	}
	if resp.HoldoutLifecycle == "" {
		t.Errorf("holdout lifecycle phase should be set")
	}
}

// TestExperimentsHandler_ContentTypeJSON — every endpoint returns
// application/json (not text/plain) so the existing securityMiddleware
// + jsonCORS contract holds.
func TestExperimentsHandler_ContentTypeJSON(t *testing.T) {
	db := openDashTestDB(t)
	cases := []struct {
		name    string
		path    string
		handler http.HandlerFunc
	}{
		{"list", "/api/experiments", handleExperimentsList(db)},
		{"fleet-progress", "/api/fleet-progress", handleFleetProgress(db)},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodGet, c.path, nil)
		w := httptest.NewRecorder()
		c.handler(w, r)
		if ct := w.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("%s: Content-Type=%q, want application/json", c.name, ct)
		}
	}
}

func itoa(i int) string {
	const digits = "0123456789"
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = digits[i%10]
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
