package git

// Audit Pattern P10 — "Shelling out to git/gh without validators."
//
// Branch names, ref strings, and repo paths flow from the DB directly into
// exec.Command("git", ...) calls with:
//   - no `git check-ref-format` validation at ingress,
//   - no `--` separator between flags and positional refs at the shell
//     boundary, and
//   - no regex on repo paths or remote URLs.
//
// The consequence is that the CVE-2017-1000117 class (ref starts with
// `--upload-pack=/…`) and the shell-argument-injection class (ref starts
// with `-rm`, `--delete`, etc.) are directly reachable from any code path
// that persists a branch name to BountyBoard.branch_name,
// Convoys.ask_branch, or ConvoyAskBranches.ask_branch.
//
// This file is the P10 pattern-proof test. It has two subtests:
//
//  1. TestPattern_P10_BranchValidatorsMissing — drives a corpus of
//     adversarial branch names through every store-level entry point that
//     accepts one (store.SetBranchName, store.SetBranchNameTx,
//     store.UpsertConvoyAskBranch, store.SetAskBranch) and asserts that the
//     call is REJECTED (returns an error, or stores empty/the prior value).
//     Today none of these paths validate — every call succeeds — so this
//     subtest is expected to FAIL until a validator is added.
//
//  2. TestPattern_P10_GitInvocationsLackDashDashSeparator — reads
//     internal/git/git.go and internal/git/askbranch.go as source text,
//     finds every exec.Command("git", ...) / exec.CommandContext(..., "git",
//     ...) invocation, and asserts that an `--` arg appears between the
//     leading flags/subcommand and the trailing positional ref arguments.
//     Today virtually none do — this subtest is expected to FAIL until the
//     shell boundary is hardened.

