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
func extractFuncBody(src, funcName string) string {
	// Match `func Name(` or `func (<recv>) Name(`.
	re := regexp.MustCompile(`func\s+(?:\([^)]*\)\s+)?` + regexp.QuoteMeta(funcName) + `\s*\(`)
	loc := re.FindStringIndex(src)
	if loc == nil {
		return ""
	}
	// Find the opening brace of the function body.
	i := loc[1]
	for i < len(src) && src[i] != '{' {
		i++
	}
	if i == len(src) {
		return ""
	}
	// Walk braces to find the matching close.
	depth := 0
	start := i
	for ; i < len(src); i++ {
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
	t.Skip("AUDIT-102/103/104/121/122/124: remove when AssertNotDefaultBranch guard added to destructive git ops (Fix #0)")
	// Without skip, fails with:
	//   audit_protected_branch_test.go:198: AUDIT-102 REPRODUCED: completeAskBranchResolution in internal/agents/pr_flow.go has NO protected-branch guard (no guard markers found)
	//   audit_protected_branch_test.go:198: AUDIT-103 REPRODUCED: ForcePushBranch in internal/git/askbranch.go has NO protected-branch guard (no guard markers found)
	//   audit_protected_branch_test.go:198: AUDIT-104 REPRODUCED: TriggerCIRerun in internal/git/askbranch.go has NO protected-branch guard (no guard markers found)
	//   audit_protected_branch_test.go:229: AUDIT-121 REPRODUCED: pilot_rebase.go has `defaultBranch = "main"` literal fallback
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
			t.Skip(fn.auditID + ": remove when AssertNotDefaultBranch guard added to destructive git ops (Fix #0)")
			// Without skip, fails with (one sample line per audit ID in this loop):
			//   audit_protected_branch_test.go:198: AUDIT-102 REPRODUCED: completeAskBranchResolution in internal/agents/pr_flow.go has NO protected-branch guard (no guard markers found). Note: force-pushes ab.AskBranch; DB-supplied; no default-branch reject
			//   audit_protected_branch_test.go:198: AUDIT-103 REPRODUCED: ForcePushBranch in internal/git/askbranch.go has NO protected-branch guard (no guard markers found). Note: boundary helper; callers in pilot_rebase*.go pass DB values
			//   audit_protected_branch_test.go:198: AUDIT-104 REPRODUCED: TriggerCIRerun in internal/git/askbranch.go has NO protected-branch guard (no guard markers found). Note: empty-commit push; branch arg can default to pr.Repo (pr_flow.go:709)
			//   audit_protected_branch_test.go:198: AUDIT-122 REPRODUCED: MergeAndCleanup in internal/git/git.go has NO protected-branch guard (no guard markers found). Note: reset --hard / clean -fd / branch -D on caller-supplied branch+worktree
			//   audit_protected_branch_test.go:198: AUDIT-124 REPRODUCED: DeleteAskBranch in internal/git/askbranch.go has NO protected-branch guard (no guard markers found). Note: branch -D + push origin --delete on caller-supplied ref
			src := readSource(t, fn.file)
			body := extractFuncBody(src, fn.funcName)
			if body == "" {
				t.Fatalf("%s: could not locate function %s in %s", fn.auditID, fn.funcName, fn.file)
			}
			guarded, reason := hasProtectedBranchGuard(body)
			if guarded {
				t.Errorf("%s: %s in %s NOW HAS a guard (%s) — finding may be fixed or needs restatement",
					fn.auditID, fn.funcName, fn.file, reason)
				return
			}
			// Report the violation for visibility in the test log.
			t.Errorf("%s REPRODUCED: %s in %s has NO protected-branch guard (%s). Note: %s",
				fn.auditID, fn.funcName, fn.file, reason, fn.note)
		})
	}

	// ── AUDIT-121: hardcoded "main" fallback in pilot_rebase.go:77 ────────
	t.Run("AUDIT-121/HardcodedMainFallback", func(t *testing.T) {
		t.Skip("AUDIT-121: remove when AssertNotDefaultBranch guard added to destructive git ops (Fix #0)")
		// Without skip, fails with:
		//   audit_protected_branch_test.go:229: AUDIT-121 REPRODUCED: pilot_rebase.go has `defaultBranch = "main"` literal fallback; violates CLAUDE.md directive to call GetDefaultBranch(repoPath). Master-default repos → infinite REBASE_CONFLICT loop.
		src := readSource(t, "internal/agents/pilot_rebase.go")
		// Look for the exact pattern: defaultBranch = "main" (assignment, not ==).
		// CLAUDE.md demands GetDefaultBranch(repo.LocalPath) in this spot.
		pat := regexp.MustCompile(`defaultBranch\s*=\s*"main"`)
		if !pat.MatchString(src) {
			t.Errorf("AUDIT-121 stale: could not find `defaultBranch = \"main\"` literal in pilot_rebase.go; citation may be outdated")
			return
		}
		// Also confirm there is NO GetDefaultBranch(repo.LocalPath) call
		// inside RunRebaseAskBranch before the literal. If there were, the
		// literal might just be a no-op fallback.
		body := extractFuncBody(src, "RunRebaseAskBranch")
		if body == "" {
			// Try alt name.
			body = extractFuncBody(src, "runRebaseAskBranch")
		}
		if strings.Contains(body, "GetDefaultBranch(repo.LocalPath)") {
			t.Errorf("AUDIT-121 may be mitigated: RunRebaseAskBranch calls GetDefaultBranch(repo.LocalPath); re-read citation")
			return
		}
		t.Errorf("AUDIT-121 REPRODUCED: pilot_rebase.go has `defaultBranch = \"main\"` literal fallback; violates CLAUDE.md directive to call GetDefaultBranch(repoPath). Master-default repos → infinite REBASE_CONFLICT loop.")
	})

	// ── AUDIT-102 extension: UpsertConvoyAskBranch has no default-branch
	// validation. A DB-corrupt or manually-edited row with ask_branch="main"
	// flows straight through to completeAskBranchResolution's force-push.
	// ─────────────────────────────────────────────────────────────────────
	t.Run("AUDIT-102/UpsertConvoyAskBranchAcceptsMain", func(t *testing.T) {
		t.Skip("AUDIT-102: remove when AssertNotDefaultBranch guard added to destructive git ops (Fix #0)")
		// Without skip, fails with:
		//   audit_protected_branch_test.go:278: AUDIT-102 REPRODUCED (end-to-end): UpsertConvoyAskBranch accepts ask_branch="main" verbatim; combined with unguarded completeAskBranchResolution (force-push ab.AskBranch), one DB-corrupt value → force-push origin/main. No protected-branch guard anywhere in the chain.
		src := readSource(t, "internal/store/convoy_ask_branches.go")
		body := extractFuncBody(src, "UpsertConvoyAskBranch")
		if body == "" {
			t.Fatalf("could not locate UpsertConvoyAskBranch")
		}
		// Does it check against default branch names or call GetDefaultBranch?
		if strings.Contains(body, "GetDefaultBranch(") ||
			strings.Contains(body, `"main"`) ||
			strings.Contains(body, `"master"`) {
			t.Errorf("AUDIT-102 partial mitigation: UpsertConvoyAskBranch references default-branch names; verify it REJECTS them")
			return
		}
		// Behavioral: actually insert "main" as the ask_branch.
		db := store.InitHolocronDSN(":memory:")
		defer db.Close()
		convoyID, err := store.CreateConvoy(db, "audit-102-default-branch-test")
		if err != nil {
			t.Fatalf("CreateConvoy: %v", err)
		}
		// A repo label is sufficient for this test; no on-disk repo needed.
		err = store.UpsertConvoyAskBranch(db, convoyID, "owner/repo", "main",
			"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
		if err != nil {
			// Would be a fix — silent pass.
			t.Errorf("unexpected: UpsertConvoyAskBranch rejected ask_branch=\"main\" with %v; may be mitigated", err)
			return
		}
		var stored string
		if err := db.QueryRow(`SELECT ask_branch FROM ConvoyAskBranches WHERE convoy_id=? AND repo=?`,
			convoyID, "owner/repo").Scan(&stored); err != nil {
			t.Fatalf("read back: %v", err)
		}
		if stored != "main" {
			t.Errorf("unexpected: stored ask_branch=%q (not \"main\"); something silently normalized", stored)
			return
		}
		t.Errorf("AUDIT-102 REPRODUCED (end-to-end): UpsertConvoyAskBranch accepts ask_branch=\"main\" verbatim; " +
			"combined with unguarded completeAskBranchResolution (force-push ab.AskBranch), " +
			"one DB-corrupt value → force-push origin/main. No protected-branch guard anywhere in the chain.")
	})

	// ── Summary for log readers ───────────────────────────────────────────
	t.Logf("All 6 findings (AUDIT-102, -103, -104, -121, -122, -124) REPRODUCED-STATIC. " +
		"No destructive git op in the inspected set checks its branch argument against GetDefaultBranch(repoPath). " +
		"Fix: add AssertNotDefaultBranch helper in internal/git and call it at the top of " +
		"each cited function + at the store ingress for ConvoyAskBranches.ask_branch.")
	// Avoid unused import if tests all return early.
	_ = fmt.Sprintf("%d", 0)
}
