package store

import (
	"database/sql"
	"strings"
	"testing"
	"time"
)

func newProposedFeaturesTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db := InitHolocronDSN(":memory:")
	t.Cleanup(func() { db.Close() })
	return db
}

// TestFingerprint_Deterministic — Pattern P22 contract: same input,
// byte-equal output across calls.
func TestFingerprint_Deterministic(t *testing.T) {
	a := Fingerprint("investigator", "missing test coverage",
		[]string{"a.go", "b.go"},
		[]string{"AT-001"},
		[]string{"rule.foo"})
	b := Fingerprint("investigator", "missing test coverage",
		[]string{"a.go", "b.go"},
		[]string{"AT-001"},
		[]string{"rule.foo"})
	if a != b {
		t.Errorf("expected identical fingerprints; got %q vs %q", a, b)
	}
	if len(a) != 64 {
		t.Errorf("expected 64-char hex SHA256, got %d chars: %q", len(a), a)
	}
}

// TestFingerprint_OrderInvariant — slice order doesn't change the hash
// (Pattern P22: canonical input requires sorted slices).
func TestFingerprint_OrderInvariant(t *testing.T) {
	a := Fingerprint("investigator", "topic",
		[]string{"a.go", "b.go", "c.go"},
		[]string{"AT-002", "AT-001"},
		[]string{"rule.b", "rule.a"})
	b := Fingerprint("investigator", "topic",
		[]string{"c.go", "a.go", "b.go"},
		[]string{"AT-001", "AT-002"},
		[]string{"rule.a", "rule.b"})
	if a != b {
		t.Errorf("fingerprint sensitive to slice order: %q vs %q", a, b)
	}
}

// TestFingerprint_CaseInsensitive — normalised inputs.
func TestFingerprint_CaseInsensitive(t *testing.T) {
	a := Fingerprint("Investigator", "Topic", []string{"A.go"}, []string{"AT-001"}, []string{"rule.X"})
	b := Fingerprint("investigator", "topic", []string{"a.go"}, []string{"at-001"}, []string{"rule.x"})
	if a != b {
		t.Errorf("expected case-insensitive match; got %q vs %q", a, b)
	}
}

// TestFingerprint_DifferentInputs — different sources or topics yield
// different fingerprints.
func TestFingerprint_DifferentInputs(t *testing.T) {
	base := Fingerprint("inv", "topic-1", []string{"a.go"}, nil, nil)
	tests := []struct {
		name string
		fp   string
	}{
		{"different_source", Fingerprint("captain", "topic-1", []string{"a.go"}, nil, nil)},
		{"different_topic", Fingerprint("inv", "topic-2", []string{"a.go"}, nil, nil)},
		{"different_paths", Fingerprint("inv", "topic-1", []string{"b.go"}, nil, nil)},
		{"different_at_refs", Fingerprint("inv", "topic-1", []string{"a.go"}, []string{"AT-001"}, nil)},
		{"different_rule_refs", Fingerprint("inv", "topic-1", []string{"a.go"}, nil, []string{"rule.x"})},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.fp == base {
				t.Errorf("expected distinct fingerprint for %s", tc.name)
			}
		})
	}
}

// TestFingerprint_NoTimestampDrift — Pattern P22 contract: even if
// time.Now() is called between the two calls, hashes must be byte-equal.
// Catches accidental inclusion of timestamps in the canonical input.
func TestFingerprint_NoTimestampDrift(t *testing.T) {
	a := Fingerprint("inv", "topic", []string{"a.go"}, nil, nil)
	time.Sleep(10 * time.Millisecond)
	b := Fingerprint("inv", "topic", []string{"a.go"}, nil, nil)
	if a != b {
		t.Errorf("fingerprint drifted with time: %q vs %q (timestamps must be excluded)", a, b)
	}
}

