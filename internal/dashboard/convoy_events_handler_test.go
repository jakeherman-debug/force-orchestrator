package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"force-orchestrator/internal/store"
)

func TestHandleConvoyEvents_ReturnsEvents(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := store.CreateConvoy(db, "[1] events-test")
	store.AppendConvoyEvent(db, cid, "status_change", "Active", "Completed", "")
	store.AppendConvoyEvent(db, cid, "ask_branch_created", "", "force/ask-1-events-test", "")
	store.AppendConvoyEvent(db, cid, "draft_pr_opened", "", "https://github.com/org/repo/pull/42", "api")

	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/convoys/%d/events", cid), nil)
	rec := httptest.NewRecorder()
	handleConvoysSubroutes(db)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var events []DashboardConvoyEvent
	if err := json.Unmarshal(rec.Body.Bytes(), &events); err != nil {
		t.Fatalf("parse: %v (body=%s)", err, rec.Body.String())
	}
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}

	e0 := events[0]
	if e0.EventType != "status_change" || e0.OldValue != "Active" || e0.NewValue != "Completed" {
		t.Errorf("event[0] wrong: %+v", e0)
	}

	e1 := events[1]
	if e1.EventType != "ask_branch_created" || e1.NewValue != "force/ask-1-events-test" {
		t.Errorf("event[1] wrong: %+v", e1)
	}

	e2 := events[2]
	if e2.EventType != "draft_pr_opened" || e2.NewValue != "https://github.com/org/repo/pull/42" || e2.Detail != "api" {
		t.Errorf("event[2] wrong: %+v", e2)
	}
}

func TestHandleConvoyEvents_EmptyConvoy(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := store.CreateConvoy(db, "[1] empty-events")

	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/convoys/%d/events", cid), nil)
	rec := httptest.NewRecorder()
	handleConvoysSubroutes(db)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var events []DashboardConvoyEvent
	if err := json.Unmarshal(rec.Body.Bytes(), &events); err != nil {
		t.Fatalf("parse: %v (body=%s)", err, rec.Body.String())
	}
	if len(events) != 0 {
		t.Fatalf("expected 0 events, got %d", len(events))
	}
}

func TestHandleConvoyEvents_MethodNotAllowed(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := store.CreateConvoy(db, "[1] method-test")

	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/convoys/%d/events", cid), nil)
	rec := httptest.NewRecorder()
	handleConvoysSubroutes(db)(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}

func TestHandleConvoyEvents_ConvoyIDInResponse(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := store.CreateConvoy(db, "[1] id-check")
	store.AppendConvoyEvent(db, cid, "shipped", "", "", "")

	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/convoys/%d/events", cid), nil)
	rec := httptest.NewRecorder()
	handleConvoysSubroutes(db)(rec, req)

	var events []DashboardConvoyEvent
	_ = json.Unmarshal(rec.Body.Bytes(), &events)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].ConvoyID != cid {
		t.Errorf("convoy_id mismatch: got %d want %d", events[0].ConvoyID, cid)
	}
	if events[0].CreatedAt == "" {
		t.Error("created_at should be populated")
	}
}
