package spring_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"force-orchestrator/internal/apiextract/spring"
	"force-orchestrator/internal/store"
)

// expectedRow is the minimal fixture shape we unmarshal from the JSON files.
type expectedRow struct {
	APIKind       string `json:"api_kind"`
	APIIdentifier string `json:"api_identifier"`
	SourceLine    int    `json:"source_line"`
	Extractor     string `json:"extractor"`
}

// loadFixture reads a source file and the corresponding JSON expectations.
func loadFixture(t *testing.T, srcFile, jsonFile string) ([]byte, []expectedRow) {
	t.Helper()
	src, err := os.ReadFile(srcFile)
	if err != nil {
		t.Fatalf("read source fixture %q: %v", srcFile, err)
	}
	raw, err := os.ReadFile(jsonFile)
	if err != nil {
		t.Fatalf("read expected JSON %q: %v", jsonFile, err)
	}
	var expected []expectedRow
	if err := json.Unmarshal(raw, &expected); err != nil {
		t.Fatalf("unmarshal %q: %v", jsonFile, err)
	}
	return src, expected
}

// testData returns the absolute path to the testdata directory.
func testDataDir(t *testing.T) string {
	t.Helper()
	// spring_test.go lives in internal/apiextract/spring/ so testdata is
	// two directories up from here.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Join(wd, "..", "testdata")
}

// assertAccuracy checks that the extracted rows cover the expected set with
// at least the given accuracy threshold (0.0–1.0) and reports per-row misses.
func assertAccuracy(t *testing.T, label string, got []store.CrossRepoAPI, expected []expectedRow, minAccuracy float64) {
	t.Helper()

	// Build a lookup set of expected identifiers.
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
		t.Logf("  got:")
		for _, g := range got {
			t.Logf("    %s  (line %d)", g.APIIdentifier, g.SourceLine)
		}
		t.Logf("  expected:")
		for _, e := range expected {
			t.Logf("    %s", e.APIIdentifier)
		}
	}
}

// assertFieldValues validates that every extracted row has the correct constant
// field values (api_kind, extractor, repo_name, source_file).
func assertFieldValues(t *testing.T, got []store.CrossRepoAPI, repoName, filePath string) {
	t.Helper()
	for _, g := range got {
		if g.APIKind != "http_route" {
			t.Errorf("row %q: api_kind = %q, want http_route", g.APIIdentifier, g.APIKind)
		}
		if g.Extractor != "spring-annotation" {
			t.Errorf("row %q: extractor = %q, want spring-annotation", g.APIIdentifier, g.Extractor)
		}
		if g.RepoName != repoName {
			t.Errorf("row %q: repo_name = %q, want %q", g.APIIdentifier, g.RepoName, repoName)
		}
		if g.SourceFile != filePath {
			t.Errorf("row %q: source_file = %q, want %q", g.APIIdentifier, g.SourceFile, filePath)
		}
		if g.SourceLine <= 0 {
			t.Errorf("row %q: source_line = %d, want > 0", g.APIIdentifier, g.SourceLine)
		}
	}
}

// TestSpringJava verifies extraction from the Java fixture.
func TestSpringJava(t *testing.T) {
	td := testDataDir(t)
	srcFile := filepath.Join(td, "spring-app", "UserController.java")
	jsonFile := filepath.Join(td, "expected_spring.json")

	content, expected := loadFixture(t, srcFile, jsonFile)

	ext := spring.New()
	if ext.Kind() != "http_route" {
		t.Errorf("Kind() = %q, want http_route", ext.Kind())
	}
	if ext.ExtractorName() != "spring-annotation" {
		t.Errorf("ExtractorName() = %q, want spring-annotation", ext.ExtractorName())
	}

	const repoName = "user-service"
	got, err := ext.Extract(repoName, srcFile, content)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("Extract returned no routes")
	}

	assertFieldValues(t, got, repoName, srcFile)
	assertAccuracy(t, "Spring Java", got, expected, 0.90)

	// Verify line numbers for each expected route match within ±1.
	gotByIdent := make(map[string]store.CrossRepoAPI, len(got))
	for _, g := range got {
		gotByIdent[g.APIIdentifier] = g
	}
	for _, e := range expected {
		g, ok := gotByIdent[e.APIIdentifier]
		if !ok {
			continue
		}
		if abs(g.SourceLine-e.SourceLine) > 1 {
			t.Errorf("source_line for %q: got %d, want %d (±1)", e.APIIdentifier, g.SourceLine, e.SourceLine)
		}
	}
}

