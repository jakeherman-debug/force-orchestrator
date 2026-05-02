package isb

import (
	"context"
	"strings"
	"testing"

	"force-orchestrator/internal/isb/scanners/manifests"
)

// TestParseSupplyBypasses_GoComment confirms the // comment style is matched.
func TestParseSupplyBypasses_GoComment(t *testing.T) {
	src := []byte(`module foo
// SUPPLY-BYPASS: SUPPLY-001 AUDIT-1234 vendored fork pending upstream merge
require golang.org/x/foo v1
`)
	got := ParseSupplyBypasses(src)
	if len(got) != 1 {
		t.Fatalf("expected 1 marker, got %d: %+v", len(got), got)
	}
	m := got[0]
	if m.AuditID != "AUDIT-1234" {
		t.Errorf("AuditID: got %q want AUDIT-1234", m.AuditID)
	}
	if m.RuleKey != "SUPPLY-001" {
		t.Errorf("RuleKey: got %q want SUPPLY-001", m.RuleKey)
	}
	if !strings.Contains(m.Reason, "vendored fork") {
		t.Errorf("Reason: %q", m.Reason)
	}
	if m.LineNumber != 2 {
		t.Errorf("LineNumber: got %d want 2", m.LineNumber)
	}
}

// TestParseSupplyBypasses_RubyComment confirms the # comment style is matched
// (Ruby/Gemfile style).
func TestParseSupplyBypasses_RubyComment(t *testing.T) {
	src := []byte(`source 'https://rubygems.org'
# SUPPLY-BYPASS: SUPPLY-002 AUDIT-2222 internal gem from corporate registry
gem 'rails', '~> 7.0'
`)
	got := ParseSupplyBypasses(src)
	if len(got) != 1 {
		t.Fatalf("expected 1 marker, got %d", len(got))
	}
	if got[0].AuditID != "AUDIT-2222" || got[0].RuleKey != "SUPPLY-002" {
		t.Errorf("unexpected marker: %+v", got[0])
	}
}

// TestParseSupplyBypasses_PythonComment — # without rule key (applies to all).
func TestParseSupplyBypasses_PythonComment(t *testing.T) {
	src := []byte(`# SUPPLY-BYPASS AUDIT-3333 internal package, registry check N/A
requests==2.31.0
`)
	got := ParseSupplyBypasses(src)
	if len(got) != 1 {
		t.Fatalf("expected 1 marker, got %d", len(got))
	}
	if got[0].RuleKey != "" {
		t.Errorf("RuleKey: got %q want empty (applies-to-all)", got[0].RuleKey)
	}
	if got[0].AuditID != "AUDIT-3333" {
		t.Errorf("AuditID: got %q", got[0].AuditID)
	}
}

// TestParseSupplyBypasses_XMLComment — pom.xml-style XML comment.
func TestParseSupplyBypasses_XMLComment(t *testing.T) {
	src := []byte(`<project>
<!-- SUPPLY-BYPASS:SUPPLY-004 AUDIT-5555 license matrix to be updated -->
  <dependencies/>
</project>
`)
	got := ParseSupplyBypasses(src)
	if len(got) != 1 {
		t.Fatalf("expected 1 marker, got %d: %+v", len(got), got)
	}
	if got[0].RuleKey != "SUPPLY-004" || got[0].AuditID != "AUDIT-5555" {
		t.Errorf("unexpected marker: %+v", got[0])
	}
	// Trailing --> must be stripped from reason.
	if strings.Contains(got[0].Reason, "-->") {
		t.Errorf("Reason should not retain XML closer: %q", got[0].Reason)
	}
}

// TestParseSupplyBypasses_RuleKeyTargeted — colon-separated rule key parses.
func TestParseSupplyBypasses_RuleKeyTargeted(t *testing.T) {
	src := []byte(`// SUPPLY-BYPASS:SUPPLY-001 AUDIT-9999 targeted bypass for one rule only
`)
	got := ParseSupplyBypasses(src)
	if len(got) != 1 {
		t.Fatalf("expected 1 marker, got %d", len(got))
	}
	if got[0].RuleKey != "SUPPLY-001" {
		t.Errorf("RuleKey: got %q want SUPPLY-001", got[0].RuleKey)
	}
}

// TestParseSupplyBypasses_NoRuleKey_AppliesToAll — no rule key → empty.
func TestParseSupplyBypasses_NoRuleKey_AppliesToAll(t *testing.T) {
	src := []byte(`// SUPPLY-BYPASS AUDIT-7777 fleet-wide override for this manifest
`)
	got := ParseSupplyBypasses(src)
	if len(got) != 1 {
		t.Fatalf("expected 1 marker, got %d", len(got))
	}
	if got[0].RuleKey != "" {
		t.Errorf("RuleKey: got %q want empty", got[0].RuleKey)
	}
}

