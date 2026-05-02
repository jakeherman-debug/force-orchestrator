package supplydeferral

import (
	"testing"
	"time"

	"force-orchestrator/internal/isb/scanners/manifests"
	"force-orchestrator/internal/store"
)

func newDeferralPayload() DeferralPayload {
	return DeferralPayload{
		RuleKey:      "SUPPLY-001",
		ManifestPath: "Gemfile",
		Branch:       "feature/cool-thing",
		CommitSHA:    "abcdef1234567890",
		DeferredAt:   time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC),
		DepsAdded: []manifests.Dependency{
			{Ecosystem: manifests.EcosystemRubyGems, Name: "redis", Version: "5.0.0", Source: manifests.SourceDirect},
		},
	}
}

func TestRecordDeferral_RoundTrip(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id, err := RecordDeferral(db, 42, newDeferralPayload())
	if err != nil {
		t.Fatalf("RecordDeferral: %v", err)
	}
	if id == 0 {
		t.Fatal("expected non-zero id")
	}

	rows, err := ListPendingDeferrals(db, "feature/cool-thing")
	if err != nil {
		t.Fatalf("ListPendingDeferrals: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 deferral row, got %d", len(rows))
	}
	got := rows[0]
	if got.Payload.RuleKey != "SUPPLY-001" {
		t.Errorf("rule_key mismatch: %q", got.Payload.RuleKey)
	}
	if got.TaskID != 42 {
		t.Errorf("task id mismatch: %d", got.TaskID)
	}
	if len(got.Payload.DepsAdded) != 1 || got.Payload.DepsAdded[0].Name != "redis" {
		t.Errorf("deps round-trip failed: %+v", got.Payload.DepsAdded)
	}
}

func TestRecordDeferral_Idempotent_SameWindow(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	first, err := RecordDeferral(db, 1, newDeferralPayload())
	if err != nil || first == 0 {
		t.Fatalf("first insert: id=%d err=%v", first, err)
	}
	// Second call with same dep set must dedup → returns 0, no error.
	second, err := RecordDeferral(db, 1, newDeferralPayload())
	if err != nil {
		t.Fatalf("second insert: %v", err)
	}
	if second != 0 {
		t.Errorf("expected dedup (0), got new id %d", second)
	}

	rows, _ := ListPendingDeferrals(db, "feature/cool-thing")
	if len(rows) != 1 {
		t.Errorf("expected exactly one row after dedup, got %d", len(rows))
	}
}

func TestRecordDeferral_DifferentBranch_NewRow(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	a := newDeferralPayload()
	b := newDeferralPayload()
	b.Branch = "feature/other"

	if _, err := RecordDeferral(db, 1, a); err != nil {
		t.Fatalf("a: %v", err)
	}
	id, err := RecordDeferral(db, 1, b)
	if err != nil || id == 0 {
		t.Fatalf("b: id=%d err=%v", id, err)
	}

	rowsA, _ := ListPendingDeferrals(db, "feature/cool-thing")
	rowsB, _ := ListPendingDeferrals(db, "feature/other")
	if len(rowsA) != 1 || len(rowsB) != 1 {
		t.Errorf("branch filter mismatch: A=%d B=%d", len(rowsA), len(rowsB))
	}

	all, _ := ListPendingDeferrals(db, "")
	if len(all) != 2 {
		t.Errorf("all-branch listing should return 2, got %d", len(all))
	}
}

func TestRecordDeferral_Validation(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	if _, err := RecordDeferral(nil, 1, newDeferralPayload()); err == nil {
		t.Errorf("nil db should error")
	}
	bad := newDeferralPayload()
	bad.RuleKey = ""
	if _, err := RecordDeferral(db, 1, bad); err == nil {
		t.Errorf("empty rule_key should error")
	}
	bad2 := newDeferralPayload()
	bad2.Branch = ""
	if _, err := RecordDeferral(db, 1, bad2); err == nil {
		t.Errorf("empty branch should error")
	}
}

// TestListPendingDeferrals_FiltersByDisposition ensures rows that
// have been resolved/superseded/closed are NOT returned by the list.
func TestListPendingDeferrals_FiltersByDisposition(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id, err := RecordDeferral(db, 1, newDeferralPayload())
	if err != nil {
		t.Fatalf("RecordDeferral: %v", err)
	}

	// Flip to resolved_late simulating a successful re-check.
	if _, err := db.Exec(`UPDATE SecurityFindings SET disposition='resolved_late' WHERE id=?`, id); err != nil {
		t.Fatalf("flip: %v", err)
	}

	rows, err := ListPendingDeferrals(db, "")
	if err != nil {
		t.Fatalf("ListPendingDeferrals: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("resolved row should not appear, got %d", len(rows))
	}
}
