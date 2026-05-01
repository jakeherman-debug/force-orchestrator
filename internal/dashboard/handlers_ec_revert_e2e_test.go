package dashboard

// handlers_ec_revert_e2e_test.go — D3 fix-loop-1 (slice δ).
//
// End-to-end coverage for the four revert / refile / discard paths
// of the EC reject handler (roadmap exit criterion 14b). The Static
// report flagged that the revert_task_id cross-reference,
// BountyBoard.deferred_revert cascade tracking, surgical-revert
// ConvoyReview re-trigger, and refiled_feature_id paths weren't
// exercised end-to-end anywhere.
//
// Each variant gets its own sub-test for clear failure attribution:
//
//   1. clean_revert    — spawns a RevertTask + links revert_task_id
//   2. surgical_revert — same task spawn shape; covers the
//      surgical-revert variant the schema permits
//   3. defer_revert    — spawns a RevertTask with
//      BountyBoard.deferred_revert=1 (operator queue picks it up
//      later)
//   4. refile          — opens a fresh ProposedFeatures row + links
//      refiled_feature_id back to the proposal
//
// We also keep a "leave_as_is" sub-test (the discard path) for
// completeness — confirms it does NOT spawn anything downstream.

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRevertE2E_CleanRevert exercises the clean_revert action:
// rejection → RevertTask spawned → revert_task_id linked.
func TestRevertE2E_CleanRevert(t *testing.T) {
	db := openDashTestDB(t)
	id := seedCandidate(t, db, "rule.fp.clean", "rule body")

	body := `{"operator_email":"op@x","rejection_action":"clean_revert","rejection_rationale":"this rule caused a regression in deploy guard"}`
	r := httptest.NewRequest(http.MethodPost,
		"/api/ec/proposals/"+itoaTest(id)+"/reject",
		bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	handleECProposalReject(db, id)(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}

	// Proposal column reflects the action.
	var act, rt int
	var actStr string
	db.QueryRow(`SELECT IFNULL(rejection_action,''), IFNULL(revert_task_id,0) FROM PromotionProposals WHERE id = ?`, id).Scan(&actStr, &rt)
	if actStr != "clean_revert" {
		t.Errorf("rejection_action: got %q want clean_revert", actStr)
	}
	if rt == 0 {
		t.Fatalf("revert_task_id was not linked on proposal %d", id)
	}
	_ = act

	// BountyBoard row exists for the spawned RevertTask.
	var btype, bstatus string
	var deferred int
	db.QueryRow(`SELECT type, status, IFNULL(deferred_revert,0) FROM BountyBoard WHERE id = ?`, rt).
		Scan(&btype, &bstatus, &deferred)
	if btype != "RevertTask" {
		t.Errorf("revert task type: got %q want RevertTask", btype)
	}
	if bstatus != "Pending" {
		t.Errorf("revert task status: got %q want Pending", bstatus)
	}
	if deferred != 0 {
		t.Errorf("clean_revert: deferred_revert flag should be 0; got %d", deferred)
	}

	// AuditLog has both ec.reject and ec.reject.revert-spawned entries.
	var auditCount int
	db.QueryRow(`SELECT COUNT(*) FROM AuditLog WHERE task_id = ? AND action IN ('ec.reject','ec.reject.revert-spawned')`, id).Scan(&auditCount)
	if auditCount < 2 {
		t.Errorf("audit log: want >=2 rows for rejection + revert-spawned; got %d", auditCount)
	}
}

// TestRevertE2E_SurgicalRevert covers the surgical_revert variant.
// Same downstream shape as clean_revert (the variant lives only in
// the BountyBoard.payload narrative — the ConvoyReview re-trigger
// reads payload to know how to scope the revert), but covers a
// different rejection_action value through the handler.
func TestRevertE2E_SurgicalRevert(t *testing.T) {
	db := openDashTestDB(t)
	id := seedCandidate(t, db, "rule.fp.surgical", "rule body")

	body := `{"operator_email":"op@x","rejection_action":"surgical_revert","rejection_rationale":"only the rate-limit clause; rest of rule still good"}`
	r := httptest.NewRequest(http.MethodPost,
		"/api/ec/proposals/"+itoaTest(id)+"/reject",
		bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	handleECProposalReject(db, id)(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}

	var rt int
	var payload string
	db.QueryRow(`SELECT IFNULL(revert_task_id,0) FROM PromotionProposals WHERE id = ?`, id).Scan(&rt)
	if rt == 0 {
		t.Fatalf("revert_task_id was not linked")
	}
	db.QueryRow(`SELECT payload FROM BountyBoard WHERE id = ?`, rt).Scan(&payload)
	if !contains(payload, "surgical_revert") {
		t.Errorf("revert payload should reference action; got %q", payload)
	}
}

// TestRevertE2E_DeferRevert covers the defer_revert action: the
// proposal flips rejected, a RevertTask row is spawned with
// deferred_revert=1, and revert_task_id is linked. The operator
// dashboard's deferred-queue picks this up later via
// `WHERE deferred_revert=1`.
func TestRevertE2E_DeferRevert(t *testing.T) {
	db := openDashTestDB(t)
	id := seedCandidate(t, db, "rule.fp.defer", "rule body")

	body := `{"operator_email":"op@x","rejection_action":"defer_revert","rejection_rationale":"agree this should revert; batching with the next deploy window on Monday"}`
	r := httptest.NewRequest(http.MethodPost,
		"/api/ec/proposals/"+itoaTest(id)+"/reject",
		bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	handleECProposalReject(db, id)(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}

	var actStr string
	var rt int
	db.QueryRow(`SELECT IFNULL(rejection_action,''), IFNULL(revert_task_id,0) FROM PromotionProposals WHERE id = ?`, id).Scan(&actStr, &rt)
	if actStr != "defer_revert" {
		t.Errorf("rejection_action: got %q want defer_revert", actStr)
	}
	if rt == 0 {
		t.Fatalf("revert_task_id was not linked on defer_revert")
	}
	var deferred int
	db.QueryRow(`SELECT IFNULL(deferred_revert,0) FROM BountyBoard WHERE id = ?`, rt).Scan(&deferred)
	if deferred != 1 {
		t.Errorf("defer_revert: BountyBoard.deferred_revert should be 1; got %d", deferred)
	}

	// Sanity: the deferred queue query that the operator dashboard
	// would run against this should return the row.
	var qCount int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE deferred_revert = 1`).Scan(&qCount)
	if qCount < 1 {
		t.Errorf("deferred-queue read: want >=1 row; got %d", qCount)
	}
}

// TestRevertE2E_Refile covers the refile action: the proposal is
// rejected and a fresh ProposedFeatures row is opened, with
// refiled_feature_id linked back from the proposal.
func TestRevertE2E_Refile(t *testing.T) {
	db := openDashTestDB(t)
	id := seedCandidate(t, db, "rule.fp.refile", "rule body")

	body := `{"operator_email":"op@x","rejection_action":"refile","rejection_rationale":"the underlying signal is real but the rule wording was off; refiling for re-author"}`
	r := httptest.NewRequest(http.MethodPost,
		"/api/ec/proposals/"+itoaTest(id)+"/reject",
		bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	handleECProposalReject(db, id)(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}

	var actStr string
	var refiled int
	db.QueryRow(`SELECT IFNULL(rejection_action,''), IFNULL(refiled_feature_id,0) FROM PromotionProposals WHERE id = ?`, id).Scan(&actStr, &refiled)
	if actStr != "refile" {
		t.Errorf("rejection_action: got %q want refile", actStr)
	}
	if refiled == 0 {
		t.Fatalf("refiled_feature_id was not linked on refile")
	}
	var summary, status string
	db.QueryRow(`SELECT IFNULL(observation_summary,''), IFNULL(status,'') FROM ProposedFeatures WHERE id = ?`, refiled).Scan(&summary, &status)
	if !contains(summary, "Refiled") {
		t.Errorf("refile summary: should mention 'Refiled'; got %q", summary)
	}
	if status != "pending" {
		t.Errorf("refile status: want pending; got %q", status)
	}

	// Audit chain: ec.reject + ec.reject.refile entries on the
	// proposal id.
	var auditCount int
	db.QueryRow(`SELECT COUNT(*) FROM AuditLog WHERE task_id = ? AND action IN ('ec.reject','ec.reject.refile')`, id).Scan(&auditCount)
	if auditCount < 2 {
		t.Errorf("audit log: want >=2 entries; got %d", auditCount)
	}
}

// TestRevertE2E_LeaveAsIs_NoSideEffects (the "discard" path) — the
// proposal is rejected but no RevertTask, no Escalation, no
// ProposedFeatures row gets spawned. A 5-min observation:
// applyRejectionSideEffects's leave_as_is branch returns immediately.
func TestRevertE2E_LeaveAsIs_NoSideEffects(t *testing.T) {
	db := openDashTestDB(t)
	id := seedCandidate(t, db, "rule.fp.leave", "rule body")

	body := `{"operator_email":"op@x","rejection_action":"leave_as_is"}`
	r := httptest.NewRequest(http.MethodPost,
		"/api/ec/proposals/"+itoaTest(id)+"/reject",
		bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	handleECProposalReject(db, id)(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}

	// Proposal flipped, but NO downstream rows.
	var rt, refiled int
	db.QueryRow(`SELECT IFNULL(revert_task_id,0), IFNULL(refiled_feature_id,0) FROM PromotionProposals WHERE id = ?`, id).Scan(&rt, &refiled)
	if rt != 0 {
		t.Errorf("leave_as_is: revert_task_id should be 0; got %d", rt)
	}
	if refiled != 0 {
		t.Errorf("leave_as_is: refiled_feature_id should be 0; got %d", refiled)
	}
	var bountyCount int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type = 'RevertTask'`).Scan(&bountyCount)
	if bountyCount != 0 {
		t.Errorf("leave_as_is: no RevertTask rows expected; got %d", bountyCount)
	}
	var featureCount int
	db.QueryRow(`SELECT COUNT(*) FROM ProposedFeatures`).Scan(&featureCount)
	if featureCount != 0 {
		t.Errorf("leave_as_is: no ProposedFeatures rows expected; got %d", featureCount)
	}
}

// TestRevertE2E_Escalate covers the escalate action: the proposal is
// rejected and an Escalations row is opened so the operator inbox
// surfaces the hard-block.
func TestRevertE2E_Escalate(t *testing.T) {
	db := openDashTestDB(t)
	id := seedCandidate(t, db, "rule.fp.escalate", "rule body")

	body := `{"operator_email":"op@x","rejection_action":"escalate","rejection_rationale":"this rule conflicts with our SLO commitment; needs ops/leadership review"}`
	r := httptest.NewRequest(http.MethodPost,
		"/api/ec/proposals/"+itoaTest(id)+"/reject",
		bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	handleECProposalReject(db, id)(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}

	var n int
	db.QueryRow(`SELECT COUNT(*) FROM Escalations WHERE task_id = ? AND status = 'Open'`, id).Scan(&n)
	if n < 1 {
		t.Errorf("escalate: want >=1 Open Escalation; got %d", n)
	}
}

// TestRevertE2E_Idempotence_ReSubmitDoesNotSpawnDuplicates — when the
// handler is hit twice for the same proposal, the second call's CAS
// fails (the row is already terminal). But to defend against a future
// "operator changed mind, retried" code path that did re-trigger
// side effects, the helpers themselves are idempotent. We verify
// that property by directly invoking applyRejectionSideEffects twice.
func TestRevertE2E_Idempotence_ReSubmitDoesNotSpawnDuplicates(t *testing.T) {
	db := openDashTestDB(t)
	id := seedCandidate(t, db, "rule.fp.idem", "rule body")

	// First call: clean_revert side-effect.
	applyRejectionSideEffects(testCtx(), db, id, "clean_revert", "rationale", "op@x")
	var n1 int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type='RevertTask'`).Scan(&n1)
	if n1 != 1 {
		t.Fatalf("after first call: want 1 RevertTask; got %d", n1)
	}

	// Second call: should be a no-op (revert_task_id != 0 already).
	applyRejectionSideEffects(testCtx(), db, id, "clean_revert", "rationale", "op@x")
	var n2 int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type='RevertTask'`).Scan(&n2)
	if n2 != 1 {
		t.Errorf("after second call: revert spawn should be idempotent; got %d (want 1)", n2)
	}

	// Same idempotence for refile.
	id2 := seedCandidate(t, db, "rule.fp.idem.refile", "rule body")
	applyRejectionSideEffects(testCtx(), db, id2, "refile", "rat", "op@x")
	applyRejectionSideEffects(testCtx(), db, id2, "refile", "rat", "op@x")
	var f int
	db.QueryRow(`SELECT COUNT(*) FROM ProposedFeatures`).Scan(&f)
	if f != 1 {
		t.Errorf("refile idempotence: want 1 ProposedFeatures; got %d", f)
	}
}

// TestRevertE2E_RevertTaskTriggersConvoyReviewRetrigger documents the
// downstream contract: a spawned RevertTask is a real BountyBoard row
// that the existing convoy-review-watch dog can pick up. We don't run
// the dog here; we just confirm the spawned row's shape matches what
// the dog/dispatcher expects.
//
// (The actual ConvoyReview re-trigger is exercised in
// dogConvoyReviewWatch tests; this test just confirms the proposal
// path produces a row with the correct status / payload / type that
// downstream watchers can claim.)
func TestRevertE2E_RevertTaskTriggersConvoyReviewRetrigger(t *testing.T) {
	db := openDashTestDB(t)
	id := seedCandidate(t, db, "rule.fp.retrigger", "rule body")

	body := `{"operator_email":"op@x","rejection_action":"clean_revert","rejection_rationale":"caused regression; full revert needed"}`
	r := httptest.NewRequest(http.MethodPost,
		"/api/ec/proposals/"+itoaTest(id)+"/reject",
		bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	handleECProposalReject(db, id)(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}

	// Confirm the spawned row is claimable: type='RevertTask',
	// status='Pending', non-zero id, deferred_revert=0.
	var rt int
	db.QueryRow(`SELECT revert_task_id FROM PromotionProposals WHERE id = ?`, id).Scan(&rt)
	var btype, bstatus string
	var deferred int
	db.QueryRow(`SELECT type, status, IFNULL(deferred_revert,0) FROM BountyBoard WHERE id = ?`, rt).
		Scan(&btype, &bstatus, &deferred)
	if btype != "RevertTask" || bstatus != "Pending" || deferred != 0 {
		t.Errorf("spawned row not claimable: type=%q status=%q deferred=%d (want RevertTask/Pending/0)",
			btype, bstatus, deferred)
	}
}

// helpers

// itoaTest is a copy of strconv.Itoa we keep local so the test file
// doesn't pull strconv just for one call. Go's strconv adds a trivial
// dependency but the existing handlers_ec_test.go pattern keeps
// helpers local.
func itoaTest(n int) string {
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

// contains is a non-importing strings.Contains. Same rationale.
func contains(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// testCtx returns the same ctx the httptest path uses — context.Background
// is fine for these unit-level invocations.
func testCtx() context.Context { return context.Background() }