// TestParseSupplyBypasses_MissingAuditID_NoMatch — anti-cheat: AUDIT-NNN required.
func TestParseSupplyBypasses_MissingAuditID_NoMatch(t *testing.T) {
	src := []byte(`// SUPPLY-BYPASS: SUPPLY-001 nope no audit id here long enough reason
`)
	got := ParseSupplyBypasses(src)
	if len(got) != 0 {
		t.Errorf("expected 0 markers (no AUDIT-NNN), got %d: %+v", len(got), got)
	}
}

// TestParseSupplyBypasses_ShortReason_Rejected — reason < 10 chars dropped.
func TestParseSupplyBypasses_ShortReason_Rejected(t *testing.T) {
	src := []byte(`// SUPPLY-BYPASS: SUPPLY-001 AUDIT-1 short
`)
	got := ParseSupplyBypasses(src)
	if len(got) != 0 {
		t.Errorf("expected 0 markers (short reason), got %d: %+v", len(got), got)
	}
}

// TestParseSupplyBypasses_MalformedReturnsEmpty — random comment-looking
// line doesn't match.
func TestParseSupplyBypasses_MalformedReturnsEmpty(t *testing.T) {
	src := []byte(`// some unrelated comment that looks similar to bypass
# random Ruby comment
<!-- random XML comment -->
// SUPPLY-BYPASS-WRONG-TOKEN AUDIT-1 nope this is the wrong token here
// ISB-BYPASS: AUDIT-1 this is the WRONG family (ISB not SUPPLY) so no match
`)
	got := ParseSupplyBypasses(src)
	if len(got) != 0 {
		t.Errorf("expected 0 markers, got %d: %+v", len(got), got)
	}
}

// TestParseSupplyBypasses_MultipleInOneFile — multiple markers parse independently.
func TestParseSupplyBypasses_MultipleInOneFile(t *testing.T) {
	src := []byte(`# header
# SUPPLY-BYPASS: SUPPLY-001 AUDIT-1111 first marker reason text here
gem 'foo'
# SUPPLY-BYPASS:SUPPLY-002 AUDIT-2222 second marker reason text here
gem 'bar'
# SUPPLY-BYPASS AUDIT-3333 third marker no rule key applies to all
gem 'baz'
`)
	got := ParseSupplyBypasses(src)
	if len(got) != 3 {
		t.Fatalf("expected 3 markers, got %d: %+v", len(got), got)
	}
	wantAudits := []string{"AUDIT-1111", "AUDIT-2222", "AUDIT-3333"}
	for i, want := range wantAudits {
		if got[i].AuditID != want {
			t.Errorf("marker %d: AuditID got %q want %q", i, got[i].AuditID, want)
		}
	}
	wantKeys := []string{"SUPPLY-001", "SUPPLY-002", ""}
	for i, want := range wantKeys {
		if got[i].RuleKey != want {
			t.Errorf("marker %d: RuleKey got %q want %q", i, got[i].RuleKey, want)
		}
	}
}

// TestParseSupplyBypasses_EmptyInput — nil/empty content returns nil.
func TestParseSupplyBypasses_EmptyInput(t *testing.T) {
	if got := ParseSupplyBypasses(nil); got != nil {
		t.Errorf("nil input: got %+v want nil", got)
	}
	if got := ParseSupplyBypasses([]byte{}); got != nil {
		t.Errorf("empty input: got %+v want nil", got)
	}
}

// TestMatchSupplyBypass_RuleKeyTargeted — targeted marker only matches its rule.
func TestMatchSupplyBypass_RuleKeyTargeted(t *testing.T) {
	markers := []SupplyBypassMarker{
		{AuditID: "AUDIT-1", Reason: "targeted reason text", RuleKey: "SUPPLY-001"},
	}
	if MatchSupplyBypass(markers, "SUPPLY-001") == nil {
		t.Errorf("targeted marker should match its rule")
	}
	if MatchSupplyBypass(markers, "SUPPLY-002") != nil {
		t.Errorf("targeted marker should NOT match a different rule")
	}
}

// TestMatchSupplyBypass_EmptyKeyAppliesToAll — operator-wide override.
func TestMatchSupplyBypass_EmptyKeyAppliesToAll(t *testing.T) {
	markers := []SupplyBypassMarker{
		{AuditID: "AUDIT-1", Reason: "operator-wide override here", RuleKey: ""},
	}
	if MatchSupplyBypass(markers, "SUPPLY-001") == nil {
		t.Errorf("empty-key marker should match SUPPLY-001")
	}
	if MatchSupplyBypass(markers, "SUPPLY-005") == nil {
		t.Errorf("empty-key marker should match SUPPLY-005")
	}
}

