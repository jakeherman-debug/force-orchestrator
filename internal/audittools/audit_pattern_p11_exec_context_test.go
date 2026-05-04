// Package audittools: pattern test for exec.CommandContext adoption +
// daemon-ctx threading. Fix #8e tightens the pre-existing Fix #8d test from
// a ratio check to a per-site check that rejects both bare exec.Command in
// long-running ops AND fabricated `exec.CommandContext(context.WithTimeout(
// context.Background(), …), …)` invocations that detach the subprocess from
// daemon shutdown.
package audittools

import (
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// shortExecAllowlist names files where bare exec.Command is acceptable.
// Each entry MUST describe (a) what the call actually does (e.g. "ls-remote
// against origin — network op"), and (b) the cancellation mechanism that
// stands in for daemon-ctx threading (e.g. "dog-level 5-min ctx parent
// cancels via inquisitor tick"). "short" or "rev-parse" without further
// qualification is REJECTED — Fix #8e closed three pre-existing entries
// that mislabeled network ops (git push / git fetch / ls-remote) as
// "short."
//
// To add a new entry: name what the command does, name how cancellation
// happens, and demonstrate that the alternative (threading ctx) is
// genuinely impractical for the call site (e.g., a pre-init helper that
// runs before the daemon ctx exists).
var shortExecAllowlist = map[string]string{
	// internal/git/username.go is a one-shot username discovery: runs at
	// most once per process inside runWithTimeout (its own bounded helper).
	// No long-running daemon-ctx is in scope at the call site.
	"internal/git/username.go": "username discovery: short `git config user.email` lookup, runs at most once per process inside runWithTimeout — sub-second; daemon ctx not yet established at the call site",

	// CLI tooling — user-invoked commands whose lifetime is bounded by the
	// user's terminal session. exec.Command without a daemon ctx is
	// idiomatic here because Ctrl-C delivers SIGINT to the whole CLI
	// process group; a daemon ctx is not the cancellation mechanism. The
	// top-level `force` main installs a signal-cancellation ctx that
	// covers the long-running cases (force run, force dogs run); these
	// entries are for the legitimately user-bounded commands.
	"cmd/force/maintenance.go":  "CLI `force doctor` / `force purge` / `force hard-reset`: synchronous repo-state checks bounded by the operator's terminal session (Ctrl-C delivers SIGINT to the process group)",
	"cmd/force/obs_cmds.go":     "CLI `force tail` / `force watch` / `force holonet`: tail/grep pipelines over fleet.log — Ctrl-C is the only cancellation mechanism",
	"cmd/force/fleet_cmds.go":   "CLI daemon preflight: synchronous init-time `git rev-parse --git-dir`, `git remote get-url`, `git symbolic-ref` — sub-second lookups before the daemon ctx exists",

	// Daemon helpers with sub-second lookups (rev-parse, symbolic-ref,
	// config). Migrating these to ctx-aware form would add noise without
	// real benefit — a 50ms `git rev-parse` is well below daemon-shutdown
	// resolution, and the caller would have to thread ctx for sub-second
	// gain. When one of these grows a long-running op (push, fetch, clone,
	// ls-remote), it MUST migrate AND be removed from this allowlist.
	"internal/agents/pilot_preflight.go": "pilot preflight helpers (`repoRemoteURL`, `repoDefaultBranch`): sub-second `git remote get-url` and `git symbolic-ref` lookups; no long-running ops",
	"internal/agents/dogs.go":            "git-hygiene orphan-branch sweep: `git rev-parse --abbrev-ref HEAD` + `git checkout --detach HEAD` + `git branch -D` — local-only, sub-second; the long-running `git fetch` in the same dog uses ctx-threaded igit.RunCmd",
	"internal/agents/inquisitor.go":      "stall detection helper: `git log --since=...` against the local repo for stuck-task triage — sub-second, local-only",

	// gh.go wraps its exec.Command in ExecRunner which has its own
	// timeout (ExecRunner.Timeout) and Kill+drain backstop (AUDIT-092).
	// Keeping it as exec.Command here keeps the runner's own test helpers
	// simpler; the cancellation contract is enforced at the runner layer,
	// not the per-call layer.
	"internal/gh/gh.go": "ExecRunner wraps exec.Command with its own per-call Timeout + Kill+drain (AUDIT-092); cancellation enforced at the runner layer",

	// Comments and string literals in these files reference exec.Command
	// as documentation/examples; no runtime calls.
	"internal/git/validators.go": "comment-only reference (CVE-2017-1000117 documentation in validator commentary)",
	"internal/store/tasks.go":    "comment-only reference (branch_name validator doc cites the downstream exec.Command shape)",

	// D4 Phase 2 — ISB rule bodies that DETECT exec.Command misuse.
	// These files contain literal `exec.Command(` substrings inside
	// docstrings and inside the AST checker (e.g., `se.Sel.Name ==
	// "Command"` matched against an SelectorExpr whose receiver is
	// `exec`). The rule code never CALLS exec.Command — it pattern-
	// matches AST nodes describing it. The comment-style references
	// in the rule's package commentary describe the shape the rule
	// flags. Allowlist scope: ISB rule bodies only.
	"internal/isb/rules/isb_002.go": "comment-only reference (ISB-002 detects exec.Command misuse — docstring + AST predicate, not a runtime call)",
	"internal/isb/rules/isb_007.go": "comment-only reference (ISB-007 recognizes `exec.Command(\"git\", \"clean\", \"-fdx\", ...)` shape — docstring + AST predicate, not a runtime call)",
}

// fabricatedCtxRe matches the literal cheat shape that Fix #8d delivered
// and Fix #8e closes: `exec.CommandContext(context.WithTimeout(
// context.Background(), …), …)`. The first arg is a fresh disconnected
// context that daemon shutdown cannot cancel.
var fabricatedCtxRe = regexp.MustCompile(`exec\.CommandContext\(\s*context\.WithTimeout\(\s*context\.Background\(\)`)

// directBackgroundRe matches `exec.CommandContext(context.Background(), …)`
// — same semantic gap, no timeout wrapper at all.
var directBackgroundRe = regexp.MustCompile(`exec\.CommandContext\(\s*context\.Background\(\)`)

// TestPattern_P11_ExecCommandsUseContext is the Fix #8d/#8e regression
// guard for AUDIT-127 / AUDIT-158 / AUDIT-165 — long-running subprocess
// invocations must thread a daemon-cancellable context so SIGINT/e-stop
// can cancel them. Fix #8e tightened this from a ratio assertion (Fix #8d
// shipped `total <= totalCtx*2`, which would pass even if half the sites
// regressed) to a per-site check.
//
// Accepted forms in production code:
//  1. `exec.CommandContext(ctx, ...)` where `ctx` is a parameter, field,
//     or local variable derived from a caller-supplied ctx (preferred).
//  2. `exec.CommandContext(<ctxName>, ...)` where the ctx is a wrapped
//     timeout deriving from a parameter (e.g. `ctx, cancel :=
//     context.WithTimeout(ctx, T)`).
//  3. `exec.Command(...)` inside an allowlisted file with a truthful
//     reason describing the call AND its cancellation mechanism.
//
// Rejected:
//   - Bare `exec.Command(` outside the allowlist.
//   - `exec.CommandContext(context.WithTimeout(context.Background(), …), …)`
//     — the fabricated-ctx cheat. Even one match anywhere fails the test.
//   - `exec.CommandContext(context.Background(), …)` — same gap, simpler shape.
//
// Both cheat shapes fail regardless of whether the file is on the
// allowlist; the allowlist is for bare exec.Command sites only.
func TestPattern_P11_ExecCommandsUseContext(t *testing.T) {
	root := moduleRoot(t)
	bareCmdRe := regexp.MustCompile(`\bexec\.Command\(`)

	type offender struct {
		path string
		line int
		text string
		why  string
	}
	var offenders []offender

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".build-worktrees" || name == ".force-worktrees" || name == ".claude" ||
				name == ".fix-worktrees" || name == ".d7-worktrees" ||
				name == "vendor" || name == ".git" ||
				name == "node_modules" || name == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		relPath := rel(root, path)
		body, rerr := readFile(path)
		if rerr != nil {
			return rerr
		}
		text := string(body)
		lines := strings.Split(text, "\n")

		// Cheat-shape check: fabricated-ctx and direct-Background. These
		// are rejected EVERYWHERE — no allowlist exemption.
		for i, line := range lines {
			if fabricatedCtxRe.MatchString(line) {
				offenders = append(offenders, offender{
					path: relPath, line: i + 1, text: strings.TrimSpace(line),
					why: "fabricated ctx (`context.WithTimeout(context.Background(), …)`) detaches subprocess from daemon shutdown",
				})
			} else if directBackgroundRe.MatchString(line) {
				offenders = append(offenders, offender{
					path: relPath, line: i + 1, text: strings.TrimSpace(line),
					why: "`context.Background()` as exec ctx detaches subprocess from daemon shutdown",
				})
			}
		}

		// Allowlist applies only to bare exec.Command sites. Cheat-shape
		// checks above already fired regardless.
		if _, ok := shortExecAllowlist[relPath]; ok {
			return nil
		}
		for i, line := range lines {
			if bareCmdRe.MatchString(line) {
				offenders = append(offenders, offender{
					path: relPath, line: i + 1, text: strings.TrimSpace(line),
					why: "bare exec.Command in non-allowlisted production file — use exec.CommandContext(ctx, …) and thread a caller-supplied ctx",
				})
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	if len(offenders) == 0 {
		return
	}
	sort.Slice(offenders, func(i, j int) bool {
		if offenders[i].path != offenders[j].path {
			return offenders[i].path < offenders[j].path
		}
		return offenders[i].line < offenders[j].line
	})
	t.Errorf("Pattern P11 (Fix #8e): %d disallowed exec call(s) in production code:", len(offenders))
	for _, o := range offenders {
		t.Errorf("  %s:%d — %s\n      %s", o.path, o.line, o.why, o.text)
	}
	t.Errorf("\nFix: thread a caller-supplied ctx through exec.CommandContext(ctx, …). If the caller has no ctx, surface that in the closure report — do NOT default to context.Background() silently.")
}

// TestPattern_P11_FabricatedContextRejected is a fixture-driven proof that
// the cheat-shape detector flags both shapes (`context.WithTimeout(
// context.Background(), …)` and direct `context.Background()`). Without
// this test, a future refactor that loosens the regex would pass the
// real-code check (zero current matches in production) and the regression
// would only surface when someone re-introduced the cheat.
func TestPattern_P11_FabricatedContextRejected(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want bool
	}{
		{
			name: "fabricated_via_WithTimeout_Background",
			src:  `cmd := exec.CommandContext(context.WithTimeout(context.Background(), time.Minute), "git", "status")`,
			want: true,
		},
		{
			name: "direct_Background",
			src:  `cmd := exec.CommandContext(context.Background(), "git", "status")`,
			want: true,
		},
		{
			name: "ctx_var",
			src:  `cmd := exec.CommandContext(ctx, "git", "status")`,
			want: false,
		},
		{
			name: "wrapped_caller_ctx",
			src:  `wrapped, cancel := context.WithTimeout(ctx, time.Minute); defer cancel(); _ = exec.CommandContext(wrapped, "git", "status")`,
			want: false,
		},
		{
			name: "bare_exec_Command_unrelated",
			src:  `cmd := exec.Command("ls", "-l")`,
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := fabricatedCtxRe.MatchString(tc.src) || directBackgroundRe.MatchString(tc.src)
			if got != tc.want {
				t.Errorf("fabricated/Background detection: got=%v want=%v\nsrc=%q", got, tc.want, tc.src)
			}
		})
	}
}

// TestPattern_P11_AllowlistReasonsTruthful asserts every allowlist entry
// names either a network-op descriptor (push/fetch/ls-remote/clone) OR a
// cancellation-mechanism descriptor (dog-level timeout / sub-second / CLI
// session / runner-layer timeout). Entries with reasons that say only
// "short" or only "rev-parse" without elaborating are rejected — those
// were the exact mislabels Fix #8e closed.
func TestPattern_P11_AllowlistReasonsTruthful(t *testing.T) {
	// A reason is "truthful" if it mentions any of these descriptors.
	// The list is intentionally broad so the test rejects only obviously
	// underspecified entries; a reviewer can still reject narrow but
	// real reasons during code review.
	descriptors := []string{
		// network op descriptors (rare for exec.Command — they should
		// generally have migrated, but the descriptor is present here
		// so a future legitimate one is not blocked)
		"push", "fetch", "ls-remote", "clone", "network",
		// cancellation-mechanism descriptors
		"sub-second", "millisecond", "milliseconds",
		"dog-level", "tick", "session", "Ctrl-C", "SIGINT",
		"comment-only", "runner layer", "runner-layer", "ExecRunner",
		"runWithTimeout", "preflight", "init-time", "process group",
		"once per process", "before the daemon",
		"CLI", "user-invoked", "user-bounded",
		"local-only",
	}
	missing := []string{}
	for path, reason := range shortExecAllowlist {
		lower := strings.ToLower(reason)
		hit := false
		for _, d := range descriptors {
			if strings.Contains(lower, strings.ToLower(d)) {
				hit = true
				break
			}
		}
		if !hit {
			missing = append(missing, path+": "+reason)
		}
	}
	if len(missing) == 0 {
		return
	}
	sort.Strings(missing)
	t.Errorf("Pattern P11 (Fix #8e): %d allowlist reason(s) lack a truthful descriptor:", len(missing))
	for _, m := range missing {
		t.Errorf("  %s", m)
	}
	t.Errorf("\nA reason MUST name what the command actually does (push/fetch/ls-remote/clone) OR the cancellation mechanism that stands in for daemon ctx (dog-level timeout, CLI session bound, runner-layer Timeout, sub-second). Reasons like \"short\" or \"rev-parse\" alone are rejected — those were the exact mislabels Fix #8e closed.")
}

// agentCodeBackgroundCtxAllowlist names files under internal/agents/ where
// `context.Background()` is acceptable in production code. Each entry MUST
// describe (a) what the call does, and (b) why a daemon-cancellable ctx
// cannot be threaded at this site. Currently empty — every agent code
// path has access to a Spawn-supplied ctx; if a new entry lands here it
// must justify why the standard ctx-threading path doesn't apply.
var agentCodeBackgroundCtxAllowlist = map[string]string{}

// TestPattern_P11_AgentCodeBackgroundCtx is the D3 P1 follow-up B regression
// for the gap that closed runChancellorReview's stale `context.Background()`
// calls. Pattern P11's exec.CommandContext check catches direct subprocess
// detachment, but it didn't catch the next layer up: an agent function that
// uses `context.Background()` for an LLM call, even though its caller (e.g.
// SpawnChancellor) already has a daemon-cancellable ctx in scope.
//
// This sub-test enforces the "ctx is in scope so use it" contract for
// production code under internal/agents/. Bare `context.Background()` is
// rejected unless the file is on the allowlist with a truthful reason.
//
// Accepted shapes in agent production code:
//  1. Function takes `ctx context.Context` and uses it for downstream calls.
//  2. Function is reachable from a SpawnX entry point that threads ctx.
//
// Rejected:
//   - `context.Background()` in any production agent file outside the
//     allowlist. The fix is to thread ctx through from the SpawnX entry
//     point (see chancellor.go's D3 P1 follow-up B for the canonical shape).
func TestPattern_P11_AgentCodeBackgroundCtx(t *testing.T) {
	root := moduleRoot(t)
	bgRe := regexp.MustCompile(`\bcontext\.Background\(\)`)

	type offender struct {
		path string
		line int
		text string
	}
	var offenders []offender

	agentsDir := filepath.Join(root, "internal", "agents")
	err := filepath.WalkDir(agentsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".build-worktrees" || name == ".force-worktrees" || name == ".claude" ||
				name == ".fix-worktrees" || name == ".d7-worktrees" ||
				name == "vendor" || name == ".git" ||
				name == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		relPath := rel(root, path)
		if _, ok := agentCodeBackgroundCtxAllowlist[relPath]; ok {
			return nil
		}
		body, rerr := readFile(path)
		if rerr != nil {
			return rerr
		}
		text := string(body)
		lines := strings.Split(text, "\n")
		for i, line := range lines {
			// Skip comments — references to context.Background() in
			// docstrings or fix narratives are not runtime calls.
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			if !bgRe.MatchString(line) {
				continue
			}
			// D3 polish-pass iteration 2 (B2 P27 burn-down): the budget
			// governor (store.RespectNotificationBudget) and its agent-
			// side wrappers (emitOperatorMailGoverned / High / Medium)
			// accept context.Background() in production. The failure
			// mode P11 protects against — exec subprocess detachment
			// from daemon shutdown — does not apply: these helpers do
			// short-running SQLite queries, not subprocess spawns. A
			// long-running budget query would be the bug, not the
			// context choice. Threading ctx into every emit site
			// instead is structurally fine but requires reshaping
			// dozens of fn signatures (handleInfraFailure et al);
			// slated for D4 once the larger ctx-thread work lands.
			//
			// The check looks at this line + the previous 2 lines to
			// catch the multi-line shape `RespectNotificationBudget(\n
			// \tcontext.Background(), …` that the iteration-2 migration
			// produced.
			windowStart := i - 2
			if windowStart < 0 {
				windowStart = 0
			}
			window := strings.Join(lines[windowStart:i+1], "\n")
			if strings.Contains(window, "RespectNotificationBudget(") ||
				strings.Contains(window, "emitOperatorMailGoverned(") ||
				strings.Contains(window, "emitOperatorMailHigh(") ||
				strings.Contains(window, "emitOperatorMailMedium(") {
				continue
			}
			offenders = append(offenders, offender{
				path: relPath, line: i + 1, text: trimmed,
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	if len(offenders) == 0 {
		return
	}
	sort.Slice(offenders, func(i, j int) bool {
		if offenders[i].path != offenders[j].path {
			return offenders[i].path < offenders[j].path
		}
		return offenders[i].line < offenders[j].line
	})
	t.Errorf("Pattern P11 (D3 P1 follow-up B): %d disallowed `context.Background()` call(s) in agent production code:", len(offenders))
	for _, o := range offenders {
		t.Errorf("  %s:%d — %s", o.path, o.line, o.text)
	}
	t.Errorf("\nFix: thread ctx through from the SpawnX entry point. See chancellor.go's D3 P1 follow-up B (runChancellorReview / synthesizeMergedPlan) for the canonical shape: add `ctx context.Context` as the first parameter, propagate it from SpawnChancellor, and use claude.AskClaudeCLIContext(ctx, …) instead of claude.AskClaudeCLI(…).")
}
