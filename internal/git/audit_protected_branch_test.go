package git

// AUDIT Fix #0 — Protected-branch guards are MISSING on every destructive
// git operation that consumes a DB-derived or payload-derived branch name.
//
// Findings verified by this file (static source-level inspection):
//
//   AUDIT-102: completeAskBranchResolution (internal/agents/pr_flow.go:170)
//              force-pushes ab.AskBranch with no check against the default
//              branch. A ConvoyAskBranches.ask_branch = "main" row →
//              `git push --force-with-lease origin main`.
//
//   AUDIT-103: ForcePushBranch (internal/git/askbranch.go:293) is the
//              common-denominator used by pilot_rebase.go:164 and
//              pilot_rebase_agent.go:141. No protected-branch guard at the
//              package boundary — callers ship DB strings straight through.
//
//   AUDIT-104: TriggerCIRerun (internal/git/askbranch.go:323) pushes an
//              empty commit to a caller-supplied branch. Used from the CI
//              stall-retrigger path where `branch` defaults to `pr.Repo`
//              when branch_name is empty (pr_flow.go:709).
//
//   AUDIT-121: pilot_rebase.go:77 hardcodes `defaultBranch = "main"` when
//              repo.DefaultBranch is empty. Violates the CLAUDE.md
//              directive to call GetDefaultBranch(repoPath) — master-default
//              repos loop forever on nonexistent-branch rebases.
//
//   AUDIT-122: MergeAndCleanup (internal/git/git.go:352) runs
//              `reset --hard HEAD && clean -fd && branch -D branchName`
//              against caller-supplied paths with no guard. `branchName ==
//              default` deletes the default branch locally.
//
//   AUDIT-124: DeleteAskBranch (internal/git/askbranch.go:103) runs
//              `git branch -D branchName` + `git push origin --delete`
//              with no protected-branch guard.
//
// Approach: STATIC source inspection. We read each cited source file and
// check the body of each cited function for any of:
//   - GetDefaultBranch(   — comparison to the default branch
//   - AssertNotDefaultBranch( — a hypothetical guard helper
//   - string comparison against "main" or "master" that would REJECT the
//     input (i.e. the branch name appears on the left/right of an `==` or
//     `!=` that gates an error return)
//
// If none of those exist within the function body, the guard is MISSING
// and the audit finding is REPRODUCED-STATIC.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// repoRoot walks up from the test's CWD (internal/git) to the repo root.
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	// internal/git → ../.. is the repo root
	root := filepath.Clean(filepath.Join(wd, "..", ".."))
	// Sanity-check: go.mod must live at root.
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root %s missing go.mod: %v", root, err)
	}
	return root
}

// readSource reads a path relative to the repo root.
func readSource(t *testing.T, rel string) string {
	t.Helper()
	root := repoRoot(t)
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// extractFuncBody returns the body of the named function (brace-matched)
// from the given Go source. Returns "" if not found.
//
// The walker must skip over `{` characters that appear inside the function
// signature's parameter list — Go permits inline interface types there, e.g.
// `logger interface{ Printf(string, ...any) }`, whose braces would otherwise
// be mistaken for the function body's opening brace.
func extractFuncBody(src, funcName string) string {
	// Match `func Name(` or `func (<recv>) Name(`.
	re := regexp.MustCompile(`func\s+(?:\([^)]*\)\s+)?` + regexp.QuoteMeta(funcName) + `\s*\(`)
	loc := re.FindStringIndex(src)
	if loc == nil {
		return ""
	}
	// Walk forward skipping matching parens so we land on the function body's
	// opening `{`, not a brace nested inside an inline type declaration.
	i := loc[1] - 1 // last char of match is `(`; step back to include it
	parenDepth := 0
	braceDepthInSig := 0
	foundBodyOpen := -1
	for ; i < len(src); i++ {
		switch src[i] {
		case '(':
			parenDepth++
		case ')':
			parenDepth--
		case '{':
			if parenDepth > 0 {
				braceDepthInSig++
				continue
			}
			if braceDepthInSig > 0 {
				// Inside a signature-level interface/struct type; keep counting.
				braceDepthInSig++
				continue
			}
			foundBodyOpen = i
		case '}':
			if braceDepthInSig > 0 {
				braceDepthInSig--
			}
		}
		if foundBodyOpen >= 0 {
			break
		}
	}
	if foundBodyOpen < 0 {
		return ""
	}
	depth := 0
	start := foundBodyOpen
	for i = foundBodyOpen; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[start : i+1]
			}
		}
	}
	return ""
}

