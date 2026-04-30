package agents

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// BashGuardBinaryName is the canonical filename of the gatekeeper
// binary built by `make build-bash-guard`. Astromech sets up a per-
// worktree bash shim that delegates to this binary before exec-ing
// real bash; the resulting bash.log lives in the worktree.
const BashGuardBinaryName = "force-bash-guard"

// bashGuardShimDirName is the per-worktree directory we prepend to the
// astromech's PATH. The directory contains a single executable named
// `bash` whose only job is to call force-bash-guard for validation,
// then exec real /bin/bash on success.
const bashGuardShimDirName = ".force-bash-guard-shim"

// resolveBashGuardBinary returns the absolute path to the
// force-bash-guard binary, preferring (in order):
//
//  1. $FORCE_BASH_GUARD_BIN (operator override)
//  2. <repoRoot>/bin/force-bash-guard relative to the daemon's CWD
//  3. force-bash-guard on $PATH
//
// An empty return means we couldn't find the binary; the caller logs
// and continues without wiring rather than refusing to run the
// astromech. Pattern P15 enforces the lookup code is present; runtime
// fallibility is acceptable because the binary itself is the security
// boundary, not the wiring.
func resolveBashGuardBinary() string {
	if v := strings.TrimSpace(os.Getenv("FORCE_BASH_GUARD_BIN")); v != "" {
		if abs, err := filepath.Abs(v); err == nil {
			return abs
		}
		return v
	}
	if cwd, err := os.Getwd(); err == nil {
		candidate := filepath.Join(cwd, "bin", BashGuardBinaryName)
		if _, statErr := os.Stat(candidate); statErr == nil {
			return candidate
		}
	}
	if onPath, err := lookPath(BashGuardBinaryName); err == nil {
		return onPath
	}
	return ""
}

// setupBashGuardShim writes the per-worktree bash shim and returns the
// env entries the caller should pass into claude.RunCLIStreamingContext
// via extraEnv. Two entries are returned, in this order:
//
//  1. PATH=<shimDir>:<inherited PATH> — covers any subprocess that
//     resolves `bash` via PATH lookup.
//  2. SHELL=<shimDir>/bash — covers Claude CLI's Bash tool, which
//     resolves the shell via $SHELL as an absolute path rather than
//     PATH. Without this entry the shim is unreachable for the only
//     invocation that matters (empirical investigation 2026-04-29).
//
// On error, the function returns (nil, err) — the caller logs the
// error and proceeds without wiring. The shim is written each call
// (idempotent: same contents on repeat) so a manually deleted shim
// self-heals on the next astromech session.
//
// D2 T1-3 / Path B per docs/roadmap.md §D2: PATH-based wrapping. Claude
// CLI's Bash tool launches `bash -c <cmd>` via $SHELL; with our shim
// pointed-at by SHELL, that bash invocation becomes our gatekeeper.
// The shim parses the `-c <cmd>` form, calls force-bash-guard, and on
// approval exec's /bin/bash.
func setupBashGuardShim(worktreeDir string) ([]string, error) {
	if worktreeDir == "" {
		return nil, fmt.Errorf("setupBashGuardShim: empty worktreeDir")
	}
	bin := resolveBashGuardBinary()
	if bin == "" {
		return nil, fmt.Errorf("setupBashGuardShim: %s binary not found (build with `make build-bash-guard` or set FORCE_BASH_GUARD_BIN)", BashGuardBinaryName)
	}
	shimDir := filepath.Join(worktreeDir, bashGuardShimDirName)
	if err := os.MkdirAll(shimDir, 0o755); err != nil {
		return nil, fmt.Errorf("setupBashGuardShim: mkdir %s: %w", shimDir, err)
	}
	shimPath := filepath.Join(shimDir, "bash")
	contents := bashShimSource(bin)
	// Write atomically — write to a tmp file in the same dir, then
	// rename. Avoids a partial shim being executed during a parallel
	// astromech start-up.
	tmp := shimPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(contents), 0o755); err != nil {
		return nil, fmt.Errorf("setupBashGuardShim: write tmp shim: %w", err)
	}
	if err := os.Rename(tmp, shimPath); err != nil {
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("setupBashGuardShim: rename tmp shim: %w", err)
	}
	pathEntry := fmt.Sprintf("PATH=%s%s%s",
		shimDir, string(os.PathListSeparator), os.Getenv("PATH"))
	shellEntry := fmt.Sprintf("SHELL=%s", shimPath)
	return []string{pathEntry, shellEntry}, nil
}

// bashShimSource returns the contents of the per-worktree bash shim.
// The shim handles three invocation shapes:
//
//   - bash -c "<command>"          (Claude CLI's Bash tool)
//   - bash -lc "<command>"         (rare; treated like -c)
//   - bash <other args>            (fall through to real bash)
//
// For -c form, the shim feeds the command line to force-bash-guard
// via argv. On exit 0, exec /bin/bash with the original argv. On any
// non-zero exit, propagate the exit code so the Bash tool surfaces a
// rejection.
func bashShimSource(guardBin string) string {
	return "#!/bin/sh\n" +
		"# force-bash-guard shim (D2 T1-3). DO NOT EDIT — regenerated per astromech run.\n" +
		"# Bypassing this shim defeats the astromech Bash boundary; see CLAUDE.md.\n" +
		"GUARD=" + shellQuote(guardBin) + "\n" +
		"if [ \"$1\" = \"-c\" ] || [ \"$1\" = \"-lc\" ]; then\n" +
		"  CMD=\"$2\"\n" +
		"  if ! \"$GUARD\" \"$CMD\"; then\n" +
		"    exit 1\n" +
		"  fi\n" +
		"fi\n" +
		"exec /bin/bash \"$@\"\n"
}

// shellQuote wraps s in single quotes, escaping any single quotes within.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// lookPath wraps exec.LookPath without pulling os/exec into this file's
// import set when not in use. Indirected through a var so tests can
// stub.
var lookPath = func(name string) (string, error) {
	// This is a thin wrapper because importing os/exec here would
	// duplicate the import already used by astromech.go's caller
	// region. Keep dependency narrow: defer to the standard library.
	return execLookPath(name)
}

// execLookPath is split into its own var so the test file can stub
// the lookup deterministically without an os/exec dependency.
var execLookPath = func(name string) (string, error) {
	return findOnPath(name)
}

// findOnPath is the minimal exec.LookPath equivalent — sufficient for
// our needs because we only look for one well-known binary name.
func findOnPath(name string) (string, error) {
	if strings.Contains(name, string(filepath.Separator)) {
		return "", fmt.Errorf("findOnPath: refusing to resolve a path-bearing name: %q", name)
	}
	pathEnv := os.Getenv("PATH")
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, name)
		if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
			if isExecutable(st.Mode()) {
				return candidate, nil
			}
		}
	}
	return "", fmt.Errorf("findOnPath: %q not found on PATH", name)
}

func isExecutable(mode os.FileMode) bool {
	if runtime.GOOS == "windows" {
		return true // Windows uses extension matching; our binary path is *.exe
	}
	return mode&0o111 != 0
}
