// Package audittools: pattern test for exec.CommandContext adoption.
package audittools

import (
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// shortExecAllowlist names files where bare exec.Command is acceptable
// because every call site is either (a) a sub-second lookup (rev-parse,
// version, config) where the caller holds no context, (b) a user-invoked
// CLI command whose lifecycle is bounded by the user's terminal session
// (tail, grep), or (c) covered by its own internal timeout mechanism.
// Each file entry includes a one-line reason.
//
// Any NEW bare exec.Command in a long-running daemon context (git push,
// git fetch, claude CLI, sub-PR ops, etc.) in a NON-allowlisted file is
// flagged. The allowlist is reason-gated — adding a file here requires
// a justification that survives reviewer scrutiny.
var shortExecAllowlist = map[string]string{
	"internal/git/username.go": "username discovery — runs at most once per process, already wrapped in runWithTimeout",

	// CLI tooling — user-invoked commands whose lifetime is bounded by the
	// user's terminal session. exec.Command without a context is idiomatic
	// here because the user has a TTY to Ctrl-C.
	"cmd/force/maintenance.go":  "CLI force doctor: version checks + synchronous repo scans, user-bounded",
	"cmd/force/obs_cmds.go":     "CLI force tail / force watch: user-bounded tail/grep pipelines",
	"cmd/force/fleet_cmds.go":   "CLI force daemon preflight: synchronous init-time git checks",

	// Daemon helpers with sub-second lookups (rev-parse, symbolic-ref,
	// config). Migrating these to CommandContext would add noise without
	// real benefit — the caller's e-stop path would never fire inside a
	// 50ms git rev-parse. When one of these grows a long-running op, it
	// must migrate AND be removed from this allowlist.
	"internal/agents/pilot_preflight.go":    "pilot preflight: sub-second symbolic-ref / rev-parse lookups",
	"internal/agents/pilot_repo_config.go":  "repo-config dog: ls-remote for pingability, guarded by dog-level 5m timeout",
	"internal/agents/dogs.go":               "git-hygiene: rev-parse / checkout-detach in orphan-branch cleanup (sub-second)",
	"internal/agents/inquisitor.go":         "commits-since check for stall detection (sub-second git log)",

	// Explicit context-bearing call sites in these files are present
	// alongside bare exec.Command for short lookups — the long-running
	// ops use CommandContext already.
	"internal/agents/pr_flow.go":            "sub-PR ops already use git.TriggerCIRerun (CommandContext); remaining bare calls are short (rev-parse)",
	"internal/agents/pilot_worktree_reset.go": "worktree reset cleanup already uses igit.runShortGit ctx helpers; remaining bare exec.Command calls are reset/clean cleanup",

	// gh.go wraps its exec.Command in ExecRunner which has its own
	// timeout (ExecRunner.Timeout) and Kill+drain backstop (AUDIT-092).
	"internal/gh/gh.go": "ExecRunner has its own Timeout + Kill+drain (AUDIT-092) — bare exec.Command is intentional",

	// Comments and string literals in these files reference exec.Command
	// as documentation/examples; no runtime calls.
	"internal/git/validators.go": "comment-only reference (CVE documentation)",
	"internal/store/tasks.go":    "comment-only reference (branch_name validator doc)",
}

// TestPattern_P11_ExecCommandsUseContext is the Fix #8d regression guard
// for AUDIT-127 / AUDIT-158 / AUDIT-165 — long-running subprocess
// invocations must thread a context so daemon shutdown and e-stop can
// cancel them. The test walks every production *.go file (non-_test.go)
// and fails if `exec.Command(` is used for a non-allowlisted binary.
//
// Accepted forms in production code:
//   1. `exec.CommandContext(ctx, ...)` — preferred, carries a cancellable ctx
//   2. `exec.Command(...)` inside an allowlisted helper file (short lookups
//      like `git rev-parse HEAD`, `git config user.name`) — these complete
//      in milliseconds and the caller holds no context
//
// The test fails on bare `exec.Command(` in any production file not on
// the allowlist.
func TestPattern_P11_ExecCommandsUseContext(t *testing.T) {
	root := moduleRoot(t)
	cmdRe := regexp.MustCompile(`\bexec\.Command\(`)

	type offender struct {
		path string
		line int
		text string
	}
	var offenders []offender

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".fix-worktrees" || name == ".force-worktrees" ||
				name == "vendor" || name == ".git" || name == "node_modules" ||
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
		// Allow allowlisted files outright — their bare exec.Command usage
		// is documented as sub-second lookups.
		relPath := rel(root, path)
		if _, ok := shortExecAllowlist[relPath]; ok {
			return nil
		}
		body, rerr := readFile(path)
		if rerr != nil {
			return rerr
		}
		lines := strings.Split(string(body), "\n")
		for i, line := range lines {
			if cmdRe.MatchString(line) {
				offenders = append(offenders, offender{
					path: relPath,
					line: i + 1,
					text: strings.TrimSpace(line),
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
	t.Errorf("Pattern P11 (Fix #8d): %d bare exec.Command call(s) in production code (non-allowlisted):", len(offenders))
	for _, o := range offenders {
		t.Errorf("  %s:%d — %s", o.path, o.line, o.text)
	}
	t.Errorf("\nFix: use exec.CommandContext(ctx, ...) with a context.WithTimeout so daemon shutdown / e-stop can cancel. Allowlist sub-second lookups in shortExecAllowlist (internal/audittools/audit_pattern_p11_exec_context_test.go) with a reason.")
}
