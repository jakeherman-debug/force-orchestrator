// D4 fix-loop-1 α4 — Senate review log + chambers handler tests.

package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"force-orchestrator/internal/store"
)

func TestSenateChambers_Empty(t *testing.T) {
	db := openDashTestDB(t)
	r := httptest.NewRequest(http.MethodGet, "/api/senate/chambers", nil)
	w := httptest.NewRecorder()
	handleSenateChambers(db)(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	var resp struct {
		Chambers []store.SenateChamber `json:"chambers"`
		Count    int                   `json:"count"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Chambers) != 0 || resp.Count != 0 {
		t.Errorf("expected empty, got %+v", resp)
	}
}

func TestSenateChambers_Populated(t *testing.T) {
	db := openDashTestDB(t)
	for _, c := range []store.SenateChamber{
		{SenatorName: "force-orchestrator", Scope: "repo:force-orchestrator", Status: "active"},
		{SenatorName: "monolith", Scope: "repo:monolith", Status: "onboarding"},
		{SenatorName: "retired-bot", Scope: "repo:archive", Status: "retired"},
	} {
		if err := store.UpsertSenateChamber(db, c); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}

	cases := []struct {
		query string
		want  int
	}{
		{"", 3},
		{"?status=all", 3},
		{"?status=active", 1},
		{"?status=onboarding", 1},
		{"?status=retired", 1},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(http.MethodGet, "/api/senate/chambers"+tc.query, nil)
		w := httptest.NewRecorder()
		handleSenateChambers(db)(w, r)
		var resp struct {
			Chambers []store.SenateChamber `json:"chambers"`
		}
		json.Unmarshal(w.Body.Bytes(), &resp)
		if len(resp.Chambers) != tc.want {
			t.Errorf("query %q: want %d, got %d", tc.query, tc.want, len(resp.Chambers))
		}
	}
}

func TestSenateChambers_BadStatus(t *testing.T) {
	db := openDashTestDB(t)
	r := httptest.NewRequest(http.MethodGet, "/api/senate/chambers?status=Bogus", nil)
	w := httptest.NewRecorder()
	handleSenateChambers(db)(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestSenateChambers_MethodNotAllowed(t *testing.T) {
	db := openDashTestDB(t)
	r := httptest.NewRequest(http.MethodPost, "/api/senate/chambers", nil)
	w := httptest.NewRecorder()
	handleSenateChambers(db)(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

func TestSenateReviews_Empty(t *testing.T) {
	db := openDashTestDB(t)
	r := httptest.NewRequest(http.MethodGet, "/api/senate/reviews", nil)
	w := httptest.NewRecorder()
	handleSenateReviews(db)(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	var resp struct {
		Reviews []senateReviewView `json:"reviews"`
		Count   int                `json:"count"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Reviews) != 0 || resp.Count != 0 {
		t.Errorf("expected empty, got %+v", resp)
	}
}

func TestSenateReviews_Populated_Filters(t *testing.T) {
	db := openDashTestDB(t)
	// Seed three reviews across two features and two senators.
	rows := []store.SenateReviewRow{
		{FeatureID: 100, Senator: "alpha", Position: "concur", Rationale: "lgtm"},
		{FeatureID: 100, Senator: "beta", Position: "amend", Rationale: "tweak"},
		{FeatureID: 200, Senator: "alpha", Position: "dissent", Rationale: "no"},
	}
	for _, r := range rows {
		if _, err := store.InsertSenateReview(db, r); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	cases := []struct {
		query string
		want  int
	}{
		{"", 3},
		{"?feature_id=100", 2},
		{"?feature_id=200", 1},
		{"?senator=alpha", 2},
		{"?position=concur", 1},
		{"?position=amend", 1},
		{"?feature_id=100&senator=alpha", 1},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(http.MethodGet, "/api/senate/reviews"+tc.query, nil)
		w := httptest.NewRecorder()
		handleSenateReviews(db)(w, r)
		var resp struct {
			Reviews []senateReviewView `json:"reviews"`
		}
		json.Unmarshal(w.Body.Bytes(), &resp)
		if len(resp.Reviews) != tc.want {
			t.Errorf("query %q: want %d, got %d", tc.query, tc.want, len(resp.Reviews))
		}
	}
}

func TestSenateReviews_BadFeatureID(t *testing.T) {
	db := openDashTestDB(t)
	r := httptest.NewRequest(http.MethodGet, "/api/senate/reviews?feature_id=abc", nil)
	w := httptest.NewRecorder()
	handleSenateReviews(db)(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestSenateReviews_BadPosition(t *testing.T) {
	db := openDashTestDB(t)
	r := httptest.NewRequest(http.MethodGet, "/api/senate/reviews?position=bogus", nil)
	w := httptest.NewRecorder()
	handleSenateReviews(db)(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestSenateReviews_FeatureTitleResolved(t *testing.T) {
	db := openDashTestDB(t)
	// Insert a BountyBoard row so lookupFeatureTitle finds something.
	_, err := db.Exec(`INSERT INTO BountyBoard (id, type, payload, status) VALUES (777, 'Feature', 'Add the thing', 'P')`)
	if err != nil {
		t.Fatalf("insert feature: %v", err)
	}
	if _, err := store.InsertSenateReview(db, store.SenateReviewRow{
		FeatureID: 777, Senator: "alpha", Position: "concur", Rationale: "ok",
	}); err != nil {
		t.Fatalf("insert review: %v", err)
	}
	r := httptest.NewRequest(http.MethodGet, "/api/senate/reviews?feature_id=777", nil)
	w := httptest.NewRecorder()
	handleSenateReviews(db)(w, r)
	var resp struct {
		Reviews []senateReviewView `json:"reviews"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Reviews) != 1 {
		t.Fatalf("expected 1 review, got %d", len(resp.Reviews))
	}
	if resp.Reviews[0].FeatureTitle != "Add the thing" {
		t.Errorf("expected title resolved, got %q", resp.Reviews[0].FeatureTitle)
	}
}

func TestSenateReviews_Pagination(t *testing.T) {
	db := openDashTestDB(t)
	for i := 0; i < 5; i++ {
		_, err := store.InsertSenateReview(db, store.SenateReviewRow{
			FeatureID: 100, Senator: "alpha", Position: "concur", Rationale: "r" + strconv.Itoa(i),
		})
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	r := httptest.NewRequest(http.MethodGet, "/api/senate/reviews?limit=2&offset=2", nil)
	w := httptest.NewRecorder()
	handleSenateReviews(db)(w, r)
	var resp struct {
		Reviews []senateReviewView `json:"reviews"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Reviews) != 2 {
		t.Errorf("expected 2 rows, got %d", len(resp.Reviews))
	}
}

func TestSenateReviewSingle_HappyPath(t *testing.T) {
	db := openDashTestDB(t)
	id, err := store.InsertSenateReview(db, store.SenateReviewRow{
		FeatureID: 555, Senator: "alpha", Position: "amend", Rationale: "tweak",
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	// Seed a memory so cited_memories renders.
	_, _ = store.InsertSenateMemory(db, store.SenateMemoryEntry{
		Senator: "alpha", Topic: "review", Summary: "saw similar feature last month",
	})
	r := httptest.NewRequest(http.MethodGet, "/api/senate/reviews/"+strconv.Itoa(id), nil)
	w := httptest.NewRecorder()
	handleSenateReviewsSubroutes(db)(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	var resp struct {
		Review        senateReviewView         `json:"review"`
		CitedMemories []store.SenateMemoryEntry `json:"cited_memories"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Review.ID != id {
		t.Errorf("expected id=%d, got %d", id, resp.Review.ID)
	}
	if len(resp.CitedMemories) != 1 {
		t.Errorf("expected 1 cited memory, got %d", len(resp.CitedMemories))
	}
}

func TestSenateReviewSingle_NotFound(t *testing.T) {
	db := openDashTestDB(t)
	r := httptest.NewRequest(http.MethodGet, "/api/senate/reviews/9999", nil)
	w := httptest.NewRecorder()
	handleSenateReviewsSubroutes(db)(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestSenateReviewSingle_BadID(t *testing.T) {
	db := openDashTestDB(t)
	r := httptest.NewRequest(http.MethodGet, "/api/senate/reviews/abc", nil)
	w := httptest.NewRecorder()
	handleSenateReviewsSubroutes(db)(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestSenateReviewSingle_MethodNotAllowed(t *testing.T) {
	db := openDashTestDB(t)
	r := httptest.NewRequest(http.MethodPost, "/api/senate/reviews/1", nil)
	w := httptest.NewRecorder()
	handleSenateReviewsSubroutes(db)(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}
