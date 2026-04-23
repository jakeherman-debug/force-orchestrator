package agents

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// seedPRCommentForTriage inserts one PRReviewComment row that's ready to be
// triaged. Returns the row ID.
func seedPRCommentForTriage(t *testing.T, db *sql.DB, convoyID int, authorKind, body string) int {
	t.Helper()
	author := "claude[bot]"
	if authorKind == "human" {
		author = "alice"
	}
	id, err := store.RecordPRComment(db, store.PRReviewComment{
		ConvoyID:        convoyID,
		Repo:            "api",
		DraftPRNumber:   42,
		GitHubCommentID: int64(1000 + convoyID),
		CommentType:     "review_comment",
		Author:          author,
		AuthorKind:      authorKind,
		Body:            body,
		Path:            "main.go",
		Line:            10,
		ReviewThreadID:  fmt.Sprintf("review:%d", 1000+convoyID),
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	return id
}

// queuePRReviewTriageTask enqueues the task row so runPRReviewTriage can claim it.
func queuePRReviewTriageTask(t *testing.T, db *sql.DB, convoyID int) int {
	t.Helper()
	payload := fmt.Sprintf(`{"convoy_id":%d}`, convoyID)
	res, err := db.Exec(
		`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, priority, created_at)
		 VALUES (0, '', 'PRReviewTriage', 'Pending', ?, 4, datetime('now'))`,
		payload)
	if err != nil {
		t.Fatalf("queue triage: %v", err)
	}
	id, _ := res.LastInsertId()
	return int(id)
}

// stubLLM returns the given canned JSON response from the LLM classifier.
// Claude's ExtractJSON accepts plain JSON so no wrapper needed.
func stubLLM(t *testing.T, decision prReviewDecision) {
	t.Helper()
	raw, _ := jsonMustMarshal(decision)
	withStubCLIRunner(t, raw, nil)
}

func jsonMustMarshal(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}

// ── Branch dispatch tests ───────────────────────────────────────────────────

// TestPRReviewTriage_InScopeFix_SpawnsCodeEditOnAskBranch verifies that a bot
// in_scope_fix classification spawns a CodeEdit task with branch_name set to
// the ask-branch, classifies the row, and posts a reply via gh.
func TestPRReviewTriage_InScopeFix_SpawnsCodeEditOnAskBranch(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := seedDraftPROpenConvoy(t, db)
	rowID := seedPRCommentForTriage(t, db, convoyID, "bot", "Rename this variable from `x` to something descriptive.")

	stubLLM(t, prReviewDecision{
		Classification: "in_scope_fix",
		Reasoning:      "Valid rename request within the convoy's scope.",
		ReplyBody:      "Queued fix in task {{TASK_ID}}; PR will update after Council review.",
		FixSummary:     "Rename the `x` variable in main.go line 10 to something descriptive.",
	})
	stub := installGHStub(t, map[string]ghStubResp{
		"api -X POST repos/acme/api/pulls/42/comments": {stdout: ""},
	})

	taskID := queuePRReviewTriageTask(t, db, convoyID)
	bounty, _ := store.GetBounty(db, taskID)
	bounty.Status = "Locked"
	runPRReviewTriage(db, "Diplomat", bounty, testLogger{})

	// Row classified in_scope_fix with a spawned CodeEdit.
	after := store.GetPRReviewComment(db, rowID)
	if after.Classification != "in_scope_fix" {
		t.Errorf("classification = %q, want in_scope_fix", after.Classification)
	}
	if after.SpawnedTaskID == 0 {
		t.Fatal("expected SpawnedTaskID > 0")
	}
	if after.RepliedAt == "" {
		t.Error("replied_at should be set for bot reply")
	}
	if !strings.Contains(after.ReplyBody, fmt.Sprintf("#%d", after.SpawnedTaskID)) {
		t.Errorf("reply body should contain spawned task ID #%d, got %q", after.SpawnedTaskID, after.ReplyBody)
	}

	// CodeEdit task exists with branch_name = ask-branch.
	var fixType, fixBranch string
	var fixConvoy int
	db.QueryRow(`SELECT type, IFNULL(branch_name, ''), convoy_id FROM BountyBoard WHERE id = ?`, after.SpawnedTaskID).
		Scan(&fixType, &fixBranch, &fixConvoy)
	if fixType != "CodeEdit" {
		t.Errorf("spawned task type = %q, want CodeEdit", fixType)
	}
	if fixBranch != "force/ask-1-test" {
		t.Errorf("spawned task branch = %q, want force/ask-1-test", fixBranch)
	}
	if fixConvoy != convoyID {
		t.Errorf("spawned task convoy_id = %d, want %d", fixConvoy, convoyID)
	}

	// A POST reply was made.
	sawReply := false
	for _, c := range stub.calls {
		j := strings.Join(c.args, " ")
		if strings.Contains(j, "api -X POST") && strings.Contains(j, "/comments/1001/replies") {
			sawReply = true
		}
	}
	if !sawReply {
		t.Errorf("expected a review-thread reply POST, got calls: %+v", stub.calls)
	}
}

// TestPRReviewTriage_OutOfScope_SpawnsFeatureTask verifies the out_of_scope path.
func TestPRReviewTriage_OutOfScope_SpawnsFeatureTask(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := seedDraftPROpenConvoy(t, db)
	rowID := seedPRCommentForTriage(t, db, convoyID, "bot", "While you're here, refactor the entire metrics module.")

	stubLLM(t, prReviewDecision{
		Classification: "out_of_scope",
		Reasoning:      "Metrics refactor is outside this convoy's scope.",
		ReplyBody:      "Good suggestion but outside this convoy's scope — deferred to feature {{FEATURE_ID}}.",
		FixSummary:     "",
	})
	installGHStub(t, map[string]ghStubResp{
		"api -X POST repos/acme/api/pulls/42/comments": {stdout: ""},
	})

	taskID := queuePRReviewTriageTask(t, db, convoyID)
	bounty, _ := store.GetBounty(db, taskID)
	runPRReviewTriage(db, "Diplomat", bounty, testLogger{})

	after := store.GetPRReviewComment(db, rowID)
	if after.Classification != "out_of_scope" {
		t.Errorf("classification = %q, want out_of_scope", after.Classification)
	}
	if after.SpawnedTaskID == 0 {
		t.Fatal("expected SpawnedTaskID > 0 for feature task")
	}

	// Feature task exists with type=Feature, parent_id=0, target_repo=api.
	var fType, fRepo string
	var fParent int
	db.QueryRow(`SELECT type, target_repo, parent_id FROM BountyBoard WHERE id = ?`, after.SpawnedTaskID).
		Scan(&fType, &fRepo, &fParent)
	if fType != "Feature" || fRepo != "api" || fParent != 0 {
		t.Errorf("feature task fields wrong: type=%q repo=%q parent=%d", fType, fRepo, fParent)
	}

	if !strings.Contains(after.ReplyBody, fmt.Sprintf("#%d", after.SpawnedTaskID)) {
		t.Errorf("reply should reference feature ID: %q", after.ReplyBody)
	}
}

// TestPRReviewTriage_NotActionable_RepliesOnly verifies that not_actionable
// posts a reply but spawns no task.
func TestPRReviewTriage_NotActionable_RepliesOnly(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := seedDraftPROpenConvoy(t, db)
	rowID := seedPRCommentForTriage(t, db, convoyID, "bot", "Why did you use a map here?")

	stubLLM(t, prReviewDecision{
		Classification: "not_actionable",
		Reasoning:      "This is a question; the choice was deliberate for O(1) lookup.",
		ReplyBody:      "A map was chosen deliberately for O(1) lookup; see task description.",
	})
	stub := installGHStub(t, map[string]ghStubResp{
		"api -X POST repos/acme/api/pulls/42/comments": {stdout: ""},
	})

	taskID := queuePRReviewTriageTask(t, db, convoyID)
	bounty, _ := store.GetBounty(db, taskID)
	runPRReviewTriage(db, "Diplomat", bounty, testLogger{})

	after := store.GetPRReviewComment(db, rowID)
	if after.Classification != "not_actionable" {
		t.Errorf("classification = %q, want not_actionable", after.Classification)
	}
	if after.SpawnedTaskID != 0 {
		t.Errorf("not_actionable should not spawn a task, got SpawnedTaskID=%d", after.SpawnedTaskID)
	}
	if after.RepliedAt == "" {
		t.Error("replied_at should be set after reply post")
	}

	// No CodeEdit or Feature tasks should exist (only the triage task itself).
	var spawned int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type IN ('CodeEdit', 'Feature')`).Scan(&spawned)
	if spawned != 0 {
		t.Errorf("no new tasks should be spawned, got %d", spawned)
	}

	sawReply := false
	for _, c := range stub.calls {
		if strings.Contains(strings.Join(c.args, " "), "api -X POST") {
			sawReply = true
		}
	}
	if !sawReply {
		t.Error("expected a reply POST")
	}
}

// TestPRReviewTriage_ConflictedLoop_EscalatesNoReply verifies that classifier
// emitting conflicted_loop creates an escalation and does NOT post a reply.
func TestPRReviewTriage_ConflictedLoop_EscalatesNoReply(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := seedDraftPROpenConvoy(t, db)
	rowID := seedPRCommentForTriage(t, db, convoyID, "bot", "Actually revert the previous change.")
	// Simulate high thread depth so classifier can legitimately return conflicted_loop.
	db.Exec(`UPDATE PRReviewComments SET thread_depth = 2 WHERE id = ?`, rowID)

	stubLLM(t, prReviewDecision{
		Classification: "conflicted_loop",
		Reasoning:      "Bot is contradicting prior direction after 2 fleet fixes — escalating.",
		ReplyBody:      "",
	})
	stub := installGHStub(t, map[string]ghStubResp{})

	taskID := queuePRReviewTriageTask(t, db, convoyID)
	bounty, _ := store.GetBounty(db, taskID)
	runPRReviewTriage(db, "Diplomat", bounty, testLogger{})

	after := store.GetPRReviewComment(db, rowID)
	if after.Classification != "conflicted_loop" {
		t.Errorf("classification = %q, want conflicted_loop", after.Classification)
	}
	if after.RepliedAt != "" {
		t.Errorf("conflicted_loop must not post reply (replied_at=%q)", after.RepliedAt)
	}
	if after.SpawnedTaskID != 0 {
		t.Errorf("conflicted_loop must not spawn a task")
	}

	// No gh POST calls at all.
	for _, c := range stub.calls {
		if strings.Contains(strings.Join(c.args, " "), "-X POST") {
			t.Errorf("no gh POST should happen on conflicted_loop, got %+v", c.args)
		}
	}

	// An escalation row must exist.
	var escCount int
	db.QueryRow(`SELECT COUNT(*) FROM Escalations WHERE message LIKE '%conflicted%' OR message LIKE '%loop%'`).Scan(&escCount)
	if escCount == 0 {
		t.Error("expected an Escalation row for the conflicted_loop case")
	}
}

// TestPRReviewTriage_HumanNeverPosts verifies that human author_kind comments
// get classification='human', reply_body drafted, and NO gh post calls.
// Even if the LLM returned some other classification, the dispatcher must
// normalize to 'human' for human authors.
func TestPRReviewTriage_HumanNeverPosts(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := seedDraftPROpenConvoy(t, db)
	rowID := seedPRCommentForTriage(t, db, convoyID, "human", "Can you explain the choice here?")

	// LLM returns in_scope_fix — normalization must override it to 'human'.
	stubLLM(t, prReviewDecision{
		Classification: "in_scope_fix",
		Reasoning:      "The LLM wrongly thinks it can auto-fix this.",
		ReplyBody:      "Draft reply for operator review: the choice was deliberate.",
	})
	stub := installGHStub(t, map[string]ghStubResp{})

	taskID := queuePRReviewTriageTask(t, db, convoyID)
	bounty, _ := store.GetBounty(db, taskID)
	runPRReviewTriage(db, "Diplomat", bounty, testLogger{})

	after := store.GetPRReviewComment(db, rowID)
	if after.Classification != "human" {
		t.Errorf("classification must be normalized to 'human' for human authors, got %q", after.Classification)
	}
	if after.RepliedAt != "" {
		t.Errorf("human comments must NEVER have replied_at set, got %q", after.RepliedAt)
	}
	if after.ReplyBody == "" {
		t.Error("human comments should still have a reply_body as operator draft")
	}
	if after.SpawnedTaskID != 0 {
		t.Errorf("human comments must not spawn tasks, got %d", after.SpawnedTaskID)
	}

	// Zero gh calls.
	if len(stub.calls) != 0 {
		t.Errorf("human flow must not call gh, got %d calls", len(stub.calls))
	}

	// No CodeEdit or Feature tasks spawned either.
	var spawned int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type IN ('CodeEdit', 'Feature')`).Scan(&spawned)
	if spawned != 0 {
		t.Errorf("no new tasks for human comment, got %d", spawned)
	}
}

// TestPRReviewTriage_BatchCapRespected verifies that the batch cap limits how
// many comments are processed per run. Remaining unclassified rows stay in
// the queue for the next poll.
func TestPRReviewTriage_BatchCapRespected(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := seedDraftPROpenConvoy(t, db)
	// Lower the cap for this test.
	store.SetConfig(db, "pr_review_batch_cap", "2")

	// Seed 5 comments.
	for i := 0; i < 5; i++ {
		if _, err := store.RecordPRComment(db, store.PRReviewComment{
			ConvoyID:        convoyID,
			Repo:            "api",
			DraftPRNumber:   42,
			GitHubCommentID: int64(2000 + i),
			CommentType:     "review_comment",
			Author:          "claude[bot]",
			AuthorKind:      "bot",
			Body:            fmt.Sprintf("nit %d", i),
			ReviewThreadID:  fmt.Sprintf("review:%d", 2000+i),
		}); err != nil {
			t.Fatal(err)
		}
	}

	stubLLM(t, prReviewDecision{
		Classification: "not_actionable",
		Reasoning:      "minor",
		ReplyBody:      "noted",
	})
	installGHStub(t, map[string]ghStubResp{
		"api -X POST repos/acme/api/pulls/42/comments": {stdout: ""},
	})

	taskID := queuePRReviewTriageTask(t, db, convoyID)
	bounty, _ := store.GetBounty(db, taskID)
	runPRReviewTriage(db, "Diplomat", bounty, testLogger{})

	var classified, unclassified int
	db.QueryRow(`SELECT COUNT(*) FROM PRReviewComments WHERE convoy_id = ? AND classification != ''`, convoyID).Scan(&classified)
	db.QueryRow(`SELECT COUNT(*) FROM PRReviewComments WHERE convoy_id = ? AND classification = ''`, convoyID).Scan(&unclassified)

	if classified != 2 {
		t.Errorf("batch cap should classify 2, got %d", classified)
	}
	if unclassified != 3 {
		t.Errorf("remaining unclassified should be 3 (for next tick), got %d", unclassified)
	}
}

// ── pollConvoyPRReviews skip-login tests ─────────────────────────────────────

// TestPollConvoyPRReviews_SkipsOperatorLogin ensures that comments from logins
// listed in pr_review_skip_logins are not inserted into PRReviewComments.
func TestPollConvoyPRReviews_SkipsOperatorLogin(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, _ := store.CreateConvoy(db, "test")
	db.Exec(`INSERT INTO Repositories (name, local_path, remote_url, pr_flow_enabled, pr_review_enabled)
		VALUES ('api', '/tmp/api', 'git@github.com:acme/api.git', 1, 1)`)
	db.Exec(`UPDATE Convoys SET status = 'DraftPROpen' WHERE id = ?`, convoyID)
	store.UpsertConvoyAskBranch(db, convoyID, "api", "force/ask-1", "abc123")
	store.SetConvoyAskBranchDraftPR(db, convoyID, "api", "https://github.com/acme/api/pull/42", 42, "Open")

	// pr_review_skip_logins = the operator's login
	store.SetConfig(db, "pr_review_skip_logins", "jake-operator")

	// Issue comment from the operator + one from a bot.
	issueComments := []map[string]any{
		{"id": 1, "body": "I already replied", "user": map[string]any{"login": "jake-operator", "type": "User"}},
		{"id": 2, "body": "Bot says: fix this", "user": map[string]any{"login": "gemini-code-assist[bot]", "type": "Bot"}},
	}
	issueJSON, _ := json.Marshal(issueComments)

	installGHStub(t, map[string]ghStubResp{
		"api --paginate repos/acme/api/issues/42/comments": {stdout: string(issueJSON)},
		"api --paginate repos/acme/api/pulls/42/comments":  {stdout: "[]"},
	})

	ghc := newGHClient()
	pollConvoyPRReviews(db, ghc, convoyID, loadBotAllowlist(db), loadSkipLogins(db), testLogger{})

	var count int
	db.QueryRow(`SELECT COUNT(*) FROM PRReviewComments WHERE convoy_id = ?`, convoyID).Scan(&count)
	if count != 1 {
		t.Errorf("expected 1 row (operator comment skipped), got %d", count)
	}

	var author string
	db.QueryRow(`SELECT author FROM PRReviewComments WHERE convoy_id = ?`, convoyID).Scan(&author)
	if author == "jake-operator" {
		t.Error("operator comment should have been skipped, but was recorded")
	}
}
