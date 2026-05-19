package openapi_test

import (
	"encoding/json"
	"os"
	"sort"
	"testing"

	"force-orchestrator/internal/apiextract/openapi"
	"force-orchestrator/internal/store"
)

const fixtureFile = "../testdata/openapi-app/openapi.yaml"
const expectedFile = "../testdata/openapi-app/expected_openapi.json"
const accuracyThreshold = 0.95

func TestOpenAPIExtractor_Fixture(t *testing.T) {
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

	e := &openapi.Extractor{}
	if e.Kind() != "openapi_op" {
		t.Errorf("Kind() = %q, want openapi_op", e.Kind())
	}
	if e.ExtractorName() != "openapi-yaml" {
		t.Errorf("ExtractorName() = %q, want openapi-yaml", e.ExtractorName())
	}

	apis, err := e.Extract("my-api", "openapi.yaml", content)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	t.Logf("OpenAPI: extracted %d operations from fixture", len(apis))

	extracted := make(map[string]bool, len(apis))
	for _, a := range apis {
		if a.APIKind != "openapi_op" {
			t.Errorf("row %q: APIKind = %q, want openapi_op", a.APIIdentifier, a.APIKind)
		}
		if a.Extractor != "openapi-yaml" {
			t.Errorf("row %q: Extractor = %q, want openapi-yaml", a.APIIdentifier, a.Extractor)
		}
		if a.RepoName != "my-api" {
			t.Errorf("row %q: RepoName = %q, want my-api", a.APIIdentifier, a.RepoName)
		}
		extracted[a.APIIdentifier] = true
	}

	matched := 0
	for _, want := range expected {
		if extracted[want] {
			matched++
		} else {
			t.Logf("MISSING: %q", want)
		}
	}
	accuracy := float64(matched) / float64(len(expected))
	t.Logf("OpenAPI accuracy: %d/%d = %.1f%%", matched, len(expected), accuracy*100)
	if accuracy < accuracyThreshold {
		t.Errorf("accuracy %.2f < threshold %.2f", accuracy, accuracyThreshold)
	}
}

// TestOpenAPIExtractor_EmptyFile verifies that a spec with no paths returns no rows.
func TestOpenAPIExtractor_EmptyFile(t *testing.T) {
	e := &openapi.Extractor{}
	apis, err := e.Extract("repo", "openapi.yaml", []byte(`openapi: "3.0.0"`))
	if err != nil {
		t.Fatalf("Extract(no paths): %v", err)
	}
	if len(apis) != 0 {
		t.Errorf("Extract(no paths): got %d rows, want 0", len(apis))
	}
}

// TestOpenAPIExtractor_PathNormalization verifies that OpenAPI {id} params
// are normalized to :id canonical form.
func TestOpenAPIExtractor_PathNormalization(t *testing.T) {
	content := []byte(`
openapi: "3.0.0"
paths:
  /items/{itemId}:
    get:
      operationId: getItem
  /orders/{orderId}/lines/{lineId}:
    get:
      operationId: getOrderLine
`)
	e := &openapi.Extractor{}
	apis, err := e.Extract("svc", "openapi.yaml", content)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	got := make(map[string]bool)
	for _, a := range apis {
		got[a.APIIdentifier] = true
	}
	wants := []string{
		"GET /items/:itemId",
		"GET /orders/:orderId/lines/:lineId",
	}
	for _, w := range wants {
		if !got[w] {
			t.Errorf("missing normalized path %q; got: %v", w, sortedKeys(got))
		}
	}
}

// TestOpenAPIExtractor_JSONFormat verifies that JSON OpenAPI specs are parsed.
func TestOpenAPIExtractor_JSONFormat(t *testing.T) {
	content := []byte(`{
  "openapi": "3.0.0",
  "paths": {
    "/ping": {
      "get": {"operationId": "ping"},
      "head": {"operationId": "pingHead"}
    }
  }
}`)
	e := &openapi.Extractor{}
	apis, err := e.Extract("svc", "openapi.json", content)
	if err != nil {
		t.Fatalf("Extract(JSON): %v", err)
	}
	got := make(map[string]bool)
	for _, a := range apis {
		got[a.APIIdentifier] = true
	}
	if !got["GET /ping"] {
		t.Errorf("missing GET /ping from JSON spec")
	}
	if !got["HEAD /ping"] {
		t.Errorf("missing HEAD /ping from JSON spec")
	}
}

// TestOpenAPIExtractor_SourceLines verifies that YAML source lines are populated.
func TestOpenAPIExtractor_SourceLines(t *testing.T) {
	content, err := os.ReadFile(fixtureFile)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	e := &openapi.Extractor{}
	apis, err := e.Extract("svc", "openapi.yaml", content)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	for _, a := range apis {
		if a.SourceLine <= 0 {
			t.Errorf("row %q: SourceLine = %d, want > 0", a.APIIdentifier, a.SourceLine)
		}
	}
}

// TestOpenAPIExtractor_RoundTrip verifies Extract → UpsertCrossRepoAPI → ListCrossRepoAPIs.
func TestOpenAPIExtractor_RoundTrip(t *testing.T) {
	content, err := os.ReadFile(fixtureFile)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	e := &openapi.Extractor{}
	apis, err := e.Extract("openapi-rt-repo", "openapi.yaml", content)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(apis) == 0 {
		t.Fatal("Extract: no rows")
	}

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	for i, a := range apis {
		if _, err := store.UpsertCrossRepoAPI(db, a); err != nil {
			t.Fatalf("UpsertCrossRepoAPI[%d]: %v", i, err)
		}
	}
	// Idempotency pass.
	for i, a := range apis {
		if _, err := store.UpsertCrossRepoAPI(db, a); err != nil {
			t.Fatalf("UpsertCrossRepoAPI idempotent[%d]: %v", i, err)
		}
	}

	recovered, err := store.ListCrossRepoAPIs(db, "openapi-rt-repo")
	if err != nil {
		t.Fatalf("ListCrossRepoAPIs: %v", err)
	}
	if len(recovered) != len(apis) {
		t.Errorf("round-trip: got %d rows, want %d", len(recovered), len(apis))
	}
	for _, r := range recovered {
		if r.APIKind != "openapi_op" {
			t.Errorf("recovered row %q: kind=%q, want openapi_op", r.APIIdentifier, r.APIKind)
		}
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
