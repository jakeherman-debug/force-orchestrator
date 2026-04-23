package store

import (
	"database/sql"
	"fmt"
)

// ── PRReviewComments CRUD ───────────────────────────────────────────────────
//
// Called by the pr-review-poll dog (RecordPRComment) to insert new rows from
// GitHub, and by Diplomat's PRReviewTriage (ClassifyPRCommentTx) to record
// LLM decisions. Tx variants exist for the dispatcher, which atomically
// classifies a comment + spawns a task + (optionally) stamps a reply.

// RecordPRComment inserts a freshly-discovered review comment. Idempotent on
// (repo, draft_pr_number, github_comment_id) — returns the existing row's ID
// if the comment was already seen. New rows always start with empty
// classification.
func RecordPRComment(db *sql.DB, c PRReviewComment) (int, error) {
	if c.Repo == "" || c.DraftPRNumber == 0 || c.GitHubCommentID == 0 {
		return 0, fmt.Errorf("RecordPRComment: repo, draft_pr_number, github_comment_id required (got %+v)", c)
	}
	if c.CommentType == "" || c.Author == "" || c.AuthorKind == "" {
		return 0, fmt.Errorf("RecordPRComment: comment_type, author, author_kind required (got %+v)", c)
	}
	res, err := db.Exec(`INSERT INTO PRReviewComments
		(convoy_id, repo, draft_pr_number, github_comment_id, comment_type,
		 author, author_kind, body, path, line, diff_hunk,
		 review_thread_id, in_reply_to_comment_id, thread_depth)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ConvoyID, c.Repo, c.DraftPRNumber, c.GitHubCommentID, c.CommentType,
		c.Author, c.AuthorKind, c.Body, c.Path, c.Line, c.DiffHunk,
		c.ReviewThreadID, c.InReplyToCommentID, c.ThreadDepth)
	if err != nil {
		// Unique constraint → return existing row.
		var existingID int
		if scanErr := db.QueryRow(
			`SELECT id FROM PRReviewComments WHERE repo = ? AND draft_pr_number = ? AND github_comment_id = ?`,
			c.Repo, c.DraftPRNumber, c.GitHubCommentID,
		).Scan(&existingID); scanErr == nil {
			return existingID, nil
		}
		return 0, err
	}
	id, _ := res.LastInsertId()
	return int(id), nil
}

// GetPRReviewComment fetches a single row by primary key.
func GetPRReviewComment(db *sql.DB, id int) *PRReviewComment {
	return scanPRReviewComment(db.QueryRow(prReviewCommentSelect+` WHERE id = ?`, id))
}

// ListUnclassifiedPRComments returns every comment in the given convoy with
// classification = '', ordered by created_at ASC (oldest first so threads are
// triaged in order). Limit 0 means no limit.
func ListUnclassifiedPRComments(db *sql.DB, convoyID, limit int) []PRReviewComment {
	q := prReviewCommentSelect + ` WHERE convoy_id = ? AND classification = '' ORDER BY id ASC`
	args := []any{convoyID}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []PRReviewComment
	for rows.Next() {
		if c := scanPRReviewCommentRow(rows); c != nil {
			out = append(out, *c)
		}
	}
	return out
}

// ListConvoyPRComments returns every comment for a convoy, newest first.
// Used by the dashboard convoy-detail endpoint.
func ListConvoyPRComments(db *sql.DB, convoyID int) []PRReviewComment {
	rows, err := db.Query(prReviewCommentSelect+` WHERE convoy_id = ? ORDER BY id DESC`, convoyID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []PRReviewComment
	for rows.Next() {
		if c := scanPRReviewCommentRow(rows); c != nil {
			out = append(out, *c)
		}
	}
	return out
}

// LoadThreadHistory returns every comment in the given review thread, ordered
// oldest-first, so the classifier can pass the entire conversation (including
// our own replies and prior fix summaries) to the LLM.
func LoadThreadHistory(db *sql.DB, convoyID int, reviewThreadID string) []PRReviewComment {
	if reviewThreadID == "" {
		return nil
	}
	rows, err := db.Query(
		prReviewCommentSelect+` WHERE convoy_id = ? AND review_thread_id = ? ORDER BY id ASC`,
		convoyID, reviewThreadID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []PRReviewComment
	for rows.Next() {
		if c := scanPRReviewCommentRow(rows); c != nil {
			out = append(out, *c)
		}
	}
	return out
}

// MaxThreadDepth returns the highest thread_depth value seen in the given
// thread. Callers computing the depth for a NEW comment use this + 1 when
// the new comment is a fleet-authored fix reply (for bot flows) or use it
// directly when recording a fresh external comment.
func MaxThreadDepth(db *sql.DB, convoyID int, reviewThreadID string) int {
	if reviewThreadID == "" {
		return 0
	}
	var depth int
	db.QueryRow(
		`SELECT COALESCE(MAX(thread_depth), 0) FROM PRReviewComments
		 WHERE convoy_id = ? AND review_thread_id = ?`,
		convoyID, reviewThreadID,
	).Scan(&depth)
	return depth
}

// ClassifyPRCommentTx records the classifier outcome inside an existing tx.
// spawnedTaskID is 0 for not_actionable / conflicted_loop / human branches.
// repliedAt should be datetime('now') for bot replies; empty for humans and
// conflicted_loop (never posted).
func ClassifyPRCommentTx(
	tx *sql.Tx,
	rowID int,
	classification, reason, replyBody, repliedAtSQL string,
	spawnedTaskID int,
) error {
	if classification == "" {
		return fmt.Errorf("ClassifyPRCommentTx: classification required")
	}
	// repliedAtSQL is either '' or datetime('now') — can't use ? for an expression,
	// so build the query conditionally.
	var q string
	args := []any{classification, reason, replyBody, spawnedTaskID, rowID}
	if repliedAtSQL == "" {
		q = `UPDATE PRReviewComments
			SET classification = ?, classification_reason = ?, reply_body = ?, spawned_task_id = ?
			WHERE id = ?`
	} else {
		q = `UPDATE PRReviewComments
			SET classification = ?, classification_reason = ?, reply_body = ?, spawned_task_id = ?,
			    replied_at = datetime('now')
			WHERE id = ?`
	}
	_, err := tx.Exec(q, args...)
	return err
}

// MarkThreadResolved stamps thread_resolved_at on the originating comment row
// after the GraphQL resolve call lands. Used by the post-Council resolve sweep.
func MarkThreadResolved(db *sql.DB, rowID int) error {
	_, err := db.Exec(`UPDATE PRReviewComments SET thread_resolved_at = datetime('now') WHERE id = ?`, rowID)
	return err
}

// ListPendingThreadResolves returns in_scope_fix rows whose spawned CodeEdit
// task has reached Completed but whose review thread hasn't been resolved yet.
// Used by the post-Council sweep.
func ListPendingThreadResolves(db *sql.DB) []PRReviewComment {
	rows, err := db.Query(prReviewCommentSelect + `
		WHERE classification = 'in_scope_fix'
		  AND spawned_task_id > 0
		  AND thread_resolved_at = ''
		  AND EXISTS (
		    SELECT 1 FROM BountyBoard b
		    WHERE b.id = PRReviewComments.spawned_task_id AND b.status = 'Completed'
		  )
		ORDER BY id ASC`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []PRReviewComment
	for rows.Next() {
		if c := scanPRReviewCommentRow(rows); c != nil {
			out = append(out, *c)
		}
	}
	return out
}

// RollupPRReviewComments returns a per-classification count for a convoy.
// Used by the dashboard convoy card.
type PRReviewCommentRollup struct {
	Total           int
	BotInScope      int
	BotOutOfScope   int
	BotNotAction    int
	BotConflicted   int
	HumanAwaiting   int
	BotUnclassified int
	// BotBlocking is the number of bot comments that are actively preventing a
	// clean ship: unclassified rows + in_scope_fix rows whose spawned CodeEdit
	// has not yet reached status=Completed. Resolved comments (thread_resolved_at
	// set) do not count even if their task is Completed.
	BotBlocking int
}

// ComputePRReviewRollup aggregates per-classification counts for a convoy.
func ComputePRReviewRollup(db *sql.DB, convoyID int) PRReviewCommentRollup {
	var r PRReviewCommentRollup
	rows, err := db.Query(`SELECT author_kind, classification, COUNT(*)
		FROM PRReviewComments WHERE convoy_id = ?
		GROUP BY author_kind, classification`, convoyID)
	if err != nil {
		return r
	}
	defer rows.Close()
	for rows.Next() {
		var kind, cls string
		var n int
		if err := rows.Scan(&kind, &cls, &n); err != nil {
			continue
		}
		r.Total += n
		switch kind {
		case "bot":
			switch cls {
			case "in_scope_fix":
				r.BotInScope = n
			case "out_of_scope":
				r.BotOutOfScope = n
			case "not_actionable":
				r.BotNotAction = n
			case "conflicted_loop":
				r.BotConflicted = n
			case "":
				r.BotUnclassified = n
			}
		case "human":
			// Every human row counts as awaiting (classification='human' after triage,
			// or '' before triage — operator still needs to look either way).
			r.HumanAwaiting += n
		}
	}

	// BotBlocking: unclassified bots + in_scope_fix whose fix hasn't landed yet.
	// A fix has landed when its spawned task is Completed (regardless of whether
	// the GitHub thread has been resolved — the code is in; ship gate is clear).
	r.BotBlocking = r.BotUnclassified
	var inFlight int
	db.QueryRow(`SELECT COUNT(*) FROM PRReviewComments p
		WHERE p.convoy_id = ? AND p.classification = 'in_scope_fix'
		AND (p.spawned_task_id = 0
		     OR (SELECT status FROM BountyBoard WHERE id = p.spawned_task_id) != 'Completed')`,
		convoyID).Scan(&inFlight)
	r.BotBlocking += inFlight

	return r
}

// ── scan helpers ────────────────────────────────────────────────────────────

const prReviewCommentSelect = `SELECT id, convoy_id, repo, draft_pr_number,
	github_comment_id, comment_type, author, author_kind, body,
	IFNULL(path, ''), IFNULL(line, 0), IFNULL(diff_hunk, ''),
	IFNULL(review_thread_id, ''), IFNULL(in_reply_to_comment_id, 0),
	IFNULL(thread_depth, 0),
	IFNULL(classification, ''), IFNULL(classification_reason, ''),
	IFNULL(spawned_task_id, 0),
	IFNULL(reply_body, ''), IFNULL(replied_at, ''),
	IFNULL(thread_resolved_at, ''),
	IFNULL(created_at, '')
	FROM PRReviewComments`

// scanPRReviewComment scans a QueryRow result into a PRReviewComment.
func scanPRReviewComment(row *sql.Row) *PRReviewComment {
	var c PRReviewComment
	err := row.Scan(&c.ID, &c.ConvoyID, &c.Repo, &c.DraftPRNumber,
		&c.GitHubCommentID, &c.CommentType, &c.Author, &c.AuthorKind, &c.Body,
		&c.Path, &c.Line, &c.DiffHunk,
		&c.ReviewThreadID, &c.InReplyToCommentID, &c.ThreadDepth,
		&c.Classification, &c.ClassificationReason, &c.SpawnedTaskID,
		&c.ReplyBody, &c.RepliedAt, &c.ThreadResolvedAt, &c.CreatedAt)
	if err != nil {
		return nil
	}
	return &c
}

// scanPRReviewCommentRow scans a *sql.Rows cursor row.
func scanPRReviewCommentRow(rows *sql.Rows) *PRReviewComment {
	var c PRReviewComment
	err := rows.Scan(&c.ID, &c.ConvoyID, &c.Repo, &c.DraftPRNumber,
		&c.GitHubCommentID, &c.CommentType, &c.Author, &c.AuthorKind, &c.Body,
		&c.Path, &c.Line, &c.DiffHunk,
		&c.ReviewThreadID, &c.InReplyToCommentID, &c.ThreadDepth,
		&c.Classification, &c.ClassificationReason, &c.SpawnedTaskID,
		&c.ReplyBody, &c.RepliedAt, &c.ThreadResolvedAt, &c.CreatedAt)
	if err != nil {
		return nil
	}
	return &c
}
