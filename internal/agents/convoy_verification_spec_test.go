package agents

// D3 fix-loop-1 / γ2 — verification spec parser + AT evaluator unit tests.

import (
	"context"
	"strings"
	"testing"
)

func TestParseVerificationSpec_Empty(t *testing.T) {
	for _, raw := range []string{"", "{}", "  "} {
		spec, err := ParseVerificationSpec(raw)
		if err != nil {
			t.Errorf("ParseVerificationSpec(%q) error = %v", raw, err)
			continue
		}
		if spec == nil {
			t.Errorf("ParseVerificationSpec(%q) returned nil spec", raw)
		}
		if len(spec.ATs) != 0 || len(spec.ExitCriteria) != 0 || len(spec.Deprecated) != 0 {
			t.Errorf("ParseVerificationSpec(%q) returned non-empty spec: %+v", raw, spec)
		}
	}
}

func TestParseVerificationSpec_FullShape(t *testing.T) {
	raw := `{
		"ats": [{"id":"AT-1","description":"x","evaluator":"substring:foo"}],
		"exit_criteria": [{"id":"EC-1","description":"y"}],
		"anti_cheat": ["no fake tests"],
		"deprecated": [{"at_id":"AT-2","removed_at":"2026-01-01","removed_by_email":"a@b","rationale":"twenty chars exactly here.","removal_kind":"mistake"}]
	}`
	spec, err := ParseVerificationSpec(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(spec.ATs) != 1 || spec.ATs[0].ID != "AT-1" {
		t.Errorf("ATs not parsed: %+v", spec.ATs)
	}
	if len(spec.ExitCriteria) != 1 || spec.ExitCriteria[0].ID != "EC-1" {
		t.Errorf("ExitCriteria not parsed: %+v", spec.ExitCriteria)
	}
	if len(spec.AntiCheat) != 1 {
		t.Errorf("AntiCheat not parsed: %+v", spec.AntiCheat)
	}
	if len(spec.Deprecated) != 1 || spec.Deprecated[0].ATID != "AT-2" {
		t.Errorf("Deprecated not parsed: %+v", spec.Deprecated)
	}
}

func TestParseVerificationSpec_MalformedRejected(t *testing.T) {
	if _, err := ParseVerificationSpec(`{not json`); err == nil {
		t.Errorf("expected error on malformed JSON")
	}
}

func TestEvaluateATs_SubstringPassFail(t *testing.T) {
	spec := &VerificationSpec{
		ATs: []SpecAT{
			{ID: "AT-1", Description: "must add foo", Evaluator: "substring:foo"},
			{ID: "AT-2", Description: "must add bar", Evaluator: "substring:bar"},
		},
	}
	results, err := EvaluateATs(context.Background(), nil, "", "", spec, "diff text contains foo here", nil)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].Status != "pass" {
		t.Errorf("AT-1 status = %s, want pass", results[0].Status)
	}
	if results[1].Status != "fail" {
		t.Errorf("AT-2 status = %s, want fail", results[1].Status)
	}
}

func TestEvaluateATs_RegexAndMustTouch(t *testing.T) {
	spec := &VerificationSpec{
		ATs: []SpecAT{
			{ID: "AT-1", Evaluator: `regex:^\+\+\+ b/api/`},
			{ID: "AT-2", Evaluator: "must_touch:api/handler.go"},
			{ID: "AT-3", Evaluator: "must_touch:nonexistent.go"},
		},
	}
	diff := "+++ b/api/handler.go\n@@@ change @@@\n+ added line\n"
	results, _ := EvaluateATs(context.Background(), nil, "", "", spec, diff, nil)
	if results[0].Status != "pass" {
		t.Errorf("AT-1 regex pass expected, got %s — evidence=%s", results[0].Status, results[0].Evidence)
	}
	if results[1].Status != "pass" {
		t.Errorf("AT-2 must_touch pass expected, got %s — evidence=%s", results[1].Status, results[1].Evidence)
	}
	if results[2].Status != "fail" {
		t.Errorf("AT-3 must_touch fail expected, got %s", results[2].Status)
	}
}

