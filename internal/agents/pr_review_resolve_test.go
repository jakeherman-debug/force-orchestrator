package agents

import (
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// TestDogPRReviewResolve_CallsGraphQLWhenFixCompleted verifies the sweep:
// find in_scope_fix rows whose spawned task is Completed, look up the thread
// node ID, call the resolve mutation, stamp thread_resolved_at.
func TestDogPRReviewResolve_CallsGraphQLWhenFixCompleted(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := seedDraftPROpenConvoy(t, db)

	// Seed a completed CodeEdit and a row that points to it.
	fixTaskID := store.AddBounty(db, 0, "CodeEdit", "fix")
	db.Exec(`UPDATE BountyBoard SET status = 'Completed' WHERE id = ?`, fixTaskID)

	rowID, _ := store.RecordPRComment(db, store.PRReviewComment{
		ConvoyID:        convoyID,
		Repo:            "api",
		DraftPRNumber:   42,
		GitHubCommentID: 555,
		CommentType:     "review_comment",
		Author:          "claude[bot]",
		AuthorKind:      "bot",
		Body:            "rename",
		ReviewThreadID:  "review:555",
	})
	db.Exec(`UPDATE PRReviewComments SET classification = 'in_scope_fix', spawned_task_id = ? WHERE id = ?`,
		fixTaskID, rowID)

	stub := installGHStub(t, map[string]ghStubResp{
		// GraphQL lookup returns a thread containing our comment.
		"api graphql": {
			stdout: `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[
				{"id":"PRRT_real","isResolved":false,"comments":{"nodes":[{"databaseId":555}]}}
			]}}}}}`,
		},
	})

	if err := dogPRReviewResolve(db, testLogger{}); err != nil {
		t.Fatalf("sweep failed: %v", err)
	}

	// thread_resolved_at should now be stamped.
	var resolvedAt string
	db.QueryRow(`SELECT thread_resolved_at FROM PRReviewComments WHERE id = ?`, rowID).Scan(&resolvedAt)
	if resolvedAt == "" {
		t.Error("expected thread_resolved_at to be stamped")
	}

	// The stub should have seen at least 2 api graphql calls: one FindReviewThreadNodeID,
	// one ResolveReviewThread.
	var graphqlCalls int
	var sawResolve bool
	for _, c := range stub.calls {
		j := strings.Join(c.args, " ")
		if strings.Contains(j, "api graphql") {
			graphqlCalls++
			if strings.Contains(j, "resolveReviewThread") {
				sawResolve = true
			}
		}
	}
	if graphqlCalls < 2 {
		t.Errorf("expected at least 2 graphql calls (lookup + resolve), got %d", graphqlCalls)
	}
	if !sawResolve {
		t.Error("expected a resolveReviewThread mutation call")
	}
}

// TestDogPRReviewResolve_SkipsWhenFixStillPending verifies that if the spawned
// CodeEdit task is not yet Completed, the sweep leaves the row alone.
func TestDogPRReviewResolve_SkipsWhenFixStillPending(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := seedDraftPROpenConvoy(t, db)

	// CodeEdit still Pending.
	fixTaskID := store.AddBounty(db, 0, "CodeEdit", "fix")

	rowID, _ := store.RecordPRComment(db, store.PRReviewComment{
		ConvoyID:        convoyID,
		Repo:            "api",
		DraftPRNumber:   42,
		GitHubCommentID: 556,
		CommentType:     "review_comment",
		Author:          "claude[bot]",
		AuthorKind:      "bot",
		Body:            "x",
		ReviewThreadID:  "review:556",
	})
	db.Exec(`UPDATE PRReviewComments SET classification = 'in_scope_fix', spawned_task_id = ? WHERE id = ?`,
		fixTaskID, rowID)

	stub := installGHStub(t, map[string]ghStubResp{})

	if err := dogPRReviewResolve(db, testLogger{}); err != nil {
		t.Fatalf("sweep failed: %v", err)
	}

	// No gh calls should have happened.
	if len(stub.calls) != 0 {
		t.Errorf("expected 0 gh calls when fix is pending, got %d", len(stub.calls))
	}
	var resolvedAt string
	db.QueryRow(`SELECT thread_resolved_at FROM PRReviewComments WHERE id = ?`, rowID).Scan(&resolvedAt)
	if resolvedAt != "" {
		t.Errorf("thread_resolved_at should remain empty while fix Pending, got %q", resolvedAt)
	}
}

// TestDogPRReviewResolve_IssueCommentsMarkedResolvedWithoutGraphQL verifies
// that issue-comment rows (which have no review thread in the GraphQL sense)
// are marked resolved locally without any gh calls.
func TestDogPRReviewResolve_IssueCommentsMarkedResolvedWithoutGraphQL(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := seedDraftPROpenConvoy(t, db)
	fixTaskID := store.AddBounty(db, 0, "CodeEdit", "fix")
	db.Exec(`UPDATE BountyBoard SET status = 'Completed' WHERE id = ?`, fixTaskID)

	rowID, _ := store.RecordPRComment(db, store.PRReviewComment{
		ConvoyID:        convoyID,
		Repo:            "api",
		DraftPRNumber:   42,
		GitHubCommentID: 777,
		CommentType:     "issue_comment",
		Author:          "claude[bot]",
		AuthorKind:      "bot",
		Body:            "x",
		ReviewThreadID:  "issue:42",
	})
	db.Exec(`UPDATE PRReviewComments SET classification = 'in_scope_fix', spawned_task_id = ? WHERE id = ?`,
		fixTaskID, rowID)

	stub := installGHStub(t, map[string]ghStubResp{})

	if err := dogPRReviewResolve(db, testLogger{}); err != nil {
		t.Fatalf("sweep failed: %v", err)
	}

	// No gh calls — issue comments skip GraphQL.
	if len(stub.calls) != 0 {
		t.Errorf("expected 0 gh calls for issue_comment path, got %d", len(stub.calls))
	}
	var resolvedAt string
	db.QueryRow(`SELECT thread_resolved_at FROM PRReviewComments WHERE id = ?`, rowID).Scan(&resolvedAt)
	if resolvedAt == "" {
		t.Error("issue_comment rows should still be marked resolved locally")
	}
}

// TestDogPRReviewResolve_KillSwitch verifies pr_review_enabled=0 skips the sweep.
func TestDogPRReviewResolve_KillSwitch(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.SetConfig(db, "pr_review_enabled", "0")

	convoyID := seedDraftPROpenConvoy(t, db)
	fixTaskID := store.AddBounty(db, 0, "CodeEdit", "fix")
	db.Exec(`UPDATE BountyBoard SET status = 'Completed' WHERE id = ?`, fixTaskID)

	rowID, _ := store.RecordPRComment(db, store.PRReviewComment{
		ConvoyID:        convoyID,
		Repo:            "api",
		DraftPRNumber:   42,
		GitHubCommentID: 888,
		CommentType:     "review_comment",
		Author:          "claude[bot]",
		AuthorKind:      "bot",
		Body:            "x",
		ReviewThreadID:  "review:888",
	})
	db.Exec(`UPDATE PRReviewComments SET classification = 'in_scope_fix', spawned_task_id = ? WHERE id = ?`,
		fixTaskID, rowID)

	stub := installGHStub(t, map[string]ghStubResp{})

	if err := dogPRReviewResolve(db, testLogger{}); err != nil {
		t.Fatalf("sweep failed: %v", err)
	}

	if len(stub.calls) != 0 {
		t.Errorf("kill switch should prevent gh calls, got %d", len(stub.calls))
	}
}