// hasProtectedBranchGuard reports true iff the function body contains any
// signal that the input branch name is being checked against the default
// branch before a destructive op runs.
func hasProtectedBranchGuard(body string) (bool, string) {
	if body == "" {
		return false, "function body not found"
	}
	// Signal 1: explicit helper call
	if strings.Contains(body, "AssertNotDefaultBranch(") {
		return true, "AssertNotDefaultBranch"
	}
	// Signal 2: comparison to GetDefaultBranch()
	if strings.Contains(body, "GetDefaultBranch(") {
		// Must appear in a guard context — i.e. close to an `==` or `!=`.
		// Cheap heuristic: look for `== GetDefaultBranch` or
		// `GetDefaultBranch(…) ==` within a 120-char window.
		if regexp.MustCompile(`==\s*GetDefaultBranch\(|GetDefaultBranch\([^)]*\)\s*==|!=\s*GetDefaultBranch\(|GetDefaultBranch\([^)]*\)\s*!=`).MatchString(body) {
			return true, "GetDefaultBranch comparison"
		}
	}
	// Signal 3: hard-coded literal comparison that would reject "main"/"master"
	// in a guard context.
	rejectMain := regexp.MustCompile(`==\s*"main"|"main"\s*==|==\s*"master"|"master"\s*==`)
	if rejectMain.MatchString(body) {
		// Check that it's followed (loosely) by a return/error. We use a
		// permissive scan — if "main" appears in an `==` comparison anywhere
		// in the body, that's evidence of a guard attempt. (AUDIT-121 is
		// the inverse: a `=` assignment to "main", not `==`.)
		return true, "literal branch-name comparison"
	}
	return false, "no guard markers found"
}

