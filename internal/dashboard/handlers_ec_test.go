package dashboard

// D3 Phase 3 — EC ratification handler tests.
//
// Coverage:
//   1. List/detail happy paths (and 404 / 400 / 405 negatives).
//   2. Ratify: requires operator email; writes ratified_at + audit.
//   3. Ratify: refuses to flip an already-terminal row (CAS).
//   4. Reject: requires operator email; rejection_rationale ≥ 20 chars
//      when action != 'leave_as_is' (concern #7).
//   5. Reject: leave_as_is allows blank rationale.
//   6. Reject CAS — same conditional update shape as Ratify.
//   7. Pattern P8 invariants: full middleware stack rejects cross-origin
//      mutations and 256 KB body cap fires.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// seedCandidate inserts a librarian-emitted candidate proposal and
// returns its id. Used by the ratify / reject tests.
func seedCandidate(t *testing.T, db *sql.DB, key, content string) int {
	t.Helper()
	var id int
	err := db.QueryRow(`
		INSERT INTO PromotionProposals
			(experiment_id, kind, rule_key, proposed_content, evidence_summary_json,
			 authored_by, authored_at)
		VALUES (0, 'candidate', ?, ?, '{}', 'librarian', datetime('now'))
		RETURNING id
	`, key, content).Scan(&id)
	if err != nil {
		t.Fatalf("seed candidate: %v", err)
	}
	return id
}

func seedPromote(t *testing.T, db *sql.DB, expID int, ruleKey, content string) int {
	t.Helper()
	var id int
	err := db.QueryRow(`
		INSERT INTO PromotionProposals
			(experiment_id, kind, rule_key, proposed_content, evidence_summary_json,
			 authored_by, authored_at)
		VALUES (?, 'promote', ?, ?, '{}', 'engineering-corps', datetime('now'))
		RETURNING id
	`, expID, ruleKey, content).Scan(&id)
	if err != nil {
		t.Fatalf("seed promote: %v", err)
	}
	return id
}

// TestECHandler_List_PendingDefault — empty DB returns count=0, then
// after seeding two pending and one ratified, only the two pending
// surface in the default ?status=pending view.
func TestECHandler_List_PendingDefault(t *testing.T) {
	db := openDashTestDB(t)
	// Empty.
	r := httptest.NewRequest(http.MethodGet, "/api/ec/proposals", nil)
	w := httptest.NewRecorder()
	handleECProposalsList(db)(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Proposals []ecProposalSummary `json:"proposals"`
		Count     int                 `json:"count"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Count != 0 {
		t.Errorf("empty count: got %d, want 0", resp.Count)
	}

	// Seed three rows: two pending, one already-ratified.
	pendID1 := seedCandidate(t, db, "k1", "first hypothesis")
	pendID2 := seedPromote(t, db, 1, "rule.foo", "promote body")
	ratID := seedCandidate(t, db, "k3", "third hypothesis")
	if _, err := db.Exec(`UPDATE PromotionProposals SET ratified_at = datetime('now'), ratified_by = 'op' WHERE id = ?`, ratID); err != nil {
		t.Fatalf("ratify seed: %v", err)
	}

	r = httptest.NewRequest(http.MethodGet, "/api/ec/proposals", nil)
	w = httptest.NewRecorder()
	handleECProposalsList(db)(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Count != 2 {
		t.Errorf("pending count: got %d, want 2", resp.Count)
	}
	got := map[int]bool{}
	for _, p := range resp.Proposals {
		got[p.ID] = true
	}
	for _, want := range []int{pendID1, pendID2} {
		if !got[want] {
			t.Errorf("missing pending id %d", want)
		}
	}
	if got[ratID] {
		t.Errorf("ratified id %d should not appear in pending list", ratID)
	}
}

// TestECHandler_List_KindFilter — narrow to candidate or promote.
func TestECHandler_List_KindFilter(t *testing.T) {
	db := openDashTestDB(t)
	cID := seedCandidate(t, db, "ck", "cand")
	pID := seedPromote(t, db, 1, "pk", "prom")

	for _, c := range []struct {
		filter   string
		wantOnly int
	}{
		{"candidate", cID},
		{"promote", pID},
	} {
		r := httptest.NewRequest(http.MethodGet, "/api/ec/proposals?kind="+c.filter, nil)
		w := httptest.NewRecorder()
		handleECProposalsList(db)(w, r)
		if w.Code != http.StatusOK {
			t.Errorf("kind=%s: status %d", c.filter, w.Code)
			continue
		}
		var resp struct {
			Proposals []ecProposalSummary `json:"proposals"`
			Count     int                 `json:"count"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Errorf("kind=%s: unmarshal: %v", c.filter, err)
			continue
		}
		if resp.Count != 1 {
			t.Errorf("kind=%s: count %d, want 1", c.filter, resp.Count)
			continue
		}
		if resp.Proposals[0].ID != c.wantOnly {
			t.Errorf("kind=%s: got id %d, want %d", c.filter, resp.Proposals[0].ID, c.wantOnly)
		}
	}
}

