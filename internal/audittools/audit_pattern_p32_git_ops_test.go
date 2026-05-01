// D3 P6B.2 — Pattern P32: every git/gh subprocess invocation must
// route through the GitOperationLog wrapper.
//
// Walks production code (non-_test.go) under internal/ and cmd/ for
// direct calls to exec.Command("git"|"gh", ...) and exec.CommandContext(
// ..., "git"|"gh", ...). Each hit must either:
//   - live in internal/git/ where the wrapper itself is defined (the
//     entry-point helpers route through LogAndRun), OR
//   - appear in p32Allowlist with a one-line truthful rationale that
//     names the call site and the migration path.
//
// Forward-going code MUST use igit.LogAndRun or one of the wrapped
// helpers (runGitCtx, runGitCtxOutput, bestEffortRun). Pre-6B direct
// call sites are recorded as a backlog (sweep target: 6B follow-up
// commit train + selected D4 work) — same allowlist shape as P27 for
// the notification-budget migration and P31 for LLM transcripts.
package audittools

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// p32Allowlist names files where a direct exec.Command("git"|"gh", ...)
// site is acceptable. Entries are the migration backlog from D3 P6B.2.
var p32Allowlist = map[string]string{
	// internal/git is the wrapper layer itself — its entry points
	// route through LogAndRun, which is the canonical chokepoint.
	"internal/git/git.go":         "internal/git: GetDefaultBranch uses raw exec.CommandContext for the symbolic-ref lookup that runs before any DB is wired (boot path); the helper layer (runGitCtx / runGitCtxOutput / bestEffortRun) routes through LogAndRun. Migration: thread igit-internal LogAndRun once boot-time DB attachment is reordered.",
	"internal/git/oplog.go":       "internal/git/oplog.go IS the wrapper; its single exec.CommandContext call IS LogAndRun's underlying invocation",
	"internal/git/askbranch.go":   "internal/git: ask-branch helpers — pre-6B direct exec; migration to LogAndRun is mechanical (route through runGitCtx). Migration target: 6B follow-up train.",
	"internal/git/validators.go":  "internal/git: branch-name + ref validators run pre-DB (CLI-stage validation); they need to log to GitOperationLog only when called from a daemon context. Migration target: 6B follow-up train.",

	// internal/gh is the gh-specific shell wrapper. Routing it through
	// LogAndRun is mechanical but invasive (signature change to thread
	// OpContext through every call site). Slated for follow-up.
	"internal/gh/gh.go": "internal/gh: gh CLI shell wrapper — pre-6B direct exec.Command. Migration: replace exec.Command with igit.LogAndRun(ctx, OpContext{}, op, \"gh\", args...). Migration target: 6B follow-up train.",

	// internal/agents — agents that make git/gh calls. Each carries a
	// migration path. Mechanical sweep: replace exec.CommandContext
	// with igit.LogAndRun(ctx, OpContext{TaskID: t.ID, Repo: r.Name},
	// op, "git", args...). Most call sites already have task + repo
	// in scope.
	"internal/agents/astromech.go":            "Astromech runs `git` against the per-task worktree from many helpers (runShortGit, combinedShortGit, …). Migration: thread OpContext{TaskID: bounty.ID, Repo: repo.Name} through each helper. Migration target: 6B follow-up train.",
	"internal/agents/divergence_detector.go":  "Divergence detector — pre-6B direct rev-parse; migration: igit.LogAndRun with OpContext{Repo: repo.Name}. Migration target: 6B follow-up train.",
	"internal/agents/dogs.go":                 "Dog git-hygiene cleanup — pre-6B direct exec (rev-parse / checkout / branch -D); migration: route through internal/git/runGitCtx (which already logs). Migration target: 6B follow-up train.",
	"internal/agents/pilot_preflight.go":      "Pilot preflight — pre-6B direct ls-remote; migration: igit.LogAndRun. Migration target: 6B follow-up train.",
	"internal/agents/pilot_repo_config.go":    "Pilot repo-config probe — ls-remote ping; migration: igit.LogAndRun. Migration target: 6B follow-up train.",
	"internal/agents/pilot_worktree_reset.go": "Pilot worktree-reset — multi-step (rebase --abort / merge --abort / reset --hard / clean -fdx); migration: each step routes through igit.LogAndRun. Migration target: 6B follow-up train.",
	"internal/agents/pr_flow.go":              "Pilot PR-flow — push + ask-branch tip lookups; migration: igit.LogAndRun with OpContext{Repo: repo.Name, ConvoyID: convoyID}. Migration target: 6B follow-up train.",
	"internal/agents/reconcile.go":            "Reconciler — branch-existence rev-parse; migration: igit.LogAndRun. Migration target: 6B follow-up train.",
	"internal/agents/shadow/worktree.go":      "Shadow worktree manager — git -C <wt> ops; migration: igit.LogAndRun. Migration target: 6B follow-up train.",

	// cmd/force — CLI entry points whose git ops run in the user's
	// terminal session, often before a daemon DB is attached. The
	// rows would still be useful when the CLI runs against an
	// existing holocron, but the migration is non-trivial because
	// CLI subcommands don't always init holocron. Slated for D4.
	"cmd/force/fleet_cmds.go":   "CLI fleet-cmds — pre-6B direct exec for one-shot fleet bootstrap ops. Migration: gate on holocron-attached. Migration target: D4.",
	"cmd/force/maintenance.go":  "CLI maintenance — pre-6B direct exec for repo-level housekeeping. Migration target: D4.",

	// internal/store — tasks.go has a startup-time git op for
	// detecting the orchestrator's own repo. Boot path, no DB yet.
	"internal/store/tasks.go": "internal/store/tasks.go: rev-parse during DB init detects orchestrator self-repo; runs BEFORE the DB is fully ready. Migration: route once the bootstrap order is restructured. Migration target: D4.",
}

