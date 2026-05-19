package express_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"force-orchestrator/internal/apiextract/express"
	"force-orchestrator/internal/store"
)

// testdataDir returns the path to internal/apiextract/testdata relative to
// this test file's location.
func testdataDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// thisFile is …/express/express_test.go; testdata is two levels up + testdata
	return filepath.Join(filepath.Dir(thisFile), "..", "testdata")
}

// expectedRow is the minimal shape we compare from expected_express.json.
type expectedRow struct {
	APIIdentifier string `json:"APIIdentifier"`
	SourceLine    int    `json:"SourceLine"`
	Extractor     string `json:"Extractor"`
}

func TestExpressExtract_Fixture(t *testing.T) {
	td := testdataDir(t)
	fixturePath := filepath.Join(td, "express-app", "routes.js")
	expectedPath := filepath.Join(td, "expected_express.json")

	content, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	var want []expectedRow
	expBytes, err := os.ReadFile(expectedPath)
	if err != nil {
		t.Fatalf("read expected: %v", err)
	}
	if err := json.Unmarshal(expBytes, &want); err != nil {
		t.Fatalf("parse expected: %v", err)
	}

	ext := express.Extractor{}
	got, err := ext.Extract("test-repo", fixturePath, content)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	// Build a map from APIIdentifier for fast lookup.
	gotByID := make(map[string]store.CrossRepoAPI, len(got))
	for _, r := range got {
		gotByID[r.APIIdentifier] = r
	}

	// Every expected row must be present.
	matched := 0
	for _, w := range want {
		r, ok := gotByID[w.APIIdentifier]
		if !ok {
			t.Errorf("missing route %q", w.APIIdentifier)
			continue
		}
		if r.SourceLine != w.SourceLine {
			t.Errorf("route %q: SourceLine = %d, want %d", w.APIIdentifier, r.SourceLine, w.SourceLine)
		}
		if r.Extractor != w.Extractor {
			t.Errorf("route %q: Extractor = %q, want %q", w.APIIdentifier, r.Extractor, w.Extractor)
		}
		matched++
	}

	// Accuracy check: matched / expected ≥ 0.70.
	const minAccuracy = 0.70
	accuracy := float64(matched) / float64(len(want))
	if accuracy < minAccuracy {
		t.Errorf("accuracy %.2f < %.2f (%d/%d routes matched)",
			accuracy, minAccuracy, matched, len(want))
	}
	t.Logf("Express extractor: %d routes extracted, %d/%d expected matched (accuracy %.0f%%)",
		len(got), matched, len(want), accuracy*100)
}

func TestExpressExtract_Metadata(t *testing.T) {
	ext := express.Extractor{}
	if got := ext.Kind(); got != "http_route" {
		t.Errorf("Kind() = %q, want %q", got, "http_route")
	}
	if got := ext.ExtractorName(); got != "express-app" {
		t.Errorf("ExtractorName() = %q, want %q", got, "express-app")
	}
}

func TestExpressExtract_Empty(t *testing.T) {
	ext := express.Extractor{}
	got, err := ext.Extract("repo", "empty.js", nil)
	if err != nil {
		t.Fatalf("Extract(nil): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Extract(nil): got %d rows, want 0", len(got))
	}
}

func TestExpressExtract_SkipsDynamicTemplateLiterals(t *testing.T) {
	src := []byte("app.get(`${prefix}/items`, handler);\n")
	ext := express.Extractor{}
	got, err := ext.Extract("repo", "f.js", src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("dynamic template literal should be skipped; got %+v", got)
	}
}

func TestExpressExtract_AllExpands(t *testing.T) {
	src := []byte("app.all('/health', h);\n")
	ext := express.Extractor{}
	got, err := ext.Extract("repo", "f.js", src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	wantMethods := map[string]bool{
		"GET /health":    false,
		"POST /health":   false,
		"PUT /health":    false,
		"PATCH /health":  false,
		"DELETE /health": false,
	}
	for _, r := range got {
		if _, ok := wantMethods[r.APIIdentifier]; ok {
			wantMethods[r.APIIdentifier] = true
		}
	}
	for id, found := range wantMethods {
		if !found {
			t.Errorf("app.all expansion missing %q", id)
		}
	}
	if len(got) != 5 {
		t.Errorf("app.all: got %d rows, want 5", len(got))
	}
}

func TestExpressExtract_Idempotent(t *testing.T) {
	td := testdataDir(t)
	content, err := os.ReadFile(filepath.Join(td, "express-app", "routes.js"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	ext := express.Extractor{}
	got1, _ := ext.Extract("repo", "f.js", content)
	got2, _ := ext.Extract("repo", "f.js", content)
	if len(got1) != len(got2) {
		t.Errorf("idempotent: first=%d, second=%d", len(got1), len(got2))
	}
}

func TestExpressExtract_PathNormalization(t *testing.T) {
	cases := []struct {
		src  string
		want string
	}{
		// :param stays as-is (already canonical)
		{`app.get('/users/:id', h);`, "GET /users/:id"},
		// {param} → :param
		{`app.get('/users/{id}', h);`, "GET /users/:id"},
		// trailing slash trimmed
		{`app.get('/users/', h);`, "GET /users"},
	}
	ext := express.Extractor{}
	for _, tc := range cases {
		got, err := ext.Extract("repo", "f.js", []byte(tc.src))
		if err != nil {
			t.Fatalf("Extract(%q): %v", tc.src, err)
		}
		if len(got) == 0 {
			t.Errorf("Extract(%q): no routes returned", tc.src)
			continue
		}
		if got[0].APIIdentifier != tc.want {
			t.Errorf("Extract(%q): got %q, want %q", tc.src, got[0].APIIdentifier, tc.want)
		}
	}
}