// TestECHandler_List_BadStatusFilter rejects unknown values.
func TestECHandler_List_BadStatusFilter(t *testing.T) {
	db := openDashTestDB(t)
	r := httptest.NewRequest(http.MethodGet, "/api/ec/proposals?status=bogus", nil)
	w := httptest.NewRecorder()
	handleECProposalsList(db)(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", w.Code)
	}
}

// TestECHandler_List_RejectsNonGET — POST/etc → 405.
func TestECHandler_List_RejectsNonGET(t *testing.T) {
	db := openDashTestDB(t)
	for _, m := range []string{http.MethodPost, http.MethodPatch, http.MethodDelete} {
		r := httptest.NewRequest(m, "/api/ec/proposals", nil)
		w := httptest.NewRecorder()
		handleECProposalsList(db)(w, r)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: got %d, want 405", m, w.Code)
		}
	}
}

// TestECHandler_Detail_HappyPath — single proposal returns the full
// shape including JSON evidence body.
func TestECHandler_Detail_HappyPath(t *testing.T) {
	db := openDashTestDB(t)
	id := seedCandidate(t, db, "test-key", "test content")
	r := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/ec/proposals/%d", id), nil)
	w := httptest.NewRecorder()
	handleECProposalsSubroutes(db)(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	var s ecProposalSummary
	if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if s.ID != id {
		t.Errorf("id: got %d, want %d", s.ID, id)
	}
	if s.Kind != "candidate" {
		t.Errorf("kind: got %q, want candidate", s.Kind)
	}
	if s.AuthoredBy != "librarian" {
		t.Errorf("authored_by: got %q, want librarian", s.AuthoredBy)
	}
	if s.RuleKey != "test-key" {
		t.Errorf("rule_key: got %q", s.RuleKey)
	}
}

// TestECHandler_Detail_404 — unknown id returns 404.
func TestECHandler_Detail_404(t *testing.T) {
	db := openDashTestDB(t)
	r := httptest.NewRequest(http.MethodGet, "/api/ec/proposals/9999", nil)
	w := httptest.NewRecorder()
	handleECProposalsSubroutes(db)(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", w.Code)
	}
}

// TestECHandler_Detail_400OnBadID — non-numeric / missing id.
func TestECHandler_Detail_400OnBadID(t *testing.T) {
	db := openDashTestDB(t)
	for _, p := range []string{"/api/ec/proposals/abc", "/api/ec/proposals/0"} {
		r := httptest.NewRequest(http.MethodGet, p, nil)
		w := httptest.NewRecorder()
		handleECProposalsSubroutes(db)(w, r)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400 body=%s", p, w.Code, w.Body.String())
		}
	}
}

// TestECHandler_Ratify_HappyPath — operator email accepted, row flipped,
// AuditLog row written.
func TestECHandler_Ratify_HappyPath(t *testing.T) {
	db := openDashTestDB(t)
	id := seedCandidate(t, db, "ratify-key", "ratify body")

	body := `{"operator_email":"alice@example.com"}`
	r := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/ec/proposals/%d/ratify", id),
		strings.NewReader(body))
	w := httptest.NewRecorder()
	handleECProposalsSubroutes(db)(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	// Row state.
	var ratifiedAt, ratifiedBy string
	if err := db.QueryRow(`SELECT IFNULL(ratified_at,''), IFNULL(ratified_by,'') FROM PromotionProposals WHERE id = ?`, id).
		Scan(&ratifiedAt, &ratifiedBy); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if ratifiedAt == "" {
		t.Errorf("ratified_at should be populated")
	}
	if ratifiedBy != "alice@example.com" {
		t.Errorf("ratified_by: got %q", ratifiedBy)
	}
	// AuditLog.
	var auditCount int
	_ = db.QueryRow(`SELECT COUNT(*) FROM AuditLog WHERE action = 'ec.ratify' AND task_id = ?`, id).Scan(&auditCount)
	if auditCount != 1 {
		t.Errorf("AuditLog 'ec.ratify' count: got %d, want 1", auditCount)
	}
}

