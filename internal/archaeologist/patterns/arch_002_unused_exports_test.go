package patterns

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"force-orchestrator/internal/archaeologist"
	"force-orchestrator/internal/store"
)

// TestArchaeologistARCH002_NoDBReturnsZero pins the P9 contract: when
// the cross-repo graph DB has not been injected, Scan returns nil and
// emits no findings. The D8-merge-gate environment override is kept
// for backwards-compat with the historical sentinel test.
func TestArchaeologistARCH002_NoDBReturnsZero(t *testing.T) {
	if os.Getenv("D8_GRAPH_AVAILABLE") != "" {
		t.Skip("D8-MERGE-GATE: ARCH-002 cross-repo wiring lit up — this no-DB test is replaced by TestArchaeologistARCH002_WalkAndQuery")
	}
	// Clear any prior injection from a sibling test in this package.
	SetCrossRepoGraphDB(nil)

	dir := t.TempDir()
	writeF(t, dir+"/exported.go", "package x\n\nfunc Public() {}\n")

	hits := NewARCH002().Scan(&archaeologist.Repo{ID: 1, Name: "stub-repo", LocalPath: dir})
	if len(hits) != 0 {
		t.Fatalf("no-DB injection: expected 0 ARCH-002 hits, got %d", len(hits))
	}
}

// TestArchaeologistARCH002_LookupSentinel pins the legacy sentinel
// return value for the D8-merge-gate test fixture. The seam is no
// longer called from production code (Scan goes through
// store.LookupCrossRepoSymbolID directly), but the function and its
// -1 sentinel are kept for backwards-compatibility.
func TestArchaeologistARCH002_LookupSentinel(t *testing.T) {
	got := lookupCrossRepoConsumers("force-orchestrator", "force-orchestrator/internal/foo.Bar")
	if got != -1 {
		t.Errorf("lookupCrossRepoConsumers stub: got %d, want -1 (sentinel)", got)
	}
}

// TestArchaeologistARCH002_WalkAndQuery is the post-D8 happy path:
//   - DB seeded with one indexed symbol (foo.Used, has 1 consumer)
//     and one indexed symbol (foo.Unused, has 0 consumers).
//   - Working tree contains both symbols + one symbol the dog has not
//     indexed yet (foo.Brand_new — skipped per P9).
//   - Scan must emit exactly one hit — for foo.Unused — at the
//     correct file:line.
func TestArchaeologistARCH002_WalkAndQuery(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	SetCrossRepoGraphDB(db)
	t.Cleanup(func() { SetCrossRepoGraphDB(nil) })

	dir := t.TempDir()
	// Two-file fixture so we exercise multi-file walking. Both files
	// declare package foo so the qualified names look like foo.X.
	writeF(t, filepath.Join(dir, "a.go"),
		"package foo\n\n// Used is consumed externally; not flagged.\nfunc Used() {}\n\n// Unused has zero consumers; flagged.\nfunc Unused() {}\n")
	writeF(t, filepath.Join(dir, "b.go"),
		"package foo\n\n// Brand_new has no CrossRepoSymbols row yet — skipped per P9.\nfunc BrandNew() {}\n\n// unexportedFunc is not a candidate.\nfunc unexportedFunc() {}\n")

	// Seed two symbol rows: Used (with a consumer edge) + Unused
	// (with no edges). BrandNew is intentionally NOT seeded so the
	// P9 "skip unindexed" branch fires.
	usedID, err := store.UpsertCrossRepoSymbol(db, store.CrossRepoSymbol{
		RepoName:   "test-repo",
		SymbolPath: "foo.Used",
		SymbolKind: "function",
		FilePath:   "a.go",
		LineNumber: 4,
		IsPublic:   true,
	})
	if err != nil {
		t.Fatalf("UpsertCrossRepoSymbol(Used): %v", err)
	}
	if _, err := store.UpsertCrossRepoSymbol(db, store.CrossRepoSymbol{
		RepoName:   "test-repo",
		SymbolPath: "foo.Unused",
		SymbolKind: "function",
		FilePath:   "a.go",
		LineNumber: 7,
		IsPublic:   true,
	}); err != nil {
		t.Fatalf("UpsertCrossRepoSymbol(Unused): %v", err)
	}
	// Seed one consumer edge pointing at Used.
	if _, err := store.UpsertCrossRepoDependency(db, store.CrossRepoDependency{
		ConsumerRepoName: "consumer-repo",
		ConsumerFile:     "main.go",
		ConsumerLine:     42,
		ProviderSymbolID: usedID,
	}); err != nil {
		t.Fatalf("UpsertCrossRepoDependency(Used): %v", err)
	}

	hits := NewARCH002().Scan(&archaeologist.Repo{ID: 1, Name: "test-repo", LocalPath: dir})
	if len(hits) != 1 {
		t.Fatalf("expected 1 hit (Unused), got %d: %+v", len(hits), hits)
	}
	if hits[0].FilePath != "a.go" {
		t.Errorf("hit file = %q, want a.go", hits[0].FilePath)
	}
	if hits[0].DetailJSON == "" || hits[0].DetailJSON == "{}" {
		t.Errorf("hit detail_json should describe the symbol, got %q", hits[0].DetailJSON)
	}
	// Sanity: the detail mentions the qualified name "foo.Unused".
	if got := hits[0].DetailJSON; !contains(got, "foo.Unused") {
		t.Errorf("detail_json = %q, want it to mention foo.Unused", got)
	}
}