// TestEmitProposedFeature_HappyPath — fresh insert.
func TestEmitProposedFeature_HappyPath(t *testing.T) {
	db := newProposedFeaturesTestDB(t)

	res, err := EmitProposedFeature(db, ProposedFeaturePayload{
		ObservationSummary: "Two convoys missed the same edge case",
		Category:           "missing_test",
		Source:             "investigator",
		SourceObservations: []SourceObservation{
			{Kind: "convoy", Ref: "12", Note: "first sighting"},
		},
		CodePaths:           []string{"internal/foo/bar.go"},
		Topic:               "edge-case-x",
		ValueScore:          "high",
		ComplexityScore:     "low",
		ValueRationale:      "blocks shipping",
		ComplexityRationale: "one-line fix",
		ScoredBy:            "investigator-v1",
	})
	if err != nil {
		t.Fatalf("EmitProposedFeature: %v", err)
	}
	if !res.Inserted {
		t.Errorf("expected fresh insert, got merge")
	}
	if res.FeatureID == 0 {
		t.Errorf("expected non-zero feature id")
	}
	if res.Suppressed {
		t.Errorf("expected not suppressed")
	}
}

// TestEmitProposedFeature_DedupViaOnConflict — second emit with same
// canonical input bumps occurrence_count instead of inserting.
func TestEmitProposedFeature_DedupViaOnConflict(t *testing.T) {
	db := newProposedFeaturesTestDB(t)

	payload := ProposedFeaturePayload{
		ObservationSummary: "duplicate observation",
		Category:           "missing_test",
		Source:             "investigator",
		CodePaths:          []string{"foo.go"},
		Topic:              "edge-case-x",
		ValueScore:         "medium",
		ComplexityScore:    "medium",
	}
	first, err := EmitProposedFeature(db, payload)
	if err != nil {
		t.Fatalf("first emit: %v", err)
	}
	if !first.Inserted {
		t.Fatal("first emit should insert")
	}

	second, err := EmitProposedFeature(db, payload)
	if err != nil {
		t.Fatalf("second emit: %v", err)
	}
	if second.Inserted {
		t.Errorf("second emit should merge, not insert")
	}
	if second.FeatureID != first.FeatureID {
		t.Errorf("expected same feature id %d, got %d", first.FeatureID, second.FeatureID)
	}

	// Verify occurrence_count bumped.
	var count int
	db.QueryRow(`SELECT occurrence_count FROM ProposedFeatures WHERE id = ?`, first.FeatureID).Scan(&count)
	if count != 2 {
		t.Errorf("expected occurrence_count=2, got %d", count)
	}
}

