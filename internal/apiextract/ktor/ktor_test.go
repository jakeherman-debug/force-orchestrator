package ktor_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"force-orchestrator/internal/apiextract/ktor"
	"force-orchestrator/internal/store"
)

type expectedRow struct {
	APIKind       string `json:"api_kind"`
	APIIdentifier string `json:"api_identifier"`
	SourceLine    int    `json:"source_line"`
	Extractor     string `json:"extractor"`
}

func testDataDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	// ktor_test.go lives in internal/apiextract/ktor/ — testdata is two dirs up.
	return filepath.Join(wd, "..", "testdata")
}

func loadFixture(t *testing.T, srcFile, jsonFile string) ([]byte, []expectedRow) {
	t.Helper()
	src, err := os.ReadFile(srcFile)
	if err != nil {
		t.Fatalf("read source %q: %v", srcFile, err)
	}
	raw, err := os.ReadFile(jsonFile)
	if err != nil {
		t.Fatalf("read expected %q: %v", jsonFile, err)
	}
	var rows []expectedRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("unmarshal %q: %v", jsonFile, err)
	}
	return src, rows
}

func assertAccuracy(t *testing.T, label string, got []store.CrossRepoAPI, expected []expectedRow, minAccuracy float64) {
	t.Helper()

	wantSet := make(map[string]bool, len(expected))
	for _, e := range expected {
		wantSet[e.APIIdentifier] = true
	}

	matched := 0
	for _, g := range got {
		if wantSet[g.APIIdentifier] {
			matched++
		}
	}

	accuracy := float64(matched) / float64(len(expected))
	t.Logf("%s: extracted %d routes, matched %d/%d expected (accuracy %.0f%%)",
		label, len(got), matched, len(expected), accuracy*100)

	if accuracy < minAccuracy {
		t.Errorf("%s: accuracy %.2f < required %.2f", label, accuracy, minAccuracy)
		t.Logf("  extracted:")
		for _, g := range got {
			t.Logf("    %s  (line %d)", g.APIIdentifier, g.SourceLine)
		}
		t.Logf("  expected:")
		for _, e := range expected {
			t.Logf("    %s", e.APIIdentifier)
		}
	}
}

// TestKtorExtractor verifies end-to-end extraction from the Ktor fixture.
func TestKtorExtractor(t *testing.T) {
	td := testDataDir(t)
	srcFile := filepath.Join(td, "ktor-app", "Application.kt")
	jsonFile := filepath.Join(td, "expected_ktor.json")

	content, expected := loadFixture(t, srcFile, jsonFile)

	ext := ktor.New()
	if ext.Kind() != "http_route" {
		t.Errorf("Kind() = %q, want http_route", ext.Kind())
	}
	if ext.ExtractorName() != "ktor-routing" {
		t.Errorf("ExtractorName() = %q, want ktor-routing", ext.ExtractorName())
	}

	const repoName = "ktor-service"
	got, err := ext.Extract(repoName, srcFile, content)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("Extract returned no routes")
	}

	// Validate field values.
	for _, g := range got {
		if g.APIKind != "http_route" {
			t.Errorf("row %q: api_kind = %q, want http_route", g.APIIdentifier, g.APIKind)
		}
		if g.Extractor != "ktor-routing" {
			t.Errorf("row %q: extractor = %q, want ktor-routing", g.APIIdentifier, g.Extractor)
		}
		if g.RepoName != repoName {
			t.Errorf("row %q: repo_name = %q, want %q", g.APIIdentifier, g.RepoName, repoName)
		}
		if g.SourceFile != srcFile {
			t.Errorf("row %q: source_file = %q, want %q", g.APIIdentifier, g.SourceFile, srcFile)
		}
		if g.SourceLine <= 0 {
			t.Errorf("row %q: source_line = %d, want > 0", g.APIIdentifier, g.SourceLine)
		}
	}

	// Accuracy threshold: ≥ 75% as stated in the spec.
	assertAccuracy(t, "Ktor", got, expected, 0.75)
}

// TestKtorIdempotent verifies that running Extract twice yields identical results.
func TestKtorIdempotent(t *testing.T) {
	td := testDataDir(t)
	srcFile := filepath.Join(td, "ktor-app", "Application.kt")
	content, _ := loadFixture(t, srcFile, filepath.Join(td, "expected_ktor.json"))

	ext := ktor.New()
	got1, err := ext.Extract("svc", srcFile, content)
	if err != nil {
		t.Fatalf("Extract (1st): %v", err)
	}
	got2, err := ext.Extract("svc", srcFile, content)
	if err != nil {
		t.Fatalf("Extract (2nd): %v", err)
	}

	if len(got1) != len(got2) {
		t.Errorf("idempotent: got %d then %d routes", len(got1), len(got2))
	}
	for i := range got1 {
		if i >= len(got2) {
			break
		}
		if got1[i].APIIdentifier != got2[i].APIIdentifier {
			t.Errorf("idempotent[%d]: %q vs %q", i, got1[i].APIIdentifier, got2[i].APIIdentifier)
		}
	}
}

// TestKtorSkipsNonKotlin confirms non-.kt files are ignored.
func TestKtorSkipsNonKotlin(t *testing.T) {
	ext := ktor.New()
	got, err := ext.Extract("svc", "routes.java", []byte(`get("/health") {}`))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected no routes for .java file, got %d", len(got))
	}
}

// TestKtorEmptyFile verifies an empty file is handled without error.
func TestKtorEmptyFile(t *testing.T) {
	ext := ktor.New()
	got, err := ext.Extract("svc", "Empty.kt", []byte{})
	if err != nil {
		t.Fatalf("Extract (empty): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected no routes from empty file, got %d", len(got))
	}
}

// TestKtorPathNormalization verifies {id} style params are normalized to :id.
func TestKtorPathNormalization(t *testing.T) {
	src := []byte(`
fun Application.module() {
    routing {
        get("/items/{itemId}/details") {
            call.respondText("ok")
        }
    }
}
`)
	ext := ktor.New()
	got, err := ext.Extract("svc", "App.kt", src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	found := false
	for _, g := range got {
		if g.APIIdentifier == "GET /items/:itemId/details" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected normalized route GET /items/:itemId/details; got: %v", identifiers(got))
	}
}

// TestKtorNestedRoutes verifies that nested route{} blocks accumulate prefixes.
func TestKtorNestedRoutes(t *testing.T) {
	src := []byte(`
fun Application.module() {
    routing {
        route("/api") {
            route("/v1") {
                get("/ping") {
                    call.respondText("pong")
                }
            }
        }
    }
}
`)
	ext := ktor.New()
	got, err := ext.Extract("svc", "App.kt", src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	found := false
	for _, g := range got {
		if g.APIIdentifier == "GET /api/v1/ping" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected GET /api/v1/ping from nested routes; got: %v", identifiers(got))
	}
}

// TestKtorTopLevelRoute verifies a verb at routing top-level (no route{} wrapper).
func TestKtorTopLevelRoute(t *testing.T) {
	src := []byte(`
fun Application.module() {
    routing {
        get("/health") {
            call.respondText("OK")
        }
    }
}
`)
	ext := ktor.New()
	got, err := ext.Extract("svc", "App.kt", src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	found := false
	for _, g := range got {
		if g.APIIdentifier == "GET /health" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected GET /health; got: %v", identifiers(got))
	}
}

func identifiers(apis []store.CrossRepoAPI) []string {
	out := make([]string, len(apis))
	for i, a := range apis {
		out[i] = a.APIIdentifier
	}
	return out
}
