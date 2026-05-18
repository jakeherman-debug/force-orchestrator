// Package forcepath is the canonical-path resolver for every runtime
// state file the daemon and CLI touch.
//
// Why this exists: pre-Sweep-F, holocron.db / fleet.log / holonet.jsonl
// / fleet.pid / fleet-task-<id>.log all resolved from CWD. That meant a
// daemon launched from ~/code/force-orchestrator/ and a CLI invoked
// from ~/code/ silently opened DIFFERENT holocron.db files — the
// operator hit "force repos shows nothing" because the CLI created a
// fresh empty DB in its CWD instead of seeing the daemon's data. Same
// class of friction for every other state file.
//
// This package picks a single canonical home — ~/.force/ — and routes
// every state-file path through one helper so the resolution chain is
// uniform and overridable for tests / operator setups.
//
// Resolution chain (for the DB; other helpers follow the same shape):
//
//  1. FORCE_HOLOCRON_DSN env var — a complete SQLite DSN. Useful for
//     tests (":memory:") and for operators who want sqlite query
//     params or a custom path. Returned VERBATIM — the caller is
//     responsible for the DSN form.
//
//  2. FORCE_DIR env var — operator-controlled state directory. Every
//     helper appends its specific filename (FORCE_DIR/holocron.db,
//     FORCE_DIR/fleet.log, etc.).
//
//  3. ~/.force/<file> — the default, aligned with the D12 trust file
//     and singleton PID file.
//
// Permissions: Dir() creates ~/.force/ at mode 0700 because the trust
// file and event streams may contain operator-private material (GitHub
// tokens that leaked into a stderr capture; ghp_ prefixes that the
// Claude wrapper redacts but a corrupted run might miss).
package forcepath

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// envHolocronDSN is the env var name for the complete SQLite DSN
// override. When set non-empty, Holocron() returns its value verbatim.
const envHolocronDSN = "FORCE_HOLOCRON_DSN"

// envForceDir is the env var name for the state-directory override.
// When set non-empty, every helper rooted at Dir() uses that path
// instead of ~/.force/.
const envForceDir = "FORCE_DIR"

// defaultDirName is the per-user state directory name under $HOME.
// Aligns with internal/daemon/singleton.DefaultPIDPath (~/.force/force.pid)
// and internal/daemon/trust.DefaultPath (~/.force/trusted-binary-hashes).
const defaultDirName = ".force"

var (
	dirMu     sync.Mutex
	dirCached string // memoised result of Dir() once mkdir succeeds
)

// Dir returns the canonical state directory and ensures it exists
// (mkdir -p at mode 0700). The first call performs the mkdir; later
// calls return the cached path. Safe to call concurrently.
//
// Resolution: FORCE_DIR env > ~/.force > /tmp/.force (HOME unset,
// CI / minimal-user environment fallback — mirrors trust.DefaultPath).
//
// Mode 0700 is intentional: ~/.force may contain the trust file
// (operator-curated SHA list), the per-task scratch log (which can
// capture redaction misses), and the legacy fleet.log (which can
// capture gh-auth stderr). Operator-private by default.
func Dir() string {
	dirMu.Lock()
	defer dirMu.Unlock()
	if dirCached != "" {
		return dirCached
	}

	var base string
	if v := os.Getenv(envForceDir); v != "" {
		base = v
	} else {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			base = filepath.Join(os.TempDir(), defaultDirName)
		} else {
			base = filepath.Join(home, defaultDirName)
		}
	}

	// Best-effort mkdir. If it fails (read-only FS, permission denied),
	// we still return the path — the caller's open will fail with a
	// useful error. Silently swallowing the mkdir error would mask the
	// root cause; returning the path lets the caller surface it.
	_ = os.MkdirAll(base, 0o700)
	dirCached = base
	return base
}

// ResetDirCacheForTests clears the memoised Dir() value. Tests that
// twiddle FORCE_DIR mid-run MUST call this between mutations or the
// stale path will leak into the next assertion. Production code never
// needs this.
func ResetDirCacheForTests() {
	dirMu.Lock()
	dirCached = ""
	dirMu.Unlock()
}

// Holocron returns the SQLite DSN for the canonical holocron.db.
//
// Resolution:
//  1. FORCE_HOLOCRON_DSN — verbatim (full DSN form; can include
//     ":memory:", custom paths, or sqlite query params).
//  2. FORCE_DIR / Dir() + "/holocron.db" — appended to the resolved
//     state directory. The standard pragmas (busy_timeout, WAL) are
//     applied in store.InitHolocronDSN, not here, so this helper
//     stays env-overridable without callers having to re-construct
//     the query string.
//
// The returned DSN can be passed directly to store.InitHolocronDSN.
func Holocron() string {
	if v := os.Getenv(envHolocronDSN); v != "" {
		return v
	}
	return filepath.Join(Dir(), "holocron.db") + "?_busy_timeout=5000&_journal_mode=WAL"
}

