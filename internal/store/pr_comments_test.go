package store

import (
	"testing"
)

func TestRecordPRComment_InsertsAndIsIdempotent(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	c := PRReviewComment{
		ConvoyID:        7,
		Repo:            "acme/api",
		DraftPRNumber:   42,
		GitHubCommentID: 1001,
		CommentType:     "review_comment",
		Author:          "claude[bot]",
		AuthorKind:      "bot",
		Body:            "Consider renaming this variable.",
		Path:            "main.go",
		Line:            12,
		ReviewThreadID:  "PRRT_aaa",
	}
	id1, err := RecordPRComment(db, c)
	if err != nil {
		t.Fatalf("first insert failed: %v", err)
	}
	if id1 == 0 {
		t.Fatal("expected non-zero ID")
	}

	// Second insert with same (repo, draft_pr_number, github_comment_id) → returns existing ID.
	id2, err := RecordPRComment(db, c)
	if err != nil {
		t.Fatalf("second insert (dedup) failed: %v", err)
	}
	if id2 != id1 {
		t.Errorf("expected same ID on dedup, got %d vs %d", id1, id2)
	}

	// Verify row was only written once.
	var count int
	db.QueryRow(`SELECT COUNT(*) FROM PRReviewComments`).Scan(&count)
	if count != 1 {
		t.Errorf("expected 1 row after dedup, got %d", count)
	}
}

func TestRecordPRComment_RequiredFields(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	cases := []struct {
		name string
		c    PRReviewComment
	}{
		{"missing repo", PRReviewComment{DraftPRNumber: 1, GitHubCommentID: 1, CommentType: "review_comment", Author: "a", AuthorKind: "human"}},
		{"missing pr number", PRReviewComment{Repo: "acme/api", GitHubCommentID: 1, CommentType: "review_comment", Author: "a", AuthorKind: "human"}},
		{"missing comment id", PRReviewComment{Repo: "acme/api", DraftPRNumber: 1, CommentType: "review_comment", Author: "a", AuthorKind: "human"}},
		{"missing comment type", PRReviewComment{Repo: "acme/api", DraftPRNumber: 1, GitHubCommentID: 1, Author: "a", AuthorKind: "human"}},
		{"missing author", PRReviewComment{Repo: "acme/api", DraftPRNumber: 1, GitHubCommentID: 1, CommentType: "review_comment", AuthorKind: "human"}},
		{"missing author_kind", PRReviewComment{Repo: "acme/api", DraftPRNumber: 1, GitHubCommentID: 1, CommentType: "review_comment", Author: "a"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := RecordPRComment(db, tc.c); err == nil {
				t.Errorf("expected validation error for %q", tc.name)
			}
		})
	}
}

func TestListUnclassifiedPRComments_OrdersAndFilters(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	base := PRReviewComment{
		ConvoyID: 1, Repo: "acme/api", DraftPRNumber: 5, CommentType: "review_comment",
		Author: "claude[bot]", AuthorKind: "bot", Body: "nit",
	}
	base.GitHubCommentID = 1
	id1, _ := RecordPRComment(db, base)
	base.GitHubCommentID = 2
	id2, _ := RecordPRComment(db, base)
	base.GitHubCommentID = 3
	id3, _ := RecordPRComment(db, base)

	// Classify id2 so it should be skipped.
	db.Exec(`UPDATE PRReviewComments SET classification = 'in_scope_fix' WHERE id = ?`, id2)

	// Different convoy → should be skipped.
	base.ConvoyID = 2
	base.GitHubCommentID = 4
	RecordPRComment(db, base)

	out := ListUnclassifiedPRComments(db, 1, 0)
	if len(out) != 2 {
		t.Fatalf("expected 2 unclassified rows in convoy 1, got %d", len(out))
	}
	if out[0].ID != id1 || out[1].ID != id3 {
		t.Errorf("expected [id1, id3] ordered ASC, got [%d, %d]", out[0].ID, out[1].ID)
	}

	// Limit respected.
	limited := ListUnclassifiedPRComments(db, 1, 1)
	if len(limited) != 1 {
		t.Errorf("limit=1: expected 1 row, got %d", len(limited))
	}
}

