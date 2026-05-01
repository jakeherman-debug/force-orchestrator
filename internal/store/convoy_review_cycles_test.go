package store

// D3 fix-loop-1 / γ1 — ConvoyReviewCycles begin/complete/list tests.
//
// Covers (per CLAUDE.md "Testing rules" — happy path + each failure mode +
// idempotence):
//   - Begin → Complete roundtrip
//   - Frozen-spec invariant: spec mutates AFTER Begin → cycle still
//     evaluates against the bytes from begin time (even after a fresh
//     Begin, the prior cycle keeps its frozen snapshot)
//   - cycle_number monotonic per convoy (UNIQUE invariant)
//   - Double-completion is rejected (immutable outcomes invariant)
//   - List returns rows in ascending order

import (
	"strings"
	"testing"
)

func TestConvoyReviewCycles_BeginCompleteRoundtrip(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, err := CreateConvoy(db, "rt-convoy")
	if err != nil {
		t.Fatalf("CreateConvoy: %v", err)
	}
	specV1 := `{"ats":[{"id":"AT-1","description":"x"}]}`
	if _, err := db.Exec(`UPDATE Convoys SET verification_spec_json = ? WHERE id = ?`, specV1, convoyID); err != nil {
		t.Fatalf("seed spec: %v", err)
	}

	cycleID, frozen, err := BeginConvoyReviewCycle(db, convoyID)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if cycleID <= 0 {
		t.Fatalf("cycleID = %d", cycleID)
	}
	if frozen != specV1 {
		t.Fatalf("frozen spec mismatch: got %q want %q", frozen, specV1)
	}

	// Cycle should be visible with cycle_number=1, no completion stamp yet.
	cycles, err := ListCyclesForConvoy(db, convoyID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(cycles) != 1 {
		t.Fatalf("expected 1 cycle, got %d", len(cycles))
	}
	if cycles[0].CycleNumber != 1 {
		t.Errorf("cycle_number = %d, want 1", cycles[0].CycleNumber)
	}
	if cycles[0].SpecVersionAtStart != specV1 {
		t.Errorf("spec_version_at_start = %q, want %q", cycles[0].SpecVersionAtStart, specV1)
	}
	if cycles[0].CycleCompletedAt != "" {
		t.Errorf("cycle should not be completed yet, got cycle_completed_at=%q", cycles[0].CycleCompletedAt)
	}

	// Complete with verdict + outcomes + fix-task IDs.
	outcomes := `{"AT-1":"pass"}`
	if err := CompleteConvoyReviewCycle(db, cycleID, "clean", outcomes, []int{42, 43}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	cycles, _ = ListCyclesForConvoy(db, convoyID)
	if cycles[0].CycleCompletedAt == "" {
		t.Errorf("cycle_completed_at not stamped after Complete")
	}
	if !strings.Contains(cycles[0].OutcomesJSON, `"verdict":"clean"`) {
		t.Errorf("verdict not folded into outcomes_json: %s", cycles[0].OutcomesJSON)
	}
	if !strings.Contains(cycles[0].OutcomesJSON, `"AT-1":"pass"`) {
		t.Errorf("outcomes did not preserve original payload: %s", cycles[0].OutcomesJSON)
	}
	if cycles[0].FixTasksSpawnedJSON != "[42,43]" {
		t.Errorf("fix_tasks_spawned_json = %q, want [42,43]", cycles[0].FixTasksSpawnedJSON)
	}
}

func TestConvoyReviewCycles_FrozenSpecInvariant(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, _ := CreateConvoy(db, "frozen-convoy")
	specV1 := `{"ats":[{"id":"AT-1"}]}`
	specV2 := `{"ats":[{"id":"AT-1"},{"id":"AT-2"}]}`
	db.Exec(`UPDATE Convoys SET verification_spec_json = ? WHERE id = ?`, specV1, convoyID)

	// Begin cycle 1 against specV1.
	cycle1ID, frozen1, err := BeginConvoyReviewCycle(db, convoyID)
	if err != nil {
		t.Fatalf("Begin 1: %v", err)
	}
	if frozen1 != specV1 {
		t.Errorf("cycle 1 frozen spec = %q, want %q", frozen1, specV1)
	}

	// Operator-style spec mutation lands MID-CYCLE — concern #6 says cycle 1
	// must keep its V1 snapshot.
	db.Exec(`UPDATE Convoys SET verification_spec_json = ? WHERE id = ?`, specV2, convoyID)

	// Cycle 1's stored spec must still be V1.
	cycles, _ := ListCyclesForConvoy(db, convoyID)
	if cycles[0].SpecVersionAtStart != specV1 {
		t.Errorf("cycle 1 spec snapshot drifted under us: got %q want %q",
			cycles[0].SpecVersionAtStart, specV1)
	}

	// Complete cycle 1 — outcomes write does not mutate the spec snapshot.
	if err := CompleteConvoyReviewCycle(db, cycle1ID, "clean", `{}`, nil); err != nil {
		t.Fatalf("Complete 1: %v", err)
	}
	cycles, _ = ListCyclesForConvoy(db, convoyID)
	if cycles[0].SpecVersionAtStart != specV1 {
		t.Errorf("cycle 1 spec snapshot drifted post-complete: got %q want %q",
			cycles[0].SpecVersionAtStart, specV1)
	}

	// Begin cycle 2 — should pick up specV2 (the next-cycle pickup contract).
	_, frozen2, err := BeginConvoyReviewCycle(db, convoyID)
	if err != nil {
		t.Fatalf("Begin 2: %v", err)
	}
	if frozen2 != specV2 {
		t.Errorf("cycle 2 frozen spec = %q, want %q (mid-cycle amendment was supposed to land at next cycle)",
			frozen2, specV2)
	}

	cycles, _ = ListCyclesForConvoy(db, convoyID)
	if len(cycles) != 2 {
		t.Fatalf("expected 2 cycles, got %d", len(cycles))
	}
	if cycles[0].CycleNumber != 1 || cycles[1].CycleNumber != 2 {
		t.Errorf("cycle_number ordering: %d, %d", cycles[0].CycleNumber, cycles[1].CycleNumber)
	}
}

func TestConvoyReviewCycles_DoubleCompleteRejected(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, _ := CreateConvoy(db, "imm-convoy")
	cycleID, _, err := BeginConvoyReviewCycle(db, convoyID)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := CompleteConvoyReviewCycle(db, cycleID, "clean", `{}`, nil); err != nil {
		t.Fatalf("Complete 1: %v", err)
	}

	// Second completion must fail to protect the immutable-outcomes
	// invariant (concern #6 / roadmap line 1171).
	if err := CompleteConvoyReviewCycle(db, cycleID, "needs_work", `{"x":1}`, []int{99}); err == nil {
		t.Fatalf("expected error on double-complete; got nil")
	}

	// Outcomes must reflect the FIRST completion, not the second attempt.
	cycles, _ := ListCyclesForConvoy(db, convoyID)
	if !strings.Contains(cycles[0].OutcomesJSON, `"verdict":"clean"`) {
		t.Errorf("first verdict was overwritten on rejected double-complete: %s", cycles[0].OutcomesJSON)
	}
}

func TestConvoyReviewCycles_RejectsBadInputs(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	if _, _, err := BeginConvoyReviewCycle(db, 0); err == nil {
		t.Errorf("expected error on convoyID=0")
	}
	if _, _, err := BeginConvoyReviewCycle(db, -5); err == nil {
		t.Errorf("expected error on negative convoyID")
	}
	if err := CompleteConvoyReviewCycle(db, 0, "clean", "{}", nil); err == nil {
		t.Errorf("expected error on cycleID=0")
	}
	if err := CompleteConvoyReviewCycle(db, 1, "", "{}", nil); err == nil {
		t.Errorf("expected error on empty verdict")
	}
}

func TestConvoyReviewCycles_ListByConvoyOrdering(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	convoyA, _ := CreateConvoy(db, "convoy-A")
	convoyB, _ := CreateConvoy(db, "convoy-B")

	a1, _, _ := BeginConvoyReviewCycle(db, convoyA)
	CompleteConvoyReviewCycle(db, a1, "clean", "{}", nil)
	a2, _, _ := BeginConvoyReviewCycle(db, convoyA)
	CompleteConvoyReviewCycle(db, a2, "needs_work", `{"a":1}`, []int{1})
	b1, _, _ := BeginConvoyReviewCycle(db, convoyB)
	CompleteConvoyReviewCycle(db, b1, "clean", "{}", nil)

	cyclesA, _ := ListCyclesForConvoy(db, convoyA)
	if len(cyclesA) != 2 {
		t.Fatalf("convoy A: expected 2 cycles, got %d", len(cyclesA))
	}
	if cyclesA[0].CycleNumber != 1 || cyclesA[1].CycleNumber != 2 {
		t.Errorf("convoy A cycles not in ASC order: %d, %d", cyclesA[0].CycleNumber, cyclesA[1].CycleNumber)
	}
	cyclesB, _ := ListCyclesForConvoy(db, convoyB)
	if len(cyclesB) != 1 {
		t.Errorf("convoy B: expected 1 cycle, got %d", len(cyclesB))
	}
	// Cross-convoy isolation: B's cycle_number is 1, not 3.
	if cyclesB[0].CycleNumber != 1 {
		t.Errorf("convoy B cycle_number = %d, want 1 (per-convoy monotonic)", cyclesB[0].CycleNumber)
	}
}