// HolocronFile returns just the filesystem path of the canonical
// holocron.db (no SQLite query params). Use this when you need to
// snapshot / move / rename the file — Holocron()'s query params are
// only valid as a DSN to sql.Open.
//
// If FORCE_HOLOCRON_DSN is set to ":memory:" or any non-file form,
// the returned path is the empty string; callers that need a file
// path must check for "" and degrade.
func HolocronFile() string {
	if v := os.Getenv(envHolocronDSN); v != "" {
		// In-memory DSN — there's no real file path. Returning "" is
		// the contract; the snapshot/migrate code paths must skip.
		if v == ":memory:" || v == "file::memory:?cache=shared" {
			return ""
		}
		// A custom file-path DSN — strip query string portion so the
		// caller has a usable filesystem path.
		if idx := indexQuery(v); idx >= 0 {
			return v[:idx]
		}
		return v
	}
	return filepath.Join(Dir(), "holocron.db")
}

// PIDFile returns the singleton-lock PID file path
// (~/.force/force.pid). Single source of truth for "where does the
// daemon write its PID" — internal/daemon/singleton.DefaultPIDPath()
// resolves to the same path under the hood; this exported helper
// exists so non-daemon callers (CLI status commands, doctor checks)
// don't need to import singleton.
func PIDFile() string {
	return filepath.Join(Dir(), "force.pid")
}

// FleetLog returns the legacy daemon-wide log path
// (~/.force/fleet.log). One file, one writer, append-only (see
// internal/agents/logger.go).
func FleetLog() string {
	return filepath.Join(Dir(), "fleet.log")
}

// HolonetEventStream returns the structured event-stream path
// (~/.force/holonet.jsonl). One file, append-only newline-delimited
// JSON (see internal/telemetry/telemetry.go).
func HolonetEventStream() string {
	return filepath.Join(Dir(), "holonet.jsonl")
}

// AstromechLog returns the per-agent log path. For now we route
// per-agent output through the shared fleet.log writer (logger.go);
// this helper exists so future per-agent log files have a canonical
// home (~/.force/logs/astromech-<agent>.log) that doesn't drift back
// to CWD. Creates the logs subdirectory lazily.
func AstromechLog(agent string) string {
	dir := filepath.Join(Dir(), "logs")
	_ = os.MkdirAll(dir, 0o700)
	return filepath.Join(dir, "astromech-"+sanitizeAgent(agent)+".log")
}

// ScratchTaskFile returns the per-task scratch log path
// (~/.force/scratch/fleet-task-<id>.log). The astromech and commander
// agents write Claude's live stdout here while the task runs; `force
// tail <id>` reads from it. Removed on task completion (see
// astromech.go's defer os.Remove). Creates the scratch subdirectory
// lazily.
func ScratchTaskFile(taskID int) string {
	dir := filepath.Join(Dir(), "scratch")
	_ = os.MkdirAll(dir, 0o700)
	return filepath.Join(dir, fmt.Sprintf("fleet-task-%d.log", taskID))
}

// HolonetRotatedPattern returns the glob pattern matching rotated
// holonet event-stream archives. The holonet-rotate dog renames
// holonet.jsonl → holonet-<stamp>.jsonl when the live file exceeds
// 50 MB; cleanup commands need a uniform way to find them all.
func HolonetRotatedPattern() string {
	return filepath.Join(Dir(), "holonet-*.jsonl")
}

// indexQuery returns the index of the first '?' in s, or -1.
// Used to strip a SQLite-style query string from a DSN that contains
// a filesystem path.
func indexQuery(s string) int {
	for i, c := range s {
		if c == '?' {
			return i
		}
	}
	return -1
}

// sanitizeAgent strips path separators / NULs from an agent name so a
// caller passing a hostile string can't escape the logs directory.
// Production agent names (R2-D2, Council-Yoda, etc.) pass through
// unchanged.
func sanitizeAgent(name string) string {
	if name == "" {
		return "unknown"
	}
	out := make([]rune, 0, len(name))
	for _, r := range name {
		switch r {
		case '/', '\\', '\x00':
			out = append(out, '_')
		default:
			out = append(out, r)
		}
	}
	return string(out)
}
