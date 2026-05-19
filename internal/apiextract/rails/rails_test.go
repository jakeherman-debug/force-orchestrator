package rails_test

import (
	"encoding/json"
	"os"
	"testing"

	"force-orchestrator/internal/apiextract/rails"
	"force-orchestrator/internal/store"
)

const fixtureFile = "../testdata/rails-app/config/routes.rb"
const expectedFile = "../testdata/rails-app/expected_rails.json"
const accuracyThreshold = 0.95

func TestRailsExtractor_Fixture(t *testing.T) {
	content, err := os.ReadFile(fixtureFile)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	var expected []string
	expBytes, err := os.ReadFile(expectedFile)
	if err != nil {
		t.Fatalf("read expected: %v", err)
	}
	if err := json.Unmarshal(expBytes, &expected); err != nil {
		t.Fatalf("unmarshal expected: %v", err)
	}

	e := &rails.Extractor{}
	if e.Kind() != "http_route" {
		t.Errorf("Kind() = %q, want http_route", e.Kind())
	}
	if e.ExtractorName() != "rails-routes" {
		t.Errorf("ExtractorName() = %q, want rails-routes", e.ExtractorName())
	}

	apis, err := e.Extract("my-app", "config/routes.rb", content)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	t.Logf("Rails: extracted %d routes from fixture", len(apis))

	// Build a set of extracted identifiers.
	extracted := make(map[string]bool, len(apis))
	for _, a := range apis {
		if a.APIKind != "http_route" {
			t.Errorf("row %q: APIKind = %q, want http_route", a.APIIdentifier, a.APIKind)
		}
		if a.Extractor != "rails-routes" {
			t.Errorf("row %q: Extractor = %q, want rails-routes", a.APIIdentifier, a.Extractor)
		}
		if a.RepoName != "my-app" {
			t.Errorf("row %q: RepoName = %q, want my-app", a.APIIdentifier, a.RepoName)
		}
		if a.SourceLine <= 0 {
			t.Errorf("row %q: SourceLine = %d, want > 0", a.APIIdentifier, a.SourceLine)
		}
		extracted[a.APIIdentifier] = true
	}

	// Measure accuracy.
	matched := 0
	for _, want := range expected {
		if extracted[want] {
			matched++
		} else {
			t.Logf("MISSING: %q", want)
		}
	}

	accuracy := float64(matched) / float64(len(expected))
	t.Logf("Rails accuracy: %d/%d = %.1f%%", matched, len(expected), accuracy*100)
	if accuracy < accuracyThreshold {
		t.Errorf("accuracy %.2f < threshold %.2f", accuracy, accuracyThreshold)
	}
}

// TestRailsExtractor_EmptyFile verifies that an empty file returns no rows.
func TestRailsExtractor_EmptyFile(t *testing.T) {
	e := &rails.Extractor{}
	apis, err := e.Extract("repo", "config/routes.rb", []byte{})
	if err != nil {
		t.Fatalf("Extract(empty): %v", err)
	}
	if len(apis) != 0 {
		t.Errorf("Extract(empty): got %d rows, want 0", len(apis))
	}
}

// TestRailsExtractor_DirectRoutes verifies individual route DSL forms.
func TestRailsExtractor_DirectRoutes(t *testing.T) {
	content := []byte(`
Rails.application.routes.draw do
  get  '/ping', to: 'health#ping'
  post '/login', to: 'sessions#create'
  delete '/logout', to: 'sessions#destroy'
  patch '/profile', to: 'profiles#update'
  put   '/profile', to: 'profiles#replace'
end
`)
	e := &rails.Extractor{}
	apis, err := e.Extract("svc", "config/routes.rb", content)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	want := map[string]bool{
		"GET /ping":      true,
		"POST /login":    true,
		"DELETE /logout": true,
		"PATCH /profile": true,
		"PUT /profile":   true,
	}
	got := make(map[string]bool)
	for _, a := range apis {
		got[a.APIIdentifier] = true
	}
	for id := range want {
		if !got[id] {
			t.Errorf("missing expected route %q", id)
		}
	}
}

