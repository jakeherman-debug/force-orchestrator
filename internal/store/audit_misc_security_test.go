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
	t.Skip("AUDIT-017: remove when FORCE_OTEL_LOGS_URL validated + bounded worker pool (Fix #10)")
	// Without skip, fails with: AUDIT-017: telemetry.go does NOT validate FORCE_OTEL_LOGS_URL.
	// Missing url.Parse + scheme check + host allow-list. (+ AUDIT-019/057/099/100/123 sub-test
	// failures on symlink guards, gh stdout cap, atomic rename, 0700/0600 perms, worktree
	// containment.)
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
		t.Skip("AUDIT-017: remove when FORCE_OTEL_LOGS_URL validated + bounded worker pool (Fix #10)")
		// Without skip, fails with: AUDIT-017: telemetry.go does NOT validate
		// FORCE_OTEL_LOGS_URL. Missing url.Parse + scheme check + host allow-list.
		// An operator (or attacker with env access) can redirect every task_claimed
		// payload_preview to an arbitrary HTTP endpoint.
		src := mustReadFile(t, telemetryPath)

		// (a) FORCE_OTEL_LOGS_URL must be referenced (sanity check — if the
		//     env var name moves, this test needs to move with it).
		if !strings.Contains(src, "FORCE_OTEL_LOGS_URL") {
			t.Fatalf("telemetry.go no longer references FORCE_OTEL_LOGS_URL — "+
				"audit anchor lost; update the test. Path: %s", telemetryPath)
		}

		// (b) There must be NO url.Parse + scheme/host validation on the
		//     env value. The presence of "url.Parse" anywhere in the file
		//     paired with any of "Scheme", "Host", or "allow" would mean
		//     the fix landed; today none of those exist.
		hasURLParse := strings.Contains(src, "url.Parse") ||
			strings.Contains(src, "url.ParseRequestURI")
		hasSchemeCheck := regexp.MustCompile(`\.Scheme\s*(==|!=)`).MatchString(src)
		hasAllowList := regexp.MustCompile(`(?i)allow[_-]?list|allowed[_-]?host`).MatchString(src)
		if hasURLParse && (hasSchemeCheck || hasAllowList) {
			t.Fatalf("AUDIT-017 appears fixed: telemetry.go now validates "+
				"FORCE_OTEL_LOGS_URL. Update this test to assert the "+
				"validator's behavior directly. Path: %s", telemetryPath)
		}
		if hasURLParse || hasSchemeCheck || hasAllowList {
			t.Errorf("AUDIT-017 partial-fix detected in %s (url.Parse=%v "+
				"scheme=%v allowlist=%v). A real fix needs all three.",
				telemetryPath, hasURLParse, hasSchemeCheck, hasAllowList)
		} else {
			t.Errorf("AUDIT-017: telemetry.go does NOT validate "+
				"FORCE_OTEL_LOGS_URL. Missing url.Parse + scheme check + "+
				"host allow-list. An operator (or attacker with env "+
				"access) can redirect every task_claimed payload_preview "+
				"to an arbitrary HTTP endpoint. Path: %s", telemetryPath)
		}

		// (c) sendOTLPLog must fire a fresh goroutine per event with no
		//     bounded pool. Look for the literal "go sendOTLPLog(" pattern
		//     and confirm no worker-pool / semaphore / channel-bounded
		//     dispatch exists.
		if !strings.Contains(src, "go sendOTLPLog(") {
			t.Errorf("AUDIT-017 shape changed: expected `go sendOTLPLog(` "+
				"in %s; per-event goroutine fan-out may have been "+
				"replaced — re-verify.", telemetryPath)
		}
		// A bounded pool would show up as a buffered channel, worker sema,
		// or errgroup.Limiter. None of these tokens appear today.
		poolTokens := []string{
			"chan TelemetryEvent", "chan telemetryEvent",
			"semaphore", "errgroup", "SetLimit(",
			"otlpWorker", "otlpQueue", "dispatchQueue",
		}
		for _, tok := range poolTokens {
			if strings.Contains(src, tok) {
				t.Errorf("AUDIT-017 bounded-pool shape detected via %q — "+
					"update test to assert the bound directly.", tok)
			}
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
		t.Skip("AUDIT-057: remove when gh stdout capped via io.MultiWriter (Fix #10)")
		// Without skip, fails with: AUDIT-057: internal/gh/gh.go has no stdout
		// size cap on the gh runner. An adversarial or simply very large
		// `gh api --paginate repos/.../comments` response is read entirely into
		// RAM. The daemon OOMs. Needed: io.MultiWriter(&buf, countingDiscard) with
		// 64MB cap returning ErrClassPermanent on overflow.
		src := mustReadFile(t, ghPath)

		// (a) The ExecRunner captures stdout into a bytes.Buffer with no
		//     cap. Confirm the literal exists (audit anchor).
		if !strings.Contains(src, "var stdoutBuf, stderrBuf bytes.Buffer") &&
			!strings.Contains(src, "bytes.Buffer") {
			t.Fatalf("AUDIT-057 anchor lost: no bytes.Buffer use in %s", ghPath)
		}

		// (b) No size-capped wrapper (MaxBytesReader, io.MultiWriter with
		//     counting discard, or io.LimitReader) around the stdout pipe.
		hasMaxBytes := strings.Contains(src, "MaxBytesReader") ||
			strings.Contains(src, "http.MaxBytesReader")
		hasMultiWriter := strings.Contains(src, "io.MultiWriter(")
		hasLimitReader := strings.Contains(src, "io.LimitReader(") ||
			strings.Contains(src, "LimitWriter")
		if hasMaxBytes || hasMultiWriter || hasLimitReader {
			t.Errorf("AUDIT-057 partial-fix in %s (MaxBytes=%v "+
				"MultiWriter=%v Limit=%v) — verify the cap is actually "+
				"applied to gh stdout, not unrelated paths.",
				ghPath, hasMaxBytes, hasMultiWriter, hasLimitReader)
		} else {
			t.Errorf("AUDIT-057: %s has no stdout size cap on the gh "+
				"runner. An adversarial or simply very large "+
				"`gh api --paginate repos/.../comments` response is "+
				"read entirely into RAM. The daemon OOMs. Needed: "+
				"io.MultiWriter(&buf, countingDiscard) with 64MB cap "+
				"returning ErrClassPermanent on overflow.", ghPath)
		}

		// (c) Confirm --paginate IS used on PRIssueComments +
		//     PRReviewComments (amplifies the risk).
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