// TestEmitProposedFeature_SuppressionBlocksInsert — operator-installed
// suppression suppresses the emit.
func TestEmitProposedFeature_SuppressionBlocksInsert(t *testing.T) {
	db := newProposedFeaturesTestDB(t)

	payload := ProposedFeaturePayload{
		ObservationSummary: "noise",
		Category:           "noise",
		Source:             "investigator",
		Topic:              "noisy-pattern",
		CodePaths:          []string{"x.go"},
	}
	fp := Fingerprint(payload.Source, payload.Topic, payload.CodePaths, nil, nil)

	// Operator installs suppression FIRST.
	_, err := SuppressProposedFeature(db, fp,
		"this fires constantly on cosmetic refactors and clutters review",
		time.Time{}, "operator@example.com")
	if err != nil {
		t.Fatalf("SuppressProposedFeature: %v", err)
	}

	res, err := EmitProposedFeature(db, payload)
	if err != nil {
		t.Fatalf("EmitProposedFeature: %v", err)
	}
	if !res.Suppressed {
		t.Errorf("expected suppressed=true, got %+v", res)
	}
	if res.FeatureID != 0 {
		t.Errorf("expected feature_id=0 on suppress, got %d", res.FeatureID)
	}

	// Verify no row landed.
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM ProposedFeatures WHERE fingerprint = ?`, fp).Scan(&n)
	if n != 0 {
		t.Errorf("expected 0 ProposedFeatures rows, got %d", n)
	}
}

// TestEmitProposedFeature_ExpiredSuppressionAllowsInsert — suppressed_until
// in the past does not block.
func TestEmitProposedFeature_ExpiredSuppressionAllowsInsert(t *testing.T) {
	db := newProposedFeaturesTestDB(t)

	payload := ProposedFeaturePayload{
		ObservationSummary: "noise",
		Category:           "noise",
		Source:             "investigator",
		Topic:              "noisy-pattern-expired",
		CodePaths:          []string{"x.go"},
	}
	fp := Fingerprint(payload.Source, payload.Topic, payload.CodePaths, nil, nil)

	// Suppression that expired an hour ago.
	_, err := SuppressProposedFeature(db, fp,
		"old suppression that has since expired and should not block",
		time.Now().Add(-1*time.Hour), "operator@example.com")
	if err != nil {
		t.Fatalf("SuppressProposedFeature: %v", err)
	}

	res, err := EmitProposedFeature(db, payload)
	if err != nil {
		t.Fatalf("EmitProposedFeature: %v", err)
	}
	if res.Suppressed {
		t.Errorf("expected expired suppression to NOT block, got suppressed=%v", res.Suppressed)
	}
	if !res.Inserted {
		t.Errorf("expected fresh insert after expired suppression")
	}
}

// TestEmitProposedFeature_RejectsInvalidScore — value_score must be in
// the enum.
func TestEmitProposedFeature_RejectsInvalidScore(t *testing.T) {
	db := newProposedFeaturesTestDB(t)
	_, err := EmitProposedFeature(db, ProposedFeaturePayload{
		ObservationSummary: "x",
		Source:             "investigator",
		Topic:              "x",
		ValueScore:         "extreme",
	})
	if err == nil || !strings.Contains(err.Error(), "value_score") {
		t.Errorf("expected value_score error, got %v", err)
	}
}

// TestEmitProposedFeature_RequiresObservationSummary — empty rejects.
func TestEmitProposedFeature_RequiresObservationSummary(t *testing.T) {
	db := newProposedFeaturesTestDB(t)
	_, err := EmitProposedFeature(db, ProposedFeaturePayload{
		Source: "investigator",
		Topic:  "x",
	})
	if err == nil {
		t.Error("expected error for empty observation_summary")
	}
}

// TestEmitProposedFeature_DefaultsCategoryAndScores — empties get
// sensible defaults.
func TestEmitProposedFeature_DefaultsCategoryAndScores(t *testing.T) {
	db := newProposedFeaturesTestDB(t)
	res, err := EmitProposedFeature(db, ProposedFeaturePayload{
		ObservationSummary: "test",
		Source:             "investigator",
		Topic:              "x",
	})
	if err != nil {
		t.Fatalf("EmitProposedFeature: %v", err)
	}
	var category, val, complexity string
	db.QueryRow(`SELECT category, value_score, complexity_score FROM ProposedFeatures WHERE id = ?`,
		res.FeatureID).Scan(&category, &val, &complexity)
	if category != "uncategorised" {
		t.Errorf("expected default category=uncategorised, got %q", category)
	}
	if val != "medium" || complexity != "medium" {
		t.Errorf("expected default scores=medium/medium, got %s/%s", val, complexity)
	}
}

// TestSuppressProposedFeature_RationaleMinLength — schema CHECK
// constraint rejects rationale < 20 chars.
func TestSuppressProposedFeature_RationaleMinLength(t *testing.T) {
	db := newProposedFeaturesTestDB(t)
	_, err := SuppressProposedFeature(db, "fp", "too short",
		time.Time{}, "operator@example.com")
	if err == nil {
		t.Error("expected rationale length error")
	}
}

// TestSuppressProposedFeature_RequiresEmail — operator email required.
func TestSuppressProposedFeature_RequiresEmail(t *testing.T) {
	db := newProposedFeaturesTestDB(t)
	_, err := SuppressProposedFeature(db, "fp",
		"a suppression rationale of sufficient length",
		time.Time{}, "")
	if err == nil {
		t.Error("expected email required error")
	}
}

// TestOverrideProposedFeatureScore_HappyPath — operator changes
// value_score from medium to high, audit row + score row both update.
func TestOverrideProposedFeatureScore_HappyPath(t *testing.T) {
	db := newProposedFeaturesTestDB(t)

	res, err := EmitProposedFeature(db, ProposedFeaturePayload{
		ObservationSummary: "test",
		Source:             "investigator",
		Topic:              "y",
		ValueScore:         "medium",
		ComplexityScore:    "medium",
	})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	err = OverrideProposedFeatureScore(db, res.FeatureID, "high", "",
		"operator wants to prioritise this", "operator@example.com")
	if err != nil {
		t.Fatalf("OverrideProposedFeatureScore: %v", err)
	}
	var val, complexity string
	db.QueryRow(`SELECT value_score, complexity_score FROM ProposedFeatures WHERE id = ?`,
		res.FeatureID).Scan(&val, &complexity)
	if val != "high" {
		t.Errorf("expected value=high, got %q", val)
	}
	if complexity != "medium" {
		t.Errorf("expected complexity preserved, got %q", complexity)
	}

	// Audit row exists.
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM ProposedFeatureScoreOverrides WHERE proposed_feature_id = ?`,
		res.FeatureID).Scan(&n)
	if n != 1 {
		t.Errorf("expected 1 audit row, got %d", n)
	}
}