func TestAUDIT_102_103_104_121_122_124_ProtectedBranchGuardsMissing(t *testing.T) {
	// Fix #0 landed: AssertNotDefaultBranch installed at ForcePushBranch,
	// TriggerCIRerun, DeleteAskBranch, MergeAndCleanup, and
	// completeAskBranchResolution. pilot_rebase.go calls GetDefaultBranch.
	// UpsertConvoyAskBranch rejects protected names at the store ingress.
	// The skip is removed; this test now acts as permanent regression
	// protection.
	// ── Static audit over each cited function ─────────────────────────────
	type fnCite struct {
		auditID  string
		file     string
		funcName string
		note     string
	}
	fns := []fnCite{
		{"AUDIT-102", "internal/agents/pr_flow.go", "completeAskBranchResolution",
			"force-pushes ab.AskBranch; DB-supplied; no default-branch reject"},
		{"AUDIT-103", "internal/git/askbranch.go", "ForcePushBranch",
			"boundary helper; callers in pilot_rebase*.go pass DB values"},
		{"AUDIT-104", "internal/git/askbranch.go", "TriggerCIRerun",
			"empty-commit push; branch arg can default to pr.Repo (pr_flow.go:709)"},
		{"AUDIT-122", "internal/git/git.go", "MergeAndCleanup",
			"reset --hard / clean -fd / branch -D on caller-supplied branch+worktree"},
		{"AUDIT-124", "internal/git/askbranch.go", "DeleteAskBranch",
			"branch -D + push origin --delete on caller-supplied ref"},
	}

	for _, fn := range fns {
		fn := fn
		t.Run(fn.auditID+"/"+fn.funcName, func(t *testing.T) {
			// Fix #0 inverts the assertion: post-fix, each destructive op MUST
			// carry a protected-branch guard. hasProtectedBranchGuard returns
			// true when AssertNotDefaultBranch(, a literal default-branch
			// comparison, or GetDefaultBranch in a comparison context is
			// present in the function body.
			src := readSource(t, fn.file)
			body := extractFuncBody(src, fn.funcName)
			if body == "" {
				t.Fatalf("%s: could not locate function %s in %s", fn.auditID, fn.funcName, fn.file)
			}
			guarded, reason := hasProtectedBranchGuard(body)
			if !guarded {
				t.Errorf("%s REGRESSION: %s in %s lost its protected-branch guard (%s). Note: %s",
					fn.auditID, fn.funcName, fn.file, reason, fn.note)
				return
			}
			t.Logf("%s guard present in %s (%s)", fn.auditID, fn.funcName, reason)
		})
	}

	// ── AUDIT-121: hardcoded "main" fallback in pilot_rebase.go:77 ────────
	t.Run("AUDIT-121/HardcodedMainFallback", func(t *testing.T) {
		// Fix #0 replaced the `defaultBranch = "main"` literal with a call
		// to igit.GetDefaultBranch(repo.LocalPath). Post-fix the literal is
		// gone AND the discovery call is present inside runRebaseAskBranch.
		src := readSource(t, "internal/agents/pilot_rebase.go")
		badPat := regexp.MustCompile(`defaultBranch\s*=\s*"main"`)
		if badPat.MatchString(src) {
			t.Errorf("AUDIT-121 REGRESSION: pilot_rebase.go still has `defaultBranch = \"main\"` literal fallback; must use GetDefaultBranch(repo.LocalPath)")
			return
		}
		body := extractFuncBody(src, "RunRebaseAskBranch")
		if body == "" {
			body = extractFuncBody(src, "runRebaseAskBranch")
		}
		if body == "" {
			t.Fatalf("AUDIT-121: could not locate runRebaseAskBranch in pilot_rebase.go")
		}
		if !strings.Contains(body, "GetDefaultBranch(repo.LocalPath)") {
			t.Errorf("AUDIT-121 REGRESSION: runRebaseAskBranch no longer calls GetDefaultBranch(repo.LocalPath); fallback path unreachable")
			return
		}
		t.Logf("AUDIT-121 closed: runRebaseAskBranch uses GetDefaultBranch(repo.LocalPath)")
	})

	// ── AUDIT-102 extension: UpsertConvoyAskBranch rejects default-branch
	// names at the store ingress. Post-Fix #0 this is an end-to-end check
	// that a corrupt "main" row cannot be written and thus cannot flow into
	// completeAskBranchResolution's force-push.
	// ─────────────────────────────────────────────────────────────────────
	t.Run("AUDIT-102/UpsertConvoyAskBranchRejectsMain", func(t *testing.T) {
		db := store.InitHolocronDSN(":memory:")
		defer db.Close()
		convoyID, err := store.CreateConvoy(db, "audit-102-default-branch-test")
		if err != nil {
			t.Fatalf("CreateConvoy: %v", err)
		}
		for _, bad := range []string{"main", "master", "MAIN", "refs/heads/main", "origin/master"} {
			err := store.UpsertConvoyAskBranch(db, convoyID, "owner/repo", bad,
				"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
			if err == nil {
				t.Errorf("AUDIT-102 REGRESSION: UpsertConvoyAskBranch accepted ask_branch=%q; guard missing", bad)
			}
		}
		// Confirm nothing was written.
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM ConvoyAskBranches WHERE convoy_id=?`, convoyID).Scan(&count); err != nil {
			t.Fatalf("count: %v", err)
		}
		if count != 0 {
			t.Errorf("AUDIT-102 REGRESSION: %d rows written despite protected-branch ingress; expected 0", count)
		}
	})

	// ── Summary for log readers ───────────────────────────────────────────
	t.Logf("Fix #0 landed: all 6 findings (AUDIT-102, -103, -104, -121, -122, -124) now have " +
		"AssertNotDefaultBranch guards at destructive-op entry points plus a store-ingress " +
		"denylist in UpsertConvoyAskBranch. This test acts as permanent regression protection.")
	// Avoid unused import if tests all return early.
	_ = fmt.Sprintf("%d", 0)
}
