package agents

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"force-orchestrator/internal/gh"
	"force-orchestrator/internal/store"
)

// ── pr-review-poll dog ──────────────────────────────────────────────────────
//
// For each DraftPROpen convoy, fetches inline review comments + PR-level
// comments from GitHub and inserts any new ones into PRReviewComments. When a
// convoy has at least one unclassified row, queues exactly one PRReviewTriage
// task for Diplomat to process. Runs on the inquisitor cadence (5 min).
//
// Author classification (bot vs human) happens at insert time using
// gh.IsBotAuthor(login, userType, allowlist) where allowlist = SystemConfig
// key "pr_review_bot_logins" (CSV) or gh.DefaultBotLogins() if unset.
//
// Comments from logins in SystemConfig "pr_review_skip_logins" (CSV) are
// silently ignored before insertion — set this to the operator's GitHub
// username to prevent the fleet from triaging the operator's own replies.
//
// Thread grouping: GitHub's REST review comments carry in_reply_to_id. Rather
// than fan out to GraphQL at poll time we derive review_thread_id as:
//   - review_comment with in_reply_to_id = 0  → "review:<github_comment_id>"
//   - review_comment with in_reply_to_id > 0  → copy parent row's review_thread_id
//   - issue_comment (PR-level discussion)     → "issue:<draft_pr_number>"
// Thread resolution (via GraphQL) happens in a separate sweep only when a fix
// actually lands (see pr_review_resolve.go).

// dogPRReviewPoll is the per-cycle job that fetches and records PR review
// comments for every DraftPROpen convoy.
func dogPRReviewPoll(db *sql.DB, logger interface{ Printf(string, ...any) }) error {
	// Global kill switch.
	if store.GetConfig(db, "pr_review_enabled", "1") != "1" {
		return nil
	}

	allowlist := loadBotAllowlist(db)
	skipLogins := loadSkipLogins(db)

	rows, err := db.Query(`SELECT id, name FROM Convoys WHERE status = 'DraftPROpen'`)
	if err != nil {
		return err
	}
	type convoyRef struct {
		id   int
		name string
	}
	var convoys []convoyRef
	for rows.Next() {
		var c convoyRef
		rows.Scan(&c.id, &c.name)
		convoys = append(convoys, c)
	}
	rows.Close()
	if len(convoys) == 0 {
		return nil
	}

	ghc := newGHClient()
	for _, c := range convoys {
		pollConvoyPRReviews(db, ghc, c.id, allowlist, skipLogins, logger)
	}
	return nil
}

// pollConvoyPRReviews fetches comments for every per-repo draft PR in one
// convoy and records new ones. Split out so tests can call it directly with
// a stub gh client.
func pollConvoyPRReviews(
	db *sql.DB,
	ghc *gh.Client,
	convoyID int,
	allowlist []string,
	skipLogins []string,
	logger interface{ Printf(string, ...any) },
) {
	branches := store.ListConvoyAskBranches(db, convoyID)
	if len(branches) == 0 {
		return
	}

	var newRowCount int
	for _, ab := range branches {
		if ab.DraftPRNumber == 0 || ab.DraftPRState != "Open" {
			continue
		}
		repo := store.GetRepo(db, ab.Repo)
		if repo == nil || repo.LocalPath == "" {
			continue
		}
		// Per-repo kill switch (Repositories.pr_review_enabled, default 1).
		var enabled int
		db.QueryRow(`SELECT IFNULL(pr_review_enabled, 1) FROM Repositories WHERE name = ?`, ab.Repo).Scan(&enabled)
		if enabled == 0 {
			continue
		}
		ghRepo := deriveGHRepoFromRemoteURL(repo.RemoteURL)
		if ghRepo == "" {
			// Can't fetch without a resolvable repo slug.
			continue
		}

		issueComments, issErr := ghc.PRIssueComments(repo.LocalPath, ghRepo, ab.DraftPRNumber)
		if issErr != nil {
			logger.Printf("pr-review-poll: issue comments #%d %s failed: %v", ab.DraftPRNumber, ab.Repo, issErr)
			// Continue to review comments — one endpoint failing shouldn't blind us to the other.
		}
		reviewComments, revErr := ghc.PRReviewComments(repo.LocalPath, ghRepo, ab.DraftPRNumber)
		if revErr != nil {
			logger.Printf("pr-review-poll: review comments #%d %s failed: %v", ab.DraftPRNumber, ab.Repo, revErr)
		}

		// Issue comments: synthesize one thread per PR.
		issueThreadID := fmt.Sprintf("issue:%d", ab.DraftPRNumber)
		for _, ic := range issueComments {
			if isSkippedLogin(ic.User.Login, skipLogins) {
				continue
			}
			kind := "human"
			if gh.IsBotAuthor(ic.User.Login, ic.User.Type, allowlist) {
				kind = "bot"
			}
			row := store.PRReviewComment{
				ConvoyID:        convoyID,
				Repo:            ab.Repo,
				DraftPRNumber:   ab.DraftPRNumber,
				GitHubCommentID: ic.ID,
				CommentType:     "issue_comment",
				Author:          ic.User.Login,
				AuthorKind:      kind,
				Body:            ic.Body,
				ReviewThreadID:  issueThreadID,
				ThreadDepth:     store.MaxThreadDepth(db, convoyID, issueThreadID),
			}
			id, err := store.RecordPRComment(db, row)
			if err != nil {
				logger.Printf("pr-review-poll: record issue comment %d failed: %v", ic.ID, err)
				continue
			}
			// id != 0 even for dedup (returns existing); check by reading pre-insert row count
			// elsewhere. For logging purposes we just count every successful call.
			_ = id
			newRowCount++
		}

		// Review comments: thread = inherit parent's thread or synthesize "review:<self_id>".
		for _, rc := range reviewComments {
			if isSkippedLogin(rc.User.Login, skipLogins) {
				continue
			}
			threadID := fmt.Sprintf("review:%d", rc.ID)
			if rc.InReplyToID > 0 {
				// Inherit parent's review_thread_id if we have the parent.
				var parentThreadID string
				db.QueryRow(
					`SELECT IFNULL(review_thread_id, '') FROM PRReviewComments
					 WHERE repo = ? AND draft_pr_number = ? AND github_comment_id = ?`,
					ab.Repo, ab.DraftPRNumber, rc.InReplyToID,
				).Scan(&parentThreadID)
				if parentThreadID != "" {
					threadID = parentThreadID
				} else {
					// Parent not seen yet (first poll sees a reply but not the top). Fall back
					// to "review:<in_reply_to_id>" so when the parent later lands it naturally
					// shares the same thread id.
					threadID = fmt.Sprintf("review:%d", rc.InReplyToID)
				}
			}
			kind := "human"
			if gh.IsBotAuthor(rc.User.Login, rc.User.Type, allowlist) {
				kind = "bot"
			}
			row := store.PRReviewComment{
				ConvoyID:           convoyID,
				Repo:               ab.Repo,
				DraftPRNumber:      ab.DraftPRNumber,
				GitHubCommentID:    rc.ID,
				CommentType:        "review_comment",
				Author:             rc.User.Login,
				AuthorKind:         kind,
				Body:               rc.Body,
				Path:               rc.Path,
				Line:               rc.Line,
				DiffHunk:           rc.DiffHunk,
				ReviewThreadID:     threadID,
				InReplyToCommentID: rc.InReplyToID,
				ThreadDepth:        store.MaxThreadDepth(db, convoyID, threadID),
			}
			id, err := store.RecordPRComment(db, row)
			if err != nil {
				logger.Printf("pr-review-poll: record review comment %d failed: %v", rc.ID, err)
				continue
			}
			_ = id
			newRowCount++
		}
	}

	// Queue a PRReviewTriage task if there's any unclassified comment in this
	// convoy and no existing triage task is already Pending/Locked.
	if hasUnclassifiedPRComments(db, convoyID) {
		if err := queuePRReviewTriageIfAbsent(db, convoyID, logger); err != nil {
			logger.Printf("pr-review-poll: convoy %d triage queue failed: %v", convoyID, err)
		}
	}
}

