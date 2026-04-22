package agents

import (
	"database/sql"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// seedDraftPROpenConvoy creates a minimal convoy in DraftPROpen state with one
// ConvoyAskBranch row pointing at draft PR #42 on repo "api".
func seedDraftPROpenConvoy(t *testing.T, db *sql.DB) (convoyID int) {
	t.Helper()
	store.AddRepo(db, "api", "/tmp/api", "")
	_ = store.SetRepoRemoteInfo(db, "api", "https://github.com/acme/api.git", "main")
	cid, _ := store.CreateConvoy(db, "[1] test convoy")
	db.Exec(`UPDATE Convoys SET status = 'DraftPROpen' WHERE id = ?`, cid)
	_ = store.UpsertConvoyAskBranch(db, cid, "api", "force/ask-1-test", "sha-base")
	// Mark the ask-branch as having an Open draft PR at #42.
	db.Exec(`UPDATE ConvoyAskBranches SET draft_pr_number = 42, draft_pr_state = 'Open', draft_pr_url = ? WHERE convoy_id = ? AND repo = ?`,
		"https://github.com/acme/api/pull/42", cid, "api")
	return cid
}

// TestDogPRReviewPoll_InsertsBotAndHumanRows verifies that the dog fetches
// both endpoints, classifies authors correctly, records new comments, and
// queues exactly one PRReviewTriage task when new rows exist.
func TestDogPRReviewPoll_InsertsBotAndHumanRows(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := seedDraftPROpenConvoy(t, db)

	installGHStub(t, map[string]ghStubResp{
		// issue comments (PR-level): one bot, one human
		"api --paginate repos/acme/api/issues/42/comments": {
			stdout: `[
				{"id":100,"body":"LGTM overall","user":{"login":"claude[bot]","type":"Bot"},"created_at":"2026-04-22T12:00:00Z","html_url":"u"},
				{"id":101,"body":"Need to discuss X","user":{"login":"alice","type":"User"},"created_at":"2026-04-22T12:01:00Z","html_url":"u"}
			]`,
		},
		// review comments (inline code): one top-level bot comment + one reply in the same thread
		"api --paginate repos/acme/api/pulls/42/comments": {
			stdout: `[
				{"id":200,"node_id":"RC_top","body":"rename this","path":"main.go","line":10,"user":{"login":"gemini-code-assist[bot]","type":"Bot"},"pull_request_review_id":50,"in_reply_to_id":0},
				{"id":201,"node_id":"RC_reply","body":"thanks","path":"main.go","line":10,"user":{"login":"bob","type":"User"},"pull_request_review_id":51,"in_reply_to_id":200}
			]`,
		},
	})

	if err := dogPRReviewPoll(db, testLogger{}); err != nil {
		t.Fatalf("dog failed: %v", err)
	}

	// Four rows should have been inserted.
	var count int
	db.QueryRow(`SELECT COUNT(*) FROM PRReviewComments WHERE convoy_id = ?`, convoyID).Scan(&count)
	if count != 4 {
		t.Errorf("expected 4 PRReviewComments rows, got %d", count)
	}

	// Bot detection: 2 bots, 2 humans.
	var bots, humans int
	db.QueryRow(`SELECT COUNT(*) FROM PRReviewComments WHERE convoy_id = ? AND author_kind = 'bot'`, convoyID).Scan(&bots)
	db.QueryRow(`SELECT COUNT(*) FROM PRReviewComments WHERE convoy_id = ? AND author_kind = 'human'`, convoyID).Scan(&humans)
	if bots != 2 || humans != 2 {
		t.Errorf("bot/human split wrong: bots=%d humans=%d", bots, humans)
	}

	// Thread grouping: reply #201 shares a thread with top-level #200.
	var topThread, replyThread string
	db.QueryRow(`SELECT review_thread_id FROM PRReviewComments WHERE github_comment_id = 200`).Scan(&topThread)
	db.QueryRow(`SELECT review_thread_id FROM PRReviewComments WHERE github_comment_id = 201`).Scan(&replyThread)
	if topThread == "" || topThread != replyThread {
		t.Errorf("reply should inherit parent thread_id: top=%q reply=%q", topThread, replyThread)
	}

	// Issue comments share the synthetic "issue:42" thread.
	var issue100, issue101 string
	db.QueryRow(`SELECT review_thread_id FROM PRReviewComments WHERE github_comment_id = 100`).Scan(&issue100)
	db.QueryRow(`SELECT review_thread_id FROM PRReviewComments WHERE github_comment_id = 101`).Scan(&issue101)
	if issue100 != "issue:42" || issue101 != "issue:42" {
		t.Errorf("issue comments should share issue:42 thread: got %q, %q", issue100, issue101)
	}

	// Exactly one PRReviewTriage task should be queued.
	var triageCount int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type = 'PRReviewTriage' AND status = 'Pending'`).Scan(&triageCount)
	if triageCount != 1 {
		t.Errorf("expected 1 PRReviewTriage queued, got %d", triageCount)
	}
}

// TestDogPRReviewPoll_Idempotent verifies that a second run with the same
// comment payload does NOT insert duplicates and does NOT queue a second
// triage task (dedup works).
func TestDogPRReviewPoll_Idempotent(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := seedDraftPROpenConvoy(t, db)

	installGHStub(t, map[string]ghStubResp{
		"api --paginate repos/acme/api/issues/42/comments": {
			stdout: `[{"id":100,"body":"hi","user":{"login":"claude[bot]","type":"Bot"},"html_url":"u"}]`,
		},
		"api --paginate repos/acme/api/pulls/42/comments": {stdout: `[]`},
	})

	for i := 0; i < 3; i++ {
		if err := dogPRReviewPoll(db, testLogger{}); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}

	var rowCount, triageCount int
	db.QueryRow(`SELECT COUNT(*) FROM PRReviewComments WHERE convoy_id = ?`, convoyID).Scan(&rowCount)
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type = 'PRReviewTriage'`).Scan(&triageCount)

	if rowCount != 1 {
		t.Errorf("expected 1 row after 3 polls, got %d", rowCount)
	}
	if triageCount != 1 {
		t.Errorf("expected 1 triage task after 3 polls, got %d", triageCount)
	}
}