// p32CallPattern detects exec.Command(... "git" ...) and
// exec.CommandContext(..., "git" ...) shaped invocations. The first
// or second positional must be the literal string "git" or "gh".
var p32CallPattern = regexp.MustCompile(
	`exec\.Command(?:Context)?\([^)]*"(?:git|gh)"`,
)

func TestPattern_P32_GitOpsLogged(t *testing.T) {
	root := repoRootP32(t)

	type hit struct {
		path string
		line int
		text string
	}
	var hits []hit
	walkDirs := []string{"internal", "cmd"}
	for _, dir := range walkDirs {
		_ = filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}
			if strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			for ln, line := range strings.Split(string(b), "\n") {
				trimmed := strings.TrimSpace(line)
				if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "*") {
					continue
				}
				if p32CallPattern.MatchString(line) {
					rel, _ := filepath.Rel(root, path)
					hits = append(hits, hit{path: rel, line: ln + 1, text: trimmed})
				}
			}
			return nil
		})
	}

	violations := map[string][]hit{}
	for _, h := range hits {
		if _, ok := p32Allowlist[h.path]; ok {
			continue
		}
		violations[h.path] = append(violations[h.path], h)
	}

	if len(violations) > 0 {
		var msg strings.Builder
		msg.WriteString("Pattern P32 violation: direct git/gh exec calls outside igit.LogAndRun:\n\n")
		paths := make([]string, 0, len(violations))
		for p := range violations {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		for _, p := range paths {
			for _, h := range violations[p] {
				msg.WriteString("  ")
				msg.WriteString(p)
				msg.WriteString(":")
				msg.WriteString(itoaP32(h.line))
				msg.WriteString(" ")
				msg.WriteString(h.text)
				msg.WriteString("\n")
			}
		}
		msg.WriteString("\nFix: replace with igit.LogAndRun(ctx, igit.OpContext{...}, op, \"git\"|\"gh\", args...)\n")
		msg.WriteString("OR: add an allowlist entry to p32Allowlist with a one-line truthful rationale.\n")
		t.Error(msg.String())
	}

	for path, rationale := range p32Allowlist {
		if strings.TrimSpace(rationale) == "" {
			t.Errorf("Pattern P32 allowlist: %q has empty rationale", path)
		}
	}
}

func repoRootP32(t *testing.T) string {
	t.Helper()
	wd, _ := filepath.Abs(".")
	for d := wd; d != "/" && d != ""; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
	}
	t.Fatalf("repo root not found from %s", wd)
	return ""
}

func itoaP32(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	buf := make([]byte, 0, 12)
	for n > 0 {
		buf = append(buf, byte('0'+n%10))
		n /= 10
	}
	if neg {
		buf = append(buf, '-')
	}
	for i, j := 0, len(buf)-1; i < j; i, j = i+1, j-1 {
		buf[i], buf[j] = buf[j], buf[i]
	}
	return string(buf)
}
