package store

import (
	"strings"
	"testing"
)

// TestInsertAndListSecurityFinding_HappyPath asserts a minimal insert
// roundtrips through ListSecurityFindings and surfaces every column.
func TestInsertAndListSecurityFinding_HappyPath(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	id, err := InsertSecurityFinding(db, SecurityFinding{
		TaskID:     42,
		Bureau:     "BoS",
		RuleID:     "BOS-001",
		Severity:   "advise",
		FilePath:   "internal/store/example.go",
		LineNumber: 17,
		Message:    "void-returning new mutator",
		CommitSHA:  "deadbeef",
	})
	if err != nil {
		t.Fatalf("InsertSecurityFinding: %v", err)
	}
	if id == 0 {
		t.Fatal("InsertSecurityFinding returned id=0")
	}
	got, err := ListSecurityFindings(db, 42)
	if err != nil {
		t.Fatalf("ListSecurityFindings: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListSecurityFindings: got %d rows, want 1", len(got))
	}
	row := got[0]
	if row.RuleID != "BOS-001" || row.Severity != "advise" || row.LineNumber != 17 {
		t.Errorf("roundtrip drift: got %+v", row)
	}
}

// TestInsertSecurityFinding_RequiresRuleID rejects empty rule ids — the
// failure is loud per CLAUDE.md No silent failures.
func TestInsertSecurityFinding_RequiresRuleID(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	_, err := InsertSecurityFinding(db, SecurityFinding{TaskID: 1})
	if err == nil {
		t.Fatal("InsertSecurityFinding(no RuleID): expected error, got nil")
	}
}

// TestSetDisposition_Overridden_RequiresAuditAndReason asserts the
// anti-cheat directive: a bypass without an AUDIT-NNN OR with a
// <10-char reason fails parse rather than being silently accepted.
func TestSetDisposition_Overridden_RequiresAuditAndReason(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	id, err := InsertSecurityFinding(db, SecurityFinding{
		TaskID: 1, RuleID: "BOS-002", Severity: "block",
	})
	if err != nil {
		t.Fatalf("InsertSecurityFinding: %v", err)
	}
	if err := SetDisposition(db, id, "overridden", "", "long enough reason text here"); err == nil {
		t.Error("SetDisposition(overridden, no audit): expected error, got nil")
	}
	if err := SetDisposition(db, id, "overridden", "AUDIT-001", "short"); err == nil {
		t.Error("SetDisposition(overridden, short reason): expected error, got nil")
	}
	if err := SetDisposition(db, id, "overridden", "AUDIT-001", "this is a fully-formed reason"); err != nil {
		t.Errorf("SetDisposition(overridden, valid): %v", err)
	}
}

// TestHasBlockingFindings_BypassDowngradesBlock asserts that an
// overridden block-severity finding no longer counts as blocking.
func TestHasBlockingFindings_BypassDowngradesBlock(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	id, err := InsertSecurityFinding(db, SecurityFinding{
		TaskID: 7, RuleID: "BOS-011", Severity: "block",
		Message: "concrete client struct construction",
	})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	blocking, err := HasBlockingFindings(db, 7)
	if err != nil {
		t.Fatalf("HasBlockingFindings: %v", err)
	}
	if !blocking {
		t.Fatal("expected blocking before override")
	}
	if err := SetDisposition(db, id, "overridden", "AUDIT-099", "Operator approved override pre-merge"); err != nil {
		t.Fatalf("SetDisposition: %v", err)
	}
	blocking, err = HasBlockingFindings(db, 7)
	if err != nil {
		t.Fatalf("HasBlockingFindings post-override: %v", err)
	}
	if blocking {
		t.Fatal("expected NOT blocking after override")
	}
}

// TestSetDisposition_NoSuchRow returns a non-nil error for a row that
// doesn't exist — silent no-op would be a regression.
func TestSetDisposition_NoSuchRow(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	err := SetDisposition(db, 99999, "resolved", "", "")
	if err == nil {
		t.Fatal("SetDisposition(no row): expected error, got nil")
	}
	if !strings.Contains(err.Error(), "no row") {
		t.Errorf("SetDisposition error message: got %q, want contains 'no row'", err.Error())
	}
}
