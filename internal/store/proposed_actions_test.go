package store

import (
	"database/sql"
	"strings"
	"testing"
)

// newProposedActionTestDB returns a fresh in-memory holocron — store
// tests never mock the DB per CLAUDE.md.
func newProposedActionTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db := InitHolocronDSN(":memory:")
	t.Cleanup(func() { db.Close() })
	return db
}

// TestSetGetProposedAction_Roundtrip — happy path.
func TestSetGetProposedAction_Roundtrip(t *testing.T) {
	db := newProposedActionTestDB(t)
	taskID := AddBounty(db, 0, "CodeEdit", "demo task")

	payload := ProposedAction{
		Action: "approve",
		CitedATs: []CitedAT{
			{ConvoyID: 1, ATID: "AT-001"},
			{ConvoyID: 1, ATID: "AT-002"},
		},
		CitedFleetRules:          []string{"rule.foo", "rule.bar"},
		SpecLink:                 "convoy-1/section-A",
		ClassificationConfidence: 0.85,
		Rationale:                "Spawn satisfies AT-001 and AT-002 because the diff implements both helpers.",
		DraftAmendment:           "",
		Alternative:              "",
	}
	if err := SetProposedAction(db, taskID, payload); err != nil {
		t.Fatalf("SetProposedAction: %v", err)
	}
	got, ok, err := GetProposedAction(db, taskID)
	if err != nil {
		t.Fatalf("GetProposedAction: %v", err)
	}
	if !ok {
		t.Fatal("expected payload present, got empty")
	}
	if got.Action != "approve" || got.ClassificationConfidence != 0.85 {
		t.Errorf("roundtrip mismatch: %+v", got)
	}
	if len(got.CitedATs) != 2 || got.CitedATs[0].ATID != "AT-001" {
		t.Errorf("CitedATs not preserved: %+v", got.CitedATs)
	}
	if len(got.CitedFleetRules) != 2 {
		t.Errorf("CitedFleetRules not preserved: %+v", got.CitedFleetRules)
	}
}

// TestGetProposedAction_EmptyRow — no proposed action emitted yet.
func TestGetProposedAction_EmptyRow(t *testing.T) {
	db := newProposedActionTestDB(t)
	taskID := AddBounty(db, 0, "CodeEdit", "demo task")

	got, ok, err := GetProposedAction(db, taskID)
	if err != nil {
		t.Fatalf("GetProposedAction: %v", err)
	}
	if ok {
		t.Errorf("expected empty, got %+v", got)
	}
}

// TestGetProposedAction_NoRow — task ID does not exist.
func TestGetProposedAction_NoRow(t *testing.T) {
	db := newProposedActionTestDB(t)
	got, ok, err := GetProposedAction(db, 99999)
	if err != nil {
		t.Fatalf("GetProposedAction: %v", err)
	}
	if ok {
		t.Errorf("expected empty for missing row, got %+v", got)
	}
}

// TestSetProposedAction_TaskNotFound — write to non-existent task ID.
func TestSetProposedAction_TaskNotFound(t *testing.T) {
	db := newProposedActionTestDB(t)
	payload := ProposedAction{
		Action:                   "approve",
		CitedATs:                 []CitedAT{},
		CitedFleetRules:          []string{},
		ClassificationConfidence: 0.5,
		Rationale:                "ok",
	}
	err := SetProposedAction(db, 99999, payload)
	if err == nil {
		t.Fatal("expected error for missing task, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' error, got %v", err)
	}
}

// TestValidateProposedAction_InvalidAction — must be one of the allowed verbs.
func TestValidateProposedAction_InvalidAction(t *testing.T) {
	tests := []struct {
		name   string
		action string
		ok     bool
	}{
		{"approve_ok", "approve", true},
		{"reject_ok", "reject", true},
		{"fix_ok", "fix", true},
		{"escalate_ok", "escalate", true},
		{"empty_rejects", "", false},
		{"bogus_rejects", "yeet", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := ProposedAction{
				Action:                   tc.action,
				CitedATs:                 []CitedAT{},
				CitedFleetRules:          []string{},
				ClassificationConfidence: 0.5,
				Rationale:                "ok",
			}
			err := ValidateProposedAction(p)
			if tc.ok && err != nil {
				t.Errorf("expected ok, got %v", err)
			}
			if !tc.ok && err == nil {
				t.Errorf("expected error for action %q, got nil", tc.action)
			}
		})
	}
}

// TestValidateProposedAction_ConfidenceRange — 0.0..1.0 enforced.
func TestValidateProposedAction_ConfidenceRange(t *testing.T) {
	tests := []struct {
		name string
		conf float64
		ok   bool
	}{
		{"zero_ok", 0.0, true},
		{"half_ok", 0.5, true},
		{"one_ok", 1.0, true},
		{"negative_rejects", -0.1, false},
		{"over_one_rejects", 1.1, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := ProposedAction{
				Action:                   "approve",
				CitedATs:                 []CitedAT{},
				CitedFleetRules:          []string{},
				ClassificationConfidence: tc.conf,
				Rationale:                "ok",
			}
			err := ValidateProposedAction(p)
			if tc.ok && err != nil {
				t.Errorf("expected ok, got %v", err)
			}
			if !tc.ok && err == nil {
				t.Errorf("expected error for conf %f, got nil", tc.conf)
			}
		})
	}
}

