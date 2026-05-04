package patterns

import (
	"path/filepath"
	"strings"
	"testing"

	"force-orchestrator/internal/archaeologist"
)

// TestArchaeologistARCH003_HappyPath plants two functions with the
// same structural signature in different files and asserts ARCH-003
// surfaces both as a duplicate-abstractions cluster.
func TestArchaeologistARCH003_HappyPath(t *testing.T) {
	dir := t.TempDir()
	// Two functions with identical signature shape: (string) (int, error)
	// and similar body kind histogram (one assignment, one return).
	writeF(t, filepath.Join(dir, "a.go"), "package x\n\nfunc CountA(s string) (int, error) {\n\tn := len(s)\n\treturn n, nil\n}\n")
	writeF(t, filepath.Join(dir, "b.go"), "package x\n\nfunc CountB(s string) (int, error) {\n\tn := len(s)\n\treturn n, nil\n}\n")
	// A third function with a different signature — should NOT join the cluster.
	writeF(t, filepath.Join(dir, "c.go"), "package x\n\nfunc Other() {}\n")

	hits := NewARCH003().Scan(&archaeologist.Repo{ID: 1, Name: "synthetic", LocalPath: dir})
	if len(hits) != 2 {
		t.Fatalf("expected 2 ARCH-003 hits (CountA + CountB cluster), got %d: %+v", len(hits), hits)
	}
	files := map[string]bool{}
	for _, h := range hits {
		files[h.FilePath] = true
		if !strings.Contains(h.DetailJSON, "signature_hash") {
			t.Errorf("hit DetailJSON missing signature_hash: %s", h.DetailJSON)
		}
	}
	if !files["a.go"] || !files["b.go"] {
		t.Errorf("expected hits in both a.go and b.go, got %v", files)
	}
}

// TestArchaeologistARCH003_NoDuplicates returns no hits when every
// function has a unique signature.
func TestArchaeologistARCH003_NoDuplicates(t *testing.T) {
	dir := t.TempDir()
	writeF(t, filepath.Join(dir, "a.go"), "package x\n\nfunc One(s string) error { return nil }\n")
	writeF(t, filepath.Join(dir, "b.go"), "package x\n\nfunc Two(n int) (string, error) { return \"\", nil }\n")

	hits := NewARCH003().Scan(&archaeologist.Repo{ID: 1, Name: "no-dup-repo", LocalPath: dir})
	if len(hits) != 0 {
		t.Fatalf("expected 0 hits (unique signatures), got %d: %+v", len(hits), hits)
	}
}

// TestArchaeologistARCH003_SkipsTestFiles ensures duplicate test
// helpers don't trigger the pattern (test helpers are expected to
// duplicate; unifying them is a separate refactor concern).
func TestArchaeologistARCH003_SkipsTestFiles(t *testing.T) {
	dir := t.TempDir()
	writeF(t, filepath.Join(dir, "a_test.go"), "package x\n\nfunc HelperA(s string) (int, error) {\n\tn := len(s)\n\treturn n, nil\n}\n")
	writeF(t, filepath.Join(dir, "b_test.go"), "package x\n\nfunc HelperB(s string) (int, error) {\n\tn := len(s)\n\treturn n, nil\n}\n")

	hits := NewARCH003().Scan(&archaeologist.Repo{ID: 1, Name: "test-only-repo", LocalPath: dir})
	if len(hits) != 0 {
		t.Fatalf("expected 0 hits (test files skipped), got %d: %+v", len(hits), hits)
	}
}
