package agents

import (
	"database/sql"
	"fmt"
	"os/exec"
	"strings"

	"force-orchestrator/internal/gh"
	"force-orchestrator/internal/store"
)

// ── PR-flow preflight & Layer B backfill ────────────────────────────────────
//
// Before the daemon spawns any agents, we must ensure the environment is sane
// and every registered repo has remote_url + default_branch populated. This
// prevents the fleet from quietly falling back to the legacy local-merge path
// for repos we think we migrated.
//
// Each check returns a structured result so the CLI (`force migrate pr-flow`)
// can surface failures without duplicating logic. The checks never mutate
// external state — they only read git/gh — so tests can drive them against
// fixture repos without risk.

// PreflightCheck is a single validation outcome.
type PreflightCheck struct {
	Name    string // short machine-readable identifier (e.g. "gh-auth")
	Passed  bool
	Detail  string // one-line explanation shown to operators
	Fatal   bool   // true = abort daemon start; false = mark repo pr_flow_enabled=0 and continue
	RepoKey string // non-empty for per-repo checks; empty for global checks like gh-auth
}

// PRFlowPreflight runs all required checks and returns their outcomes. The
// caller decides whether to abort on fatal failures. ghClient allows tests to
// inject a stub; pass gh.NewClient() in production.
//
// Contract (must hold before the daemon spawns agents):
//
//  1. `gh auth status` succeeds → Fatal on failure.
//  2. Every registered repo has a reachable `origin` remote → NOT fatal; each
//     failing repo is marked pr_flow_enabled=0 by the caller so new tasks in
//     that repo fall back to the legacy local-merge path.
//
// Tests do not run gh.NewClient() because it would shell out to the real gh
// binary; use NewClientWithRunner with a stub Runner.
func PRFlowPreflight(db *sql.DB, ghClient *gh.Client) []PreflightCheck {
	var results []PreflightCheck

	// Check 1: gh auth status.
	authOK, detail, _ := ghClient.AuthStatus()
	results = append(results, PreflightCheck{
		Name:   "gh-auth",
		Passed: authOK,
		Detail: firstLine(detail),
		Fatal:  !authOK,
	})

	// Check 2: each registered repo has a reachable origin remote.
	for _, repo := range store.ListRepos(db) {
		res := PreflightCheck{Name: "repo-origin", RepoKey: repo.Name}
		if repo.LocalPath == "" {
			res.Passed = false
			res.Detail = "no local_path recorded"
			results = append(results, res)
			continue
		}
		remote, err := repoRemoteURL(repo.LocalPath)
		if err != nil {
			res.Passed = false
			res.Detail = fmt.Sprintf("git remote get-url origin: %v", err)
			results = append(results, res)
			continue
		}
		res.Passed = true
		res.Detail = remote
		results = append(results, res)
	}

	return results
}

// BackfillRepoRemoteInfo runs Layer B: for every repo where remote_url or
// default_branch is empty, shell out to git and populate the columns. Repos
// whose origin is unreachable are marked pr_flow_enabled=0 so they fall back
// to the legacy path — the daemon doesn't refuse to start over a missing repo.
//
// Returns a human-readable summary for logging.
func BackfillRepoRemoteInfo(db *sql.DB) string {
	var (
		populated, disabled int
		disabledNames       []string
	)
	for _, repo := range store.ListRepos(db) {
		if repo.RemoteURL != "" && repo.DefaultBranch != "" {
			continue // already populated
		}
		if repo.LocalPath == "" {
			_ = store.SetRepoPRFlowEnabled(db, repo.Name, false)
			disabled++
			disabledNames = append(disabledNames, repo.Name+" (no local_path)")
			continue
		}
		remote, rErr := repoRemoteURL(repo.LocalPath)
		if rErr != nil {
			_ = store.SetRepoPRFlowEnabled(db, repo.Name, false)
			disabled++
			disabledNames = append(disabledNames, fmt.Sprintf("%s (remote error: %v)", repo.Name, rErr))
			continue
		}
		branch := repoDefaultBranch(repo.LocalPath)
		if branch == "" {
			// git commands all failed — can't detect default branch. Disable flow.
			_ = store.SetRepoPRFlowEnabled(db, repo.Name, false)
			disabled++
			disabledNames = append(disabledNames, repo.Name+" (default branch undetectable)")
			continue
		}
		if setErr := store.SetRepoRemoteInfo(db, repo.Name, remote, branch); setErr != nil {
			_ = store.SetRepoPRFlowEnabled(db, repo.Name, false)
			disabled++
			disabledNames = append(disabledNames, fmt.Sprintf("%s (store error: %v)", repo.Name, setErr))
			continue
		}
		populated++
	}
	summary := fmt.Sprintf("backfilled %d repo(s)", populated)
	if disabled > 0 {
		summary += fmt.Sprintf("; disabled pr_flow for %d repo(s): %s",
			disabled, strings.Join(disabledNames, ", "))
	}
	return summary
}

// EnqueueMissingFindPRTemplate queues a FindPRTemplate task for every repo that
// has pr_flow_enabled=1 and an empty pr_template_path. Safe to call every
// startup — the WHERE clause means repos whose path was already discovered
// stay idle. Returns (queued, skipped) counts.
func EnqueueMissingFindPRTemplate(db *sql.DB) (queued, skipped int) {
	for _, repo := range store.ListRepos(db) {
		if !repo.PRFlowEnabled {
			skipped++
			continue
		}
		if repo.PRTemplatePath != "" {
			skipped++
			continue
		}
		if repo.LocalPath == "" {
			skipped++
			continue
		}
		// Avoid enqueueing a duplicate if one is already pending or in-flight.
		var existing int
		db.QueryRow(`SELECT COUNT(*) FROM BountyBoard
			WHERE type = 'FindPRTemplate' AND target_repo = ?
			  AND status IN ('Pending', 'Locked', 'Classifying')`, repo.Name).Scan(&existing)
		if existing > 0 {
			skipped++
			continue
		}
		if _, err := QueueFindPRTemplate(db, repo.Name, repo.LocalPath); err == nil {
			queued++
		} else {
			skipped++
		}
	}
	return
}

// ── helpers ──────────────────────────────────────────────────────────────────

// repoRemoteURL returns the origin URL of a local repo, or error.
func repoRemoteURL(localPath string) (string, error) {
	cmd := exec.Command("git", "-C", localPath, "remote", "get-url", "origin")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	url := strings.TrimSpace(string(out))
	if url == "" {
		return "", fmt.Errorf("empty origin URL")
	}
	return url, nil
}

// repoDefaultBranch returns the default branch name, or "" if undetectable.
// Mirrors internal/git.GetDefaultBranch but does NOT fall back to "main" so
// callers can tell genuine detection failure from a real "main" result.
func repoDefaultBranch(localPath string) string {
	if out, err := exec.Command("git", "-C", localPath, "symbolic-ref", "refs/remotes/origin/HEAD", "--short").CombinedOutput(); err == nil {
		parts := strings.SplitN(strings.TrimSpace(string(out)), "/", 2)
		if len(parts) == 2 && parts[1] != "" {
			return parts[1]
		}
	}
	for _, branch := range []string{"main", "master", "develop"} {
		if exec.Command("git", "-C", localPath, "rev-parse", "--verify", branch).Run() == nil {
			return branch
		}
	}
	return ""
}

// firstLine returns the first non-empty line of s, trimmed.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