// TestSpringKotlin verifies extraction from the Kotlin fixture.
func TestSpringKotlin(t *testing.T) {
	td := testDataDir(t)
	srcFile := filepath.Join(td, "spring-app-kotlin", "UserController.kt")
	jsonFile := filepath.Join(td, "expected_spring_kotlin.json")

	content, expected := loadFixture(t, srcFile, jsonFile)

	ext := spring.New()
	const repoName = "user-service-kt"
	got, err := ext.Extract(repoName, srcFile, content)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("Extract returned no routes")
	}

	assertFieldValues(t, got, repoName, srcFile)
	assertAccuracy(t, "Spring Kotlin", got, expected, 0.90)
}

// TestSpringIdempotent verifies that running Extract twice on the same content
// returns the same results.
func TestSpringIdempotent(t *testing.T) {
	td := testDataDir(t)
	srcFile := filepath.Join(td, "spring-app", "UserController.java")
	content, _ := loadFixture(t, srcFile, filepath.Join(td, "expected_spring.json"))

	ext := spring.New()
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

// TestSpringSkipsNonJavaKotlin confirms that non-.java/.kt files are ignored.
func TestSpringSkipsNonJavaKotlin(t *testing.T) {
	ext := spring.New()
	got, err := ext.Extract("svc", "routes.rb", []byte(`@GetMapping("/users")`))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected no routes for .rb file, got %d", len(got))
	}
}

// TestSpringEmptyFile verifies that an empty file returns no routes without error.
func TestSpringEmptyFile(t *testing.T) {
	ext := spring.New()
	got, err := ext.Extract("svc", "Empty.java", []byte{})
	if err != nil {
		t.Fatalf("Extract (empty): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected no routes from empty file, got %d", len(got))
	}
}

// TestSpringNoController verifies a file with annotations but no @RestController
// / @Controller does not produce routes from method annotations that lack a
// class-level base (they still extract, just with no base path).
func TestSpringNoController(t *testing.T) {
	src := []byte(`
public class SomeService {
    @GetMapping("/items")
    public List<Item> list() { return null; }
}
`)
	ext := spring.New()
	got, err := ext.Extract("svc", "SomeService.java", src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	// Without @RestController we still extract the method-level annotation
	// (the extractor is intentionally greedy — the Spring spec allows
	// @GetMapping on any @Component-derived bean).
	for _, g := range got {
		if g.APIIdentifier != "GET /items" {
			t.Errorf("unexpected identifier %q", g.APIIdentifier)
		}
	}
}

// TestSpringPathNormalization verifies {id} style params are normalized to :id.
func TestSpringPathNormalization(t *testing.T) {
	src := []byte(`
@RestController
@RequestMapping("/api")
public class Ctrl {
    @GetMapping("/items/{itemId}/details")
    public Item detail() { return null; }
}
`)
	ext := spring.New()
	got, err := ext.Extract("svc", "Ctrl.java", src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	found := false
	for _, g := range got {
		if g.APIIdentifier == "GET /api/items/:itemId/details" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected normalized route GET /api/items/:itemId/details; got: %v", identifiers(got))
	}
}

func identifiers(apis []store.CrossRepoAPI) []string {
	out := make([]string, len(apis))
	for i, a := range apis {
		out[i] = a.APIIdentifier
	}
	return out
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
