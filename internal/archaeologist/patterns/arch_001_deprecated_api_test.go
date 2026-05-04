package patterns

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"force-orchestrator/internal/archaeologist"
)

// TestArchaeologistARCH001_HappyPath plants three deprecated-symbol
// hits in a synthetic repo (one ioutil.ReadFile call, two
// ioutil.WriteFile calls) and asserts ARCH-001 surfaces all three.
func TestArchaeologistARCH001_HappyPath(t *testing.T) {
	dir := t.TempDir()
	writeF(t, filepath.Join(dir, "main.go"), "package main\n\nimport \"io/ioutil\"\n\nfunc Read() { ioutil.ReadFile(\"x\") }\nfunc WriteOne() { ioutil.WriteFile(\"a\", nil, 0) }\nfunc WriteTwo() { ioutil.WriteFile(\"b\", nil, 0) }\n")

	hits := NewARCH001().Scan(&archaeologist.Repo{ID: 1, Name: "synthetic", LocalPath: dir})
	if len(hits) != 3 {
		t.Fatalf("expected 3 ARCH-001 hits, got %d: %+v", len(hits), hits)
	}
	for _, h := range hits {
		if h.FilePath != "main.go" {
			t.Errorf("hit FilePath = %q, want main.go", h.FilePath)
		}
		if h.LineNumber <= 0 {
			t.Errorf("hit LineNumber = %d, want > 0", h.LineNumber)
		}
		if !strings.Contains(h.DetailJSON, "deprecated_symbol") {
			t.Errorf("hit DetailJSON missing deprecated_symbol field: %s", h.DetailJSON)
		}
	}
}

// TestArchaeologistARCH001_LanguageAware is the anti-cheat #2 pin:
// a Go-API pattern MUST NOT scan Rust files. Plant a .rs file
// containing the deprecated Go symbol; ARCH-001 must return zero hits.
func TestArchaeologistARCH001_LanguageAware(t *testing.T) {
	dir := t.TempDir()
	writeF(t, filepath.Join(dir, "lib.rs"), "// fn read() { ioutil.ReadFile(\"x\") }\n")
	writeF(t, filepath.Join(dir, "lib.py"), "import os; ioutil.ReadFile('x')\n")

	hits := NewARCH001().Scan(&archaeologist.Repo{ID: 1, Name: "synthetic-rust", LocalPath: dir})
	if len(hits) != 0 {
		t.Fatalf("anti-cheat #2: ARCH-001 (Go-only) returned %d hits on Rust/Python files: %+v", len(hits), hits)
	}
}

// TestArchaeologistARCH001_EmptyRepo tolerates an empty / non-existent
// repo gracefully (returns nil, no panic).
func TestArchaeologistARCH001_EmptyRepo(t *testing.T) {
	if hits := NewARCH001().Scan(nil); hits != nil {
		t.Errorf("nil repo: hits = %v, want nil", hits)
	}
	if hits := NewARCH001().Scan(&archaeologist.Repo{}); hits != nil {
		t.Errorf("empty repo: hits = %v, want nil", hits)
	}
}

// writeF writes content to path (creating parent dirs as needed).
func writeF(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
