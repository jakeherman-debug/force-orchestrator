package dashboard

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// seedReviewRow inserts one PRReviewComment and returns its row ID.
func seedReviewRow(t *testing.T, db *sql.DB, convoyID int, kind, classification string) int {
	t.Helper()
	id, err := store.RecordPRComment(db, store.PRReviewComment{
		ConvoyID:        convoyID,
		Repo:            "api",
		DraftPRNumber:   42,
		GitHubCommentID: int64(id_nonce()),
		CommentType:     "review_comment",
		Author:          authorFor(kind),
		AuthorKind:      kind,
		Body:            "test body",
		Path:            "main.go",
		Line:            10,
		ReviewThreadID:  "review:test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if classification != "" {
		db.Exec(`UPDATE PRReviewComments SET classification = ? WHERE id = ?`, classification, id)
	}
	if classification == "human" {
		db.Exec(`UPDATE PRReviewComments SET reply_body = 'draft reply text' WHERE id = ?`, id)
	}
	return id
}

func authorFor(kind string) string {
	if kind == "bot" {
		return "claude[bot]"
	}
	return "alice"
}

var _nonce int64 = 5000

func id_nonce() int64 {
	_nonce++
	return _nonce
}

// ── list endpoint ──────────────────────────────────────────────────────────

func TestWriteConvoyPRReviewComments_ReturnsRows(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	cid, _ := store.CreateConvoy(db, "t")
	seedReviewRow(t, db, cid, "bot", "in_scope_fix")
	seedReviewRow(t, db, cid, "human", "human")

	r := httptest.NewRequest(http.MethodGet, "/api/convoys/"+toStr(cid)+"/pr-review-comments", nil)
	w := httptest.NewRecorder()
	handleConvoysSubroutes(db)(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Comments []DashboardPRReviewComment `json:"comments"`
		Total    int                        `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Total != 2 {
		t.Errorf("expected 2 comments, got %d", resp.Total)
	}
	// Human row must carry reply_body (draft).
	sawHumanDraft := false
	for _, c := range resp.Comments {
		if c.AuthorKind == "human" && c.ReplyBody != "" {
			sawHumanDraft = true
		}
	}
	if !sawHumanDraft {
		t.Error("expected human row to have reply_body")
	}
}

func TestWriteConvoyPRReviewComments_RequiresGET(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	cid, _ := store.CreateConvoy(db, "t")

	r := httptest.NewRequest(http.MethodPut, "/api/convoys/"+toStr(cid)+"/pr-review-comments", nil)
	w := httptest.NewRecorder()
	handleConvoysSubroutes(db)(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("PUT should be 405, got %d", w.Code)
	}
}

// ── retry endpoint ─────────────────────────────────────────────────────────

func TestWritePRReviewRetry_QueuesTask(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	cid, _ := store.CreateConvoy(db, "t")

	r := httptest.NewRequest(http.MethodPost, "/api/convoys/"+toStr(cid)+"/pr-review-retry", nil)
	w := httptest.NewRecorder()
	handleConvoysSubroutes(db)(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var count int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type = 'PRReviewTriage' AND payload LIKE '%convoy_id%'`).Scan(&count)
	if count != 1 {
		t.Errorf("expected 1 PRReviewTriage queued, got %d", count)
	}
}

// ── human action endpoints ─────────────────────────────────────────────────

func TestDismissHumanComment_SetsIgnored(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	cid, _ := store.CreateConvoy(db, "t")
	rowID := seedReviewRow(t, db, cid, "human", "human")

	r := httptest.NewRequest(http.MethodPost, "/api/pr-comments/"+toStr(rowID)+"/dismiss", nil)
	w := httptest.NewRecorder()
	handlePRCommentsSubroutes(db)(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var cls string
	db.QueryRow(`SELECT classification FROM PRReviewComments WHERE id = ?`, rowID).Scan(&cls)
	if cls != "ignored" {
		t.Errorf("classification = %q, want ignored", cls)
	}
}

func TestQueueFollowupFromComment_SpawnsFeature(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "api", "/tmp/api", "")
	cid, _ := store.CreateConvoy(db, "t")
	rowID := seedReviewRow(t, db, cid, "human", "human")

	r := httptest.NewRequest(http.MethodPost, "/api/pr-comments/"+toStr(rowID)+"/queue-followup", nil)
	w := httptest.NewRecorder()
	handlePRCommentsSubroutes(db)(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var ftype, frepo string
	db.QueryRow(`SELECT type, target_repo FROM BountyBoard WHERE type = 'Feature' ORDER BY id DESC LIMIT 1`).Scan(&ftype, &frepo)
	if ftype != "Feature" || frepo != "api" {
		t.Errorf("feature task fields wrong: type=%q repo=%q", ftype, frepo)
	}
	// Row should have spawned_task_id now.
	var spawned int
	db.QueryRow(`SELECT spawned_task_id FROM PRReviewComments WHERE id = ?`, rowID).Scan(&spawned)
	if spawned == 0 {
		t.Error("spawned_task_id should be populated")
	}
}

func TestPostHumanReply_RejectsBotComments(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "api", "/tmp/api", "")
	cid, _ := store.CreateConvoy(db, "t")
	rowID := seedReviewRow(t, db, cid, "bot", "in_scope_fix")

	r := httptest.NewRequest(http.MethodPost, "/api/pr-comments/"+toStr(rowID)+"/post-reply", nil)
	w := httptest.NewRecorder()
	handlePRCommentsSubroutes(db)(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("post-reply on bot comment should be 400, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "human") {
		t.Errorf("error should mention humans: %s", w.Body.String())
	}
}

func TestPostHumanReply_RejectsAlreadyReplied(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "api", "/tmp/api", "")
	cid, _ := store.CreateConvoy(db, "t")
	rowID := seedReviewRow(t, db, cid, "human", "human")
	db.Exec(`UPDATE PRReviewComments SET replied_at = '2026-01-01 00:00:00' WHERE id = ?`, rowID)

	r := httptest.NewRequest(http.MethodPost, "/api/pr-comments/"+toStr(rowID)+"/post-reply", nil)
	w := httptest.NewRecorder()
	handlePRCommentsSubroutes(db)(w, r)

	if w.Code != http.StatusConflict {
		t.Errorf("already-replied should be 409, got %d", w.Code)
	}
}

func TestPostHumanReply_UnknownRow(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	r := httptest.NewRequest(http.MethodPost, "/api/pr-comments/99999/dismiss", nil)
	w := httptest.NewRecorder()
	handlePRCommentsSubroutes(db)(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown row should be 404, got %d", w.Code)
	}
}

// ── helpers ────────────────────────────────────────────────────────────────

func toStr(n int) string {
	return intToString(n)
}

func intToString(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
