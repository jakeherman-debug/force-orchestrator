// D3 fix-loop-1 / γ3 — spec deprecation handler tests.
package dashboard

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

func TestHandleSpecDeprecation_HappyPath(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, _ := store.CreateConvoy(db, "h-spec")
	db.Exec(`UPDATE Convoys SET verification_spec_json = ? WHERE id = ?`,
		`{"ats":[{"id":"AT-1","description":"x"}]}`, convoyID)

	body, _ := json.Marshal(map[string]any{
		"item_id":        "AT-1",
		"item_kind":      "at",
		"rationale":      "twenty-plus chars rationale provided here",
		"removal_kind":   "mistake",
		"operator_email": "op@example.com",
	})
	req := httptest.NewRequest(http.MethodPost,
		"/api/convoy/"+itoa(convoyID)+"/deprecate-spec-item", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handleSpecDeprecation(db).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var spec string
	db.QueryRow(`SELECT verification_spec_json FROM Convoys WHERE id = ?`, convoyID).Scan(&spec)
	if !store.IsDeprecated(spec, "AT-1") {
		t.Errorf("AT-1 not deprecated post-call: %s", spec)
	}
}

func TestHandleSpecDeprecation_RejectsMissingEmail(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, _ := store.CreateConvoy(db, "no-email")
	db.Exec(`UPDATE Convoys SET verification_spec_json = ? WHERE id = ?`,
		`{"ats":[{"id":"AT-1"}]}`, convoyID)

	body, _ := json.Marshal(map[string]any{
		"item_id":      "AT-1",
		"item_kind":    "at",
		"rationale":    "twenty-plus chars rationale provided here",
		"removal_kind": "mistake",
		// operator_email missing — Pattern P21 runtime guard
	})
	req := httptest.NewRequest(http.MethodPost,
		"/api/convoy/"+itoa(convoyID)+"/deprecate-spec-item", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handleSpecDeprecation(db).ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Errorf("expected non-200 status when operator_email missing, got %d", rec.Code)
	}
}

func TestHandleSpecDeprecation_InflightTasks_409WithoutDisposition(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, _ := store.CreateConvoy(db, "inflight")
	db.Exec(`UPDATE Convoys SET verification_spec_json = ? WHERE id = ?`,
		`{"ats":[{"id":"AT-1"}]}`, convoyID)
	// Seed an in-flight task spawned by AT-1.
	db.Exec(`INSERT INTO BountyBoard (parent_id,target_repo,type,status,payload,convoy_id,priority,spawning_at_id,created_at)
		VALUES (0,'api','CodeEdit','Pending','x',?,5,'AT-1',datetime('now'))`, convoyID)

	body, _ := json.Marshal(map[string]any{
		"item_id":        "AT-1",
		"item_kind":      "at",
		"rationale":      "twenty-plus chars rationale provided here",
		"removal_kind":   "mistake",
		"operator_email": "op@example.com",
	})
	req := httptest.NewRequest(http.MethodPost,
		"/api/convoy/"+itoa(convoyID)+"/deprecate-spec-item", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handleSpecDeprecation(db).ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "inflight_task_ids") {
		t.Errorf("expected inflight_task_ids in body, got %s", rec.Body.String())
	}
}

func TestHandleSpecDeprecation_CancelAndRemove(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, _ := store.CreateConvoy(db, "c-and-r")
	db.Exec(`UPDATE Convoys SET verification_spec_json = ? WHERE id = ?`,
		`{"ats":[{"id":"AT-2"}]}`, convoyID)
	res, _ := db.Exec(`INSERT INTO BountyBoard (parent_id,target_repo,type,status,payload,convoy_id,priority,spawning_at_id,created_at)
		VALUES (0,'api','CodeEdit','Pending','x',?,5,'AT-2',datetime('now'))`, convoyID)
	taskID, _ := res.LastInsertId()

	body, _ := json.Marshal(map[string]any{
		"item_id":              "AT-2",
		"item_kind":            "at",
		"rationale":            "twenty-plus chars rationale provided here",
		"removal_kind":         "satisfied",
		"operator_email":       "op@example.com",
		"inflight_disposition": "cancel_and_remove",
	})
	req := httptest.NewRequest(http.MethodPost,
		"/api/convoy/"+itoa(convoyID)+"/deprecate-spec-item", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handleSpecDeprecation(db).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var status string
	db.QueryRow(`SELECT status FROM BountyBoard WHERE id = ?`, taskID).Scan(&status)
	if status != "Cancelled" {
		t.Errorf("task should be Cancelled, got %s", status)
	}
}

func TestHandleSpecDeprecation_CancelRemoval_NoOp(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, _ := store.CreateConvoy(db, "cancel-removal")
	spec := `{"ats":[{"id":"AT-3"}]}`
	db.Exec(`UPDATE Convoys SET verification_spec_json = ? WHERE id = ?`, spec, convoyID)
	db.Exec(`INSERT INTO BountyBoard (parent_id,target_repo,type,status,payload,convoy_id,priority,spawning_at_id,created_at)
		VALUES (0,'api','CodeEdit','Pending','x',?,5,'AT-3',datetime('now'))`, convoyID)

	body, _ := json.Marshal(map[string]any{
		"item_id":              "AT-3",
		"item_kind":            "at",
		"rationale":            "twenty-plus chars rationale provided here",
		"removal_kind":         "mistake",
		"operator_email":       "op@example.com",
		"inflight_disposition": "cancel_removal",
	})
	req := httptest.NewRequest(http.MethodPost,
		"/api/convoy/"+itoa(convoyID)+"/deprecate-spec-item", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handleSpecDeprecation(db).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	// Spec should be UNCHANGED — operator chose to cancel the removal.
	var stored string
	db.QueryRow(`SELECT verification_spec_json FROM Convoys WHERE id = ?`, convoyID).Scan(&stored)
	if store.IsDeprecated(stored, "AT-3") {
		t.Errorf("AT-3 was deprecated despite cancel_removal: %s", stored)
	}
}

func TestHandleSpecDeprecation_RejectsMethodNotPost(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	req := httptest.NewRequest(http.MethodGet, "/api/convoy/1/deprecate-spec-item", nil)
	rec := httptest.NewRecorder()
	handleSpecDeprecation(db).ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

// itoa lives in handlers_experiments_test.go; we reuse it here.
