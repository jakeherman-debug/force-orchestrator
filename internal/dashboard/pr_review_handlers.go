package dashboard

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"force-orchestrator/internal/gh"
	"force-orchestrator/internal/store"
)

// ── PR review comments — dashboard endpoints ────────────────────────────────
//
// GET  /api/convoys/{id}/pr-review-comments      list every comment with classification
// POST /api/convoys/{id}/pr-review-retry         queue a fresh PRReviewTriage
//
// Human-comment action endpoints (operator decides whether to post the AI draft):
// POST /api/pr-comments/{row_id}/post-reply      post reply_body to GitHub, mark replied
// POST /api/pr-comments/{row_id}/dismiss         set classification='ignored', no post
// POST /api/pr-comments/{row_id}/queue-followup  spawn a Feature task from the comment
//
// All of these are per-repo-authorized only by the daemon's gh auth; there's
// no per-operator ACL in this app today.

// writeConvoyPRReviewComments serves GET /api/convoys/{id}/pr-review-comments.
// Response: list of DashboardPRReviewComment rows, newest first.
func writeConvoyPRReviewComments(db *sql.DB, w http.ResponseWriter, convoyID int) {
	rows := store.ListConvoyPRComments(db, convoyID)
	out := make([]DashboardPRReviewComment, 0, len(rows))
	for _, c := range rows {
		taskStatus := ""
		if c.SpawnedTaskID > 0 {
			db.QueryRow(`SELECT IFNULL(status,'') FROM BountyBoard WHERE id = ?`, c.SpawnedTaskID).Scan(&taskStatus)
		}
		out = append(out, DashboardPRReviewComment{
			ID:                   c.ID,
			Repo:                 c.Repo,
			DraftPRNumber:        c.DraftPRNumber,
			GitHubCommentID:      c.GitHubCommentID,
			CommentType:          c.CommentType,
			Author:               c.Author,
			AuthorKind:           c.AuthorKind,
			Body:                 c.Body,
			Path:                 c.Path,
			Line:                 c.Line,
			Classification:       c.Classification,
			ClassificationReason: c.ClassificationReason,
			SpawnedTaskID:        c.SpawnedTaskID,
			SpawnedTaskStatus:    taskStatus,
			ReplyBody:            c.ReplyBody,
			RepliedAt:            c.RepliedAt,
			ThreadResolvedAt:     c.ThreadResolvedAt,
			ThreadDepth:          c.ThreadDepth,
			CreatedAt:            c.CreatedAt,
		})
	}
	json.NewEncoder(w).Encode(map[string]any{"comments": out, "total": len(out)})
}

// writePRReviewRetry serves POST /api/convoys/{id}/pr-review-retry — queues
// a fresh PRReviewTriage task for the convoy even if unclassified rows exist
// (or not — the handler just inserts; the handler itself gates on unclassified
// rows). Used when the operator wants to re-run classification (e.g. after
// the LLM's prior output was unsatisfactory).
func writePRReviewRetry(db *sql.DB, w http.ResponseWriter, convoyID int) {
	payload := fmt.Sprintf(`{"convoy_id":%d}`, convoyID)
	res, err := db.Exec(
		`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, priority, created_at)
		 VALUES (0, '', 'PRReviewTriage', 'Pending', ?, 4, datetime('now'))`,
		payload)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
		return
	}
	id, _ := res.LastInsertId()
	store.LogAudit(db, "dashboard", "pr-review-retry", int(id),
		fmt.Sprintf("operator triggered PRReviewTriage for convoy #%d", convoyID))
	fmt.Fprintf(w, `{"ok":true,"triage_task_id":%d}`, id)
}

// handlePRCommentsSubroutes handles /api/pr-comments/{row_id}/{action}.
func handlePRCommentsSubroutes(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		// Expected: ["api", "pr-comments", "{row_id}", "{action}"]
		if len(parts) != 4 {
			http.NotFound(w, r)
			return
		}
		var rowID int
		fmt.Sscanf(parts[2], "%d", &rowID)
		if rowID <= 0 {
			http.Error(w, `{"error":"invalid row id"}`, http.StatusBadRequest)
			return
		}
		row := store.GetPRReviewComment(db, rowID)
		if row == nil {
			http.NotFound(w, r)
			return
		}

		switch parts[3] {
		case "post-reply":
			handlePostHumanReply(db, w, r, row)
		case "dismiss":
			handleDismissHumanComment(db, w, row)
		case "queue-followup":
			handleQueueFollowupFromComment(db, w, r, row)
		default:
			http.NotFound(w, r)
		}
	}
}