func TestEvaluateATs_DeprecatedSkipped(t *testing.T) {
	spec := &VerificationSpec{
		ATs: []SpecAT{
			{ID: "AT-1", Evaluator: "substring:never"},
			{ID: "AT-2", Evaluator: "substring:matched"},
		},
		Deprecated: []SpecDeprecation{
			{ATID: "AT-1", Rationale: "deprecated by operator"},
		},
	}
	skips := 0
	results, _ := EvaluateATs(context.Background(), nil, "", "", spec, "matched here", func(atID string) {
		skips++
		if atID != "AT-1" {
			t.Errorf("unexpected skip event for %s", atID)
		}
	})
	if skips != 1 {
		t.Errorf("expected 1 skip, got %d", skips)
	}
	if results[0].Status != "skipped_deprecated" {
		t.Errorf("AT-1 should be skipped_deprecated, got %s", results[0].Status)
	}
	if results[1].Status != "pass" {
		t.Errorf("AT-2 should pass, got %s", results[1].Status)
	}
}

func TestEvaluateATs_UnknownEvaluator_Inconclusive(t *testing.T) {
	spec := &VerificationSpec{
		ATs: []SpecAT{
			{ID: "AT-X", Evaluator: "voodoo:mystery"},
			{ID: "AT-Y"}, // empty evaluator
		},
	}
	results, _ := EvaluateATs(context.Background(), nil, "", "", spec, "diff", nil)
	for _, r := range results {
		if r.Status != "inconclusive" {
			t.Errorf("%s status = %s, want inconclusive", r.ATID, r.Status)
		}
	}
}

func TestATResultsToFindings_OnlyFailuresEmitted(t *testing.T) {
	results := []ATResult{
		{ATID: "AT-1", Status: "pass", Description: "x"},
		{ATID: "AT-2", Status: "fail", Description: "y", Evidence: "missing y"},
		{ATID: "AT-3", Status: "skipped_deprecated"},
		{ATID: "AT-4", Status: "inconclusive"},
	}
	findings := ATResultsToFindings(47, results)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding for the failure, got %d", len(findings))
	}
	// Concern #8 UI labeling: convoy prefix in the description.
	if !strings.Contains(findings[0].Description, "Convoy #47") {
		t.Errorf("finding missing convoy prefix: %s", findings[0].Description)
	}
	if !strings.Contains(findings[0].Description, "AT-2") {
		t.Errorf("finding missing AT id: %s", findings[0].Description)
	}
	if !strings.Contains(findings[0].Fix, "AT-2") || !strings.Contains(findings[0].Fix, "convoy #47") {
		t.Errorf("fix payload missing identifiers: %s", findings[0].Fix)
	}
}

func TestSerializeATResults_StableShape(t *testing.T) {
	results := []ATResult{
		{ATID: "AT-1", Status: "pass"},
		{ATID: "AT-2", Status: "fail"},
		{ATID: "AT-3", Status: "skipped_deprecated"},
	}
	out := SerializeATResults(results)
	for _, want := range []string{`"AT-1":"pass"`, `"AT-2":"fail"`, `"AT-3":"skipped_deprecated"`} {
		if !strings.Contains(out, want) {
			t.Errorf("SerializeATResults output missing %q: %s", want, out)
		}
	}
	if SerializeATResults(nil) != "{}" {
		t.Errorf("SerializeATResults(nil) = %q, want {}", SerializeATResults(nil))
	}
}

func TestIsATDeprecated(t *testing.T) {
	spec := &VerificationSpec{
		Deprecated: []SpecDeprecation{
			{ATID: "AT-7"},
		},
	}
	if !IsATDeprecated(spec, "AT-7") {
		t.Errorf("AT-7 should be deprecated")
	}
	if IsATDeprecated(spec, "AT-1") {
		t.Errorf("AT-1 should NOT be deprecated")
	}
	if IsATDeprecated(nil, "AT-7") {
		t.Errorf("nil spec should report nothing deprecated")
	}
}