// TestRailsExtractor_Resources verifies resources expansion.
func TestRailsExtractor_Resources(t *testing.T) {
	content := []byte(`
Rails.application.routes.draw do
  resources :widgets
end
`)
	e := &rails.Extractor{}
	apis, err := e.Extract("svc", "config/routes.rb", content)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	wantIDs := []string{
		"GET /widgets",
		"POST /widgets",
		"GET /widgets/new",
		"GET /widgets/:id",
		"GET /widgets/:id/edit",
		"PATCH /widgets/:id",
		"PUT /widgets/:id",
		"DELETE /widgets/:id",
	}
	got := make(map[string]bool)
	for _, a := range apis {
		got[a.APIIdentifier] = true
	}
	for _, id := range wantIDs {
		if !got[id] {
			t.Errorf("resources :widgets missing %q", id)
		}
	}
	if len(apis) != len(wantIDs) {
		t.Errorf("resources :widgets: got %d routes, want %d", len(apis), len(wantIDs))
	}
}

// TestRailsExtractor_Namespace verifies namespace prefix propagation.
func TestRailsExtractor_Namespace(t *testing.T) {
	content := []byte(`
Rails.application.routes.draw do
  namespace :api do
    get '/status', to: 'status#index'
  end
end
`)
	e := &rails.Extractor{}
	apis, err := e.Extract("svc", "config/routes.rb", content)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(apis) != 1 {
		t.Fatalf("namespace: got %d routes, want 1; routes: %v", len(apis), apiIdentifiers(apis))
	}
	if apis[0].APIIdentifier != "GET /api/status" {
		t.Errorf("namespace: got %q, want %q", apis[0].APIIdentifier, "GET /api/status")
	}
}

// TestRailsExtractor_Root verifies root mapping.
func TestRailsExtractor_Root(t *testing.T) {
	content := []byte(`
Rails.application.routes.draw do
  root 'home#index'
end
`)
	e := &rails.Extractor{}
	apis, err := e.Extract("svc", "config/routes.rb", content)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(apis) != 1 {
		t.Fatalf("root: got %d routes, want 1", len(apis))
	}
	if apis[0].APIIdentifier != "GET /" {
		t.Errorf("root: got %q, want %q", apis[0].APIIdentifier, "GET /")
	}
}

// TestRailsExtractor_Match verifies match with via: clause.
func TestRailsExtractor_Match(t *testing.T) {
	content := []byte(`
Rails.application.routes.draw do
  match '/search', via: [:get, :post]
end
`)
	e := &rails.Extractor{}
	apis, err := e.Extract("svc", "config/routes.rb", content)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	got := make(map[string]bool)
	for _, a := range apis {
		got[a.APIIdentifier] = true
	}
	if !got["GET /search"] {
		t.Errorf("match: missing GET /search")
	}
	if !got["POST /search"] {
		t.Errorf("match: missing POST /search")
	}
}

// TestRailsExtractor_RoundTrip verifies Extract → UpsertCrossRepoAPI → ListCrossRepoAPIs.
func TestRailsExtractor_RoundTrip(t *testing.T) {
	content, err := os.ReadFile(fixtureFile)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	e := &rails.Extractor{}
	apis, err := e.Extract("round-trip-repo", "config/routes.rb", content)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(apis) == 0 {
		t.Fatal("Extract: no rows — nothing to round-trip")
	}

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	for i, a := range apis {
		if _, err := store.UpsertCrossRepoAPI(db, a); err != nil {
			t.Fatalf("UpsertCrossRepoAPI[%d]: %v", i, err)
		}
	}

	// Second pass — idempotency.
	for i, a := range apis {
		if _, err := store.UpsertCrossRepoAPI(db, a); err != nil {
			t.Fatalf("UpsertCrossRepoAPI idempotent[%d]: %v", i, err)
		}
	}

	recovered, err := store.ListCrossRepoAPIs(db, "round-trip-repo")
	if err != nil {
		t.Fatalf("ListCrossRepoAPIs: %v", err)
	}
	if len(recovered) != len(apis) {
		t.Errorf("round-trip: got %d rows, want %d", len(recovered), len(apis))
	}
	for _, r := range recovered {
		if r.APIKind != "http_route" {
			t.Errorf("recovered row %q: kind=%q, want http_route", r.APIIdentifier, r.APIKind)
		}
	}
}

func apiIdentifiers(apis []store.CrossRepoAPI) []string {
	out := make([]string, len(apis))
	for i, a := range apis {
		out[i] = a.APIIdentifier
	}
	return out
}
