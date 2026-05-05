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

// TestHandleFeatureConsumerInteg_AggregatesPersistedRows asserts the
// /consumer-integ subroute (D8 Track 3) returns the persisted
// ConsumerIntegrationResults for a Feature with the precomputed
// any_blocking + blocking_repos aggregation so the SPA doesn't have to
// walk the array twice.
func TestHandleFeatureConsumerInteg_AggregatesPersistedRows(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	res, err := db.Exec(`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, priority, created_at)
		VALUES (0, 'force', 'Feature', 'Completed', 'd8 t3 dashboard test', 0, datetime('now'))`)
	if err != nil {
		t.Fatalf("seed feature: %v", err)
	}
	id, _ := res.LastInsertId()
	featureID := int(id)

	// Establish a blast-radius row so the existence-check passes.
	if err := store.SetFeatureBlastRadius(db, featureID, store.BlastRadiusRecord{
		AffectedConsumerRepos: []string{"consumer-green", "consumer-red"},
	}); err != nil {
		t.Fatalf("SetFeatureBlastRadius: %v", err)
	}

	// One green + one red row → aggregation must surface the red as blocking.
	if _, err := store.UpsertConsumerIntegrationResult(db, store.ConsumerIntegrationResult{
		FeatureID:        featureID,
		ConsumerRepoName: "consumer-green",
		Status:           store.CIStatusGreen,
		ExitCode:         0,
		TestCommand:      "go test ./...",
		DurationSeconds:  3,
		StdoutTail:       "PASS",
		RanAt:            store.NowSQLite(),
	}); err != nil {
		t.Fatalf("upsert green: %v", err)
	}
	if _, err := store.UpsertConsumerIntegrationResult(db, store.ConsumerIntegrationResult{
		FeatureID:        featureID,
		ConsumerRepoName: "consumer-red",
		Status:           store.CIStatusRed,
		ExitCode:         1,
		TestCommand:      "go test ./...",
		DurationSeconds:  4,
		StderrTail:       "compile error",
		RanAt:            store.NowSQLite(),
	}); err != nil {
		t.Fatalf("upsert red: %v", err)
	}

	r := httptest.NewRequest(http.MethodGet,
		"/api/features/"+itoa(featureID)+"/consumer-integ", nil)
	w := httptest.NewRecorder()
	handleFeatureBlastRadius(db)(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", w.Code, w.Body.String())
	}
	var got consumerIntegResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, w.Body.String())
	}
	if got.FeatureID != featureID {
		t.Errorf("FeatureID: got %d want %d", got.FeatureID, featureID)
	}
	if !got.AnyBlocking {
		t.Errorf("AnyBlocking: got false want true (consumer-red is red)")
	}
	if !reflect.DeepEqual(got.BlockingRepos, []string{"consumer-red"}) {
		t.Errorf("BlockingRepos: got %v want [consumer-red]", got.BlockingRepos)
	}
	if len(got.Results) != 2 {
		t.Fatalf("Results: got %d rows want 2", len(got.Results))
	}
	// Ordered by consumer_repo_name ASC: green first, red second.
	if got.Results[0].ConsumerRepoName != "consumer-green" || got.Results[0].Status != store.CIStatusGreen {
		t.Errorf("Results[0]: got %+v want consumer-green/green", got.Results[0])
	}
	if got.Results[1].ConsumerRepoName != "consumer-red" || got.Results[1].Status != store.CIStatusRed {
		t.Errorf("Results[1]: got %+v want consumer-red/red", got.Results[1])
	}
}

// TestHandleFeatureConsumerInteg_EmptyArraysWhenNoResults asserts a
// Feature with no ConsumerIntegrationResults rows yet returns the
// canonical empty-arrays shape (not null) so the SPA can render "no
// consumer integration runs yet" without branching on missing fields.
func TestHandleFeatureConsumerInteg_EmptyArraysWhenNoResults(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	res, err := db.Exec(`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, priority, created_at)
		VALUES (0, 'force', 'Feature', 'Pending', 'd8 t3 empty', 0, datetime('now'))`)
	if err != nil {
		t.Fatalf("seed feature: %v", err)
	}
	id, _ := res.LastInsertId()
	if err := store.SetFeatureBlastRadius(db, int(id), store.BlastRadiusRecord{}); err != nil {
		t.Fatalf("SetFeatureBlastRadius: %v", err)
	}

	r := httptest.NewRequest(http.MethodGet,
		"/api/features/"+itoa(int(id))+"/consumer-integ", nil)
	w := httptest.NewRecorder()
	handleFeatureBlastRadius(db)(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", w.Code, w.Body.String())
	}
	// Decode into a generic map so we can prove blocking_repos serializes
	// as [] (not null) on the wire — reflect.DeepEqual on the typed struct
	// would coerce []string(nil) and []string{} as equal.
	var raw map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if blocking, ok := raw["any_blocking"].(bool); !ok || blocking {
		t.Errorf("any_blocking: got %v want false", raw["any_blocking"])
	}
	if br, ok := raw["blocking_repos"].([]any); !ok {
		t.Errorf("blocking_repos: got %T want [] (must serialize as JSON array, not null)", raw["blocking_repos"])
	} else if len(br) != 0 {
		t.Errorf("blocking_repos: got %v want []", br)
	}
	if rs, ok := raw["results"].([]any); !ok {
		t.Errorf("results: got %T want []", raw["results"])
	} else if len(rs) != 0 {
		t.Errorf("results: got %v want []", rs)
	}
}

// TestHandleFeatureBlastRadius_400OnUnknownSubroute asserts the
// extended handler rejects unknown subroutes with 400 (rather than
// 404'ing or silently routing to blast-radius).
func TestHandleFeatureBlastRadius_400OnUnknownSubroute(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	r := httptest.NewRequest(http.MethodGet, "/api/features/1/bogus", nil)
	w := httptest.NewRecorder()
	handleFeatureBlastRadius(db)(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status: got %d want 400; body=%s", w.Code, w.Body.String())
	}
}

