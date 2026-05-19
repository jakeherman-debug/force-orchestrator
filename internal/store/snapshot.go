// snapshot.go — D12 P3 pre-sleep holocron snapshots.
//
// SnapshotHolocron writes a consistent point-in-time copy of the
// holocron.db file to ~/.force/backups/snapshot-<label>-<RFC3339>.db
// so a post-wake reconcile that finds the DB in a half-open state can
// roll back to a known-good shape.
//
// Why VACUUM INTO: SQLite's `VACUUM INTO <path>` produces an atomic,
// self-consistent copy of the database without needing a busy-wait
// loop over a separate transaction. The source DB stays online and
// the snapshot file lands in a single statement. It's the same
// primitive `sqlite3 .backup` uses under the hood for file-backed
// databases.
//
// Pruning: snapshots older than snapshotRetention (7d) are removed on
// each call to avoid disk bloat. Sleep cycles fire often (every laptop
// lid close, ~5-20× per workday) so the retention window must be
// short relative to the snapshot rate.
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"force-orchestrator/internal/forcepath"
)

// snapshotRetention is the per-snapshot TTL — any file in
// ~/.force/backups/ older than this is pruned on the next call to
// SnapshotHolocron. 7 days is the operator-visible window: a Sunday-
// night sleep produces a snapshot the operator can roll back to the
// following Sunday but no further.
const snapshotRetention = 7 * 24 * time.Hour

// snapshotDirName is the per-fleet subdirectory under forcepath.Dir()
// that holds the snapshot files. Lazily created at mode 0700 (same
// posture as the rest of ~/.force/).
const snapshotDirName = "backups"

// snapshotFilePrefix is the leading component every snapshot file
// uses. The label (e.g. "pre-sleep") and RFC3339 timestamp follow.
const snapshotFilePrefix = "snapshot-"

// SnapshotHolocron writes a consistent copy of the live holocron DB
// to ~/.force/backups/snapshot-<label>-<RFC3339-timestamp>.db and
// returns the path on success. Caller must pass a non-nil *sql.DB
// already opened against the live holocron; a label is required so
// operators eyeballing the backups dir can identify what produced
// each file ("pre-sleep", "pre-migrate", etc.).
//
// Prunes any files in the snapshots directory whose mtime is older
// than snapshotRetention BEFORE writing the new snapshot so a long-
// running daemon doesn't accumulate disk-resident copies forever.
// The prune step is best-effort: if a single file's stat or remove
// fails (FS race, permissions), we log via the returned error chain
// path but the snapshot itself still writes.
//
// Idempotence: calling SnapshotHolocron N times in a row produces N
// snapshot files (each timestamped). The contract is "the latest
// snapshot reflects the latest call"; older snapshots remain on disk
// until they fall outside the retention window.
func SnapshotHolocron(db *sql.DB, label string) (string, error) {
	if db == nil {
		return "", fmt.Errorf("SnapshotHolocron: db is nil")
	}
	label = sanitizeSnapshotLabel(label)
	if label == "" {
		return "", fmt.Errorf("SnapshotHolocron: label is required (after sanitization)")
	}

	dir := filepath.Join(forcepath.Dir(), snapshotDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("SnapshotHolocron: mkdir %s: %w", dir, err)
	}

	// Prune old snapshots first. Errors here are non-fatal — we still
	// want to write the new snapshot even if a stale file can't be
	// removed. The pruning function logs via its return; we ignore
	// per spec ("best-effort").
	_, _ = pruneOldSnapshots(dir, time.Now(), snapshotRetention)

	ts := time.Now().UTC().Format(time.RFC3339)
	// RFC3339 contains ':' which is forbidden on some filesystems
	// (vfat-formatted external drives, Windows). Replace with '-' so
	// the path is portable.
	tsSafe := strings.ReplaceAll(ts, ":", "-")
	path := filepath.Join(dir, fmt.Sprintf("%s%s-%s.db", snapshotFilePrefix, label, tsSafe))

	// VACUUM INTO is a single atomic statement — no BEGIN/COMMIT
	// dance. SQLite handles the consistency boundary internally.
	// We pass the path as a SQL literal because VACUUM INTO does
	// not accept bound parameters; we sanitize the label above and
	// the timestamp is derived from time.Now() so there's no
	// untrusted input in the path string.
	escaped := strings.ReplaceAll(path, "'", "''")
	if _, err := db.Exec(fmt.Sprintf(`VACUUM INTO '%s'`, escaped)); err != nil {
		return "", fmt.Errorf("SnapshotHolocron: VACUUM INTO %s: %w", path, err)
	}
	return path, nil
}

// sanitizeSnapshotLabel strips path separators and other shell-hostile
// characters from a snapshot label so a hostile caller can't escape
// the snapshots directory or inject SQL when the label is interpolated
// into the VACUUM INTO statement. Production labels ("pre-sleep") are
// already safe and pass through unchanged.
func sanitizeSnapshotLabel(label string) string {
	label = strings.TrimSpace(label)
	if label == "" {
		return ""
	}
	out := make([]rune, 0, len(label))
	for _, r := range label {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '_':
			out = append(out, r)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}

// pruneOldSnapshots removes files in dir whose mtime is older than
// now.Add(-retention). Returns the number of files removed. Errors
// for individual files are tolerated (the function does its best and
// returns the count of successes); a directory-read failure is
// returned so the caller can log it.
//
// Exported only via test hooks below; the production call site is
// SnapshotHolocron above.
func pruneOldSnapshots(dir string, now time.Time, retention time.Duration) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("pruneOldSnapshots: read dir %s: %w", dir, err)
	}
	cutoff := now.Add(-retention)
	removed := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, snapshotFilePrefix) {
			continue // not one of ours
		}
		info, ierr := e.Info()
		if ierr != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			path := filepath.Join(dir, name)
			if rerr := os.Remove(path); rerr == nil {
				removed++
			}
		}
	}
	return removed, nil
}
