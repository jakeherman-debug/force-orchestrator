package patterns

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"force-orchestrator/internal/archaeologist"
)

// TestArchaeologistARCH004_HappyPath plants two stale yaml files
// (older than the threshold) and one fresh one; asserts ARCH-004
// flags the stale pair only.
func TestArchaeologistARCH004_HappyPath(t *testing.T) {
	dir := t.TempDir()
	stalePath1 := filepath.Join(dir, "old1.yaml")
	stalePath2 := filepath.Join(dir, "configs", "old2.toml")
	freshPath := filepath.Join(dir, "fresh.yaml")
	writeF(t, stalePath1, "key: value\n")
	writeF(t, stalePath2, "[section]\nkey = \"value\"\n")
	writeF(t, freshPath, "key: value\n")

	// Backdate the stale files by 400 days (> 365 threshold).
	old := time.Now().Add(-400 * 24 * time.Hour)
	if err := os.Chtimes(stalePath1, old, old); err != nil {
		t.Fatalf("chtimes %s: %v", stalePath1, err)
	}
	if err := os.Chtimes(stalePath2, old, old); err != nil {
		t.Fatalf("chtimes %s: %v", stalePath2, err)
	}

	hits := NewARCH004().Scan(&archaeologist.Repo{ID: 1, Name: "stale-repo", LocalPath: dir})
	if len(hits) != 2 {
		t.Fatalf("expected 2 ARCH-004 hits, got %d: %+v", len(hits), hits)
	}
	files := map[string]bool{}
	for _, h := range hits {
		files[filepath.ToSlash(h.FilePath)] = true
		if !strings.Contains(h.DetailJSON, "age_days") {
			t.Errorf("hit DetailJSON missing age_days: %s", h.DetailJSON)
		}
	}
	if !files["old1.yaml"] || !files["configs/old2.toml"] {
		t.Errorf("missing expected stale entries; got %v", files)
	}
	if files["fresh.yaml"] {
		t.Errorf("fresh.yaml should not have been flagged")
	}
}

// TestArchaeologistARCH004_SkipsLockFiles confirms .lock / .sum files
// (which legitimately go untouched between dep updates) are not
// flagged.
func TestArchaeologistARCH004_SkipsLockFiles(t *testing.T) {
	dir := t.TempDir()
	stalePath := filepath.Join(dir, "config.json.lock")
	writeF(t, stalePath, "{}\n")
	old := time.Now().Add(-500 * 24 * time.Hour)
	_ = os.Chtimes(stalePath, old, old)

	// Note: .json is in the extension allowlist, but .lock isn't —
	// the walker filters by extension, so the .lock suffix means
	// extension is .lock (not .json), so it's filtered before the
	// special-case check fires. Either way: zero hits.
	hits := NewARCH004().Scan(&archaeologist.Repo{ID: 1, Name: "lock-repo", LocalPath: dir})
	if len(hits) != 0 {
		t.Fatalf("expected 0 hits (.lock filtered), got %d: %+v", len(hits), hits)
	}
}
