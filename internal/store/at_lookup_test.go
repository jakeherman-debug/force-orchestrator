package store

import (
	"database/sql"
	"errors"
	"testing"
)

// TestATLookup_CompoundKeyRequired_ConvoyID_Zero confirms the helper
// fails closed on a missing convoy_id — the whole point of the
// compound-key contract.
func TestATLookup_CompoundKeyRequired_ConvoyID_Zero(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	if _, err := GetATByID(db, 0, "AT-1"); !errors.Is(err, ErrConvoyIDRequired) {
		t.Errorf("convoy_id=0: want ErrConvoyIDRequired; got %v", err)
	}
	if _, err := GetATByID(db, -5, "AT-1"); !errors.Is(err, ErrConvoyIDRequired) {
		t.Errorf("convoy_id=-5: want ErrConvoyIDRequired; got %v", err)
	}
}

// TestATLookup_CompoundKeyRequired_ATID_Empty confirms an empty
// at_id is also fail-closed. Whitespace-only counts as empty.
func TestATLookup_CompoundKeyRequired_ATID_Empty(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	mustExecAT(t, db, `INSERT INTO Convoys (id, name, status) VALUES (1, 'c1', 'Active')`)

	for _, id := range []string{"", "   ", "\t\n"} {
		if _, err := GetATByID(db, 1, id); !errors.Is(err, ErrATIDRequired) {
			t.Errorf("at_id=%q: want ErrATIDRequired; got %v", id, err)
		}
	}
}

// TestATLookup_ConvoyMissing returns ErrConvoyMissing when the convoy
// id resolves to no row.
func TestATLookup_ConvoyMissing(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	if _, err := GetATByID(db, 9999, "AT-1"); !errors.Is(err, ErrConvoyMissing) {
		t.Errorf("missing convoy: want ErrConvoyMissing; got %v", err)
	}
}

// TestATLookup_SpecEmpty distinguishes "convoy exists but spec not
// authored" from "convoy exists with spec but at_id absent."
func TestATLookup_SpecEmpty(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	mustExecAT(t, db, `INSERT INTO Convoys (id, name, status) VALUES (1, 'c1', 'Active')`)
	if _, err := GetATByID(db, 1, "AT-1"); !errors.Is(err, ErrSpecEmpty) {
		t.Errorf("empty spec: want ErrSpecEmpty; got %v", err)
	}
}

// TestATLookup_ATNotFound_DistinctConvoy is the core of the compound-
// key contract. Two convoys both have an "AT-1" in their spec but
// the lookup against convoy 2 with at_id from convoy 1 must NOT
// silently resolve to convoy 1's row.
func TestATLookup_ATNotFound_DistinctConvoy(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	// Convoy 1: AT-1 = "title-A".
	mustExecAT(t, db, `INSERT INTO Convoys (id, name, status, verification_spec_json) VALUES (1, 'c1', 'Active', '{"acceptance_tests":[{"at_id":"AT-1","title":"title-A","rubric":"R-A"}]}')`)
	// Convoy 2: AT-2 only — NO AT-1 in this convoy's spec.
	mustExecAT(t, db, `INSERT INTO Convoys (id, name, status, verification_spec_json) VALUES (2, 'c2', 'Active', '{"acceptance_tests":[{"at_id":"AT-2","title":"title-B","rubric":"R-B"}]}')`)

	// Looking up AT-1 in convoy 2 must MISS — even though AT-1
	// exists in convoy 1, the compound-key contract forbids cross-
	// convoy resolution.
	_, err := GetATByID(db, 2, "AT-1")
	if !errors.Is(err, ErrATNotFound) {
		t.Errorf("compound-key isolation: convoy=2 at_id=AT-1 should ErrATNotFound; got %v", err)
	}

	// Looking up AT-1 in convoy 1 must SUCCEED.
	at, err := GetATByID(db, 1, "AT-1")
	if err != nil {
		t.Fatalf("convoy=1 at_id=AT-1: %v", err)
	}
	if at.Title != "title-A" {
		t.Errorf("convoy 1 AT-1 title: got %q want %q", at.Title, "title-A")
	}
}

// TestATLookup_HappyPath_AllFieldsRecovered confirms the full struct
// shape decodes correctly when the spec carries title + rubric +
// oracle_sql.
func TestATLookup_HappyPath_AllFieldsRecovered(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	mustExecAT(t, db, `INSERT INTO Convoys (id, name, status, verification_spec_json) VALUES (
		1, 'c1', 'Active',
		'{"acceptance_tests":[{"at_id":"AT-7","title":"latency under 200ms","rubric":"p99 measured at gateway","oracle_sql":"SELECT 1"}]}'
	)`)

	at, err := GetATByID(db, 1, "AT-7")
	if err != nil {
		t.Fatalf("happy path: %v", err)
	}
	if at.ATID != "AT-7" || at.Title != "latency under 200ms" || at.Rubric != "p99 measured at gateway" || at.OracleSQL != "SELECT 1" {
		t.Errorf("decoded shape mismatch: %+v", at)
	}
}

// TestATLookup_MalformedSpecJSON surfaces the parse error to the
// caller rather than silently returning ErrATNotFound — a corrupt
// spec is a real problem the caller must see.
func TestATLookup_MalformedSpecJSON(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	mustExecAT(t, db, `INSERT INTO Convoys (id, name, status, verification_spec_json) VALUES (1, 'c1', 'Active', '{not valid json')`)

	_, err := GetATByID(db, 1, "AT-1")
	if err == nil {
		t.Fatalf("malformed spec: expected an error")
	}
	if errors.Is(err, ErrATNotFound) || errors.Is(err, ErrSpecEmpty) {
		t.Errorf("malformed spec: should surface parse error, not ErrATNotFound/ErrSpecEmpty; got %v", err)
	}
}

// TestATLookup_ListATs_Empty returns nil slice + nil error when the
// convoy has no spec authored yet.
func TestATLookup_ListATs_Empty(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	mustExecAT(t, db, `INSERT INTO Convoys (id, name, status) VALUES (1, 'c1', 'Active')`)

	got, err := ListATsForConvoy(db, 1)
	if err != nil {
		t.Fatalf("list ATs (empty spec): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty spec: want zero ATs; got %v", got)
	}
}

// TestATLookup_ListATs_PreservesOrder returns ATs in the order
// declared in the JSON.
func TestATLookup_ListATs_PreservesOrder(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	mustExecAT(t, db, `INSERT INTO Convoys (id, name, status, verification_spec_json) VALUES (
		1, 'c1', 'Active',
		'{"acceptance_tests":[{"at_id":"AT-3","title":"third"},{"at_id":"AT-1","title":"first"},{"at_id":"AT-2","title":"second"}]}'
	)`)

	got, err := ListATsForConvoy(db, 1)
	if err != nil {
		t.Fatalf("list ATs: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 ATs; got %d", len(got))
	}
	want := []string{"AT-3", "AT-1", "AT-2"}
	for i, w := range want {
		if got[i].ATID != w {
			t.Errorf("idx %d: got %q want %q", i, got[i].ATID, w)
		}
	}
}

// TestATLookup_NilDB_Defensive ensures the nil-DB defensive branch
// returns an error instead of panicking.
func TestATLookup_NilDB_Defensive(t *testing.T) {
	if _, err := GetATByID(nil, 1, "AT-1"); err == nil {
		t.Errorf("nil db: want error; got nil")
	}
	if _, err := ListATsForConvoy(nil, 1); err == nil {
		t.Errorf("nil db: want error; got nil")
	}
}

// mustExecAT — local exec helper to keep the test setup terse.
func mustExecAT(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}
