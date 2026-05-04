// handlers_arch_health_test.go — D9 ArchHealth dashboard handler smoke.
package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"force-orchestrator/internal/store"
)

func TestHandleArchHealth_LatestEmpty(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	r := httptest.NewRequest(http.MethodGet, "/api/arch-health/latest", nil)
	w := httptest.NewRecorder()
	handleArchHealthRoot(db)(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body=%s)", w.Code, w.Body.String())
	}
	var resp archHealthResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, w.Body.String())
	}
	if resp.Month != "" || resp.TotalViolations != 0 {
		t.Errorf("expected empty latest response; got month=%q total=%d", resp.Month, resp.TotalViolations)
	}
}

func TestHandleArchHealth_MonthsList(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	for _, m := range []string{"2026-03", "2026-04", "2026-05"} {
		if err := store.UpsertArchHealthAggregate(db, store.ArchHealthAggregate{
			ReportMonth: m, RuleID: "BOS-001", RepoID: 1,
			AuthorType: "human", ViolationCount: 1,
		}); err != nil {
			t.Fatalf("seed %s: %v", m, err)
		}
	}

	r := httptest.NewRequest(http.MethodGet, "/api/arch-health/months", nil)
	w := httptest.NewRecorder()
	handleArchHealthRoot(db)(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp struct {
		Months []string `json:"months"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Months) != 3 {
		t.Errorf("expected 3 months, got %d (%v)", len(resp.Months), resp.Months)
	}
}

func TestHandleArchHealth_SpecificMonth(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	if err := store.UpsertArchHealthAggregate(db, store.ArchHealthAggregate{
		ReportMonth: "2026-05", RuleID: "BOS-001", RepoID: 1,
		AuthorType: "human", ViolationCount: 7,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := store.UpsertArchHealthAggregate(db, store.ArchHealthAggregate{
		ReportMonth: "2026-05", RuleID: "BOS-002", RepoID: 1,
		AuthorType: "astromech", ViolationCount: 3,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	r := httptest.NewRequest(http.MethodGet, "/api/arch-health/2026-05", nil)
	w := httptest.NewRecorder()
	handleArchHealthRoot(db)(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp archHealthResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Month != "2026-05" || resp.TotalViolations != 10 {
		t.Errorf("expected month=2026-05 total=10; got month=%q total=%d", resp.Month, resp.TotalViolations)
	}
	if resp.PerAuthorTotal["human"] != 7 || resp.PerAuthorTotal["astromech"] != 3 {
		t.Errorf("per-author totals wrong: %v", resp.PerAuthorTotal)
	}
}

func TestHandleArchHealth_RejectsNonGET(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	r := httptest.NewRequest(http.MethodPost, "/api/arch-health/latest", nil)
	w := httptest.NewRecorder()
	handleArchHealthRoot(db)(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}