// hasUnclassifiedPRComments returns true iff the convoy has at least one
// PRReviewComments row with classification=''.
func hasUnclassifiedPRComments(db *sql.DB, convoyID int) bool {
	var count int
	db.QueryRow(`SELECT COUNT(*) FROM PRReviewComments WHERE convoy_id = ? AND classification = ''`, convoyID).Scan(&count)
	return count > 0
}

// queuePRReviewTriageIfAbsent inserts a PRReviewTriage task for the convoy
// unless one is already Pending or Locked (boundary-matched on convoy_id to
// avoid dedup collisions between id=1 and id=10/100 etc.).
func queuePRReviewTriageIfAbsent(db *sql.DB, convoyID int, logger interface{ Printf(string, ...any) }) error {
	var existing int
	err := db.QueryRow(`SELECT COUNT(*) FROM BountyBoard
		WHERE type = 'PRReviewTriage' AND status IN ('Pending', 'Locked')
		  AND (payload LIKE '%"convoy_id":' || ? || ',%'
		    OR payload LIKE '%"convoy_id":' || ? || '}%')`,
		convoyID, convoyID).Scan(&existing)
	if err != nil {
		return fmt.Errorf("dedup query: %w", err)
	}
	if existing > 0 {
		return nil
	}
	payload, _ := json.Marshal(map[string]any{"convoy_id": convoyID})
	_, err = db.Exec(
		`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, priority, created_at)
		 VALUES (0, '', 'PRReviewTriage', 'Pending', ?, 4, datetime('now'))`,
		string(payload))
	if err != nil {
		return fmt.Errorf("insert PRReviewTriage: %w", err)
	}
	logger.Printf("pr-review-poll: queued PRReviewTriage for convoy %d", convoyID)
	return nil
}

// loadSkipLogins reads SystemConfig "pr_review_skip_logins" (CSV) and returns
// the list of GitHub logins whose comments are silently ignored by the poll.
// Set this to the operator's GitHub username to prevent the fleet from
// triaging the operator's own replies on the draft PR thread.
func loadSkipLogins(db *sql.DB) []string {
	raw := strings.TrimSpace(store.GetConfig(db, "pr_review_skip_logins", ""))
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, strings.ToLower(t))
		}
	}
	return out
}

// isSkippedLogin reports whether login is in the skip list (case-insensitive).
func isSkippedLogin(login string, skipLogins []string) bool {
	lower := strings.ToLower(login)
	for _, s := range skipLogins {
		if s == lower {
			return true
		}
	}
	return false
}

// loadBotAllowlist reads SystemConfig "pr_review_bot_logins" (CSV) and returns
// the parsed list, falling back to gh.DefaultBotLogins() when the key is unset
// or empty.
func loadBotAllowlist(db *sql.DB) []string {
	raw := strings.TrimSpace(store.GetConfig(db, "pr_review_bot_logins", ""))
	if raw == "" {
		return gh.DefaultBotLogins()
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return gh.DefaultBotLogins()
	}
	return out
}
