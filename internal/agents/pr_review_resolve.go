package agents

import (
	"database/sql"

	"force-orchestrator/internal/store"
)

// ── pr-review-resolve sweep ─────────────────────────────────────────────────
//
// After a bot-comment in_scope_fix spawns a CodeEdit on the ask-branch, we
// classify the comment and post a "queued in task #N" reply immediately. But
// the GitHub review thread shouldn't be marked Resolved until the fix
// actually lands (Council approves + force-push happens). This sweep checks
// every tick for rows where:
//
//   classification = 'in_scope_fix'
//   spawned_task_id > 0 AND the spawned task is Completed
//   thread_resolved_at = ''
//
// …and calls gh.ResolveReviewThread via the GraphQL resolve mutation. The
// REST comment id → GraphQL thread node id lookup is a second gh call per
// row; we cache nothing — the sweep runs infrequently and batches are small.

func dogPRReviewResolve(db *sql.DB, logger interface{ Printf(string, ...any) }) error {
	// Honor the same kill switch as pr-review-poll.
	if store.GetConfig(db, "pr_review_enabled", "1") != "1" {
		return nil
	}
	rows := store.ListPendingThreadResolves(db)
	if len(rows) == 0 {
		return nil
	}
	ghc := newGHClient()
	for _, c := range rows {
		repoCfg := store.GetRepo(db, c.Repo)
		if repoCfg == nil || repoCfg.LocalPath == "" {
			continue
		}
		ghRepo := deriveGHRepoFromRemoteURL(repoCfg.RemoteURL)
		if ghRepo == "" {
			continue
		}
		// review_thread_id we stored is a synthetic ID ("review:<id>" or
		// "issue:<pr>"); we need the GraphQL node ID of the actual thread
		// before we can resolve. Issue-comment threads are not "review
		// threads" in the GraphQL sense and can't be resolved — skip them.
		if c.CommentType != "review_comment" {
			// Still stamp as resolved to drop it from the sweep; the fix
			// landed, there's nothing else to do.
			_ = store.MarkThreadResolved(db, c.ID)
			continue
		}
		nodeID, err := ghc.FindReviewThreadNodeID(repoCfg.LocalPath, ghRepo, c.DraftPRNumber, c.GitHubCommentID)
		if err != nil {
			logger.Printf("pr-review-resolve: lookup thread for comment #%d failed: %v", c.GitHubCommentID, err)
			continue
		}
		if nodeID == "" {
			// Thread not found (comment deleted? thread detached?). Stamp as
			// resolved so we don't re-check forever.
			_ = store.MarkThreadResolved(db, c.ID)
			continue
		}
		if err := ghc.ResolveReviewThread(repoCfg.LocalPath, nodeID); err != nil {
			logger.Printf("pr-review-resolve: resolve thread %s failed: %v", nodeID, err)
			continue
		}
		if err := store.MarkThreadResolved(db, c.ID); err != nil {
			logger.Printf("pr-review-resolve: mark resolved for row %d failed: %v", c.ID, err)
			continue
		}
		logger.Printf("pr-review-resolve: thread %s resolved (comment #%d, task #%d)",
			nodeID, c.GitHubCommentID, c.SpawnedTaskID)
	}
	return nil
}
