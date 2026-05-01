package bos

import (
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// helper: parse a fragment with comments and run ParseBypasses.
func parseFragment(t *testing.T, src string) ([]Bypass, []MalformedBypass) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "fragment.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return ParseBypasses(fset, f)
}

func TestParseBypasses_HappyPath(t *testing.T) {
	src := `package x
func F() {
	// BOS-BYPASS: AUDIT-099 Operator approved override pre-merge.
	BadCall()
}
func BadCall() {}
`
	valid, malformed := parseFragment(t, src)
	if len(malformed) != 0 {
		t.Fatalf("malformed: got %d, want 0: %v", len(malformed), malformed)
	}
	if len(valid) != 1 {
		t.Fatalf("valid: got %d, want 1", len(valid))
	}
	if valid[0].AuditID != "AUDIT-099" {
		t.Errorf("AuditID: got %q, want AUDIT-099", valid[0].AuditID)
	}
	if !strings.Contains(valid[0].Reason, "Operator approved") {
		t.Errorf("Reason: %q", valid[0].Reason)
	}
}

// TestBypassWithoutReasonFailsParse — anti-cheat directive: a bypass
// without a reason fails parse rather than silently passing.
func TestBypassWithoutReasonFailsParse(t *testing.T) {
	src := `package x
// BOS-BYPASS: AUDIT-001 nope
func F() {}
`
	_, malformed := parseFragment(t, src)
	if len(malformed) == 0 {
		t.Fatal("expected malformed for short reason; got none")
	}
}

// TestBypassWithoutAuditIDFailsParse — anti-cheat: missing AUDIT-NNN
// is caught.
func TestBypassWithoutAuditIDFailsParse(t *testing.T) {
	src := `package x
// BOS-BYPASS: this is a long but un-audited reason text here
func F() {}
`
	_, malformed := parseFragment(t, src)
	if len(malformed) == 0 {
		t.Fatal("expected malformed for missing AUDIT-NNN; got none")
	}
}

// TestBypassMissingColonFailsParse — punctuation drift.
func TestBypassMissingColonFailsParse(t *testing.T) {
	src := `package x
// BOS-BYPASS AUDIT-001 this should fail because of missing colon
func F() {}
`
	_, malformed := parseFragment(t, src)
	if len(malformed) == 0 {
		t.Fatal("expected malformed for missing colon; got none")
	}
}

func TestMatchBypass_GuardsLineBelow(t *testing.T) {
	src := `package x
// BOS-BYPASS: AUDIT-007 this is the long-enough reason text here
func F() {}
`
	valid, _ := parseFragment(t, src)
	if len(valid) != 1 {
		t.Fatalf("expected one valid; got %d", len(valid))
	}
	// The bypass is on a known line; we don't hardcode the number,
	// just confirm GuardLine == comment line + 1.
	if MatchBypass(valid, valid[0].GuardLine) == nil {
		t.Fatal("MatchBypass on GuardLine returned nil")
	}
	if MatchBypass(valid, valid[0].GuardLine+5) != nil {
		t.Fatal("MatchBypass on far line should be nil")
	}
}