// TestDogPRReviewPoll_GlobalKillSwitch verifies that pr_review_enabled=0
// skips the entire poll (no gh calls, no rows).
func TestDogPRReviewPoll_GlobalKillSwitch(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	seedDraftPROpenConvoy(t, db)
	store.SetConfig(db, "pr_review_enabled", "0")

	stub := installGHStub(t, map[string]ghStubResp{
		// If these get called, the test should fail.
		"api --paginate repos/acme/api/issues/42/comments": {stdout: `[]`},
		"api --paginate repos/acme/api/pulls/42/comments":  {stdout: `[]`},
	})

	if err := dogPRReviewPoll(db, testLogger{}); err != nil {
		t.Fatalf("dog failed: %v", err)
	}

	if len(stub.calls) != 0 {
		t.Errorf("kill switch should prevent gh calls, got %d", len(stub.calls))
	}
	var triageCount int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type = 'PRReviewTriage'`).Scan(&triageCount)
	if triageCount != 0 {
		t.Errorf("no triage tasks expected when disabled, got %d", triageCount)
	}
}

// TestDogPRReviewPoll_PerRepoKillSwitch verifies that Repositories.pr_review_enabled=0
// on a repo skips that repo but not others.
func TestDogPRReviewPoll_PerRepoKillSwitch(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := seedDraftPROpenConvoy(t, db)
	db.Exec(`UPDATE Repositories SET pr_review_enabled = 0 WHERE name = 'api'`)

	installGHStub(t, map[string]ghStubResp{
		"api --paginate repos/acme/api/issues/42/comments": {stdout: `[{"id":100,"body":"x","user":{"login":"claude[bot]","type":"Bot"}}]`},
		"api --paginate repos/acme/api/pulls/42/comments":  {stdout: `[]`},
	})

	if err := dogPRReviewPoll(db, testLogger{}); err != nil {
		t.Fatalf("dog failed: %v", err)
	}

	var rowCount int
	db.QueryRow(`SELECT COUNT(*) FROM PRReviewComments WHERE convoy_id = ?`, convoyID).Scan(&rowCount)
	if rowCount != 0 {
		t.Errorf("repo kill switch should skip — expected 0 rows, got %d", rowCount)
	}
}

// TestDogPRReviewPoll_SkipsNonDraftPROpen verifies that convoys in other states
// (Shipped, Abandoned, Active) are ignored.
func TestDogPRReviewPoll_SkipsNonDraftPROpen(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := seedDraftPROpenConvoy(t, db)
	db.Exec(`UPDATE Convoys SET status = 'Shipped' WHERE id = ?`, convoyID)

	stub := installGHStub(t, map[string]ghStubResp{})

	if err := dogPRReviewPoll(db, testLogger{}); err != nil {
		t.Fatalf("dog failed: %v", err)
	}
	if len(stub.calls) != 0 {
		t.Errorf("Shipped convoys should be skipped, got %d gh calls", len(stub.calls))
	}
}

// TestLoadBotAllowlist_UsesDefaultWhenUnset checks the fallback path.
func TestLoadBotAllowlist_UsesDefaultWhenUnset(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	list := loadBotAllowlist(db)
	if len(list) == 0 {
		t.Fatal("expected non-empty default allowlist")
	}
	found := false
	for _, l := range list {
		if l == "claude[bot]" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("default allowlist missing claude[bot]: %v", list)
	}
}

func TestLoadBotAllowlist_UsesConfigOverride(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.SetConfig(db, "pr_review_bot_logins", "custombot, another-bot , still-one")
	list := loadBotAllowlist(db)
	if len(list) != 3 {
		t.Fatalf("expected 3 entries, got %d: %v", len(list), list)
	}
	want := map[string]bool{"custombot": false, "another-bot": false, "still-one": false}
	for _, l := range list {
		if _, ok := want[l]; ok {
			want[l] = true
		} else {
			t.Errorf("unexpected entry (should have been trimmed and split): %q", l)
		}
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("missing expected entry %q", k)
		}
	}
}

// TestQueuePRReviewTriage_Dedup verifies the dedup query handles id boundaries
// correctly (so convoy_id=1 doesn't falsely dedup against convoy_id=10 etc).
func TestQueuePRReviewTriage_Dedup(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Seed a pending triage for convoy 10. Must NOT dedup convoy 1.
	db.Exec(`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, priority, created_at)
		VALUES (0, '', 'PRReviewTriage', 'Pending', '{"convoy_id":10}', 4, datetime('now'))`)

	if err := queuePRReviewTriageIfAbsent(db, 1, testLogger{}); err != nil {
		t.Fatalf("queue failed: %v", err)
	}
	var count int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type = 'PRReviewTriage'`).Scan(&count)
	if count != 2 {
		t.Errorf("expected convoy 1 to queue separately from convoy 10; got %d total", count)
	}

	// Now queue again for convoy 1 — should dedup to same row.
	if err := queuePRReviewTriageIfAbsent(db, 1, testLogger{}); err != nil {
		t.Fatalf("second queue failed: %v", err)
	}
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type = 'PRReviewTriage'`).Scan(&count)
	if count != 2 {
		t.Errorf("dedup should prevent second insert for same convoy; got %d total", count)
	}

	// Verify convoy 1 row has the correct payload.
	var payload string
	db.QueryRow(`SELECT payload FROM BountyBoard WHERE type = 'PRReviewTriage' AND payload LIKE '%"convoy_id":1%' ORDER BY id DESC LIMIT 1`).Scan(&payload)
	if !strings.Contains(payload, `"convoy_id":1`) {
		t.Errorf("payload mismatch: %q", payload)
	}
}
