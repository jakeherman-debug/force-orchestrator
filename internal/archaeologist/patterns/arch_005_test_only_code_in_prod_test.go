package patterns

import (
	"path/filepath"
	"strings"
	"testing"

	"force-orchestrator/internal/archaeologist"
)

// TestArchaeologistARCH005_HappyPath plants one non-_test.go file
// that imports "testing" and another that takes a *testing.T
// parameter; asserts ARCH-005 surfaces both.
func TestArchaeologistARCH005_HappyPath(t *testing.T) {
	dir := t.TempDir()
	// Production file importing "testing" — anti-pattern.
	writeF(t, filepath.Join(dir, "leaky.go"), "package x\n\nimport \"testing\"\n\nfunc Helper(t *testing.T) {}\n")
	// Production file with no test-only imports.
	writeF(t, filepath.Join(dir, "clean.go"), "package x\n\nimport \"strings\"\n\nfunc Trim(s string) string { return strings.TrimSpace(s) }\n")

	hits := NewARCH005().Scan(&archaeologist.Repo{ID: 1, Name: "leaky-repo", LocalPath: dir})
	if len(hits) < 2 {
		// Expect at least 2: the import line + the testing.T parameter.
		t.Fatalf("expected at least 2 ARCH-005 hits (import + parameter), got %d: %+v", len(hits), hits)
	}
	for _, h := range hits {
		if h.FilePath != "leaky.go" {
			t.Errorf("hit FilePath = %q, want leaky.go (clean.go must not be flagged)", h.FilePath)
		}
	}
	// Detail must reference the import path or the function name.
	combined := ""
	for _, h := range hits {
		combined += h.DetailJSON
	}
	if !strings.Contains(combined, "testing") {
		t.Errorf("hit details lack 'testing' reference: %s", combined)
	}
}

// TestArchaeologistARCH005_SkipsTestFiles confirms that _test.go
// files are exempt — they are allowed to import "testing".
func TestArchaeologistARCH005_SkipsTestFiles(t *testing.T) {
	dir := t.TempDir()
	writeF(t, filepath.Join(dir, "x_test.go"), "package x\n\nimport \"testing\"\n\nfunc TestX(t *testing.T) {}\n")

	hits := NewARCH005().Scan(&archaeologist.Repo{ID: 1, Name: "test-only-repo", LocalPath: dir})
	if len(hits) != 0 {
		t.Fatalf("expected 0 hits (_test.go exempt), got %d: %+v", len(hits), hits)
	}
}

// TestArchaeologistARCH005_LanguageAware (anti-cheat #2): a Rust
// file containing the literal substring "testing" must NOT trigger
// ARCH-005 (which is Go-only).
func TestArchaeologistARCH005_LanguageAware(t *testing.T) {
	dir := t.TempDir()
	writeF(t, filepath.Join(dir, "lib.rs"), "// import \"testing\"\nfn main() {}\n")

	hits := NewARCH005().Scan(&archaeologist.Repo{ID: 1, Name: "rust-repo", LocalPath: dir})
	if len(hits) != 0 {
		t.Fatalf("anti-cheat #2: ARCH-005 returned %d hits on Rust file: %+v", len(hits), hits)
	}
}
