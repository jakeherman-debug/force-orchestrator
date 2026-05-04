package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"force-orchestrator/internal/store"
)

// TestHandleFeatureBlastRadius_Populated asserts the GET handler returns
// the persisted BlastRadiusRecord for a Feature whose Chancellor
// post-process has run.
func TestHandleFeatureBlastRadius_Populated(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	res, err := db.Exec(`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, priority, created_at)
		VALUES (0, 'force', 'Feature', 'Completed', 'test feature', 0, datetime('now'))`)
	if err != nil {
		t.Fatalf("seed feature: %v", err)
	}
	id, _ := res.LastInsertId()
	featureID := int(id)

	rec := store.BlastRadiusRecord{
		ModifiedSymbols: []store.BlastRadiusSymbol{{
			SymbolPath: "auth.LoginHandler", Kind: "function",
			FilePath: "auth/login.go", LineNumber: 42,
		}},
		AffectedConsumerRepos: []string{"consumer-a", "consumer-b"},
		AutoIncludedTasks:     []int{101, 102},
	}
	if err := store.SetFeatureBlastRadius(db, featureID, rec); err != nil {
		t.Fatalf("SetFeatureBlastRadius: %v", err)
	}

	r := httptest.NewRequest(http.MethodGet,
		"/api/features/"+itoa(featureID)+"/blast-radius", nil)
	w := httptest.NewRecorder()
	handleFeatureBlastRadius(db)(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", w.Code, w.Body.String())
	}
	var got blastRadiusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal response: %v (body=%s)", err, w.Body.String())
	}
	if got.FeatureID != featureID {
		t.Errorf("FeatureID: got %d want %d", got.FeatureID, featureID)
	}
	if !reflect.DeepEqual(got.AffectedConsumerRepos, rec.AffectedConsumerRepos) {
		t.Errorf("AffectedConsumerRepos: got %v want %v", got.AffectedConsumerRepos, rec.AffectedConsumerRepos)
	}
	if !reflect.DeepEqual(got.AutoIncludedTasks, rec.AutoIncludedTasks) {
		t.Errorf("AutoIncludedTasks: got %v want %v", got.AutoIncludedTasks, rec.AutoIncludedTasks)
	}
	if !reflect.DeepEqual(got.ModifiedSymbols, rec.ModifiedSymbols) {
		t.Errorf("ModifiedSymbols: got %v want %v", got.ModifiedSymbols, rec.ModifiedSymbols)
	}
}

// TestHandleFeatureBlastRadius_EmptyForNewFeature asserts a Feature
// with no blast-radius computed yet returns the canonical empty-arrays
// shape (not 404, not null) so the dashboard SPA renders "no consumer
// impact" without branching on missing fields.
func TestHandleFeatureBlastRadius_EmptyForNewFeature(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	res, err := db.Exec(`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, priority, created_at)
		VALUES (0, 'force', 'Feature', 'Pending', 'pre-T2 feature', 0, datetime('now'))`)
	if err != nil {
		t.Fatalf("seed feature: %v", err)
	}
	id, _ := res.LastInsertId()

	r := httptest.NewRequest(http.MethodGet,
		"/api/features/"+itoa(int(id))+"/blast-radius", nil)
	w := httptest.NewRecorder()
	handleFeatureBlastRadius(db)(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", w.Code, w.Body.String())
	}
	var got blastRadiusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ModifiedSymbols == nil {
		t.Errorf("ModifiedSymbols: must be []  not null on empty Feature")
	}
	if got.AffectedConsumerRepos == nil {
		t.Errorf("AffectedConsumerRepos: must be []  not null on empty Feature")
	}
	if got.AutoIncludedTasks == nil {
		t.Errorf("AutoIncludedTasks: must be []  not null on empty Feature")
	}
	if len(got.AffectedConsumerRepos) != 0 || len(got.AutoIncludedTasks) != 0 || len(got.ModifiedSymbols) != 0 {
		t.Errorf("expected empty arrays on a pre-T2 Feature; got %+v", got)
	}
}

// TestHandleFeatureBlastRadius_404OnUnknownFeature asserts a missing
// Feature returns 404, not an empty payload — the operator should know
// the URL is wrong rather than silently see a "no impact" view.
func TestHandleFeatureBlastRadius_404OnUnknownFeature(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	r := httptest.NewRequest(http.MethodGet, "/api/features/99999/blast-radius", nil)
	w := httptest.NewRecorder()
	handleFeatureBlastRadius(db)(w, r)

	if w.Code != http.StatusNotFound {
		t.Errorf("status: got %d want 404; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleFeatureBlastRadius_405OnPost asserts the handler is GET-only.
func TestHandleFeatureBlastRadius_405OnPost(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	r := httptest.NewRequest(http.MethodPost, "/api/features/1/blast-radius", nil)
	w := httptest.NewRecorder()
	handleFeatureBlastRadius(db)(w, r)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status: got %d want 405", w.Code)
	}
}