// ── Integration: DispatchManifestGated wires SUPPLY-BYPASS through. ─────

// TestSupplyBypass_FindingMarkedOverridden_WhenManifestHasBypass confirms a
// finding emitted by a rule against a manifest that contains a matching
// SUPPLY-BYPASS marker is downgraded (severity → advise) and the message
// is prefixed with [BYPASSED ...] so the agents-side persistence records
// disposition='overridden' on the SecurityFindings row.
func TestSupplyBypass_FindingMarkedOverridden_WhenManifestHasBypass(t *testing.T) {
	withRegistryReset(t, func() {
		RegisterManifestGated(&realStubRule{
			id:   "SUPPLY-001",
			ecos: []manifests.Ecosystem{manifests.EcosystemRubyGems},
			findings: []Finding{{
				RuleID:   "SUPPLY-001",
				Severity: SeverityBlock,
				Path:     "Gemfile",
				Line:     3,
				Message:  "unverified gem source",
			}},
		})
		input := ManifestGatedInput{
			ChangedManifests: []ChangedManifest{{
				Path:      "Gemfile",
				Ecosystem: manifests.EcosystemRubyGems,
				AfterBytes: []byte(`source 'https://rubygems.org'
# SUPPLY-BYPASS:SUPPLY-001 AUDIT-9999 vendored fork pending upstream merge
gem 'foo'
`),
			}},
		}
		findings, errs := DispatchManifestGated(context.Background(), nil, gateAlwaysOn(), input)
		if len(errs) != 0 {
			t.Fatalf("unexpected errs: %v", errs)
		}
		if len(findings) != 1 {
			t.Fatalf("expected 1 finding, got %d", len(findings))
		}
		f := findings[0]
		if f.Severity != SeverityAdvise {
			t.Errorf("Severity: got %q want advise", f.Severity)
		}
		if !strings.HasPrefix(f.Message, "[BYPASSED AUDIT-9999: ") {
			t.Errorf("Message should be prefixed with [BYPASSED ...]: %q", f.Message)
		}
		if !strings.Contains(f.Message, "vendored fork") {
			t.Errorf("Message should contain reason: %q", f.Message)
		}
		if !strings.Contains(f.Message, "unverified gem source") {
			t.Errorf("Message should retain original text: %q", f.Message)
		}
	})
}

// TestSupplyBypass_FindingNotMarked_WhenNoBypass — without a matching marker,
// findings pass through with their original severity + message.
func TestSupplyBypass_FindingNotMarked_WhenNoBypass(t *testing.T) {
	withRegistryReset(t, func() {
		RegisterManifestGated(&realStubRule{
			id:   "SUPPLY-001",
			ecos: []manifests.Ecosystem{manifests.EcosystemRubyGems},
			findings: []Finding{{
				RuleID:   "SUPPLY-001",
				Severity: SeverityBlock,
				Path:     "Gemfile",
				Line:     3,
				Message:  "unverified gem source",
			}},
		})
		input := ManifestGatedInput{
			ChangedManifests: []ChangedManifest{{
				Path:       "Gemfile",
				Ecosystem:  manifests.EcosystemRubyGems,
				AfterBytes: []byte(`source 'https://rubygems.org'
gem 'foo'
`),
			}},
		}
		findings, _ := DispatchManifestGated(context.Background(), nil, gateAlwaysOn(), input)
		if len(findings) != 1 {
			t.Fatalf("expected 1 finding, got %d", len(findings))
		}
		f := findings[0]
		if f.Severity != SeverityBlock {
			t.Errorf("Severity: got %q want block (no bypass)", f.Severity)
		}
		if strings.HasPrefix(f.Message, "[BYPASSED ") {
			t.Errorf("Message should NOT be prefixed with [BYPASSED ...]: %q", f.Message)
		}
	})
}

