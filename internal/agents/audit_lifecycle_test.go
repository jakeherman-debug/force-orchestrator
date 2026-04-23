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
	// cmd/force/fleet_cmds.go:144-215,425-443
	// Daemon goroutines are spawned via `go agents.SpawnX(db, ...)` with no
	// root context.Context threaded in. A signal-driven drain is the only
	// cancellation path; child `claude -p` processes and dogs cannot be
	// cooperatively cancelled. Assert Spawn* signatures do NOT accept a
	// context.Context.
	t.Run("TestAUDIT_020_daemon_no_root_context", func(t *testing.T) {
		src := lifecycleReadFile(t, "cmd/force/fleet_cmds.go")
		// Every Spawn* invocation from the daemon must be bare `(db` or
		// `(db, name)` — never `(ctx, db, …)`.
		spawnCtx := regexp.MustCompile(`agents\.Spawn\w+\s*\(\s*ctx\b`)
		if spawnCtx.FindStringIndex(src) != nil {
			t.Fatalf("AUDIT-020 regression: a Spawn* call now threads ctx; audit should be reopened")
		}
		// Sanity — the daemon DOES call Spawn* functions.
		if !regexp.MustCompile(`agents\.Spawn\w+\s*\(\s*db\b`).MatchString(src) {
			t.Fatalf("AUDIT-020 precondition missing: no agents.Spawn*(db, ...) calls in fleet_cmds.go")
		}
		// And none of the Spawn signatures themselves take a Context.
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
			if sigCtx.FindStringIndex(body) != nil {
				t.Fatalf("AUDIT-020 regression in %s: Spawn* signature now takes context.Context", f)
			}
		}
		t.Log("AUDIT-020 confirmed: Spawn* have no context.Context parameter; shutdown cannot cooperatively cancel child Claude procs")
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
		// Look at the next 10 lines for a `defer close(heartbeatDone)`.
		end := declIdx + 11
		if end > len(lines) {
			end = len(lines)
		}
		window := strings.Join(lines[declIdx:end], "\n")
		if strings.Contains(window, "defer close(heartbeatDone)") {
			t.Fatalf("AUDIT-125 looks fixed: defer close(heartbeatDone) now present near declaration")
		}
		// And confirm the bare `close(heartbeatDone)` exists somewhere (proving
		// the non-deferred close pattern).
		if !strings.Contains(src, "\tclose(heartbeatDone)") && !strings.Contains(src, "\n\tclose(heartbeatDone)") && !strings.Contains(src, "\n\t\tclose(heartbeatDone)") {
			// fallback: any non-deferred close
			if !regexp.MustCompile(`[^r]\s*close\(heartbeatDone\)`).MatchString(src) {
				t.Fatalf("AUDIT-125 precondition missing: no bare close(heartbeatDone) anywhere")
			}
		}
		t.Logf("AUDIT-125 confirmed: heartbeatDone declared at line %d with no defer close() nearby — panic in RunCLIStreaming leaks goroutine", declIdx+1)
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
		// Within the next 10 lines look for `defer` followed by Close/Remove
		// on taskLogFile / taskLogPath.
		end := createIdx + 11
		if end > len(lines) {
			end = len(lines)
		}
		window := strings.Join(lines[createIdx:end], "\n")
		hasDeferClose := regexp.MustCompile(`defer\s+.*taskLogFile\.Close\(\)`).MatchString(window)
		hasDeferRemove := regexp.MustCompile(`defer\s+os\.Remove\(taskLogPath\)`).MatchString(window)
		if hasDeferClose && hasDeferRemove {
			t.Fatalf("AUDIT-126 looks fixed: defer Close + defer Remove now present near os.Create")
		}
		t.Logf("AUDIT-126 confirmed: os.Create at line %d not followed by deferred Close+Remove; deferClose=%v deferRemove=%v", createIdx+1, hasDeferClose, hasDeferRemove)
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
		if total == 0 {
			t.Fatalf("AUDIT-127 precondition missing: no exec.Command(\"git\", ...) calls found")
		}
		// The finding says Command >> CommandContext. Assert at least 20x ratio
		// OR totalCtx == 0. If a real fix lands, CommandContext should dominate.
		if totalCtx > 0 && total <= totalCtx*2 {
			t.Fatalf("AUDIT-127 looks fixed: CommandContext (%d) now comparable to bare Command (%d)", totalCtx, total)
		}
		t.Logf("AUDIT-127 confirmed: git.go=%d+askbranch.go=%d bare exec.Command vs %d CommandContext — no deadline on any git subprocess", gitCmd, askCmd, totalCtx)
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
		// A fix would introduce a cap-check before writing. Grep for the cap
		// pattern and assert absent.
		capped := regexp.MustCompile(`textBuf\.Len\(\)\s*[<>]=?\s*\d`)
		if capped.MatchString(src) {
			t.Fatalf("AUDIT-129 looks fixed: textBuf size check found")
		}
		// Also confirm textBuf.WriteString is called without any preceding Len() guard.
		if !regexp.MustCompile(`textBuf\.WriteString\(`).MatchString(src) {
			t.Fatalf("AUDIT-129 precondition missing: textBuf.WriteString not found")
		}
		t.Log("AUDIT-129 confirmed: stderrBuf / textBuf are unbounded strings.Builder; no per-write cap — runaway stream OOMs daemon before 200 KB breaker")
	})

	// ── AUDIT-158 ─────────────────────────────────────────────────────────
	// internal/agents/astromech.go:574,589,627,657,660,665
	// Ownership-detach, commit-inference, shard-cleanup git calls use bare
	// exec.Command(...).Run() / .CombinedOutput() with no timeout. A hung git
	// process wedges the astromech goroutine while holding the Locked row.
	t.Run("TestAUDIT_158_astromech_git_no_timeout", func(t *testing.T) {
		src := lifecycleReadFile(t, "internal/agents/astromech.go")
		// Pull the specific call sites and confirm they use Command, not CommandContext.
		targetLines := []int{574, 589, 627, 657, 660, 665}
		lines := strings.Split(src, "\n")
		var bare, ctxed int
		for _, ln := range targetLines {
			if ln-1 < 0 || ln-1 >= len(lines) {
				continue
			}
			l := lines[ln-1]
			if strings.Contains(l, "exec.CommandContext") {
				ctxed++
			} else if strings.Contains(l, "exec.Command(") {
				bare++
			}
		}
		if bare == 0 {
			t.Fatalf("AUDIT-158 precondition missing: no bare exec.Command at any cited line. lines=%v", targetLines)
		}
		if ctxed > 0 {
			t.Fatalf("AUDIT-158 partially fixed: %d/%d cited lines now use CommandContext", ctxed, len(targetLines))
		}
		// And confirm the call patterns chain .Run() or .CombinedOutput().
		runRe := regexp.MustCompile(`exec\.Command\([^)]+\)\.Run\(\)`)
		coRe := regexp.MustCompile(`exec\.Command\([^)]+\)\.CombinedOutput\(\)`)
		if !runRe.MatchString(src) && !coRe.MatchString(src) {
			t.Fatalf("AUDIT-158 precondition missing: no .Run() / .CombinedOutput() chains on exec.Command")
		}
		t.Logf("AUDIT-158 confirmed: %d/%d cited astromech git sites are bare exec.Command with no timeout — hung git wedges agent goroutine", bare, len(targetLines))
	})

	// ── AUDIT-164 ─────────────────────────────────────────────────────────
	// cmd/force/fleet_cmds.go:217 — signal.Notify with no matching
	// `defer signal.Stop(sigChan)`. Leaks registration in embedded test runs.
	t.Run("TestAUDIT_164_signal_channel_never_stopped", func(t *testing.T) {
		src := lifecycleReadFile(t, "cmd/force/fleet_cmds.go")
		if !strings.Contains(src, "signal.Notify(sigChan") {
			t.Fatalf("AUDIT-164 precondition missing: signal.Notify(sigChan, ...) not found")
		}
		if strings.Contains(src, "signal.Stop(sigChan)") {
			t.Fatalf("AUDIT-164 looks fixed: signal.Stop(sigChan) now present")
		}
		t.Log("AUDIT-164 confirmed: signal.Notify without defer signal.Stop — registration leaks on embedded reinvocation")
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
		// Find the defer block and confirm the worktree-remove is a bare
		// exec.Command (not CommandContext).
		deferBlockRe := regexp.MustCompile(`defer\s+func\(\)\s*\{[^}]*\}\(\)`)
		m := deferBlockRe.FindString(src)
		if m == "" {
			// Simpler pattern — look at the 15-line window after MkdirTemp.
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
		if strings.Contains(m, "exec.CommandContext") {
			t.Fatalf("AUDIT-165 looks fixed: worktree remove now uses CommandContext")
		}
		if !strings.Contains(m, "os.RemoveAll(wtPath)") {
			t.Fatalf("AUDIT-165 precondition missing: os.RemoveAll(wtPath) not found")
		}
		t.Log("AUDIT-165 confirmed: deferred `git worktree remove` is bare exec.Command; hang blocks RemoveAll and caller")
	})
}