func TestLoadThreadHistory_OrdersOldestFirst(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	base := PRReviewComment{
		ConvoyID: 1, Repo: "acme/api", DraftPRNumber: 5, CommentType: "review_comment",
		Author: "claude[bot]", AuthorKind: "bot", Body: "n", ReviewThreadID: "PRRT_aaa",
	}
	// Thread aaa: 3 comments in order.
	base.GitHubCommentID = 1
	id1, _ := RecordPRComment(db, base)
	base.GitHubCommentID = 2
	id2, _ := RecordPRComment(db, base)
	base.GitHubCommentID = 3
	id3, _ := RecordPRComment(db, base)

	// Different thread: excluded.
	base.ReviewThreadID = "PRRT_bbb"
	base.GitHubCommentID = 4
	RecordPRComment(db, base)

	out := LoadThreadHistory(db, 1, "PRRT_aaa")
	if len(out) != 3 {
		t.Fatalf("expected 3 rows in thread aaa, got %d", len(out))
	}
	if out[0].ID != id1 || out[1].ID != id2 || out[2].ID != id3 {
		t.Errorf("expected ASC order [%d, %d, %d], got [%d, %d, %d]",
			id1, id2, id3, out[0].ID, out[1].ID, out[2].ID)
	}

	// Empty thread ID returns nil.
	if out := LoadThreadHistory(db, 1, ""); out != nil {
		t.Errorf("empty thread_id should return nil, got %d rows", len(out))
	}
}

func TestMaxThreadDepth(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	base := PRReviewComment{
		ConvoyID: 1, Repo: "acme/api", DraftPRNumber: 5, CommentType: "review_comment",
		Author: "claude[bot]", AuthorKind: "bot", Body: "n", ReviewThreadID: "PRRT_aaa",
	}
	base.GitHubCommentID = 1
	base.ThreadDepth = 0
	RecordPRComment(db, base)
	base.GitHubCommentID = 2
	base.ThreadDepth = 1
	RecordPRComment(db, base)
	base.GitHubCommentID = 3
	base.ThreadDepth = 2
	RecordPRComment(db, base)

	if got := MaxThreadDepth(db, 1, "PRRT_aaa"); got != 2 {
		t.Errorf("expected max depth 2, got %d", got)
	}
	if got := MaxThreadDepth(db, 1, "PRRT_nonexistent"); got != 0 {
		t.Errorf("unknown thread should be 0, got %d", got)
	}
}

func TestClassifyPRCommentTx_WithReply(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	id, err := RecordPRComment(db, PRReviewComment{
		ConvoyID: 1, Repo: "acme/api", DraftPRNumber: 5, GitHubCommentID: 1,
		CommentType: "review_comment", Author: "claude[bot]", AuthorKind: "bot", Body: "nit",
	})
	if err != nil {
		t.Fatal(err)
	}

	tx, _ := db.Begin()
	if err := ClassifyPRCommentTx(tx, id, "in_scope_fix", "ok", "queued fix", "now", 99); err != nil {
		t.Fatalf("classify failed: %v", err)
	}
	tx.Commit()

	got := GetPRReviewComment(db, id)
	if got.Classification != "in_scope_fix" {
		t.Errorf("classification mismatch: %q", got.Classification)
	}
	if got.ReplyBody != "queued fix" {
		t.Errorf("reply body mismatch: %q", got.ReplyBody)
	}
	if got.SpawnedTaskID != 99 {
		t.Errorf("spawned task id mismatch: %d", got.SpawnedTaskID)
	}
	if got.RepliedAt == "" {
		t.Error("replied_at should be set when repliedAtSQL is non-empty")
	}
}

func TestClassifyPRCommentTx_HumanNoReply(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	id, _ := RecordPRComment(db, PRReviewComment{
		ConvoyID: 1, Repo: "acme/api", DraftPRNumber: 5, GitHubCommentID: 1,
		CommentType: "review_comment", Author: "alice", AuthorKind: "human", Body: "lgtm?",
	})

	tx, _ := db.Begin()
	// Human: classification='human', reply_body populated, repliedAtSQL empty (draft only).
	if err := ClassifyPRCommentTx(tx, id, "human", "operator to decide", "draft reply text", "", 0); err != nil {
		t.Fatalf("classify failed: %v", err)
	}
	tx.Commit()

	got := GetPRReviewComment(db, id)
	if got.Classification != "human" {
		t.Errorf("classification mismatch: %q", got.Classification)
	}
	if got.ReplyBody != "draft reply text" {
		t.Errorf("reply draft should be stored: %q", got.ReplyBody)
	}
	if got.RepliedAt != "" {
		t.Errorf("replied_at must stay empty for human-drafted reply, got %q", got.RepliedAt)
	}
}