import (
	"database/sql"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// adversarialBranchNames is the P10 corpus. Each entry is a branch name that
// a correctly validating store boundary MUST reject. A few notes:
//
//   - "--upload-pack=/tmp/evil" is the canonical CVE-2017-1000117 payload.
//     When such a string reaches `git fetch origin <ref>` or `git ls-remote
//     origin <ref>`, git re-parses it as a flag and executes the attacker-
//     controlled upload-pack binary. (See
//     https://git-scm.com/docs/git-check-ref-format and Junio Hamano's 2017
//     advisory.)
//   - "-rm -rf /" exercises the broader flag-injection class: any leading
//     hyphen lets git/gh re-interpret the ref as a flag.
//   - ".." and "refs/../evil" test the path-traversal / refname-normalization
//     rules from git-check-ref-format(1).
//   - "foo\x00bar" embeds a NUL byte; `check-ref-format` rejects this but
//     database columns and Go strings happily store it.
//   - "@{" and ".lock" are explicitly forbidden by refname grammar.
//   - "" (empty) is trivially invalid and also bypasses idempotence checks
//     downstream that key on branch_name != ''.
var adversarialBranchNames = []struct {
	name string
	why  string
}{
	{"--upload-pack=/tmp/evil", "CVE-2017-1000117: leading double-dash → git interprets as flag"},
	{"-rm -rf /", "leading hyphen → flag injection class"},
	{"..", "refname grammar: '..' is forbidden (check-ref-format(1))"},
	{"refs/../evil", "path traversal via refname"},
	{"foo\x00bar", "embedded NUL byte"},
	{"@{", "refname grammar: '@{' is forbidden"},
	{"hotfix.lock", "refname grammar: cannot end in .lock"},
	{"", "empty refname"},
	{"branch with spaces", "refname grammar: ASCII space forbidden"},
	{"branch~tilde", "refname grammar: '~' forbidden"},
	{"branch^caret", "refname grammar: '^' forbidden"},
	{"branch:colon", "refname grammar: ':' forbidden"},
	{"branch?q", "refname grammar: '?' forbidden"},
	{"branch*star", "refname grammar: '*' forbidden"},
	{"branch[bracket", "refname grammar: '[' forbidden"},
	{"branch\\back", "refname grammar: '\\' forbidden"},
	{"/leading-slash", "refname grammar: leading '/' forbidden"},
	{"trailing-slash/", "refname grammar: trailing '/' forbidden"},
	{"double//slash", "refname grammar: consecutive '/' forbidden"},
}

// isRejected reports true iff the given stored value represents a refusal of
// the adversarial input. A correctly validating store either (a) returns a
// non-nil error from the setter, or (b) silently normalizes/drops the value
// to the empty string. Anything else — i.e. the adversarial string is
// persisted verbatim — is a P10 violation.
func isRejected(stored string, setterErr error, adversarial string) bool {
	if setterErr != nil {
		return true
	}
	if stored == "" && adversarial != "" {
		return true
	}
	if stored != adversarial {
		// The store returned a sanitized/different value — acceptable as a
		// defence, though a hard error is preferable.
		return true
	}
	return false
}

// seedBounty inserts a Pending CodeEdit row so SetBranchName/Tx have a target.
func seedBounty(t *testing.T, db *sql.DB) int {
	t.Helper()
	return store.AddBounty(db, 0, "CodeEdit", "p10-seed-payload")
}

// seedConvoy inserts a convoy so UpsertConvoyAskBranch has a FK target.
func seedConvoy(t *testing.T, db *sql.DB) int {
	t.Helper()
	id, err := store.CreateConvoy(db, "p10-adversarial-convoy")
	if err != nil {
		t.Fatalf("CreateConvoy: %v", err)
	}
	return id
}

// TestPattern_P10_BranchValidatorsMissing is the empirical proof that no
// validator guards the store-level ingress points for branch/ref names.
//
// For each adversarial input we drive:
//   - store.SetBranchName(db, taskID, adversarial)
//   - store.SetBranchNameTx(tx, taskID, adversarial)
//   - store.UpsertConvoyAskBranch(db, convoyID, repo, adversarial, baseSHA)
//
// and then read the persisted value back. If the store is P10-correct, the
// adversarial string never lands in the column. Today it lands verbatim in
// every case — this subtest is expected to FAIL until the fix ships.
func TestPattern_P10_BranchValidatorsMissing(t *testing.T) {
	t.Skip("AUDIT-018/019/049/050/051/052/098/099/140/153/154: remove when validRef/validRepoPath/validRemoteURL land at store ingress + `--` separator inserted (Fix #9)")
	// Without skip, fails with:
	//   audit_pattern_p10_test.go:154: P10 VIOLATION: SetBranchName accepted adversarial ref "--upload-pack=/tmp/evil" (CVE-2017-1000117: leading double-dash → git interprets as flag); stored verbatim
	//   audit_pattern_p10_test.go:173: P10 VIOLATION: SetBranchNameTx accepted adversarial ref "--upload-pack=/tmp/evil" (CVE-2017-1000117: leading double-dash → git interprets as flag); stored verbatim (setErr=<nil>)
	//   audit_pattern_p10_test.go:189: P10 VIOLATION: UpsertConvoyAskBranch accepted adversarial ref "--upload-pack=/tmp/evil" (CVE-2017-1000117: leading double-dash → git interprets as flag); stored verbatim (err=<nil>)
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := seedConvoy(t, db)

	for _, tc := range adversarialBranchNames {
		tc := tc
		t.Run("SetBranchName/"+safeLabel(tc.name), func(t *testing.T) {
			taskID := seedBounty(t, db)
			store.SetBranchName(db, taskID, tc.name)
			var got string
			if err := db.QueryRow(`SELECT IFNULL(branch_name,'') FROM BountyBoard WHERE id=?`, taskID).Scan(&got); err != nil {
				t.Fatalf("read back: %v", err)
			}
			if !isRejected(got, nil, tc.name) {
				t.Errorf("P10 VIOLATION: SetBranchName accepted adversarial ref %q (%s); stored verbatim", tc.name, tc.why)
			}
		})

		t.Run("SetBranchNameTx/"+safeLabel(tc.name), func(t *testing.T) {
			taskID := seedBounty(t, db)
			tx, err := db.Begin()
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			setErr := store.SetBranchNameTx(tx, taskID, tc.name)
			if err := tx.Commit(); err != nil {
				t.Fatalf("commit: %v", err)
			}
			var got string
			if err := db.QueryRow(`SELECT IFNULL(branch_name,'') FROM BountyBoard WHERE id=?`, taskID).Scan(&got); err != nil {
				t.Fatalf("read back: %v", err)
			}
			if !isRejected(got, setErr, tc.name) {
				t.Errorf("P10 VIOLATION: SetBranchNameTx accepted adversarial ref %q (%s); stored verbatim (setErr=%v)", tc.name, tc.why, setErr)
			}
		})

		t.Run("UpsertConvoyAskBranch/"+safeLabel(tc.name), func(t *testing.T) {
			// Use a fresh repo label per subtest so the (convoy_id, repo)
			// unique index doesn't cause prior-test collisions to mask the
			// adversarial input as "already upserted".
			repo := "p10-repo-" + safeLabel(tc.name)
			err := store.UpsertConvoyAskBranch(db, convoyID, repo, tc.name, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
			// Even if err != nil (e.g. empty-string path already guards), we
			// also read back to make sure no row slipped through.
			var got string
			row := db.QueryRow(`SELECT IFNULL(ask_branch,'') FROM ConvoyAskBranches WHERE convoy_id=? AND repo=?`, convoyID, repo)
			_ = row.Scan(&got)
			if !isRejected(got, err, tc.name) {
				t.Errorf("P10 VIOLATION: UpsertConvoyAskBranch accepted adversarial ref %q (%s); stored verbatim (err=%v)", tc.name, tc.why, err)
			}
		})
	}
}

// safeLabel turns an adversarial branch name into something usable as a
// subtest name / repo key — Go's testing framework does not love NUL bytes
// or slashes in t.Run labels, and SQLite doesn't love NUL bytes in repo
// columns.
func safeLabel(s string) string {
	if s == "" {
		return "empty"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" {
		return "sym"
	}
	if len(out) > 48 {
		out = out[:48]
	}
	return out
}

// TestPattern_P10_GitInvocationsLackDashDashSeparator reads the source of
// internal/git/git.go and internal/git/askbranch.go, finds every
// exec.Command("git", ...) / exec.CommandContext(..., "git", ...) call, and
// asserts that an `--` separator appears between the flags/subcommand and
// the positional ref args.
//
// The check is deliberately conservative: if the call has NO `--` anywhere
// in its arg list AND it passes any arg that could plausibly be a ref (a
// string literal that looks like a branch/SHA, OR a Go identifier like
// `base`, `branch`, `branchName`, `askBranch`, `onto`, `existingBranch`,
// `newBranch`, `conflictBranch`, `baseRef`, `remoteRef`, `baseSHA`), we
// flag it. Today virtually every invocation fails this check.
func TestPattern_P10_GitInvocationsLackDashDashSeparator(t *testing.T) {
	t.Skip("AUDIT-018/019/049/050/051/052/098/099/140/153/154: remove when validRef/validRepoPath/validRemoteURL land at store ingress + `--` separator inserted (Fix #9)")
	// Without skip, fails with:
	//   audit_pattern_p10_test.go:308: P10 VIOLATION: git.go:20 lacks `--` separator before ref arg:
	//       exec.Command("git", "-C", repoPath, "symbolic-ref", "refs/remotes/origin/HEAD", "--short")
	//   audit_pattern_p10_test.go:308: P10 VIOLATION: git.go:29 lacks `--` separator before ref arg:
	//       exec.Command("git", "-C", repoPath, "rev-parse", "--verify", branch)
	files := []string{
		mustAbs(t, "git.go"),
		mustAbs(t, "askbranch.go"),
	}

	// Matches an entire exec.Command / exec.CommandContext invocation, from
	// the opening paren through the matching close paren on a single logical
	// line. We build this with a non-greedy body and anchor on the
	// .CombinedOutput()/.Output()/.Run() that always follows — or a newline
	// that ends the statement.
	callRe := regexp.MustCompile(`exec\.Command(?:Context)?\([^)]*\)`)

	// Ref-ish identifiers / literals that, if present in an invocation
	// without a preceding `--`, indicate the positional slot is a ref.
	refishTokens := []string{
		"base", "branch", "branchName", "askBranch", "onto",
		"existingBranch", "newBranch", "conflictBranch",
		"baseRef", "remoteRef", "baseSHA", "treeSHA",
		"origin/", "refs/remotes/origin/", "refs/heads/",
		"HEAD", "--abort", // abort is a flag-subcmd; safe to allow, but leave
	}

	violations := 0
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}

		// Strip // line comments so they don't produce false positives.
		lines := strings.Split(string(src), "\n")
		for lineIdx, line := range lines {
			// Trim trailing // comment.
			if i := strings.Index(line, "//"); i >= 0 {
				// don't strip inside strings; cheap approximation is fine for
				// our in-repo source.
				if !strings.Contains(line[:i], `"`) || strings.Count(line[:i], `"`)%2 == 0 {
					line = line[:i]
				}
			}

			for _, m := range callRe.FindAllString(line, -1) {
				// Skip invocations that aren't spawning `git` (we don't care
				// about `go`, `gh`, etc., for this audit).
				if !strings.Contains(m, `"git"`) {
					continue
				}

				// If the invocation already has `--` somewhere, good.
				if strings.Contains(m, `"--"`) {
					continue
				}

				// Whitelist a few known-safe read-only shapes that have NO
				// ref-valued positional args (e.g. `git worktree list
				// --porcelain`, `git status --porcelain`). We detect these by
				// absence of any ref-ish token.
				hasRefish := false
				for _, tok := range refishTokens {
					if tok == "--abort" {
						continue // don't count flags
					}
					if strings.Contains(m, tok) {
						hasRefish = true
						break
					}
				}
				if !hasRefish {
					continue
				}

				violations++
				t.Errorf("P10 VIOLATION: %s:%d lacks `--` separator before ref arg:\n  %s",
					filepath.Base(f), lineIdx+1, strings.TrimSpace(m))
			}
		}
	}

	if violations == 0 {
		// If this ever passes, the P10 shell boundary has been hardened.
		// Flip the surrounding TestPattern_P10_* into a regression guard.
		t.Log("P10 shell boundary now appears safe — convert this subtest into a regression guard.")
	}
}

// mustAbs resolves a filename in the internal/git package's own directory.
// The test runs from that directory, so relative is fine, but we make the
// path explicit for clarity when failures surface.
func mustAbs(t *testing.T, name string) string {
	t.Helper()
	p, err := filepath.Abs(name)
	if err != nil {
		t.Fatalf("abs(%s): %v", name, err)
	}
	return p
}
