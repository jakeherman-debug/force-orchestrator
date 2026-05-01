package store

import (
	"context"
	"strings"
	"testing"

	"force-orchestrator/internal/bos"
	_ "force-orchestrator/internal/bos/rules" // ensure registry is populated
)

// TestBosRulesAllSeededInFleetRules — every Rule in bos.All() must
// have a corresponding FleetRules row of category 'bos'. Anti-cheat
// per docs/roadmap.md § D4: a rule body without a FleetRules row is
// NOT active (the run-time gate enforces this; the test guards the
// audit slice).
func TestBosRulesAllSeededInFleetRules(t *testing.T) {
	seedKeys := map[string]bool{}
	for _, s := range BootstrapAudit() {
		if s.Category == "bos" {
			seedKeys[s.RuleKey] = true
		}
	}

	missing := []string{}
	for _, r := range bos.All() {
		if !seedKeys[r.ID()] {
			missing = append(missing, r.ID())
		}
	}
	if len(missing) > 0 {
		t.Fatalf("BoS rules without FleetRules seed: %v\nseed every rule via internal/store/fleet_rules_audit.go", missing)
	}
}

// TestBosNewRulesShipAtAdvise — anti-cheat directive: every NEW BoS
// rule ships at advise severity. BOS-011 is the sole exception — it
// graduates D0 Pattern P16 (already CI-enforced) so it ships at
// block. Any other rule shipping at block fails this test.
func TestBosNewRulesShipAtAdvise(t *testing.T) {
	for _, r := range bos.All() {
		if r.ID() == "BOS-011" {
			if r.Severity() != bos.SeverityBlock {
				t.Errorf("BOS-011 must ship at BLOCK (graduates Pattern P16); got %v", r.Severity())
			}
			continue
		}
		if r.Severity() != bos.SeverityAdvise {
			t.Errorf("rule %s must ship at advise (anti-cheat directive); got %v", r.ID(), r.Severity())
		}
	}
}

// TestBosRuleWithoutFleetRulesRowIsInactive — a rule body in the
// registry but with no FleetRules row must be silent under
// DBFleetRulesGate. Demonstrates the run-time gate.
func TestBosRuleWithoutFleetRulesRowIsInactive(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	if _, err := BootstrapFleetRules(context.Background(), db, ""); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	// Pick a rule, drop its FleetRules row, assert the gate now
	// returns inactive. We pick BOS-007 because it's not block; the
	// signal is "gate returns false," not "verdict differs."
	if _, err := db.Exec(`UPDATE FleetRules SET active_until = datetime('now') WHERE rule_key = 'BOS-007'`); err != nil {
		t.Fatalf("retire BOS-007: %v", err)
	}

	gate := bos.DBFleetRulesGate(db)
	active, _, ok := gate("BOS-007")
	if !ok {
		t.Fatal("DBFleetRulesGate returned ok=false for known rule_key — should be true")
	}
	if active {
		t.Fatal("BOS-007 returned active=true after active_until set — gate failure")
	}

	// Sibling: BOS-001 still active.
	active2, _, _ := gate("BOS-001")
	if !active2 {
		t.Fatal("BOS-001 should still be active after BOS-007 retired")
	}

	// Synthetic rule never seeded.
	active3, _, _ := gate("BOS-DUMMY-NONE")
	if active3 {
		t.Fatal("BOS-DUMMY-NONE returned active=true with no FleetRules row")
	}
}

// TestBosRuleSeedsCarryRuleBodyPath — every BoS seed must reference
// the .go file that contains its check body via enforced_by. This
// is operator-discipline: an audit reviewer can immediately locate
// the AST check for a given rule_key.
func TestBosRuleSeedsCarryRuleBodyPath(t *testing.T) {
	for _, s := range BootstrapAudit() {
		if s.Category != "bos" {
			continue
		}
		if !strings.HasPrefix(s.EnforcedBy, "internal/bos/rules/bos_") {
			t.Errorf("BoS seed %q EnforcedBy=%q; expected internal/bos/rules/bos_*.go", s.RuleKey, s.EnforcedBy)
		}
	}
}