func TestListPendingThreadResolves(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	// Seed two comments classified in_scope_fix; spawn two BountyBoard tasks;
	// mark one Completed; assert only that row is returned.
	taskCompleted := AddBounty(db, 0, "CodeEdit", "fix")
	db.Exec(`UPDATE BountyBoard SET status = 'Completed' WHERE id = ?`, taskCompleted)

	taskPending := AddBounty(db, 0, "CodeEdit", "fix")
	// Leave status=Pending.

	id1, _ := RecordPRComment(db, PRReviewComment{
		ConvoyID: 1, Repo: "acme/api", DraftPRNumber: 5, GitHubCommentID: 1,
		CommentType: "review_comment", Author: "claude[bot]", AuthorKind: "bot", Body: "a",
	})
	id2, _ := RecordPRComment(db, PRReviewComment{
		ConvoyID: 1, Repo: "acme/api", DraftPRNumber: 5, GitHubCommentID: 2,
		CommentType: "review_comment", Author: "claude[bot]", AuthorKind: "bot", Body: "b",
	})
	db.Exec(`UPDATE PRReviewComments SET classification='in_scope_fix', spawned_task_id=? WHERE id=?`, taskCompleted, id1)
	db.Exec(`UPDATE PRReviewComments SET classification='in_scope_fix', spawned_task_id=? WHERE id=?`, taskPending, id2)

	out := ListPendingThreadResolves(db)
	if len(out) != 1 {
		t.Fatalf("expected 1 pending resolve (task complete, not resolved), got %d", len(out))
	}
	if out[0].ID != id1 {
		t.Errorf("expected row with completed task (id1=%d), got id=%d", id1, out[0].ID)
	}

	// After marking id1 resolved, sweep returns empty.
	MarkThreadResolved(db, id1)
	out = ListPendingThreadResolves(db)
	if len(out) != 0 {
		t.Errorf("after MarkThreadResolved, expected 0 pending, got %d", len(out))
	}
}

func TestComputePRReviewRollup(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	base := PRReviewComment{
		ConvoyID: 1, Repo: "acme/api", DraftPRNumber: 5,
		CommentType: "review_comment", Body: "n",
	}

	insertWithClass := func(cid int64, author, kind, cls string) int {
		base.GitHubCommentID = cid
		base.Author = author
		base.AuthorKind = kind
		id, err := RecordPRComment(db, base)
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
		if cls != "" {
			db.Exec(`UPDATE PRReviewComments SET classification = ? WHERE id = ?`, cls, id)
		}
		return id
	}
	insertWithClass(1, "claude[bot]", "bot", "in_scope_fix")
	insertWithClass(2, "claude[bot]", "bot", "in_scope_fix")
	insertWithClass(3, "claude[bot]", "bot", "out_of_scope")
	insertWithClass(4, "claude[bot]", "bot", "not_actionable")
	insertWithClass(5, "claude[bot]", "bot", "conflicted_loop")
	insertWithClass(6, "claude[bot]", "bot", "") // unclassified bot
	insertWithClass(7, "alice", "human", "human")
	insertWithClass(8, "bob", "human", "") // human not yet triaged still awaiting

	r := ComputePRReviewRollup(db, 1)
	if r.Total != 8 {
		t.Errorf("Total: expected 8, got %d", r.Total)
	}
	if r.BotInScope != 2 {
		t.Errorf("BotInScope: expected 2, got %d", r.BotInScope)
	}
	if r.BotOutOfScope != 1 || r.BotNotAction != 1 || r.BotConflicted != 1 {
		t.Errorf("bot counts wrong: %+v", r)
	}
	if r.BotUnclassified != 1 {
		t.Errorf("BotUnclassified: expected 1, got %d", r.BotUnclassified)
	}
	if r.HumanAwaiting != 2 {
		t.Errorf("HumanAwaiting: expected 2 (classified + unclassified), got %d", r.HumanAwaiting)
	}
}

func TestInfrastructureTaskTypes_IncludesPRReviewTriage(t *testing.T) {
	if !IsInfrastructureTask("PRReviewTriage") {
		t.Error("PRReviewTriage should be classified as infrastructure")
	}
}