// TestArchaeologistARCH002_EmitsTypesAndVars exercises the full AST
// walker on every supported kind (function / method / type / var /
// const). All exported, all with zero consumers — expect a hit per
// symbol (kind passed through in detail_json).
func TestArchaeologistARCH002_EmitsTypesAndVars(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	SetCrossRepoGraphDB(db)
	t.Cleanup(func() { SetCrossRepoGraphDB(nil) })

	dir := t.TempDir()
	src := `package bar

// ExportedFunc.
func ExportedFunc() {}

// ExportedType is a type.
type ExportedType struct{}

// ExportedMethod on ExportedType.
func (t *ExportedType) ExportedMethod() {}

// ExportedVar.
var ExportedVar = 1

// ExportedConst.
const ExportedConst = "k"

// unexportedFunc should be skipped.
func unexportedFunc() {}
`
	writeF(t, filepath.Join(dir, "s.go"), src)

	// Seed all 5 exported symbols with zero consumers.
	want := map[string]string{
		"bar.ExportedFunc":                "function",
		"bar.ExportedType":                "type",
		"bar.ExportedType.ExportedMethod": "method",
		"bar.ExportedVar":                 "var",
		"bar.ExportedConst":               "const",
	}
	for path, kind := range want {
		if _, err := store.UpsertCrossRepoSymbol(db, store.CrossRepoSymbol{
			RepoName:   "test-repo",
			SymbolPath: path,
			SymbolKind: kind,
			FilePath:   "s.go",
			LineNumber: 1,
			IsPublic:   true,
		}); err != nil {
			t.Fatalf("seed %s: %v", path, err)
		}
	}

	hits := NewARCH002().Scan(&archaeologist.Repo{ID: 1, Name: "test-repo", LocalPath: dir})
	if len(hits) != len(want) {
		gotPaths := make([]string, 0, len(hits))
		for _, h := range hits {
			gotPaths = append(gotPaths, h.DetailJSON)
		}
		sort.Strings(gotPaths)
		t.Fatalf("expected %d hits, got %d: %v", len(want), len(hits), gotPaths)
	}
	// Each expected qualified name must appear in some hit's detail_json.
	for path := range want {
		found := false
		for _, h := range hits {
			if contains(h.DetailJSON, path) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no hit found for %s", path)
		}
	}
}

// contains is a tiny strings.Contains shim so this file doesn't need
// to import strings (writeF is in arch_001_deprecated_api_test.go and
// uses os.WriteFile).
func contains(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
