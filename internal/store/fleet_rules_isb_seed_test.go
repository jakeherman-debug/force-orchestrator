package store

import (
	"context"
	"strings"
	"testing"

	"force-orchestrator/internal/isb"
	_ "force-orchestrator/internal/isb/rules" // ensure registry is populated
)

// TestIsbRulesAllSeededInFleetRules — every Rule in isb.All() must
// have a corresponding FleetRules row of category 'isb'. Anti-cheat
// per docs/roadmap.md § D4: a rule body without a FleetRules row is
// NOT active.
func TestIsbRulesAllSeededInFleetRules(t *testing.T) {
	seedKeys := map[string]bool{}
	for _, s := range BootstrapAudit() {
		if s.Category == "isb" {
			seedKeys[s.RuleKey] = true
		}
	}

	missing := []string{}
	for _, r := range isb.All() {
		if !seedKeys[r.ID()] {
			missing = append(missing, r.ID())
		}
	}
	if len(missing) > 0 {
		t.Fatalf("ISB rules without FleetRules seed: %v\nseed every rule via internal/store/fleet_rules_audit.go", missing)
	}
}

// TestIsbNewRulesShipAtAdvise — anti-cheat directive: every NEW ISB
// rule ships at advise severity at launch. There is no equivalent of
// the BOS-011 graduate-from-D0 exception this round; all 10 ISB rules
// must return SeverityAdvise.
func TestIsbNewRulesShipAtAdvise(t *testing.T) {
	for _, r := range isb.All() {
		if r.Severity() != isb.SeverityAdvise {
			t.Errorf("rule %s must ship at advise (anti-cheat: no block-default on new rules); got %v", r.ID(), r.Severity())
		}
	}
}

// TestIsbRuleWithoutFleetRulesRowIsInactive — a rule body in the
// registry but with no FleetRules row must be silent under
// DBFleetRulesGate. Demonstrates the run-time gate.
func TestIsbRuleWithoutFleetRulesRowIsInactive(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	if _, err := BootstrapFleetRules(context.Background(), db, ""); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	// Pick a known-rule, drop its FleetRules row, assert the gate
	// returns inactive.
	if _, err := db.Exec(`UPDATE FleetRules SET active_until = datetime('now') WHERE rule_key = 'ISB-007'`); err != nil {
		t.Fatalf("retire ISB-007: %v", err)
	}

	gate := isb.DBFleetRulesGate(db)
	active, _, ok := gate("ISB-007")
	if !ok {
		t.Fatal("DBFleetRulesGate returned ok=false for known rule_key — should be true")
	}
	if active {
		t.Fatal("ISB-007 returned active=true after active_until set — gate failure")
	}

	// Sibling: ISB-001 still active.
	active2, _, _ := gate("ISB-001")
	if !active2 {
		t.Fatal("ISB-001 should still be active after ISB-007 retired")
	}

	// Synthetic rule never seeded.
	active3, _, _ := gate("ISB-DUMMY-NONE")
	if active3 {
		t.Fatal("ISB-DUMMY-NONE returned active=true with no FleetRules row")
	}
}

// TestIsbRuleSeedsCarryRuleBodyPath — every ISB seed must reference
// the .go file that contains its check body via enforced_by.
func TestIsbRuleSeedsCarryRuleBodyPath(t *testing.T) {
	for _, s := range BootstrapAudit() {
		if s.Category != "isb" {
			continue
		}
		if !strings.HasPrefix(s.EnforcedBy, "internal/isb/rules/isb_") {
			t.Errorf("ISB seed %q EnforcedBy=%q; expected internal/isb/rules/isb_*.go", s.RuleKey, s.EnforcedBy)
		}
	}
}
