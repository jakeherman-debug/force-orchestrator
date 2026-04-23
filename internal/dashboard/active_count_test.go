package dashboard

// Campaign 2 — AUDIT-085: /api/stats ActiveCount must include every real
// in-flight status. Before the fix, it omitted Classifying,
// AwaitingChancellorReview, ConflictPending, and Planned — so the dashboard
// could read "0 active" while 50 tasks were mid-LLM-classification.
//
// This test seeds one BountyBoard row in each previously-omitted status +
// one in an already-counted status, hits /api/stats, and asserts ActiveCount
// equals the full union. If any of the four previously-missing statuses
// drops off the list again, this test fails pointing at which one.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"force-orchestrator/internal/store"
)

func TestAUDIT_085_ActiveCountCoversAllInFlightStatuses(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Every status that MUST be counted. The first four are the AUDIT-085
	// previously-missing set; the rest are pre-existing.
	inFlight := []string{
		// AUDIT-085 additions:
		"Classifying",
		"AwaitingChancellorReview",
		"ConflictPending",
		"Planned",
		// Already counted pre-fix:
		"Locked",
		"AwaitingCaptainReview",
		"UnderCaptainReview",
		"AwaitingCouncilReview",
		"UnderReview",
		"AwaitingSubPRCI",
	}
	// Non-counted statuses — must NOT affect ActiveCount.
	nonCounted := []string{
		"Pending", // counted separately as PendingCount
		"Completed",
		"Cancelled",
		"Failed",
		"Escalated",
	}

	for _, s := range inFlight {
		if _, err := db.Exec(`INSERT INTO BountyBoard
			(parent_id, target_repo, type, status, payload, priority, created_at)
			VALUES (0, 'api', 'CodeEdit', ?, ?, 5, datetime('now'))`,
			s, "seed "+s); err != nil {
			t.Fatalf("seed %s: %v", s, err)
		}
	}
	for _, s := range nonCounted {
		if _, err := db.Exec(`INSERT INTO BountyBoard
			(parent_id, target_repo, type, status, payload, priority, created_at)
			VALUES (0, 'api', 'CodeEdit', ?, ?, 5, datetime('now'))`,
			s, "seed "+s); err != nil {
			t.Fatalf("seed %s: %v", s, err)
		}
	}

	r := httptest.NewRequest(http.MethodGet, "/api/stats", nil)
	w := httptest.NewRecorder()
	handleStats(db)(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var got StatsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if got.ActiveCount != len(inFlight) {
		t.Errorf("AUDIT-085: ActiveCount=%d, expected %d (one per in-flight status). "+
			"The dashboard SQL at handleStats is omitting statuses.\nfull response tasks=%v",
			got.ActiveCount, len(inFlight), got.Tasks)
	}

	// Cross-check: the Tasks map should carry one row per status we seeded,
	// so we can pinpoint which status dropped off ActiveCount if it did.
	for _, s := range inFlight {
		if got.Tasks[s] != 1 {
			t.Errorf("Tasks[%q] = %d, expected 1 (seed should be present regardless of ActiveCount)",
				s, got.Tasks[s])
		}
	}
}