// TestOverrideProposedFeatureScore_RequiresAtLeastOneScore — empty
// rejects.
func TestOverrideProposedFeatureScore_RequiresAtLeastOneScore(t *testing.T) {
	db := newProposedFeaturesTestDB(t)

	res, _ := EmitProposedFeature(db, ProposedFeaturePayload{
		ObservationSummary: "x", Source: "investigator", Topic: "z",
	})
	err := OverrideProposedFeatureScore(db, res.FeatureID, "", "",
		"some rationale", "op@example.com")
	if err == nil {
		t.Error("expected error when both scores empty")
	}
}

// TestDecayProposedFeatureScores_HighToMedium — stale high → medium.
func TestDecayProposedFeatureScores_HighToMedium(t *testing.T) {
	db := newProposedFeaturesTestDB(t)

	res, err := EmitProposedFeature(db, ProposedFeaturePayload{
		ObservationSummary: "old high-value",
		Source:             "investigator",
		Topic:              "decay-1",
		ValueScore:         "high",
	})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	// Backdate last_seen_at to 30 days ago.
	db.Exec(`UPDATE ProposedFeatures SET last_seen_at = datetime('now', '-30 days') WHERE id = ?`,
		res.FeatureID)

	n, err := DecayProposedFeatureScores(db, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("DecayProposedFeatureScores: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 row decayed, got %d", n)
	}
	var val string
	db.QueryRow(`SELECT value_score FROM ProposedFeatures WHERE id = ?`, res.FeatureID).Scan(&val)
	if val != "medium" {
		t.Errorf("expected value=medium after decay, got %q", val)
	}

	// Audit row written.
	var rationale string
	db.QueryRow(`SELECT rationale FROM ProposedFeatureScoreOverrides WHERE proposed_feature_id = ?`,
		res.FeatureID).Scan(&rationale)
	if !strings.Contains(rationale, "auto-decay") {
		t.Errorf("expected auto-decay audit rationale, got %q", rationale)
	}
}

