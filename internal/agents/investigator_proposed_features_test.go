package agents

import (
	"log"
	"os"
	"testing"
	"time"

	"force-orchestrator/internal/store"
)

// TestParseProposedFeatureBlocks_HappyPath — well-formed block.
func TestParseProposedFeatureBlocks_HappyPath(t *testing.T) {
	output := `Some prose investigation findings here.

[PROPOSED_FEATURE]
{"observation_summary":"add coverage for X","category":"missing_test","topic":"x-coverage","code_paths":["a.go","b.go"],"at_refs":[],"fleet_rule_refs":[],"value_score":"medium","complexity_score":"low","value_rationale":"recurring miss","complexity_rationale":"helper exists"}
[/PROPOSED_FEATURE]

[DONE]`
	features, parseErrs := ParseProposedFeatureBlocks(output)
	if len(parseErrs) > 0 {
		t.Fatalf("unexpected parse errors: %v", parseErrs)
	}
	if len(features) != 1 {
		t.Fatalf("expected 1 feature, got %d", len(features))
	}
	f := features[0]
	if f.ObservationSummary != "add coverage for X" {
		t.Errorf("summary mismatch: %q", f.ObservationSummary)
	}
	if f.ValueScore != "medium" || f.ComplexityScore != "low" {
		t.Errorf("scores: %s/%s", f.ValueScore, f.ComplexityScore)
	}
	if len(f.CodePaths) != 2 {
		t.Errorf("expected 2 code paths, got %d", len(f.CodePaths))
	}
}

// TestParseProposedFeatureBlocks_NoBlocks — output without any
// [PROPOSED_FEATURE] envelopes returns empty.
func TestParseProposedFeatureBlocks_NoBlocks(t *testing.T) {
	features, parseErrs := ParseProposedFeatureBlocks("plain prose [DONE]")
	if len(features) != 0 || len(parseErrs) != 0 {
		t.Errorf("expected empty, got features=%d errs=%d", len(features), len(parseErrs))
	}
}

// TestParseProposedFeatureBlocks_MultipleBlocks — non-greedy match.
func TestParseProposedFeatureBlocks_MultipleBlocks(t *testing.T) {
	output := `prose

[PROPOSED_FEATURE]
{"observation_summary":"first","category":"a","topic":"t1"}
[/PROPOSED_FEATURE]

more prose

[PROPOSED_FEATURE]
{"observation_summary":"second","category":"b","topic":"t2"}
[/PROPOSED_FEATURE]

[DONE]`
	features, errs := ParseProposedFeatureBlocks(output)
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	if len(features) != 2 {
		t.Fatalf("expected 2 features, got %d", len(features))
	}
	if features[0].ObservationSummary != "first" || features[1].ObservationSummary != "second" {
		t.Errorf("multiple blocks not parsed in order: %v", features)
	}
}

// TestParseProposedFeatureBlocks_MalformedJSON — bad JSON in block,
// returns it as parse error and continues.
func TestParseProposedFeatureBlocks_MalformedJSON(t *testing.T) {
	output := `[PROPOSED_FEATURE]
{"observation_summary": missing quote here, broken: x}
[/PROPOSED_FEATURE]

[PROPOSED_FEATURE]
{"observation_summary":"good","topic":"t"}
[/PROPOSED_FEATURE]
[DONE]`
	features, errs := ParseProposedFeatureBlocks(output)
	if len(errs) != 1 {
		t.Errorf("expected 1 parse error, got %d", len(errs))
	}
	if len(features) != 1 {
		t.Errorf("expected 1 valid feature after skipping malformed, got %d", len(features))
	}
}

// TestParseProposedFeatureBlocks_TitleAlias — `title` field aliases to
// observation_summary for LLM convenience.
func TestParseProposedFeatureBlocks_TitleAlias(t *testing.T) {
	output := `[PROPOSED_FEATURE]
{"title":"aliased title","topic":"t"}
[/PROPOSED_FEATURE]
[DONE]`
	features, _ := ParseProposedFeatureBlocks(output)
	if len(features) != 1 || features[0].ObservationSummary != "aliased title" {
		t.Errorf("title alias not applied: %+v", features)
	}
}

