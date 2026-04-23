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
	t.Run("AUDIT_099_attributes_no_atomic_rename", func(t *testing.T) {
		t.Skip("AUDIT-099: remove when attributes rewrite uses atomic rename + signal handler (Fix #9)")
		// Without skip, fails with: AUDIT-099: internal/git/askbranch.go rewrites
		// .git/info/attributes in-place with os.WriteFile and uses a `defer` to
		// restore it. A crash or SIGKILL between write and defer leaves the repo
		// with globally-scoped `*.md merge=union` rules corrupting ALL future
		// merges. Also: no SIGINT/SIGTERM handler to restore attributes.
		src := mustReadFile(t, askBranchPath)

		// Anchors — the write+restore pattern on .git/info/attributes.
		if !strings.Contains(src, ".git") || !strings.Contains(src, "attributes") {
			t.Fatalf("AUDIT-099 anchor lost: no .git/info/attributes "+
				"references in %s", askBranchPath)
		}
		if !strings.Contains(src, "os.WriteFile(attrPath") {
			t.Fatalf("AUDIT-099 anchor lost: attrPath WriteFile pattern "+
				"not in %s", askBranchPath)
		}

		// (a) No atomic rename (`.tmp` + os.Rename) pattern. A real fix
		//     would write to attrPath+".tmp" then os.Rename into place.
		hasTmpWrite := regexp.MustCompile(`attrPath\s*\+\s*\"\.tmp\"`).MatchString(src) ||
			strings.Contains(src, `".tmp"`) && strings.Contains(src, "os.Rename(")
		hasRename := strings.Contains(src, "os.Rename(")
		if hasTmpWrite && hasRename {
			t.Errorf("AUDIT-099 appears fixed in %s — atomic rename "+
				"pattern detected. Update test to assert rename order.",
				askBranchPath)
		} else {
			t.Errorf("AUDIT-099: %s rewrites .git/info/attributes "+
				"in-place with os.WriteFile and uses a `defer` to "+
				"restore it. A crash or SIGKILL between write and "+
				"defer leaves the repo with globally-scoped "+
				"`*.md merge=union` rules corrupting ALL future "+
				"merges. Missing .tmp + os.Rename atomic swap.",
				askBranchPath)
		}

		// (b) No signal handler for SIGINT/SIGTERM that restores the
		//     original attributes content before exit.
		hasSignalHandler := strings.Contains(src, "signal.Notify(") ||
			strings.Contains(src, "syscall.SIGINT") ||
			strings.Contains(src, "syscall.SIGTERM") ||
			strings.Contains(src, "os.Interrupt")
		if hasSignalHandler {
			t.Errorf("AUDIT-099 partial-fix: signal handling now present "+
				"in %s — verify it actually restores attributes on "+
				"SIGINT/SIGTERM.", askBranchPath)
		} else {
			t.Errorf("AUDIT-099: %s has no SIGINT/SIGTERM handler to "+
				"restore .git/info/attributes if the daemon is killed "+
				"mid-union-merge. defer is not enough — signals "+
				"bypass deferred functions on SIGKILL and race with "+
				"the handler on SIGTERM.", askBranchPath)
		}
	})

	// ── AUDIT-100 ────────────────────────────────────────────────────────
	t.Run("AUDIT_100_worktree_perms_too_loose", func(t *testing.T) {
		t.Skip("AUDIT-100: remove when worktree dirs 0700 / task logs 0600 (Fix #9)")
		// Without skip, fails with: AUDIT-100: internal/git/git.go creates
		// .force-worktrees with mode 0755 (expected 0700); and
		// internal/agents/astromech.go uses os.Create(taskLogPath) which defaults
		// to 0644 (expected os.OpenFile(..., 0600)). Task logs contain Claude
		// stdout including injected inbox mail and prior agent transcripts.
		gitSrc := mustReadFile(t, gitPath)
		astroSrc := mustReadFile(t, astromechPath)

		// (a) .force-worktrees base directory MkdirAll uses 0755.
		// Expected fix: 0700. Current code: 0755.
		if !strings.Contains(gitSrc, "os.MkdirAll(worktreeBase, 0755)") {
			t.Errorf("AUDIT-100 anchor drift in %s: expected "+
				"`os.MkdirAll(worktreeBase, 0755)`; check whether the "+
				"fix landed or the code shape moved.", gitPath)
		}
		if strings.Contains(gitSrc, "os.MkdirAll(worktreeBase, 0700)") {
			t.Errorf("AUDIT-100 appears fixed in %s — worktree base is "+
				"now 0700. Update this test to assert the owner-only "+
				"permission directly.", gitPath)
		} else {
			t.Errorf("AUDIT-100: %s creates the .force-worktrees base "+
				"directory with mode 0755 (world-readable on multi-user "+
				"hosts). Expected 0700. Claude output and injected "+
				"inbox-mail payloads live under this tree.", gitPath)
		}

		// (b) Per-task log file — os.Create yields 0644 by default.
		// Expected fix: os.OpenFile with 0600.
		if !strings.Contains(astroSrc, `os.Create(taskLogPath)`) {
			t.Errorf("AUDIT-100 anchor drift in %s: expected "+
				"`os.Create(taskLogPath)`; verify shape.", astromechPath)
		}
		if strings.Contains(astroSrc, "0600") &&
			strings.Contains(astroSrc, "os.OpenFile(taskLogPath") {
			t.Errorf("AUDIT-100 appears fixed in %s — log file now "+
				"opened with mode 0600. Update this test.", astromechPath)
		} else {
			t.Errorf("AUDIT-100: %s uses os.Create(taskLogPath) which "+
				"defaults to 0644 (world-readable). Task logs contain "+
				"Claude stdout including injected inbox mail and prior "+
				"agent transcripts. Expected os.OpenFile(..., 0600).",
				astromechPath)
		}

		// (c) Counter-assert: mode 0700/0600 must NOT be the current
		// shape — if it is, the test framing is stale.
		if strings.Contains(gitSrc, "os.MkdirAll(worktreeBase, 0700)") &&
			strings.Contains(astroSrc, "0600") {
			t.Errorf("AUDIT-100: both permissions have been tightened — "+
				"this sub-test should be rewritten to pin the new "+
				"invariant rather than flag the old one.")
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
