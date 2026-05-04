package store

// D10 — store-side coverage for PRHandoffSyntheses + the
// per-repo handoff_synthesis_enabled flag.
//
// Coverage:
//   - Default OFF: a freshly-registered repo has the flag at 0
//     (roadmap anti-cheat #1).
//   - Set/Get round-trip: SetHandoffSynthesisEnabled flips the flag
//     and HandoffSynthesisEnabled / GetRepo both surface the new
//     value.
//   - Insert + List round-trip: InsertPRHandoffSynthesis +
//     ListPRHandoffSynthesesForConvoy.

import (
	"testing"
)

// TestHandoffSynthesisEnabled_DefaultOff covers anti-cheat #1: a
// fresh repo defaults to handoff_synthesis_enabled=0 and GetRepo
// surfaces HandoffSynthesisEnabled=false.
func TestHandoffSynthesisEnabled_DefaultOff(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	AddRepo(db, "default-off", t.TempDir(), "test")
	if HandoffSynthesisEnabled(db, "default-off") {
		t.Errorf("expected default OFF; got true")
	}
	r := GetRepo(db, "default-off")
	if r == nil {
		t.Fatalf("GetRepo returned nil")
	}
	if r.HandoffSynthesisEnabled {
		t.Errorf("expected GetRepo HandoffSynthesisEnabled=false; got true")
	}
}

func TestHandoffSynthesisEnabled_FlipRoundTrip(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	AddRepo(db, "flip-repo", t.TempDir(), "test")
	if err := SetHandoffSynthesisEnabled(db, "flip-repo", true); err != nil {
		t.Fatalf("SetHandoffSynthesisEnabled(true): %v", err)
	}
	if !HandoffSynthesisEnabled(db, "flip-repo") {
		t.Errorf("expected enabled after flip; got false")
	}
	r := GetRepo(db, "flip-repo")
	if r == nil || !r.HandoffSynthesisEnabled {
		t.Errorf("expected GetRepo HandoffSynthesisEnabled=true; got %+v", r)
	}
	// Flip back off.
	if err := SetHandoffSynthesisEnabled(db, "flip-repo", false); err != nil {
		t.Fatalf("SetHandoffSynthesisEnabled(false): %v", err)
	}
	if HandoffSynthesisEnabled(db, "flip-repo") {
		t.Errorf("expected disabled after flip-back; got true")
	}
}

func TestSetHandoffSynthesisEnabled_UnknownRepoErrors(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	if err := SetHandoffSynthesisEnabled(db, "no-such-repo", true); err == nil {
		t.Errorf("expected error on unknown repo; got nil")
	}
}

func TestPRHandoffSynthesis_InsertAndList(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, err := CreateConvoy(db, "audit-trail-convoy")
	if err != nil {
		t.Fatalf("CreateConvoy: %v", err)
	}

	id1, err := InsertPRHandoffSynthesis(db, PRHandoffSynthesis{
		ConvoyID:      convoyID,
		PRURL:         "https://github.com/acme/foo/pull/1",
		ExperimentArm: "treatment_on",
	})
	if err != nil {
		t.Fatalf("InsertPRHandoffSynthesis #1: %v", err)
	}
	id2, err := InsertPRHandoffSynthesis(db, PRHandoffSynthesis{
		ConvoyID:  convoyID,
		PRURL:     "https://github.com/acme/foo/pull/2",
		CommentID: 12345,
	})
	if err != nil {
		t.Fatalf("InsertPRHandoffSynthesis #2: %v", err)
	}
	if id1 == id2 || id1 == 0 || id2 == 0 {
		t.Errorf("expected distinct positive IDs; got %d, %d", id1, id2)
	}

	rows, err := ListPRHandoffSynthesesForConvoy(db, convoyID)
	if err != nil {
		t.Fatalf("ListPRHandoffSynthesesForConvoy: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows; got %d", len(rows))
	}
	// Newest first.
	if rows[0].ID != id2 || rows[1].ID != id1 {
		t.Errorf("expected newest-first ordering; got %v / %v", rows[0].ID, rows[1].ID)
	}
	if rows[1].ExperimentArm != "treatment_on" {
		t.Errorf("ExperimentArm round-trip mismatch: got %q", rows[1].ExperimentArm)
	}
	if rows[0].CommentID != 12345 {
		t.Errorf("CommentID round-trip mismatch: got %d", rows[0].CommentID)
	}
	if rows[0].PostedAt == "" || rows[1].PostedAt == "" {
		t.Errorf("PostedAt should be auto-stamped via NowSQLite when empty; got %+v", rows)
	}
}

func TestInsertPRHandoffSynthesis_RejectsZeroConvoyID(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	if _, err := InsertPRHandoffSynthesis(db, PRHandoffSynthesis{
		PRURL: "https://example.com/pr",
	}); err == nil {
		t.Errorf("expected error on zero ConvoyID")
	}
}
