package bos_test

import (
	"strings"
	"testing"

	"force-orchestrator/internal/bos"
	_ "force-orchestrator/internal/bos/rules" // register rules via init
)

// allActiveGate is the gate used in tests where we want every
// registered rule to fire.
func allActiveGate(ruleID string) (bool, bos.Severity, bool) {
	return true, "", true
}

// TestReviewFiles_BlockSeverityRuleSurfaces — BOS-011 (block) fires on
// a literal-construction violation and HasBlock is set. The Path is
// chosen to match BOS-011's scope filter (path contains
// "internal/agents", not _test.go).
func TestReviewFiles_BlockSeverityRuleSurfaces(t *testing.T) {
	src := `
package agents
import "force-orchestrator/internal/clients/librarian"
func setup() { _ = &librarian.InProcessClient{} }
`
	res := bos.ReviewFiles(allActiveGate, []bos.ReviewInput{
		{Path: "internal/agents/example_d4p1_review.go", Source: src},
	})
	if !res.HasBlock {
		t.Fatalf("expected HasBlock=true; got false. Findings: %v", res.Findings)
	}
	found := false
	for _, f := range res.Findings {
		if f.RuleID == "BOS-011" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a BOS-011 finding; got %v", res.Findings)
	}
}

// TestReviewFiles_BypassDowngradesBlock — // BOS-BYPASS comment on the
// line directly above the violation downgrades the finding to advise.
func TestReviewFiles_BypassDowngradesBlock(t *testing.T) {
	src := `
package agents
import "force-orchestrator/internal/clients/librarian"
func setup() {
	// BOS-BYPASS: AUDIT-042 Operator approved override pre-merge for D4 P1 shakedown
	_ = &librarian.InProcessClient{}
}
`
	res := bos.ReviewFiles(allActiveGate, []bos.ReviewInput{
		{Path: "internal/agents/example_d4p1_review.go", Source: src},
	})
	for _, f := range res.Findings {
		if f.RuleID == "BOS-011" && f.Severity == bos.SeverityBlock {
			t.Fatalf("BOS-011 should have been downgraded to advise; got %+v", f)
		}
	}
	if res.HasBlock {
		// HasBlock must be false because the only block finding was
		// downgraded by the bypass.
		t.Fatalf("HasBlock=true after bypass; expected false. Findings: %v", res.Findings)
	}
	// The downgraded finding must carry the audit trail in its message.
	hasAudit := false
	for _, f := range res.Findings {
		if f.RuleID == "BOS-011" && strings.Contains(f.Message, "AUDIT-042") {
			hasAudit = true
		}
	}
	if !hasAudit {
		t.Fatalf("expected BOS-011 finding to carry AUDIT-042 in message; got %v", res.Findings)
	}
}

// TestReviewFiles_MalformedBypassFailsParse — anti-cheat directive: a
// bypass comment without an AUDIT-NNN OR with a short reason emits a
// BOS-BYPASS-MALFORMED hard-block finding.
func TestReviewFiles_MalformedBypassFailsParse(t *testing.T) {
	src := `
package x
// BOS-BYPASS: AUDIT-001 short
func F() {}
`
	res := bos.ReviewFiles(allActiveGate, []bos.ReviewInput{
		{Path: "x.go", Source: src},
	})
	if !res.HasBlock {
		t.Fatal("malformed bypass: expected HasBlock=true")
	}
	hasMalformed := false
	for _, f := range res.Findings {
		if f.RuleID == "BOS-BYPASS-MALFORMED" {
			hasMalformed = true
		}
	}
	if !hasMalformed {
		t.Fatalf("expected BOS-BYPASS-MALFORMED finding; got %v", res.Findings)
	}
}

// TestReviewFiles_InactiveRuleSilent — anti-cheat directive: a rule
// whose ID has no FleetRules row is NOT active even if the body is
// registered.
func TestReviewFiles_InactiveRuleSilent(t *testing.T) {
	src := `
package agents
import "force-orchestrator/internal/clients/librarian"
func setup() { _ = &librarian.InProcessClient{} }
`
	// gate that returns inactive for everything
	inactiveGate := func(ruleID string) (bool, bos.Severity, bool) {
		return false, "", true
	}
	res := bos.ReviewFiles(inactiveGate, []bos.ReviewInput{
		{Path: "internal/agents/example_d4p1_inactive.go", Source: src},
	})
	if res.HasBlock {
		t.Fatalf("expected HasBlock=false when all rules inactive; got %v", res.Findings)
	}
	for _, f := range res.Findings {
		if f.RuleID == "BOS-011" {
			t.Fatalf("BOS-011 finding present despite inactive gate: %+v", f)
		}
	}
}

// TestReviewFiles_ParseError — unparseable source surfaces as a
// BOS-PARSE-ERROR (advise) finding so downstream cannot silently
// green-light it.
func TestReviewFiles_ParseError(t *testing.T) {
	src := `package x
this is not valid Go
`
	res := bos.ReviewFiles(allActiveGate, []bos.ReviewInput{
		{Path: "garbage.go", Source: src},
	})
	hasParseErr := false
	for _, f := range res.Findings {
		if f.RuleID == "BOS-PARSE-ERROR" {
			hasParseErr = true
		}
	}
	if !hasParseErr {
		t.Fatal("expected BOS-PARSE-ERROR finding for unparseable source")
	}
}