// TestDecayProposedFeatureScores_LowFloor — low never decays past low.
func TestDecayProposedFeatureScores_LowFloor(t *testing.T) {
	db := newProposedFeaturesTestDB(t)

	res, _ := EmitProposedFeature(db, ProposedFeaturePayload{
		ObservationSummary: "stale low",
		Source:             "investigator",
		Topic:              "decay-low",
		ValueScore:         "low",
	})
	db.Exec(`UPDATE ProposedFeatures SET last_seen_at = datetime('now', '-30 days') WHERE id = ?`,
		res.FeatureID)

	n, err := DecayProposedFeatureScores(db, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("DecayProposedFeatureScores: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 rows decayed (low is the floor), got %d", n)
	}
}

// TestDecayProposedFeatureScores_RecentRowsUntouched — fresh rows stay.
func TestDecayProposedFeatureScores_RecentRowsUntouched(t *testing.T) {
	db := newProposedFeaturesTestDB(t)

	res, _ := EmitProposedFeature(db, ProposedFeaturePayload{
		ObservationSummary: "fresh",
		Source:             "investigator",
		Topic:              "decay-fresh",
		ValueScore:         "high",
	})

	n, _ := DecayProposedFeatureScores(db, 7*24*time.Hour)
	if n != 0 {
		t.Errorf("expected fresh row untouched, got %d decays", n)
	}
	var val string
	db.QueryRow(`SELECT value_score FROM ProposedFeatures WHERE id = ?`, res.FeatureID).Scan(&val)
	if val != "high" {
		t.Errorf("fresh row decayed unexpectedly to %q", val)
	}
}

// TestListProposedFeatures_NonArchived — default lists active rows.
func TestListProposedFeatures_NonArchived(t *testing.T) {
	db := newProposedFeaturesTestDB(t)

	r1, _ := EmitProposedFeature(db, ProposedFeaturePayload{
		ObservationSummary: "active", Source: "investigator", Topic: "a",
	})
	r2, _ := EmitProposedFeature(db, ProposedFeaturePayload{
		ObservationSummary: "to-archive", Source: "investigator", Topic: "b",
	})
	db.Exec(`UPDATE ProposedFeatures SET archived_at = datetime('now'), archive_reason = 'old' WHERE id = ?`, r2.FeatureID)

	rows, err := ListProposedFeatures(db, "")
	if err != nil {
		t.Fatalf("ListProposedFeatures: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != r1.FeatureID {
		t.Errorf("expected only active row, got %+v", rows)
	}
}

// TestPromoteProposedFeature_HappyPath — pending → promoted.
func TestPromoteProposedFeature_HappyPath(t *testing.T) {
	db := newProposedFeaturesTestDB(t)

	r, _ := EmitProposedFeature(db, ProposedFeaturePayload{
		ObservationSummary: "x", Source: "investigator", Topic: "p",
	})
	if err := PromoteProposedFeature(db, r.FeatureID, "2026-05-15", "op@example.com"); err != nil {
		t.Fatalf("PromoteProposedFeature: %v", err)
	}
	var status, deadline, decidedBy string
	db.QueryRow(`SELECT status, promotion_deadline, decided_by FROM ProposedFeatures WHERE id = ?`,
		r.FeatureID).Scan(&status, &deadline, &decidedBy)
	if status != "promoted" {
		t.Errorf("expected status=promoted, got %q", status)
	}
	if deadline != "2026-05-15" {
		t.Errorf("expected deadline propagated, got %q", deadline)
	}
	if decidedBy != "op@example.com" {
		t.Errorf("expected decided_by set, got %q", decidedBy)
	}
}

// TestPromoteProposedFeature_AlreadyPromotedRejects — second promote
// errors.
func TestPromoteProposedFeature_AlreadyPromotedRejects(t *testing.T) {
	db := newProposedFeaturesTestDB(t)

	r, _ := EmitProposedFeature(db, ProposedFeaturePayload{
		ObservationSummary: "x", Source: "investigator", Topic: "p2",
	})
	if err := PromoteProposedFeature(db, r.FeatureID, "2026-05-15", "op@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := PromoteProposedFeature(db, r.FeatureID, "2026-06-15", "op@example.com"); err == nil {
		t.Error("expected double-promote to error")
	}
}