// TestValidateProposedAction_NilSlicesRejected — Pattern P23.
func TestValidateProposedAction_NilSlicesRejected(t *testing.T) {
	p := ProposedAction{
		Action:                   "approve",
		CitedATs:                 nil,
		CitedFleetRules:          []string{},
		ClassificationConfidence: 0.5,
		Rationale:                "ok",
	}
	if err := ValidateProposedAction(p); err == nil {
		t.Error("expected error for nil CitedATs")
	}
	p.CitedATs = []CitedAT{}
	p.CitedFleetRules = nil
	if err := ValidateProposedAction(p); err == nil {
		t.Error("expected error for nil CitedFleetRules")
	}
}

// TestValidateProposedAction_BareATIDRejected — Pattern P20: convoy-scoped.
func TestValidateProposedAction_BareATIDRejected(t *testing.T) {
	p := ProposedAction{
		Action: "approve",
		CitedATs: []CitedAT{
			{ConvoyID: 0, ATID: "AT-001"}, // missing convoy_id
		},
		CitedFleetRules:          []string{},
		ClassificationConfidence: 0.5,
		Rationale:                "AT-001",
	}
	err := ValidateProposedAction(p)
	if err == nil || !strings.Contains(err.Error(), "P20") {
		t.Errorf("expected P20 error, got %v", err)
	}
}

// TestValidateProposedAction_OrphanProseRefRejected — concern #1: every
// prose AT-NNN token must appear in cited_ats.
func TestValidateProposedAction_OrphanProseRefRejected(t *testing.T) {
	p := ProposedAction{
		Action: "approve",
		CitedATs: []CitedAT{
			{ConvoyID: 1, ATID: "AT-001"},
		},
		CitedFleetRules:          []string{},
		ClassificationConfidence: 0.5,
		// AT-002 is in prose but NOT in cited_ats.
		Rationale: "Spawn satisfies AT-001 and AT-002.",
	}
	err := ValidateProposedAction(p)
	if err == nil || !strings.Contains(err.Error(), "AT-002") {
		t.Errorf("expected orphan-ref error mentioning AT-002, got %v", err)
	}
}

// TestValidateProposedAction_ProseRefMatchesCited — happy path.
func TestValidateProposedAction_ProseRefMatchesCited(t *testing.T) {
	p := ProposedAction{
		Action: "approve",
		CitedATs: []CitedAT{
			{ConvoyID: 1, ATID: "AT-001"},
			{ConvoyID: 1, ATID: "AT-002"},
		},
		CitedFleetRules:          []string{},
		ClassificationConfidence: 0.5,
		Rationale:                "Spawn satisfies AT-001 and AT-002.",
	}
	if err := ValidateProposedAction(p); err != nil {
		t.Errorf("expected ok, got %v", err)
	}
}

// TestSetProposedAction_Idempotent — write twice, last write wins.
func TestSetProposedAction_Idempotent(t *testing.T) {
	db := newProposedActionTestDB(t)
	taskID := AddBounty(db, 0, "CodeEdit", "demo task")

	payload := ProposedAction{
		Action:                   "approve",
		CitedATs:                 []CitedAT{},
		CitedFleetRules:          []string{},
		ClassificationConfidence: 0.5,
		Rationale:                "first",
	}
	if err := SetProposedAction(db, taskID, payload); err != nil {
		t.Fatalf("first set: %v", err)
	}
	payload.Rationale = "second"
	if err := SetProposedAction(db, taskID, payload); err != nil {
		t.Fatalf("second set: %v", err)
	}
	got, _, err := GetProposedAction(db, taskID)
	if err != nil {
		t.Fatalf("GetProposedAction: %v", err)
	}
	if got.Rationale != "second" {
		t.Errorf("expected last-write-wins, got %q", got.Rationale)
	}
}

// TestSetProposedAction_ValidationErrorPropagated — bad payload never writes.
func TestSetProposedAction_ValidationErrorPropagated(t *testing.T) {
	db := newProposedActionTestDB(t)
	taskID := AddBounty(db, 0, "CodeEdit", "demo task")

	bad := ProposedAction{
		Action:                   "yeet",
		CitedATs:                 []CitedAT{},
		CitedFleetRules:          []string{},
		ClassificationConfidence: 0.5,
		Rationale:                "ok",
	}
	if err := SetProposedAction(db, taskID, bad); err == nil {
		t.Error("expected validation error")
	}
	got, ok, _ := GetProposedAction(db, taskID)
	if ok {
		t.Errorf("expected empty after failed write, got %+v", got)
	}
}
