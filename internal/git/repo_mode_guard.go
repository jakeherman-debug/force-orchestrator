package git

import (
	"database/sql"
	"fmt"

	"force-orchestrator/internal/store"
)

// AssertRepoWritable returns store.ErrRepoNotWritable when the named repo's
// mode column is anything other than 'write'. Called as the SECOND check
// (after AssertNotDefaultBranch) in every destructive git op:
// ForcePushBranch, TriggerCIRerun, DeleteAskBranch, MergeAndCleanup, and
// completeAskBranchResolution. The order matters — protected-branch is the
// load-bearing safety guard and runs first; the mode check is the
// per-repo opt-in policy.
//
// Classification is permanent (not transient). A read_only or quarantined
// repo will not become writable mid-task; the calling agent should route
// to handleInfraFailure or escalate rather than retry.
//
// Defined in the git package (rather than alongside SetRepoMode in store)
// so the existing `igit.AssertNotDefaultBranch` callers have a sibling
// helper at the same import path. The actual mode read goes through
// store.GetRepoMode, which is the single source of truth for mode lookups.
//
// Test-mode escape hatch: when db is nil, the guard is a no-op. Existing
// git-level unit tests that exercise raw git mechanics (push, fetch,
// merge plumbing) without spinning up a Holocron pass nil db and a
// repoName they never registered. The integration tests in
// internal/agents/repo_mode_integration_test.go exercise the real
// production path with a non-nil db and a registered repo.
func AssertRepoWritable(db *sql.DB, repoName string) error {
	if db == nil {
		// No DB → no mode lookup possible. This is the test-mode path.
		// Production callers always pass a non-nil db.
		return nil
	}
	if repoName == "" {
		// Empty repo name with a non-nil db is a guard failure. The
		// destructive ops accept a repoPath, not a repo name, so an
		// empty repoName here means the caller had a db handy but
		// couldn't (or didn't bother to) name the registered repo.
		// Refusing surfaces the bug instead of silently allowing the op.
		return fmt.Errorf("%w: empty repoName", store.ErrRepoNotWritable)
	}
	mode, err := store.GetRepoMode(db, repoName)
	if err != nil {
		return fmt.Errorf("AssertRepoWritable(%q): %w", repoName, err)
	}
	if mode != store.ModeWrite {
		return fmt.Errorf("%w: repo %q mode=%s", store.ErrRepoNotWritable, repoName, mode)
	}
	return nil
}
