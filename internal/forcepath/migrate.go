package forcepath

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// MigrateLegacyHolocronDB moves a CWD-resident holocron.db to the
// canonical ~/.force/holocron.db path when the canonical path is empty
// and the legacy path has data.
//
// Pre-Sweep-F, holocron.db lived in CWD. Operators who upgrade to a
// canonical-path build still have legacy DBs in ~/code/force-orchestrator/.
// This helper is called BEFORE store.InitHolocronDSN on every entrypoint
// (daemon, CLI commands) so the old data follows the operator to the
// new location automatically.
//
// Safety contract:
//   - Canonical DSN is in-memory (FORCE_HOLOCRON_DSN=":memory:") → no-op.
//   - Canonical file exists AND has nonzero size AND legacy file has
//     nonzero size → ERROR. We refuse to clobber either; the operator
//     must inspect both and decide which to keep. The error message
//     names both paths.
//   - Canonical file exists empty (e.g. created by a partial run) →
//     remove it and proceed with the move.
//   - Legacy file missing → no-op.
//   - Canonical file missing AND legacy has data → atomic move
//     (os.Rename across same FS; copy+delete fallback for cross-FS).
//     The -shm / -wal SQLite sidecars are moved alongside.
//
// candidates is an optional list of directories to inspect for the
// legacy holocron.db. The default (when called with no args) inspects
// only os.Getwd(). Tests pass an explicit list; the daemon entrypoint
// also passes only os.Getwd() to stay conservative.
//
// Returns nil for no-op or successful migration. Logs (via the
// supplied io.Writer) when work was done so the operator sees the
// move in fleet output. Returns a wrapped error for the ambiguous
// case so callers can surface it without losing the path detail.
func MigrateLegacyHolocronDB(ctx context.Context, log io.Writer, candidates ...string) error {
	if log == nil {
		log = io.Discard
	}

	canonical := HolocronFile()
	if canonical == "" {
		// In-memory DSN — no filesystem to migrate to.
		return nil
	}

	if len(candidates) == 0 {
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("forcepath.MigrateLegacyHolocronDB: os.Getwd: %w", err)
		}
		candidates = []string{cwd}
	}

	// Find the first candidate dir that holds a non-trivial legacy DB.
	// Pre-Sweep-F there is only ever one legacy location per operator;
	// we don't merge multiple sources because that would be ambiguous.
	//
	// Compare by EvalSymlinks-resolved path so macOS's /var → /private/var
	// alias (and similar symlink shenanigans) doesn't make the same file
	// look like two distinct paths — which would otherwise produce a
	// false-positive AMBIGUOUS error.
	var legacy string
	canonicalDir := filepath.Dir(canonical)
	canonicalResolved := canonicalDir
	if r, err := filepath.EvalSymlinks(canonicalDir); err == nil {
		canonicalResolved = r
	}
	for _, dir := range candidates {
		if dir == "" {
			continue
		}
		dirResolved := dir
		if r, err := filepath.EvalSymlinks(dir); err == nil {
			dirResolved = r
		}
		// Skip if candidate resolves to the canonical dir (same file).
		if dirResolved == canonicalResolved {
			continue
		}
		candidate := filepath.Join(dir, "holocron.db")
		fi, err := os.Stat(candidate)
		if err != nil || fi.Size() == 0 {
			continue
		}
		legacy = candidate
		break
	}
	if legacy == "" {
		// No legacy file with data — nothing to migrate.
		return nil
	}

	// Inspect canonical.
	canonicalFi, statErr := os.Stat(canonical)
	switch {
	case statErr != nil && errors.Is(statErr, os.ErrNotExist):
		// Canonical doesn't exist — straightforward move.
		fmt.Fprintf(log, "forcepath: migrating legacy holocron.db %s → %s\n", legacy, canonical)
		return moveDB(legacy, canonical)
	case statErr != nil:
		return fmt.Errorf("forcepath.MigrateLegacyHolocronDB: stat canonical %s: %w", canonical, statErr)
	case canonicalFi.Size() == 0:
		// Canonical exists but is empty (stale shell from a fresh InitHolocron
		// race). Remove it and migrate.
		fmt.Fprintf(log, "forcepath: removing empty canonical %s; migrating legacy holocron.db %s → %s\n",
			canonical, legacy, canonical)
		if rmErr := removeAllSidecars(canonical); rmErr != nil {
			return fmt.Errorf("forcepath.MigrateLegacyHolocronDB: cleanup empty canonical: %w", rmErr)
		}
		return moveDB(legacy, canonical)
	default:
		// Both files have data. Refuse — operator must decide.
		return fmt.Errorf(
			"forcepath.MigrateLegacyHolocronDB: AMBIGUOUS — both legacy %s (%d bytes) "+
				"and canonical %s (%d bytes) contain data. Move/delete one before continuing. "+
				"Recommended: stop the daemon, inspect both with `sqlite3 <path> .tables`, "+
				"and keep whichever has the rows you need. The OTHER must be moved aside "+
				"(rename to .pre-migrate so a future audit can find it).",
			legacy, sizeOf(legacy), canonical, canonicalFi.Size())
	}
}

// moveDB performs the rename of the .db file plus its -shm / -wal
// SQLite sidecars. Uses os.Rename when src and dst share a filesystem;
// falls back to copy+delete for cross-FS moves (e.g. /tmp on macOS
// vs $HOME on a separate volume).
func moveDB(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return fmt.Errorf("mkdir canonical dir: %w", err)
	}
	for _, suffix := range []string{"", "-shm", "-wal"} {
		srcPath := src + suffix
		dstPath := dst + suffix
		if _, err := os.Stat(srcPath); err != nil {
			// Sidecar absent — skip silently (the main .db must succeed;
			// sidecars are SQLite-WAL artefacts that the next open
			// rebuilds).
			if suffix == "" {
				return fmt.Errorf("legacy %s missing during move: %w", srcPath, err)
			}
			continue
		}
		if err := renameOrCopy(srcPath, dstPath); err != nil {
			return fmt.Errorf("move %s → %s: %w", srcPath, dstPath, err)
		}
	}
	return nil
}

// renameOrCopy tries os.Rename first; on EXDEV (cross-FS) falls back
// to a stream copy + delete. The fallback preserves the file mode but
// not atime/mtime — that's acceptable for a one-time migration.
func renameOrCopy(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	// Fallback: stream copy.
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	srcInfo, _ := in.Stat()
	mode := os.FileMode(0o600)
	if srcInfo != nil {
		mode = srcInfo.Mode().Perm()
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dst)
		return err
	}
	// Source remove is the LAST step — if anything above fails we leave
	// the legacy file in place so the operator can retry.
	if err := os.Remove(src); err != nil {
		return fmt.Errorf("copy succeeded but legacy %s remains: %w", src, err)
	}
	return nil
}

// removeAllSidecars removes path, path-shm, and path-wal. Silently
// skips files that don't exist.
func removeAllSidecars(path string) error {
	for _, suffix := range []string{"", "-shm", "-wal"} {
		p := path + suffix
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// sizeOf is a best-effort byte-size lookup used only inside the error
// message; never propagates errors.
func sizeOf(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}