// handlePostHumanReply posts the reply_body to GitHub for a human-authored
// comment. Body (optional, JSON): {"body": "<override reply body>"}. If empty
// or missing, uses the LLM-drafted reply_body that was stored at classification
// time. On success, sets replied_at.
func handlePostHumanReply(db *sql.DB, w http.ResponseWriter, r *http.Request, row *store.PRReviewComment) {
	if row.AuthorKind != "human" {
		http.Error(w, `{"error":"post-reply is only valid on human-authored comments"}`, http.StatusBadRequest)
		return
	}
	if row.RepliedAt != "" {
		http.Error(w, `{"error":"already replied"}`, http.StatusConflict)
		return
	}

	// Operator may override the draft with their own text.
	body := row.ReplyBody
	if r.Body != nil {
		raw, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if len(raw) > 0 {
			var req struct {
				Body string `json:"body"`
			}
			if err := json.Unmarshal(raw, &req); err == nil && strings.TrimSpace(req.Body) != "" {
				body = req.Body
			}
		}
	}
	if strings.TrimSpace(body) == "" {
		http.Error(w, `{"error":"no reply body"}`, http.StatusBadRequest)
		return
	}

	repoCfg := store.GetRepo(db, row.Repo)
	if repoCfg == nil {
		http.Error(w, `{"error":"repo not registered"}`, http.StatusInternalServerError)
		return
	}
	ghc := gh.NewClient()
	ghRepo := deriveGHRepoSlug(repoCfg.RemoteURL)
	var postErr error
	switch row.CommentType {
	case "issue_comment":
		postErr = ghc.PostIssueComment(repoCfg.LocalPath, ghRepo, row.DraftPRNumber, body)
	case "review_comment":
		postErr = ghc.PostReviewThreadReply(repoCfg.LocalPath, ghRepo, row.DraftPRNumber, row.GitHubCommentID, body)
	default:
		http.Error(w, `{"error":"unknown comment_type"}`, http.StatusInternalServerError)
		return
	}
	if postErr != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, postErr.Error()), http.StatusInternalServerError)
		return
	}
	// Stamp replied_at and update reply_body (if operator edited).
	if _, err := db.Exec(`UPDATE PRReviewComments SET reply_body = ?, replied_at = datetime('now') WHERE id = ?`,
		body, row.ID); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
		return
	}
	store.LogAudit(db, "dashboard", "pr-review-human-reply", row.ID,
		fmt.Sprintf("operator posted reply to comment #%d", row.GitHubCommentID))
	fmt.Fprintf(w, `{"ok":true}`)
}

// handleDismissHumanComment sets classification='ignored' so the row stops
// surfacing as awaiting operator action. No gh calls; no reply posted.
func handleDismissHumanComment(db *sql.DB, w http.ResponseWriter, row *store.PRReviewComment) {
	if _, err := db.Exec(`UPDATE PRReviewComments SET classification = 'ignored' WHERE id = ?`, row.ID); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
		return
	}
	store.LogAudit(db, "dashboard", "pr-review-dismiss", row.ID,
		fmt.Sprintf("operator dismissed comment #%d", row.GitHubCommentID))
	fmt.Fprintf(w, `{"ok":true}`)
}

// handleQueueFollowupFromComment spawns a Feature task derived from the row's
// body and marks the row's classification accordingly. Intended for human
// comments the operator wants to defer as future work.
func handleQueueFollowupFromComment(db *sql.DB, w http.ResponseWriter, r *http.Request, row *store.PRReviewComment) {
	featurePayload := fmt.Sprintf(
		"[PR_REVIEW_FOLLOWUP from convoy #%d comment #%d by %s]\n\n%s",
		row.ConvoyID, row.GitHubCommentID, row.Author, row.Body,
	)
	tx, err := db.Begin()
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	featureID, addErr := store.AddFeatureTaskTx(tx, row.Repo, featurePayload, 0)
	if addErr != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, addErr.Error()), http.StatusInternalServerError)
		return
	}
	// Mark the row as handled; keep classification='human' but stamp spawned_task_id
	// so the dashboard UI can show "→ feature #F".
	if _, err := tx.Exec(`UPDATE PRReviewComments SET spawned_task_id = ? WHERE id = ?`, featureID, row.ID); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
		return
	}
	store.LogAudit(db, "dashboard", "pr-review-queue-followup", featureID,
		fmt.Sprintf("operator queued feature #%d from comment #%d", featureID, row.GitHubCommentID))
	fmt.Fprintf(w, `{"ok":true,"feature_id":%d}`, featureID)
	_ = r // future: operator-provided payload override
}

// deriveGHRepoSlug is a local copy of the agents-package helper so the
// dashboard can resolve owner/name without a cross-package dependency.
// Keep in sync with agents/pr_flow.go's deriveGHRepoFromRemoteURL.
func deriveGHRepoSlug(remoteURL string) string {
	if remoteURL == "" {
		return ""
	}
	if strings.HasPrefix(remoteURL, "git@") {
		if idx := strings.Index(remoteURL, ":"); idx > 0 {
			return strings.TrimSuffix(remoteURL[idx+1:], ".git")
		}
		return ""
	}
	if strings.HasPrefix(remoteURL, "https://") || strings.HasPrefix(remoteURL, "http://") {
		u := strings.TrimPrefix(strings.TrimPrefix(remoteURL, "https://"), "http://")
		if idx := strings.Index(u, "/"); idx > 0 {
			return strings.TrimSuffix(u[idx+1:], ".git")
		}
	}
	return ""
}
