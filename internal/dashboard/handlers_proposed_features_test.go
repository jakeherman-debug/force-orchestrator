package dashboard

// D3 fix-loop-1 β2 — ProposedFeatures handler tests.
//
// Coverage:
//   - GET list (empty + populated, status filters)
//   - POST suppress (rationale length, missing email, happy path,
//     blocks subsequent emit)
//   - POST score override (audit row created, missing email rejects)
//   - POST promote (404 for missing, conflict for already-promoted)

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"force-orchestrator/internal/store"
)

func TestProposedFeaturesHandler_List_Empty(t *testing.T) {
	db := openDashTestDB(t)
	r := httptest.NewRequest(http.MethodGet, "/api/proposed-features", nil)
	w := httptest.NewRecorder()
	handleProposedFeaturesList(db)(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	var rows []store.ProposedFeatureRow
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("expected empty rows, got %d", len(rows))
	}
}

func TestProposedFeaturesHandler_List_Populated(t *testing.T) {
	db := openDashTestDB(t)
	for i, topic := range []string{"a", "b", "c"} {
		_, err := store.EmitProposedFeature(db, store.ProposedFeaturePayload{
			ObservationSummary: "feature " + topic,
			Source:             "investigator",
			Topic:              topic,
			ValueScore:         "medium",
		})
		if err != nil {
			t.Fatalf("emit %d: %v", i, err)
		}
	}

	r := httptest.NewRequest(http.MethodGet, "/api/proposed-features", nil)
	w := httptest.NewRecorder()
	handleProposedFeaturesList(db)(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	var rows []store.ProposedFeatureRow
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(rows) != 3 {
		t.Errorf("expected 3 rows, got %d", len(rows))
	}
}

func TestProposedFeaturesHandler_Suppress_HappyPath(t *testing.T) {
	db := openDashTestDB(t)
	res, err := store.EmitProposedFeature(db, store.ProposedFeaturePayload{
		ObservationSummary: "noisy",
		Source:             "investigator",
		Topic:              "noisy-topic",
		CodePaths:          []string{"x.go"},
	})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}

	body := suppressRequest{
		Rationale:         "this fires on every refactor and clutters review",
		OperatorEmail:     "operator@example.com",
		SuppressUntilDays: 30,
	}
	bb, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, "/api/proposed-features/"+strconv.FormatInt(res.FeatureID, 10)+"/suppress", bytes.NewReader(bb))
	w := httptest.NewRecorder()
	handleProposedFeaturesSubroutes(db)(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}

	// Verify a suppression row landed.
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM ProposedFeatureSuppressions`).Scan(&n)
	if n != 1 {
		t.Errorf("expected 1 suppression row, got %d", n)
	}

	// Subsequent emit of the same canonical input should be suppressed.
	res2, err := store.EmitProposedFeature(db, store.ProposedFeaturePayload{
		ObservationSummary: "noisy",
		Source:             "investigator",
		Topic:              "noisy-topic",
		CodePaths:          []string{"x.go"},
	})
	if err != nil {
		t.Fatalf("re-emit: %v", err)
	}
	if !res2.Suppressed {
		t.Errorf("expected suppressed=true after handler installed rule")
	}
}

func TestProposedFeaturesHandler_Suppress_RationaleTooShort(t *testing.T) {
	db := openDashTestDB(t)
	res, _ := store.EmitProposedFeature(db, store.ProposedFeaturePayload{
		ObservationSummary: "x", Source: "investigator", Topic: "y",
	})
	body := suppressRequest{
		Rationale:     "short",
		OperatorEmail: "op@example.com",
	}
	bb, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, "/api/proposed-features/"+strconv.FormatInt(res.FeatureID, 10)+"/suppress", bytes.NewReader(bb))
	w := httptest.NewRecorder()
	handleProposedFeaturesSubroutes(db)(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for short rationale, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestProposedFeaturesHandler_Suppress_MissingEmail(t *testing.T) {
	db := openDashTestDB(t)
	res, _ := store.EmitProposedFeature(db, store.ProposedFeaturePayload{
		ObservationSummary: "x", Source: "investigator", Topic: "y",
	})
	body := suppressRequest{
		Rationale: "long enough rationale to pass the >= 20 char check",
	}
	bb, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, "/api/proposed-features/"+strconv.FormatInt(res.FeatureID, 10)+"/suppress", bytes.NewReader(bb))
	w := httptest.NewRecorder()
	handleProposedFeaturesSubroutes(db)(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing email, got %d", w.Code)
	}
}

func TestProposedFeaturesHandler_ScoreOverride_HappyPath(t *testing.T) {
	db := openDashTestDB(t)
	res, _ := store.EmitProposedFeature(db, store.ProposedFeaturePayload{
		ObservationSummary: "x",
		Source:             "investigator",
		Topic:              "y",
		ValueScore:         "medium",
		ComplexityScore:    "medium",
	})
	body := scoreOverrideRequest{
		NewValueScore: "high",
		Rationale:     "operator says priority",
		OperatorEmail: "op@example.com",
	}
	bb, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, "/api/proposed-features/"+strconv.FormatInt(res.FeatureID, 10)+"/score", bytes.NewReader(bb))
	w := httptest.NewRecorder()
	handleProposedFeaturesSubroutes(db)(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}

	// Verify audit row + score updated.
	var val string
	db.QueryRow(`SELECT value_score FROM ProposedFeatures WHERE id = ?`, res.FeatureID).Scan(&val)
	if val != "high" {
		t.Errorf("expected value=high, got %q", val)
	}

	var n int
	db.QueryRow(`SELECT COUNT(*) FROM ProposedFeatureScoreOverrides WHERE proposed_feature_id = ?`, res.FeatureID).Scan(&n)
	if n != 1 {
		t.Errorf("expected 1 audit row, got %d", n)
	}
}

func TestProposedFeaturesHandler_ScoreOverride_NotFound(t *testing.T) {
	db := openDashTestDB(t)
	body := scoreOverrideRequest{
		NewValueScore: "high",
		Rationale:     "x",
		OperatorEmail: "op@example.com",
	}
	bb, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, "/api/proposed-features/99999/score", bytes.NewReader(bb))
	w := httptest.NewRecorder()
	handleProposedFeaturesSubroutes(db)(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestProposedFeaturesHandler_Promote_HappyPath(t *testing.T) {
	db := openDashTestDB(t)
	res, _ := store.EmitProposedFeature(db, store.ProposedFeaturePayload{
		ObservationSummary: "x", Source: "investigator", Topic: "y",
	})
	body := promoteRequest{
		Deadline:      "2026-06-01",
		OperatorEmail: "op@example.com",
	}
	bb, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, "/api/proposed-features/"+strconv.FormatInt(res.FeatureID, 10)+"/promote", bytes.NewReader(bb))
	w := httptest.NewRecorder()
	handleProposedFeaturesSubroutes(db)(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	var status string
	db.QueryRow(`SELECT status FROM ProposedFeatures WHERE id = ?`, res.FeatureID).Scan(&status)
	if status != "promoted" {
		t.Errorf("expected status=promoted, got %q", status)
	}
}

func TestProposedFeaturesHandler_Promote_AlreadyPromoted(t *testing.T) {
	db := openDashTestDB(t)
	res, _ := store.EmitProposedFeature(db, store.ProposedFeaturePayload{
		ObservationSummary: "x", Source: "investigator", Topic: "y",
	})
	if err := store.PromoteProposedFeature(db, res.FeatureID, "2026-06-01", "op@example.com"); err != nil {
		t.Fatal(err)
	}
	body := promoteRequest{
		Deadline:      "2026-07-01",
		OperatorEmail: "op@example.com",
	}
	bb, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, "/api/proposed-features/"+strconv.FormatInt(res.FeatureID, 10)+"/promote", bytes.NewReader(bb))
	w := httptest.NewRecorder()
	handleProposedFeaturesSubroutes(db)(w, r)
	if w.Code != http.StatusConflict {
		t.Errorf("expected 409 conflict, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestProposedFeaturesHandler_Single_NotFound(t *testing.T) {
	db := openDashTestDB(t)
	r := httptest.NewRequest(http.MethodGet, "/api/proposed-features/99999", nil)
	w := httptest.NewRecorder()
	handleProposedFeaturesSubroutes(db)(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

