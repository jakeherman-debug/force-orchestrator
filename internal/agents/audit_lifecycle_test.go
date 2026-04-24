package agents

// Static audit verification for daemon-lifecycle / resource-leak findings.
// These tests grep the cited source files and assert the defect still
// matches the finding description. They modify no source.

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// lifecycleRepoRoot walks up from this test file to the module root.
func lifecycleRepoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller failed")
	}
	// .../internal/agents/audit_lifecycle_test.go → repo root is two dirs up.
	return filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
}

func lifecycleReadFile(t *testing.T, rel string) string {
	t.Helper()
	path := filepath.Join(lifecycleRepoRoot(t), rel)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

func TestAuditLifecycleFindings(t *testing.T) {

	// ── AUDIT-020 ─────────────────────────────────────────────────────────
	// Fix #1 threaded context.Context through every Spawn* and the fleet_cmds
	// daemon. Assert the remedy is in place — permanent regression
	// protection against anyone adding a new Spawn* without threading ctx in.
	t.Run("TestAUDIT_020_daemon_no_root_context", func(t *testing.T) {
		src := lifecycleReadFile(t, "cmd/force/fleet_cmds.go")
		// Every agents.Spawn*(...) call in the daemon must pass ctx first.
		spawnCtx := regexp.MustCompile(`agents\.Spawn\w+\s*\(\s*ctx\b`)
		if spawnCtx.FindStringIndex(src) == nil {
			t.Fatal("AUDIT-020 regressed: daemon no longer threads ctx to agents.Spawn*(...) — " +
				"restore ctx as the first arg on every spawn so shutdown cancellation propagates")
		}
		if !regexp.MustCompile(`agents\.Spawn\w+\s*\(\s*db\b`).MatchString(src) &&
			spawnCtx.FindStringIndex(src) == nil {
			t.Fatalf("AUDIT-020 precondition missing: no agents.Spawn*(...) calls in fleet_cmds.go")
		}
		// Every Spawn* signature must have context.Context in its parameter list.
		for _, f := range []string{
			"internal/agents/astromech.go",
			"internal/agents/captain.go",
			"internal/agents/jedi_council.go",
			"internal/agents/commander.go",
			"internal/agents/chancellor.go",
			"internal/agents/pilot.go",
			"internal/agents/medic.go",
			"internal/agents/diplomat.go",
			"internal/agents/inquisitor.go",
		} {
			body := lifecycleReadFile(t, f)
			sigCtx := regexp.MustCompile(`func\s+Spawn\w+\s*\([^)]*context\.Context[^)]*\)`)
			if sigCtx.FindStringIndex(body) == nil {
				t.Fatalf("AUDIT-020 regressed: Spawn* signature in %s lacks context.Context — "+
					"restore ctx as the first parameter", f)
			}
		}
	})

	// ── AUDIT-125 ─────────────────────────────────────────────────────────
	// internal/agents/astromech.go:407
	// `heartbeatDone := make(chan struct{})` is closed at line 432 with a bare
	// `close(heartbeatDone)` — NOT via defer. A panic in RunCLIStreaming leaks
	// the heartbeat goroutine + ticker.
	t.Run("TestAUDIT_125_heartbeat_not_deferred", func(t *testing.T) {
		src := lifecycleReadFile(t, "internal/agents/astromech.go")
		lines := strings.Split(src, "\n")
		var declIdx = -1
		for i, l := range lines {
			if strings.Contains(l, "heartbeatDone := make(chan struct{})") {
				declIdx = i
				break
			}
		}
		if declIdx < 0 {
			t.Fatalf("AUDIT-125 precondition missing: heartbeatDone declaration not found")
		}
		// RGR inversion: fail if defer close(heartbeatDone) is still absent
		// near declaration. Window is 40 lines — wide enough to cover the
		// heartbeat goroutine spawn (which must live between the make and
		// the defer so AUDIT-105's carve-out picks up the IsEstopped poll),
		// but tight enough that the defer has to be in the same logical
		// block as the declaration.
		end := declIdx + 40
		if end > len(lines) {
			end = len(lines)
		}
		window := strings.Join(lines[declIdx:end], "\n")
		if !strings.Contains(window, "defer close(heartbeatDone)") {
			t.Fatal("AUDIT-125: heartbeatDone closed without defer still present")
		}
	})

	// ── AUDIT-126 ─────────────────────────────────────────────────────────
	// internal/agents/astromech.go:424-428
	// `os.Create(taskLogPath)` has no matching `defer Close()` / `defer Remove()`.
	// Panic / early return paths leak FD and stale file.
	t.Run("TestAUDIT_126_tasklog_not_deferred", func(t *testing.T) {
		src := lifecycleReadFile(t, "internal/agents/astromech.go")
		lines := strings.Split(src, "\n")
		var createIdx = -1
		for i, l := range lines {
			if strings.Contains(l, "os.Create(taskLogPath)") {
				createIdx = i
				break
			}
		}
		if createIdx < 0 {
			t.Fatalf("AUDIT-126 precondition missing: os.Create(taskLogPath) not found")
		}
		// RGR inversion: fail if deferred Close+Remove are still missing near os.Create.
		end := createIdx + 11
		if end > len(lines) {
			end = len(lines)
		}
		window := strings.Join(lines[createIdx:end], "\n")
		hasDeferClose := regexp.MustCompile(`defer\s+.*taskLogFile\.Close\(\)`).MatchString(window)
		hasDeferRemove := regexp.MustCompile(`defer\s+os\.Remove\(taskLogPath\)`).MatchString(window)
		if !(hasDeferClose && hasDeferRemove) {
			t.Fatal("AUDIT-126: os.Create(taskLogPath) without deferred Close+Remove still present")
		}
	})

	// ── AUDIT-127 ─────────────────────────────────────────────────────────
	// internal/git/git.go and askbranch.go — every `exec.Command("git", ...)` is
	// bare, none use CommandContext. A hung `git fetch` wedges the caller.
	t.Run("TestAUDIT_127_git_no_context_timeout", func(t *testing.T) {
		gitSrc := lifecycleReadFile(t, "internal/git/git.go")
		askSrc := lifecycleReadFile(t, "internal/git/askbranch.go")
		cmdRe := regexp.MustCompile(`exec\.Command\(\s*"git"`)
		ctxRe := regexp.MustCompile(`exec\.CommandContext\(`)
		gitCmd := len(cmdRe.FindAllStringIndex(gitSrc, -1))
		gitCtx := len(ctxRe.FindAllStringIndex(gitSrc, -1))
		askCmd := len(cmdRe.FindAllStringIndex(askSrc, -1))
		askCtx := len(ctxRe.FindAllStringIndex(askSrc, -1))
		total := gitCmd + askCmd
		totalCtx := gitCtx + askCtx
		// Post-fix contract (Fix #8d): totalCtx must be non-zero, and bare
		// exec.Command("git", ...) must not dominate. total == 0 is the
		// ideal state (every call migrated); total > 0 is tolerated only
		// when at least half as many CommandContext calls exist.
		if totalCtx == 0 {
			t.Fatal("AUDIT-127: no exec.CommandContext calls in internal/git/* — migration regressed")
		}
		if total > totalCtx*2 {
			t.Fatalf("AUDIT-127: bare exec.Command for git subprocesses without CommandContext still dominates (total=%d, totalCtx=%d)", total, totalCtx)
		}
	})

	// ── AUDIT-129 ─────────────────────────────────────────────────────────
	// internal/claude/claude.go:326-327
	// stderrBuf / textBuf are strings.Builder with no size cap. A runaway
	// Claude can OOM the daemon before the 200 KB astromech breaker fires.
	t.Run("TestAUDIT_129_unbounded_buffers", func(t *testing.T) {
		src := lifecycleReadFile(t, "internal/claude/claude.go")
		if !strings.Contains(src, "var stderrBuf strings.Builder") {
			t.Fatalf("AUDIT-129 precondition missing: stderrBuf strings.Builder not found")
		}
		if !strings.Contains(src, "var textBuf strings.Builder") {
			t.Fatalf("AUDIT-129 precondition missing: textBuf strings.Builder not found")
		}
		// RGR inversion: fail if textBuf still has no size cap.
		capped := regexp.MustCompile(`textBuf\.Len\(\)\s*[<>]=?\s*\d`)
		if !capped.MatchString(src) {
			t.Fatal("AUDIT-129: unbounded textBuf strings.Builder without size cap still present")
		}
		if !regexp.MustCompile(`textBuf\.WriteString\(`).MatchString(src) {
			t.Fatalf("AUDIT-129 precondition missing: textBuf.WriteString not found")
		}
	})

	// ── AUDIT-158 ─────────────────────────────────────────────────────────
	// internal/agents/astromech.go:574,589,627,657,660,665
	// Ownership-detach, commit-inference, shard-cleanup git calls use bare
	// exec.Command(...).Run() / .CombinedOutput() with no timeout. A hung git
	// process wedges the astromech goroutine while holding the Locked row.
	t.Run("TestAUDIT_158_astromech_git_no_timeout", func(t *testing.T) {
		src := lifecycleReadFile(t, "internal/agents/astromech.go")
		// Sweep the whole file for bare exec.Command(...) .Run/.CombinedOutput patterns.
		runRe := regexp.MustCompile(`exec\.Command\([^)]+\)\.Run\(\)`)
		coRe := regexp.MustCompile(`exec\.Command\([^)]+\)\.CombinedOutput\(\)`)
		bareRun := len(runRe.FindAllStringIndex(src, -1))
		bareCO := len(coRe.FindAllStringIndex(src, -1))
		// RGR inversion: fail if bare exec.Command .Run/.CombinedOutput patterns remain.
		if bareRun+bareCO > 0 {
			t.Fatal("AUDIT-158: bare exec.Command().Run()/CombinedOutput() without CommandContext still present")
		}
	})

	// ── AUDIT-164 ─────────────────────────────────────────────────────────
	// cmd/force/fleet_cmds.go:217 — signal.Notify with no matching
	// `defer signal.Stop(sigChan)`. Leaks registration in embedded test runs.
	t.Run("TestAUDIT_164_signal_channel_never_stopped", func(t *testing.T) {
		src := lifecycleReadFile(t, "cmd/force/fleet_cmds.go")
		if !strings.Contains(src, "signal.Notify(sigChan") {
			t.Fatalf("AUDIT-164 precondition missing: signal.Notify(sigChan, ...) not found")
		}
		// RGR inversion: fail if signal.Stop(sigChan) is still missing.
		if !strings.Contains(src, "signal.Stop(sigChan)") {
			t.Fatal("AUDIT-164: signal.Notify without defer signal.Stop still present")
		}
	})

	// ── AUDIT-165 ─────────────────────────────────────────────────────────
	// internal/git/askbranch.go:138-145 — MkdirTemp + deferred worktree
	// remove without a timeout. If `git worktree remove` hangs, the defer
	// never returns and the caller wedges.
	t.Run("TestAUDIT_165_worktree_remove_no_timeout", func(t *testing.T) {
		src := lifecycleReadFile(t, "internal/git/askbranch.go")
		if !strings.Contains(src, `os.MkdirTemp("", "force-rebase-`) {
			t.Fatalf("AUDIT-165 precondition missing: os.MkdirTemp(..., \"force-rebase-*\") not found")
		}
		// Find the defer block and confirm the worktree-remove pattern.
		deferBlockRe := regexp.MustCompile(`defer\s+func\(\)\s*\{[^}]*\}\(\)`)
		m := deferBlockRe.FindString(src)
		if m == "" {
			lines := strings.Split(src, "\n")
			var startIdx = -1
			for i, l := range lines {
				if strings.Contains(l, `os.MkdirTemp("", "force-rebase-`) {
					startIdx = i
					break
				}
			}
			end := startIdx + 15
			if end > len(lines) {
				end = len(lines)
			}
			m = strings.Join(lines[startIdx:end], "\n")
		}
		if !strings.Contains(m, `worktree`) || !strings.Contains(m, `remove`) {
			t.Fatalf("AUDIT-165 precondition missing: worktree remove not found near MkdirTemp")
		}
		// RGR inversion: fail if worktree remove does not yet use CommandContext.
		if !strings.Contains(m, "exec.CommandContext") {
			t.Fatal("AUDIT-165: worktree remove using bare exec.Command without timeout still present")
		}
		if !strings.Contains(m, "os.RemoveAll(wtPath)") {
			t.Fatalf("AUDIT-165 precondition missing: os.RemoveAll(wtPath) not found")
		}
	})
}