// TestSupplyBypass_FindingNotMarked_WhenRuleKeyMismatch — a SUPPLY-001 bypass
// must NOT silence a SUPPLY-002 finding (anti-cheat: bypasses are targeted).
func TestSupplyBypass_FindingNotMarked_WhenRuleKeyMismatch(t *testing.T) {
	withRegistryReset(t, func() {
		RegisterManifestGated(&realStubRule{
			id:   "SUPPLY-002",
			ecos: []manifests.Ecosystem{manifests.EcosystemRubyGems},
			findings: []Finding{{
				RuleID:   "SUPPLY-002",
				Severity: SeverityBlock,
				Path:     "Gemfile",
				Line:     3,
				Message:  "untyped pin",
			}},
		})
		input := ManifestGatedInput{
			ChangedManifests: []ChangedManifest{{
				Path:      "Gemfile",
				Ecosystem: manifests.EcosystemRubyGems,
				AfterBytes: []byte(`# SUPPLY-BYPASS:SUPPLY-001 AUDIT-9999 targeted only at SUPPLY-001 here
gem 'foo'
`),
			}},
		}
		findings, _ := DispatchManifestGated(context.Background(), nil, gateAlwaysOn(), input)
		if len(findings) != 1 {
			t.Fatalf("expected 1 finding, got %d", len(findings))
		}
		f := findings[0]
		if f.Severity != SeverityBlock {
			t.Errorf("Severity: got %q want block (rule-key mismatch)", f.Severity)
		}
		if strings.HasPrefix(f.Message, "[BYPASSED ") {
			t.Errorf("Message should NOT be prefixed: %q", f.Message)
		}
	})
}

// TestSupplyBypass_EmptyKeyAppliesAcrossRules — empty RuleKey marker applies
// to all SUPPLY rules in the manifest.
func TestSupplyBypass_EmptyKeyAppliesAcrossRules(t *testing.T) {
	withRegistryReset(t, func() {
		RegisterManifestGated(&realStubRule{
			id:   "SUPPLY-001",
			ecos: []manifests.Ecosystem{manifests.EcosystemRubyGems},
			findings: []Finding{{
				RuleID: "SUPPLY-001", Severity: SeverityBlock, Path: "Gemfile", Line: 3, Message: "msg-1",
			}},
		})
		RegisterManifestGated(&realStubRule{
			id:   "SUPPLY-002",
			ecos: []manifests.Ecosystem{manifests.EcosystemRubyGems},
			findings: []Finding{{
				RuleID: "SUPPLY-002", Severity: SeverityBlock, Path: "Gemfile", Line: 4, Message: "msg-2",
			}},
		})
		input := ManifestGatedInput{
			ChangedManifests: []ChangedManifest{{
				Path:      "Gemfile",
				Ecosystem: manifests.EcosystemRubyGems,
				AfterBytes: []byte(`# SUPPLY-BYPASS AUDIT-1234 fleet-wide override for this manifest
gem 'foo'
gem 'bar'
`),
			}},
		}
		findings, _ := DispatchManifestGated(context.Background(), nil, gateAlwaysOn(), input)
		if len(findings) != 2 {
			t.Fatalf("expected 2 findings, got %d: %+v", len(findings), findings)
		}
		for _, f := range findings {
			if f.Severity != SeverityAdvise {
				t.Errorf("rule %s: severity got %q want advise (empty-key bypass)", f.RuleID, f.Severity)
			}
			if !strings.HasPrefix(f.Message, "[BYPASSED AUDIT-1234: ") {
				t.Errorf("rule %s: message should be bypass-prefixed: %q", f.RuleID, f.Message)
			}
		}
	})
}

// TestSupplyBypass_BypassDoesNotLeakAcrossManifests — a bypass in Gemfile
// must NOT suppress a finding emitted against package.json.
func TestSupplyBypass_BypassDoesNotLeakAcrossManifests(t *testing.T) {
	withRegistryReset(t, func() {
		RegisterManifestGated(&realStubRule{
			id:   "SUPPLY-001",
			ecos: []manifests.Ecosystem{manifests.EcosystemRubyGems, manifests.EcosystemNPM},
			findings: []Finding{{
				RuleID: "SUPPLY-001", Severity: SeverityBlock, Path: "package.json", Line: 1, Message: "npm finding",
			}},
		})
		input := ManifestGatedInput{
			ChangedManifests: []ChangedManifest{
				{
					Path:      "Gemfile",
					Ecosystem: manifests.EcosystemRubyGems,
					AfterBytes: []byte(`# SUPPLY-BYPASS AUDIT-1234 ruby-side bypass should NOT cross over
gem 'foo'
`),
				},
				{
					Path:       "package.json",
					Ecosystem:  manifests.EcosystemNPM,
					AfterBytes: []byte(`{"dependencies": {"foo": "1.0"}}`),
				},
			},
		}
		findings, _ := DispatchManifestGated(context.Background(), nil, gateAlwaysOn(), input)
		if len(findings) != 1 {
			t.Fatalf("expected 1 finding, got %d", len(findings))
		}
		f := findings[0]
		if f.Severity != SeverityBlock {
			// Confirms cross-file leakage: the Gemfile bypass should NOT
			// have applied to the package.json finding.
			t.Errorf("Severity: got %q want block (cross-manifest leakage)", f.Severity)
		}
		if strings.HasPrefix(f.Message, "[BYPASSED ") {
			t.Errorf("Message should NOT be bypass-prefixed: %q", f.Message)
		}
	})
}
