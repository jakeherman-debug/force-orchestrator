package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"force-orchestrator/internal/forcepath"
)

// withTempForceDir points forcepath.Dir() at a temp directory for the
// duration of one test. Returns the path of the dir. The caller does
// NOT have to clean up — t.Cleanup handles the env-var restore.
func withTempForceDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	prev := os.Getenv("FORCE_DIR")
	os.Setenv("FORCE_DIR", dir)
	forcepath.ResetDirCacheForTests()
	t.Cleanup(func() {
		if prev == "" {
			os.Unsetenv("FORCE_DIR")
		} else {
			os.Setenv("FORCE_DIR", prev)
		}
		forcepath.ResetDirCacheForTests()
	})
	return dir
}

// TestSnapshotHolocron_CreatesFile asserts the happy path: a snapshot
// file lands under ~/.force/backups/ and is non-empty.
func TestSnapshotHolocron_CreatesFile(t *testing.T) {
	forceDir := withTempForceDir(t)

	// Use a file-backed DB (not :memory:) so VACUUM INTO has real bytes
	// to copy. We point at a per-test file under the temp dir.
	dbPath := filepath.Join(forceDir, "holocron.db")
	db := InitHolocronDSN(dbPath + "?_busy_timeout=5000&_journal_mode=WAL")
	defer db.Close()

	// Seed one row so the snapshot has content.
	if _, err := db.Exec(`INSERT INTO BountyBoard (type, status) VALUES ('snapshot-test', 'Pending')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	path, err := SnapshotHolocron(db, "pre-sleep")
	if err != nil {
		t.Fatalf("SnapshotHolocron: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat snapshot %s: %v", path, err)
	}
	if info.Size() == 0 {
		t.Errorf("snapshot file is empty: %s", path)
	}
	// Path should be in ~/.force/backups/ and start with snapshot-pre-sleep-.
	expectedDir := filepath.Join(forceDir, "backups")
	if dir := filepath.Dir(path); dir != expectedDir {
		t.Errorf("snapshot dir = %q, want %q", dir, expectedDir)
	}
	if base := filepath.Base(path); !strings.HasPrefix(base, "snapshot-pre-sleep-") {
		t.Errorf("snapshot basename = %q, want prefix snapshot-pre-sleep-", base)
	}
	if !strings.HasSuffix(path, ".db") {
		t.Errorf("snapshot path = %q, want .db extension", path)
	}
}

// TestSnapshotHolocron_PrunesOldSnapshots seeds an 8-day-old fake
// snapshot file, runs SnapshotHolocron, asserts the old one is gone
// and the new one is present.
func TestSnapshotHolocron_PrunesOldSnapshots(t *testing.T) {
	forceDir := withTempForceDir(t)

	dbPath := filepath.Join(forceDir, "holocron.db")
	db := InitHolocronDSN(dbPath + "?_busy_timeout=5000&_journal_mode=WAL")
	defer db.Close()

	// Pre-create the backups dir and drop in an 8-day-old fake snapshot.
	backupsDir := filepath.Join(forceDir, "backups")
	if err := os.MkdirAll(backupsDir, 0o700); err != nil {
		t.Fatalf("mkdir backups: %v", err)
	}
	oldPath := filepath.Join(backupsDir, "snapshot-pre-sleep-2024-01-01T00-00-00Z.db")
	if err := os.WriteFile(oldPath, []byte("stale snapshot bytes"), 0o600); err != nil {
		t.Fatalf("write old snapshot: %v", err)
	}
	stale := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(oldPath, stale, stale); err != nil {
		t.Fatalf("chtimes old snapshot: %v", err)
	}

	// Also drop a recent snapshot file (3 days old) — must NOT be pruned.
	recentPath := filepath.Join(backupsDir, "snapshot-pre-sleep-2026-05-15T00-00-00Z.db")
	if err := os.WriteFile(recentPath, []byte("recent snapshot bytes"), 0o600); err != nil {
		t.Fatalf("write recent snapshot: %v", err)
	}
	recent := time.Now().Add(-3 * 24 * time.Hour)
	if err := os.Chtimes(recentPath, recent, recent); err != nil {
		t.Fatalf("chtimes recent snapshot: %v", err)
	}

	newPath, err := SnapshotHolocron(db, "pre-sleep")
	if err != nil {
		t.Fatalf("SnapshotHolocron: %v", err)
	}

	// Old file should be gone.
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Errorf("stale snapshot still present: stat err=%v", err)
	}
	// Recent file should still be there.
	if _, err := os.Stat(recentPath); err != nil {
		t.Errorf("recent snapshot was incorrectly pruned: %v", err)
	}
	// New file should exist.
	if _, err := os.Stat(newPath); err != nil {
		t.Errorf("new snapshot missing: %v", err)
	}
}

// TestSnapshotHolocron_NilDB asserts the nil-DB guard returns an error
// instead of panicking. The daemon test path that exercises the
// reconcilePostWakeLoop GoingToSleep branch hits this with an in-memory
// DB whose VACUUM INTO would also fail, but the nil case is the
// simplest path to exercise.
func TestSnapshotHolocron_NilDB(t *testing.T) {
	_, err := SnapshotHolocron(nil, "pre-sleep")
	if err == nil {
		t.Fatalf("SnapshotHolocron(nil): want error, got nil")
	}
	if !strings.Contains(err.Error(), "db is nil") {
		t.Errorf("SnapshotHolocron(nil): err = %v, want 'db is nil'", err)
	}
}

// TestSnapshotHolocron_EmptyLabel asserts a blank label is rejected so
// no `snapshot--<ts>.db` files land in backups/.
func TestSnapshotHolocron_EmptyLabel(t *testing.T) {
	withTempForceDir(t)
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	_, err := SnapshotHolocron(db, "   ")
	if err == nil {
		t.Fatalf("SnapshotHolocron(blank label): want error, got nil")
	}
	if !strings.Contains(err.Error(), "label is required") {
		t.Errorf("SnapshotHolocron(blank): err = %v, want 'label is required'", err)
	}
}

// TestSnapshotHolocron_SanitizesLabel asserts a path-traversal-y label
// is rendered safe. The legitimate "pre-sleep" label passes through
// unchanged.
func TestSnapshotHolocron_SanitizesLabel(t *testing.T) {
	forceDir := withTempForceDir(t)

	dbPath := filepath.Join(forceDir, "holocron.db")
	db := InitHolocronDSN(dbPath + "?_busy_timeout=5000&_journal_mode=WAL")
	defer db.Close()

	path, err := SnapshotHolocron(db, "../../etc/passwd")
	if err != nil {
		t.Fatalf("SnapshotHolocron: %v", err)
	}
	// Path must remain in backups/ — sanitised label has no path separators.
	expectedDir := filepath.Join(forceDir, "backups")
	if dir := filepath.Dir(path); dir != expectedDir {
		t.Errorf("snapshot escaped backups/: dir = %q, want %q", dir, expectedDir)
	}
	if strings.Contains(filepath.Base(path), "/") || strings.Contains(filepath.Base(path), "..") {
		t.Errorf("snapshot basename contains path traversal: %q", filepath.Base(path))
	}
}

// TestPruneOldSnapshots_IgnoresUnrelatedFiles asserts that non-snapshot
// files in backups/ are left alone.
func TestPruneOldSnapshots_IgnoresUnrelatedFiles(t *testing.T) {
	dir := t.TempDir()
	// Drop an unrelated old file.
	unrelated := filepath.Join(dir, "README.md")
	if err := os.WriteFile(unrelated, []byte("hi"), 0o600); err != nil {
		t.Fatalf("write unrelated: %v", err)
	}
	stale := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(unrelated, stale, stale); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	// Drop a stale snapshot.
	staleSnap := filepath.Join(dir, "snapshot-x-old.db")
	if err := os.WriteFile(staleSnap, []byte("x"), 0o600); err != nil {
		t.Fatalf("write stale snap: %v", err)
	}
	if err := os.Chtimes(staleSnap, stale, stale); err != nil {
		t.Fatalf("chtimes snap: %v", err)
	}

	removed, err := pruneOldSnapshots(dir, time.Now(), snapshotRetention)
	if err != nil {
		t.Fatalf("pruneOldSnapshots: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1 (only the snapshot)", removed)
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Errorf("unrelated file was removed: %v", err)
	}
}
