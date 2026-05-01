package store

// D3 fix-loop-1 / γ3 — DeprecateSpecItem unit tests.

import (
	"strings"
	"testing"
)

func TestDeprecateSpecItem_HappyPath(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, _ := CreateConvoy(db, "happy-spec")
	spec := `{"ats":[{"id":"AT-1","description":"add foo"},{"id":"AT-2","description":"add bar"}]}`
	db.Exec(`UPDATE Convoys SET verification_spec_json = ? WHERE id = ?`, spec, convoyID)

	args := DeprecateSpecItemArgs{
		ConvoyID:       convoyID,
		ItemID:         "AT-1",
		ItemKind:       SpecItemAT,
		Rationale:      "AT was a duplicate of AT-2 — operator caught it",
		RemovalKind:    "mistake",
		RemovedByEmail: "op@example.com",
	}
	if err := DeprecateSpecItem(db, args); err != nil {
		t.Fatalf("Deprecate: %v", err)
	}

	var out string
	db.QueryRow(`SELECT verification_spec_json FROM Convoys WHERE id = ?`, convoyID).Scan(&out)
	if !IsDeprecated(out, "AT-1") {
		t.Errorf("AT-1 not in deprecated[]: %s", out)
	}
	if strings.Contains(out, `"id":"AT-1"`) && !strings.Contains(out, `"at_id":"AT-1"`) {
		t.Errorf("AT-1 still appears as active spec entry — should have been moved: %s", out)
	}

	// spec_history_json should carry the deprecation event.
	var hist string
	db.QueryRow(`SELECT spec_history_json FROM Convoys WHERE id = ?`, convoyID).Scan(&hist)
	if !strings.Contains(hist, `"kind":"deprecate"`) {
		t.Errorf("spec_history_json missing deprecate event: %s", hist)
	}
	if !strings.Contains(hist, `"ratified_by_email":"op@example.com"`) {
		t.Errorf("spec_history_json missing operator email: %s", hist)
	}
}

func TestDeprecateSpecItem_ValidationErrors(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, _ := CreateConvoy(db, "v-spec")
	db.Exec(`UPDATE Convoys SET verification_spec_json = ? WHERE id = ?`,
		`{"ats":[{"id":"AT-1"}]}`, convoyID)

	cases := []struct {
		name string
		args DeprecateSpecItemArgs
	}{
		{"missing convoy", DeprecateSpecItemArgs{ItemID: "AT-1", ItemKind: SpecItemAT, Rationale: "twenty plus chars rationale here", RemovalKind: "mistake", RemovedByEmail: "op@x"}},
		{"missing itemID", DeprecateSpecItemArgs{ConvoyID: convoyID, ItemKind: SpecItemAT, Rationale: "twenty plus chars rationale here", RemovalKind: "mistake", RemovedByEmail: "op@x"}},
		{"bad item kind", DeprecateSpecItemArgs{ConvoyID: convoyID, ItemID: "AT-1", ItemKind: "wat", Rationale: "twenty plus chars rationale here", RemovalKind: "mistake", RemovedByEmail: "op@x"}},
		{"short rationale", DeprecateSpecItemArgs{ConvoyID: convoyID, ItemID: "AT-1", ItemKind: SpecItemAT, Rationale: "too short", RemovalKind: "mistake", RemovedByEmail: "op@x"}},
		{"bad removal kind", DeprecateSpecItemArgs{ConvoyID: convoyID, ItemID: "AT-1", ItemKind: SpecItemAT, Rationale: "twenty plus chars rationale here", RemovalKind: "totally-bogus", RemovedByEmail: "op@x"}},
		{"missing email (P21 guard)", DeprecateSpecItemArgs{ConvoyID: convoyID, ItemID: "AT-1", ItemKind: SpecItemAT, Rationale: "twenty plus chars rationale here", RemovalKind: "mistake", RemovedByEmail: ""}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := DeprecateSpecItem(db, c.args); err == nil {
				t.Errorf("expected error for %s", c.name)
			}
		})
	}
}