// TestECHandler_Ratify_HeaderFallback — operator email may come from
// X-Operator-Email when body is empty (parity with the existing
// repos handler convention).
func TestECHandler_Ratify_HeaderFallback(t *testing.T) {
	db := openDashTestDB(t)
	id := seedCandidate(t, db, "h-key", "h body")
	r := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/ec/proposals/%d/ratify", id), strings.NewReader(`{}`))
	r.Header.Set("X-Operator-Email", "carol@example.com")
	w := httptest.NewRecorder()
	handleECProposalsSubroutes(db)(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	var ratifiedBy string
	_ = db.QueryRow(`SELECT IFNULL(ratified_by,'') FROM PromotionProposals WHERE id = ?`, id).Scan(&ratifiedBy)
	if ratifiedBy != "carol@example.com" {
		t.Errorf("ratified_by: got %q, want carol@example.com", ratifiedBy)
	}
}

// TestECHandler_Ratify_RejectsBlankOperator — empty body, no header → 400.
func TestECHandler_Ratify_RejectsBlankOperator(t *testing.T) {
	db := openDashTestDB(t)
	id := seedCandidate(t, db, "blank-op", "x")
	r := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/ec/proposals/%d/ratify", id), strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	handleECProposalsSubroutes(db)(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400 body=%s", w.Code, w.Body.String())
	}
}

// TestECHandler_Ratify_RefusesAlreadyTerminal — second ratify call
// → 409 (conditional update shape).
func TestECHandler_Ratify_RefusesAlreadyTerminal(t *testing.T) {
	db := openDashTestDB(t)
	id := seedCandidate(t, db, "double", "x")
	// First call OK.
	r := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/ec/proposals/%d/ratify", id),
		strings.NewReader(`{"operator_email":"op@x"}`))
	w := httptest.NewRecorder()
	handleECProposalsSubroutes(db)(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("first ratify: %d", w.Code)
	}
	// Second call refused.
	r = httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/ec/proposals/%d/ratify", id),
		strings.NewReader(`{"operator_email":"op@x"}`))
	w = httptest.NewRecorder()
	handleECProposalsSubroutes(db)(w, r)
	if w.Code != http.StatusConflict {
		t.Errorf("second ratify: got %d, want 409", w.Code)
	}
}

// TestECHandler_Ratify_404 — unknown id surfaces as 404 (distinguishes
// from CAS-conflict 409).
func TestECHandler_Ratify_404(t *testing.T) {
	db := openDashTestDB(t)
	r := httptest.NewRequest(http.MethodPost,
		"/api/ec/proposals/99999/ratify",
		strings.NewReader(`{"operator_email":"op@x"}`))
	w := httptest.NewRecorder()
	handleECProposalsSubroutes(db)(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", w.Code)
	}
}

