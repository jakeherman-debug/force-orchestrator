package store

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestAUDIT_MiscSecurity verifies a batch of miscellaneous security /
// injection findings from AUDIT.md. Each sub-test is a STATIC source-grep
// assertion: it reads the relevant source file and checks for the presence
// or absence of a guard. All sub-tests are EXPECTED TO FAIL under the
// current codebase — they describe the invariant the fix must restore.
//
// When a fix lands, the corresponding sub-test will flip to green. Do not
// weaken these assertions; if a finding is intentionally WONTFIX, remove
// the sub-test with a note rather than relaxing the check.
//
// Findings covered:
//   AUDIT-017 — FORCE_OTEL_LOGS_URL env var is unvalidated; sendOTLPLog
//               spawns an unbounded goroutine per telemetry event.
//   AUDIT-019 — Worktree path discovery / cleanup follows symlinks;
//               no Lstat/ModeSymlink gate, no filepath.Rel ".." check,
//               no EvalSymlinks containment check.
//   AUDIT-057 — gh runner captures stdout into an unbounded bytes.Buffer;
//               --paginate is used on PR comment listings with no size cap.
//   AUDIT-099 — .git/info/attributes is rewritten in-place with no atomic
//               rename and no SIGINT/SIGTERM handler for the restore.
//   AUDIT-100 — .force-worktrees directory created 0755; per-task log file
//               created with default 0644 — world-readable on multi-user
//               hosts.
//   AUDIT-123 — DUPLICATE-OF-019: resetAndCleanWorktree operates on paths
//               that were never re-verified to live under the forceWorktree
//               base. Same shape of check; kept distinct because the fix
//               boundary differs (operation dispatch vs path discovery).
func TestAUDIT_MiscSecurity(t *testing.T) {
	// Fix #10 removes the outer skip: AUDIT-017 and AUDIT-057 sub-tests
	// assert their invariants directly. The remaining sub-tests
	// (AUDIT-019/099/100/123) belong to Fix #9 and stay skipped
	// individually until that fix lands.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("cannot resolve cwd: %v", err)
	}
	// This file lives at internal/store/ — repo root is two levels up.
	repoRoot := filepath.Clean(filepath.Join(cwd, "..", ".."))

	telemetryPath := filepath.Join(repoRoot, "internal", "telemetry", "telemetry.go")
	gitPath := filepath.Join(repoRoot, "internal", "git", "git.go")
	ghPath := filepath.Join(repoRoot, "internal", "gh", "gh.go")
	askBranchPath := filepath.Join(repoRoot, "internal", "git", "askbranch.go")
	astromechPath := filepath.Join(repoRoot, "internal", "agents", "astromech.go")
	worktreeResetPath := filepath.Join(repoRoot, "internal", "agents", "pilot_worktree_reset.go")

	// ── AUDIT-017 ────────────────────────────────────────────────────────
	t.Run("AUDIT_017_otel_url_unvalidated", func(t *testing.T) {
		// Closed by: Fix #10. FORCE_OTEL_LOGS_URL is now routed through
		// store.ValidateOutboundURL (scheme + host + DNS check) at init.
		src := mustReadFile(t, telemetryPath)

		// (a) FORCE_OTEL_LOGS_URL must still be referenced.
		if !strings.Contains(src, "FORCE_OTEL_LOGS_URL") {
			t.Fatalf("telemetry.go no longer references FORCE_OTEL_LOGS_URL — "+
				"audit anchor lost; update the test. Path: %s", telemetryPath)
		}

		// (b) The env value MUST pass through a validator. Accept either
		//     an in-file url.Parse + scheme check OR a delegated call to
		//     store.ValidateOutboundURL — Fix #10 chose delegation.
		hasURLParse := strings.Contains(src, "url.Parse") ||
			strings.Contains(src, "url.ParseRequestURI")
		hasSchemeCheck := regexp.MustCompile(`\.Scheme\s*(==|!=)`).MatchString(src)
		hasDelegated := strings.Contains(src, "ValidateOutboundURL")
		if !(hasDelegated || (hasURLParse && hasSchemeCheck)) {
			t.Errorf("AUDIT-017 regression: telemetry.go no longer "+
				"validates FORCE_OTEL_LOGS_URL. Expected either a call "+
				"to store.ValidateOutboundURL or inline url.Parse + "+
				"scheme check. Path: %s (urlParse=%v schemeCheck=%v "+
				"delegated=%v)", telemetryPath, hasURLParse,
				hasSchemeCheck, hasDelegated)
		}
	})

	// ── AUDIT-019 ────────────────────────────────────────────────────────
	t.Run("AUDIT_019_worktree_symlink_follow", func(t *testing.T) {
		// Closed by Fix #9: ListAgentWorktreePaths now Lstats each entry and
		// skips symlinks. This test is the permanent regression guard —
		// removing either the Lstat call or the ModeSymlink reference will
		// flip it back to red.
		src := mustReadFile(t, gitPath)

		// ListAgentWorktreePaths must exist — test anchor.
		if !strings.Contains(src, "func ListAgentWorktreePaths(") {
			t.Fatalf("audit anchor lost: ListAgentWorktreePaths not in %s", gitPath)
		}
		if !strings.Contains(src, "func ResolveWorktreeDir(") {
			t.Fatalf("audit anchor lost: ResolveWorktreeDir not in %s", gitPath)
		}

		// (a) Post-Fix-#9 invariant: Lstat + ModeSymlink gate MUST be
		//     present in git.go so ListAgentWorktreePaths refuses symlinked
		//     entries during discovery.
		hasLstat := strings.Contains(src, "os.Lstat(") ||
			strings.Contains(src, "Lstat(")
		hasModeSymlink := strings.Contains(src, "ModeSymlink") ||
			strings.Contains(src, "fs.ModeSymlink")
		if !(hasLstat && hasModeSymlink) {
			t.Errorf("AUDIT-019 regression: %s must contain Lstat+ModeSymlink "+
				"to refuse symlinked worktree entries (Fix #9 invariant). "+
				"Found Lstat=%v ModeSymlink=%v.", gitPath, hasLstat, hasModeSymlink)
		}
	})

	// ── AUDIT-123 (DUPLICATE-OF-019) ─────────────────────────────────────
	t.Run("AUDIT_123_worktree_reset_path_unverified_DUPLICATE_OF_019", func(t *testing.T) {
		// Closed by Fix #9: resetAndCleanWorktree now calls
		// filepath.EvalSymlinks and asserts the resolved path is still
		// under the .force-worktrees base before running destructive
		// operations. This test is the permanent regression guard.
		src := mustReadFile(t, worktreeResetPath)

		// Anchors.
		if !strings.Contains(src, "func resetAndCleanWorktree(") {
			t.Fatalf("audit anchor lost: resetAndCleanWorktree not in %s",
				worktreeResetPath)
		}
		if !strings.Contains(src, "func discoverWorktrees(") {
			t.Fatalf("audit anchor lost: discoverWorktrees not in %s",
				worktreeResetPath)
		}

		// Post-Fix-#9 invariant: EvalSymlinks + .force-worktrees containment
		// check are BOTH present.
		hasEvalSymlinks := strings.Contains(src, "filepath.EvalSymlinks(")
		hasBaseContainment := strings.Contains(src, "forceWorktreeBase") ||
			(strings.Contains(src, "filepath.Rel(") &&
				strings.Contains(src, ".force-worktrees"))
		if !(hasEvalSymlinks && hasBaseContainment) {
			t.Errorf("AUDIT-123 regression: %s must call "+
				"filepath.EvalSymlinks + containment-check the resolved "+
				"path against .force-worktrees before reset/clean. "+
				"Found EvalSymlinks=%v containment=%v.", worktreeResetPath,
				hasEvalSymlinks, hasBaseContainment)
		}
	})

	// ── AUDIT-057 ────────────────────────────────────────────────────────
	t.Run("AUDIT_057_unbounded_gh_stdout_buffer", func(t *testing.T) {
		// Closed by: Fix #10. gh.go now wraps the stdout buffer in a
		// capWriter bounded at maxGHStdoutBytes (64 MiB); overflow
		// returns ErrOverflow which ClassifyError maps to
		// ErrClassPermanent.
		src := mustReadFile(t, ghPath)

		if !strings.Contains(src, "bytes.Buffer") {
			t.Fatalf("AUDIT-057 anchor lost: no bytes.Buffer use in %s", ghPath)
		}

		// Accept any of: explicit cap wrapper, MaxBytesReader,
		// io.LimitReader, or the capWriter struct Fix #10 introduced.
		hasCapWrapper := strings.Contains(src, "capWriter") ||
			strings.Contains(src, "maxGHStdoutBytes") ||
			strings.Contains(src, "MaxBytesReader") ||
			strings.Contains(src, "io.MultiWriter(") ||
			strings.Contains(src, "io.LimitReader(")
		if !hasCapWrapper {
			t.Errorf("AUDIT-057 regression: %s has no stdout size cap "+
				"on the gh runner. Fix #10 introduced capWriter + "+
				"maxGHStdoutBytes; if either was renamed, update this "+
				"static check.", ghPath)
		}

		// Amplifier: confirm --paginate is still used (paginated
		// comment endpoints are the main risk vector).
		if !strings.Contains(src, `"--paginate"`) {
			t.Errorf("AUDIT-057 amplifier missing: --paginate not found "+
				"in %s. If pagination was dropped, the finding's blast "+
				"radius changed — re-audit.", ghPath)
		}
	})

	// ── AUDIT-099 ────────────────────────────────────────────────────────
	// Post-fix contract (Fix #8d): MergeWithUnionStrategy writes
	// .git/info/attributes atomically (write to tmp + os.Rename) and
	// installs a SIGINT/SIGTERM handler that restores the original
	// attributes on operator-initiated shutdown. Pre-fix, a crash or
	// signal between the WriteFile and the deferred restore would
	// leave the repo with globally-scoped `*.md merge=union` rules.
	t.Run("AUDIT_099_attributes_atomic_rename_and_signal_handler", func(t *testing.T) {
		src := mustReadFile(t, askBranchPath)

		// Anchors — the write+restore pattern on .git/info/attributes.
		if !strings.Contains(src, ".git") || !strings.Contains(src, "attributes") {
			t.Fatalf("AUDIT-099 anchor lost: no .git/info/attributes references in %s", askBranchPath)
		}

		// (a) Atomic rename: writes go to attrPath + ".tmp" then os.Rename.
		hasTmpWrite := regexp.MustCompile(`attrPath\s*\+\s*"\.tmp"`).MatchString(src)
		hasRename := strings.Contains(src, "os.Rename(")
		if !hasTmpWrite || !hasRename {
			t.Errorf("AUDIT-099 REGRESSION: %s no longer writes attributes "+
				"atomically (tmp + os.Rename). hasTmpWrite=%v hasRename=%v",
				askBranchPath, hasTmpWrite, hasRename)
		}

		// (b) SIGINT/SIGTERM handler restores attributes on signal.
		hasSignalHandler := strings.Contains(src, "signal.Notify(") &&
			(strings.Contains(src, "syscall.SIGINT") || strings.Contains(src, "syscall.SIGTERM"))
		if !hasSignalHandler {
			t.Errorf("AUDIT-099 REGRESSION: %s no longer installs a "+
				"signal.Notify handler for SIGINT/SIGTERM that restores "+
				"attributes on operator-initiated shutdown.", askBranchPath)
		}
	})

	// ── AUDIT-100 ────────────────────────────────────────────────────────
	// Post-fix contract (Fix #8d): .force-worktrees base is 0700, and
	// the per-task log file is chmod'd to 0600 after creation. Both
	// contain Claude output (including injected inbox mail and prior
	// transcripts) which must not be world-readable on multi-user hosts.
	t.Run("AUDIT_100_worktree_perms_tightened", func(t *testing.T) {
		gitSrc := mustReadFile(t, gitPath)
		astroSrc := mustReadFile(t, astromechPath)

		// (a) worktree base uses 0700.
		if !strings.Contains(gitSrc, "os.MkdirAll(worktreeBase, 0700)") {
			t.Errorf("AUDIT-100 REGRESSION: %s no longer creates "+
				".force-worktrees base with mode 0700.", gitPath)
		}
		if strings.Contains(gitSrc, "os.MkdirAll(worktreeBase, 0755)") {
			t.Errorf("AUDIT-100 REGRESSION: %s reintroduced "+
				"os.MkdirAll(worktreeBase, 0755).", gitPath)
		}

		// (b) Task log file is chmod'd to 0600 after os.Create.
		// We allow either a literal os.OpenFile(..., 0600) form OR
		// os.Chmod(taskLogPath, 0600) directly after os.Create.
		hasChmod := regexp.MustCompile(`os\.Chmod\(taskLogPath,\s*0600\)`).MatchString(astroSrc)
		hasOpenFile := regexp.MustCompile(`os\.OpenFile\(taskLogPath[^)]+0600`).MatchString(astroSrc)
		if !hasChmod && !hasOpenFile {
			t.Errorf("AUDIT-100 REGRESSION: %s no longer tightens the "+
				"task log file mode to 0600 (neither os.Chmod nor "+
				"os.OpenFile with 0600 found).", astromechPath)
		}
	})
}

// mustReadFile reads an audit target file or fails the test with a clear
// pointer to the repo-relative path.
func mustReadFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read %s: %v", path, err)
	}
	return string(b)
}