func TestDeprecateSpecItem_UnknownItem(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, _ := CreateConvoy(db, "u-spec")
	db.Exec(`UPDATE Convoys SET verification_spec_json = ? WHERE id = ?`,
		`{"ats":[{"id":"AT-1"}]}`, convoyID)

	err := DeprecateSpecItem(db, DeprecateSpecItemArgs{
		ConvoyID:       convoyID,
		ItemID:         "AT-999",
		ItemKind:       SpecItemAT,
		Rationale:      "twenty chars exactly here yes",
		RemovalKind:    "mistake",
		RemovedByEmail: "op@x",
	})
	if err == nil {
		t.Errorf("expected error on unknown item")
	}
}

func TestDeprecateSpecItem_Idempotent(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, _ := CreateConvoy(db, "i-spec")
	db.Exec(`UPDATE Convoys SET verification_spec_json = ? WHERE id = ?`,
		`{"ats":[{"id":"AT-1"}]}`, convoyID)

	args := DeprecateSpecItemArgs{
		ConvoyID:       convoyID,
		ItemID:         "AT-1",
		ItemKind:       SpecItemAT,
		Rationale:      "operator deprecation rationale here",
		RemovalKind:    "satisfied",
		RemovedByEmail: "op@x",
	}
	if err := DeprecateSpecItem(db, args); err != nil {
		t.Fatalf("first call: %v", err)
	}
	// Second call against an already-deprecated AT should be a no-op.
	if err := DeprecateSpecItem(db, args); err != nil {
		t.Errorf("second call should be idempotent, got %v", err)
	}

	var spec string
	db.QueryRow(`SELECT verification_spec_json FROM Convoys WHERE id = ?`, convoyID).Scan(&spec)
	// Count occurrences of "AT-1" in deprecated section — must be exactly 1.
	count := strings.Count(spec, `"at_id":"AT-1"`)
	if count != 1 {
		t.Errorf("expected exactly 1 deprecated entry for AT-1, got %d (spec=%s)", count, spec)
	}
}

func TestIsDeprecated(t *testing.T) {
	spec := `{"ats":[{"id":"AT-1"}],"deprecated":[{"at_id":"AT-9"}]}`
	if !IsDeprecated(spec, "AT-9") {
		t.Errorf("AT-9 should be deprecated")
	}
	if IsDeprecated(spec, "AT-1") {
		t.Errorf("AT-1 in active should not be flagged deprecated")
	}
	if IsDeprecated("", "AT-1") {
		t.Errorf("empty spec should not flag any AT deprecated")
	}
	if IsDeprecated("{not json", "AT-1") {
		t.Errorf("malformed spec should not flag any AT deprecated")
	}
}

func TestInflightTasksForAT_ScopedByConvoyAndAT(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	// Two convoys, both with a task spawned by AT-1; verify lookup is
	// correctly scoped (concern #8 — compound-key invariant).
	c1, _ := CreateConvoy(db, "c1")
	c2, _ := CreateConvoy(db, "c2")

	db.Exec(`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, convoy_id, priority, spawning_at_id, created_at)
		VALUES (0,'api','CodeEdit','Pending','x',?,5,'AT-1',datetime('now'))`, c1)
	db.Exec(`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, convoy_id, priority, spawning_at_id, created_at)
		VALUES (0,'api','CodeEdit','Pending','y',?,5,'AT-1',datetime('now'))`, c2)
	// Completed task for c1/AT-1 should NOT show up.
	db.Exec(`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, convoy_id, priority, spawning_at_id, created_at)
		VALUES (0,'api','CodeEdit','Completed','z',?,5,'AT-1',datetime('now'))`, c1)

	ids1, err := InflightTasksForAT(db, c1, "AT-1")
	if err != nil {
		t.Fatalf("Inflight c1: %v", err)
	}
	if len(ids1) != 1 {
		t.Errorf("c1/AT-1: expected 1 in-flight task, got %d", len(ids1))
	}
	ids2, _ := InflightTasksForAT(db, c2, "AT-1")
	if len(ids2) != 1 {
		t.Errorf("c2/AT-1: expected 1 in-flight task, got %d", len(ids2))
	}
	// Bare-at_id semantics would return 2 here; compound-key returns 1.
	idsOther, _ := InflightTasksForAT(db, c1, "AT-9")
	if len(idsOther) != 0 {
		t.Errorf("AT-9 lookup leaked rows: got %d", len(idsOther))
	}
}