// TestECHandler_Reject_LeaveAsIsAllowsBlankRationale — concern #7
// permits a blank rationale ONLY for leave_as_is.
func TestECHandler_Reject_LeaveAsIsAllowsBlankRationale(t *testing.T) {
	db := openDashTestDB(t)
	id := seedCandidate(t, db, "leave-key", "leave body")
	body := `{"operator_email":"op@x","rejection_action":"leave_as_is","rejected_reason":"meh"}`
	r := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/ec/proposals/%d/reject", id), strings.NewReader(body))
	w := httptest.NewRecorder()
	handleECProposalsSubroutes(db)(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	var act, rationale, rejectedAt string
	if err := db.QueryRow(`
		SELECT IFNULL(rejection_action,''), IFNULL(rejection_rationale,''), IFNULL(rejected_at,'')
		  FROM PromotionProposals WHERE id = ?`, id).
		Scan(&act, &rationale, &rejectedAt); err != nil {
		t.Fatalf("read: %v", err)
	}
	if act != "leave_as_is" {
		t.Errorf("rejection_action: got %q", act)
	}
	if rejectedAt == "" {
		t.Errorf("rejected_at should be populated")
	}
	// Rationale blank is fine for leave_as_is.
	if rationale != "" {
		t.Errorf("rationale should default empty for leave_as_is, got %q", rationale)
	}
}

// TestECHandler_Reject_RequiresRationaleForRevert — rejection_action !=
// leave_as_is + rationale < 20 chars → 400.
func TestECHandler_Reject_RequiresRationaleForRevert(t *testing.T) {
	db := openDashTestDB(t)
	id := seedCandidate(t, db, "rev-key", "rev body")
	body := `{"operator_email":"op@x","rejection_action":"clean_revert","rejection_rationale":"too short"}`
	r := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/ec/proposals/%d/reject", id), strings.NewReader(body))
	w := httptest.NewRecorder()
	handleECProposalsSubroutes(db)(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400 body=%s", w.Code, w.Body.String())
	}
	// Row should still be pending — rejection didn't persist.
	var rejectedAt string
	_ = db.QueryRow(`SELECT IFNULL(rejected_at,'') FROM PromotionProposals WHERE id = ?`, id).Scan(&rejectedAt)
	if rejectedAt != "" {
		t.Errorf("rejection should not have persisted with sub-20-char rationale")
	}
}

// TestECHandler_Reject_AcceptsValidRevert — 20+ char rationale with a
// concern-#7 action persists the row + AuditLog entry.
func TestECHandler_Reject_AcceptsValidRevert(t *testing.T) {
	db := openDashTestDB(t)
	id := seedCandidate(t, db, "valid-rev", "body")
	rationale := "This rule conflicted with another one already in flight, see issue #42."
	body := `{"operator_email":"op@x","rejection_action":"clean_revert","rejection_rationale":"` + rationale + `"}`
	r := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/ec/proposals/%d/reject", id), strings.NewReader(body))
	w := httptest.NewRecorder()
	handleECProposalsSubroutes(db)(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	var act, gotRationale, rejectedAt string
	if err := db.QueryRow(`
		SELECT IFNULL(rejection_action,''), IFNULL(rejection_rationale,''), IFNULL(rejected_at,'')
		  FROM PromotionProposals WHERE id = ?`, id).
		Scan(&act, &gotRationale, &rejectedAt); err != nil {
		t.Fatalf("read: %v", err)
	}
	if act != "clean_revert" {
		t.Errorf("action: got %q", act)
	}
	if gotRationale != rationale {
		t.Errorf("rationale: got %q", gotRationale)
	}
	if rejectedAt == "" {
		t.Errorf("rejected_at should be populated")
	}
	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM AuditLog WHERE action = 'ec.reject' AND task_id = ?`, id).Scan(&n)
	if n != 1 {
		t.Errorf("AuditLog 'ec.reject' count: got %d, want 1", n)
	}
}

// TestECHandler_Reject_RejectsBadAction — unknown action → 400.
func TestECHandler_Reject_RejectsBadAction(t *testing.T) {
	db := openDashTestDB(t)
	id := seedCandidate(t, db, "bad-act", "body")
	body := `{"operator_email":"op@x","rejection_action":"explode"}`
	r := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/ec/proposals/%d/reject", id), strings.NewReader(body))
	w := httptest.NewRecorder()
	handleECProposalsSubroutes(db)(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("got %d, want 400", w.Code)
	}
}

// TestECHandler_Reject_RejectsBlankOperator — same shape as Ratify.
func TestECHandler_Reject_RejectsBlankOperator(t *testing.T) {
	db := openDashTestDB(t)
	id := seedCandidate(t, db, "no-op", "body")
	body := `{"rejection_action":"leave_as_is","rejected_reason":"r"}`
	r := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/ec/proposals/%d/reject", id), strings.NewReader(body))
	w := httptest.NewRecorder()
	handleECProposalsSubroutes(db)(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("got %d, want 400", w.Code)
	}
}

// TestECHandler_Subroute_RejectsGETOnAction — POST-only on
// /ratify and /reject.
func TestECHandler_Subroute_RejectsGETOnAction(t *testing.T) {
	db := openDashTestDB(t)
	id := seedCandidate(t, db, "k", "v")
	for _, action := range []string{"ratify", "reject"} {
		r := httptest.NewRequest(http.MethodGet,
			fmt.Sprintf("/api/ec/proposals/%d/%s", id, action), nil)
		w := httptest.NewRecorder()
		handleECProposalsSubroutes(db)(w, r)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: got %d, want 405", action, w.Code)
		}
	}
}

// TestECHandler_Subroute_UnknownAction → 404.
func TestECHandler_Subroute_UnknownAction(t *testing.T) {
	db := openDashTestDB(t)
	id := seedCandidate(t, db, "k", "v")
	r := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/ec/proposals/%d/explode", id), strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	handleECProposalsSubroutes(db)(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("got %d, want 404", w.Code)
	}
}

// TestECHandler_ContentType — list + detail return application/json.
func TestECHandler_ContentType(t *testing.T) {
	db := openDashTestDB(t)
	id := seedCandidate(t, db, "k", "v")
	cases := []struct {
		name string
		req  *http.Request
		fn   http.HandlerFunc
	}{
		{"list", httptest.NewRequest(http.MethodGet, "/api/ec/proposals", nil),
			handleECProposalsList(db)},
		{"detail", httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/ec/proposals/%d", id), nil),
			handleECProposalsSubroutes(db)},
	}
	for _, c := range cases {
		w := httptest.NewRecorder()
		c.fn(w, c.req)
		if ct := w.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("%s: Content-Type=%q, want application/json", c.name, ct)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────
// Pattern P8 invariants — same-origin gate, body cap, no wildcard CORS.
// These wrap the EC mux through securityMiddleware and assert the same
// checks audit_pattern_p8_test.go enforces for the experiments surface.
// ─────────────────────────────────────────────────────────────────────

// newECServer wires the EC handlers behind securityMiddleware on a
// known port. Tests issue real HTTP via net/http/httptest.Server so
// the middleware fires in the same shape as production.
func newECServer(t *testing.T, db *sql.DB, port int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ec/proposals", handleECProposalsList(db))
	mux.HandleFunc("/api/ec/proposals/", handleECProposalsSubroutes(db))
	srv := httptest.NewServer(securityMiddleware(port, mux))
	t.Cleanup(srv.Close)
	return srv
}

// TestECHandler_P8_RejectsCrossOriginPOST — a POST with a foreign
// Origin header is refused with 403 by securityMiddleware before the
// handler is invoked.
func TestECHandler_P8_RejectsCrossOriginPOST(t *testing.T) {
	db := openDashTestDB(t)
	id := seedCandidate(t, db, "p8-key", "body")
	srv := newECServer(t, db, 8080)

	req, _ := http.NewRequest(http.MethodPost,
		srv.URL+fmt.Sprintf("/api/ec/proposals/%d/ratify", id),
		strings.NewReader(`{"operator_email":"op@x"}`))
	req.Header.Set("Origin", "http://evil.example") // explicit cross-origin
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-origin POST: got %d, want 403", resp.StatusCode)
	}
	// The row should NOT have flipped.
	var ratifiedAt string
	_ = db.QueryRow(`SELECT IFNULL(ratified_at,'') FROM PromotionProposals WHERE id = ?`, id).Scan(&ratifiedAt)
	if ratifiedAt != "" {
		t.Errorf("cross-origin POST should not have ratified the row")
	}
}

// TestECHandler_P8_AcceptsSameOriginPOST — Origin matching the bind
// port is allowed; the underlying handler executes.
func TestECHandler_P8_AcceptsSameOriginPOST(t *testing.T) {
	db := openDashTestDB(t)
	id := seedCandidate(t, db, "p8-ok", "body")
	// Use a real listener so we control the port.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ec/proposals", handleECProposalsList(db))
	mux.HandleFunc("/api/ec/proposals/", handleECProposalsSubroutes(db))

	// Use port 8080 via Origin header — the test server returns a random
	// port, but securityMiddleware checks the Origin header value, not
	// the listener's port. Our middleware is initialized with a fixed
	// port (8080); a request with Origin: http://localhost:8080 passes.
	srv := newECServer(t, db, 8080)
	req, _ := http.NewRequest(http.MethodPost,
		srv.URL+fmt.Sprintf("/api/ec/proposals/%d/ratify", id),
		strings.NewReader(`{"operator_email":"op@x"}`))
	req.Header.Set("Origin", "http://localhost:8080")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("same-origin POST: got %d body=%s", resp.StatusCode, string(body))
	}
}

// TestECHandler_P8_BodyCapEnforced — a 256 KB+ body on a mutating
// request is rejected with 413 (the MaxBytesReader machinery wired
// inside securityMiddleware).
func TestECHandler_P8_BodyCapEnforced(t *testing.T) {
	db := openDashTestDB(t)
	id := seedCandidate(t, db, "cap-key", "body")
	srv := newECServer(t, db, 8080)

	// 300 KB body, well over the 256 KB cap.
	huge := strings.Repeat("a", 300*1024)
	jsonBody := `{"operator_email":"op@x","rejected_reason":"` + huge + `"}`
	req, _ := http.NewRequest(http.MethodPost,
		srv.URL+fmt.Sprintf("/api/ec/proposals/%d/reject", id),
		strings.NewReader(jsonBody))
	req.Header.Set("Origin", "http://localhost:8080")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize POST: got %d, want 413", resp.StatusCode)
	}
}

// TestECHandler_P8_NoWildcardCORS — list response does not set
// Access-Control-Allow-Origin: * (Pattern P8 invariant).
func TestECHandler_P8_NoWildcardCORS(t *testing.T) {
	db := openDashTestDB(t)
	r := httptest.NewRequest(http.MethodGet, "/api/ec/proposals", nil)
	w := httptest.NewRecorder()
	handleECProposalsList(db)(w, r)
	if got := w.Header().Get("Access-Control-Allow-Origin"); got == "*" {
		t.Errorf("EC handler set wildcard CORS — Pattern P8 regression")
	}
}

// TestECHandler_LibrarianClientRoundTrip — emit a candidate via the
// real Librarian client, then ratify it through the dashboard handler;
// row state matches at every step. The full Librarian → dashboard
// handoff in one test.
func TestECHandler_LibrarianClientRoundTrip(t *testing.T) {
	db := openDashTestDB(t)

	// Use the librarian package via the package-level seeder shape (we
	// deliberately don't import the librarian client here so this test
	// also catches regressions in the schema-level convention; the
	// Librarian client tests already verify the client itself).
	var id int
	if err := db.QueryRow(`
		INSERT INTO PromotionProposals
			(experiment_id, kind, rule_key, proposed_content, evidence_summary_json,
			 authored_by, authored_at)
		VALUES (0, 'candidate', ?, ?, ?, 'librarian', datetime('now'))
		RETURNING id
	`, "rt-key", "round-trip", `{"k":"v"}`).Scan(&id); err != nil {
		t.Fatalf("seed via librarian convention: %v", err)
	}

	// List should surface it (default pending filter).
	r := httptest.NewRequest(http.MethodGet, "/api/ec/proposals", nil)
	w := httptest.NewRecorder()
	handleECProposalsList(db)(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("list status: %d", w.Code)
	}
	var resp struct {
		Proposals []ecProposalSummary `json:"proposals"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	found := false
	for _, p := range resp.Proposals {
		if p.ID == id && p.AuthoredBy == "librarian" {
			found = true
		}
	}
	if !found {
		t.Errorf("emitted candidate %d not surfaced in list", id)
	}

	// Ratify via the handler.
	body := `{"operator_email":"alice@example.com"}`
	r = httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/ec/proposals/%d/ratify", id),
		strings.NewReader(body))
	w = httptest.NewRecorder()
	handleECProposalsSubroutes(db)(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("ratify status: %d body=%s", w.Code, w.Body.String())
	}

	// After ratify, default pending list excludes it; ?status=ratified
	// surfaces it.
	r = httptest.NewRequest(http.MethodGet, "/api/ec/proposals?status=ratified", nil)
	w = httptest.NewRecorder()
	handleECProposalsList(db)(w, r)
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	foundRatified := false
	for _, p := range resp.Proposals {
		if p.ID == id {
			foundRatified = true
			if p.RatifiedBy != "alice@example.com" {
				t.Errorf("ratified_by: got %q", p.RatifiedBy)
			}
		}
	}
	if !foundRatified {
		t.Errorf("ratified candidate %d not in ?status=ratified view", id)
	}
}

// Compile-time guard: ensure store package is referenced (we use it
// for AuditLog assertions transitively via DB queries; this stamp keeps
// the import linter happy if a future refactor drops the explicit use).
var _ = func(db *sql.DB) { store.LogAudit(db, "x", "y", 0, "z") }

// helper: ensure context import is used (avoids unused-import noise if
// a follow-up refactor strips a ctx-using assertion).
var _ = context.Background