// TestEmitInvestigatorProposedFeatures_InsertsRow — happy path: a
// well-formed block lands as a fresh ProposedFeatures row.
func TestEmitInvestigatorProposedFeatures_InsertsRow(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	logger := log.New(os.Stderr, "test ", log.LstdFlags)

	output := `[PROPOSED_FEATURE]
{"observation_summary":"two convoys missed edge case Y","category":"missing_test","topic":"y-edge","code_paths":["foo.go","bar.go"],"value_score":"high","complexity_score":"low","value_rationale":"shipping hazard","complexity_rationale":"one-line helper"}
[/PROPOSED_FEATURE]
[DONE]`

	ins, merged, supp := EmitInvestigatorProposedFeatures(db, 42, "investigator-test", output, logger)
	if ins != 1 || merged != 0 || supp != 0 {
		t.Errorf("expected ins=1 merged=0 supp=0, got ins=%d merged=%d supp=%d", ins, merged, supp)
	}

	// Row landed.
	var summary string
	err := db.QueryRow(`SELECT observation_summary FROM ProposedFeatures WHERE source = 'investigator' ORDER BY id DESC LIMIT 1`).Scan(&summary)
	if err != nil {
		t.Fatalf("query inserted row: %v", err)
	}
	if summary != "two convoys missed edge case Y" {
		t.Errorf("row mismatch: %q", summary)
	}

	// AuditLog entry present.
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM AuditLog WHERE action = 'proposed-feature-emit' AND task_id = ?`, 42).Scan(&n)
	if n == 0 {
		t.Errorf("expected proposed-feature-emit audit row")
	}
}

// TestEmitInvestigatorProposedFeatures_DedupMerges — second emission
// of the same canonical input bumps occurrence_count.
func TestEmitInvestigatorProposedFeatures_DedupMerges(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	logger := log.New(os.Stderr, "test ", log.LstdFlags)

	output := `[PROPOSED_FEATURE]
{"observation_summary":"recurring pattern Z","category":"x","topic":"z-rec","code_paths":["x.go"],"value_score":"medium","complexity_score":"medium"}
[/PROPOSED_FEATURE]
[DONE]`

	EmitInvestigatorProposedFeatures(db, 1, "inv-1", output, logger)
	ins, merged, _ := EmitInvestigatorProposedFeatures(db, 2, "inv-2", output, logger)
	if ins != 0 || merged != 1 {
		t.Errorf("expected dedup merge, got ins=%d merged=%d", ins, merged)
	}
}

// TestEmitInvestigatorProposedFeatures_SuppressionLogged — operator-
// installed suppression blocks the emit and writes a suppressed audit row.
func TestEmitInvestigatorProposedFeatures_SuppressionLogged(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	logger := log.New(os.Stderr, "test ", log.LstdFlags)

	// Same payload the LLM will emit, used to compute the fingerprint.
	fp := store.Fingerprint("investigator", "noise-topic",
		[]string{"x.go"}, nil, nil)
	_, err := store.SuppressProposedFeature(db, fp,
		"this fires on every refactor and clutters review",
		time.Time{}, "operator@example.com")
	if err != nil {
		t.Fatalf("SuppressProposedFeature: %v", err)
	}

	output := `[PROPOSED_FEATURE]
{"observation_summary":"noise","category":"noise","topic":"noise-topic","code_paths":["x.go"]}
[/PROPOSED_FEATURE]
[DONE]`
	ins, merged, supp := EmitInvestigatorProposedFeatures(db, 99, "inv-test", output, logger)
	if ins != 0 || merged != 0 || supp != 1 {
		t.Errorf("expected supp=1, got ins=%d merged=%d supp=%d", ins, merged, supp)
	}

	// Audit row present.
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM AuditLog WHERE action = 'proposed-feature-suppressed' AND task_id = ?`, 99).Scan(&n)
	if n == 0 {
		t.Errorf("expected proposed-feature-suppressed audit row")
	}
}

// TestEmitInvestigatorProposedFeatures_ParseErrorsLogged — malformed
// blocks land as audit rows, no panic.
func TestEmitInvestigatorProposedFeatures_ParseErrorsLogged(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	logger := log.New(os.Stderr, "test ", log.LstdFlags)

	output := `[PROPOSED_FEATURE]
{"observation_summary": missing quote, "topic": "broken"}
[/PROPOSED_FEATURE]
[DONE]`
	ins, merged, supp := EmitInvestigatorProposedFeatures(db, 100, "inv", output, logger)
	if ins != 0 || merged != 0 || supp != 0 {
		t.Errorf("expected all-zero counts, got %d/%d/%d", ins, merged, supp)
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM AuditLog WHERE action = 'proposed-feature-parse-error' AND task_id = ?`, 100).Scan(&n)
	if n == 0 {
		t.Errorf("expected parse-error audit row")
	}
}

// TestEmitInvestigatorProposedFeatures_NoBlocksNoOp — output without
// any blocks is a no-op.
func TestEmitInvestigatorProposedFeatures_NoBlocksNoOp(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	logger := log.New(os.Stderr, "test ", log.LstdFlags)

	ins, merged, supp := EmitInvestigatorProposedFeatures(db, 1, "inv", "plain report [DONE]", logger)
	if ins+merged+supp != 0 {
		t.Errorf("expected no-op, got %d/%d/%d", ins, merged, supp)
	}
}
