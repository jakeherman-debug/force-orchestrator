// Package senate — unit tests for verdict + senator + registry helpers.
// D4 Phase 3.
package senate_test

import (
	"testing"

	"force-orchestrator/internal/senate"
	"force-orchestrator/internal/store"
)

// TestVerdict_ApprovesUnderTeeth covers the spec's gating rule:
// dissent + confidence >= 0.8 → blocks; below threshold → permits.
func TestVerdict_ApprovesUnderTeeth(t *testing.T) {
	cases := []struct {
		name string
		v    senate.Verdict
		want bool
	}{
		{
			name: "concur_with_high_conf",
			v:    senate.Verdict{Position: senate.PositionConcur, Confidence: 0.95},
			want: true,
		},
		{
			name: "dissent_high_conf_blocks",
			v:    senate.Verdict{Position: senate.PositionDissent, Confidence: 0.9},
			want: false,
		},
		{
			name: "dissent_low_conf_permits",
			v:    senate.Verdict{Position: senate.PositionDissent, Confidence: 0.5},
			want: true,
		},
		{
			name: "amend_no_concerns",
			v:    senate.Verdict{Position: senate.PositionAmend, Confidence: 0.9},
			want: true,
		},
		{
			name: "block_concern_high_conf_blocks",
			v: senate.Verdict{Position: senate.PositionConcur, Confidence: 0.85,
				Concerns: []senate.Concern{{Severity: senate.SeverityBlock, Concern: "x"}}},
			want: false,
		},
		{
			name: "warn_concern_high_conf_permits",
			v: senate.Verdict{Position: senate.PositionConcur, Confidence: 0.85,
				Concerns: []senate.Concern{{Severity: senate.SeverityWarn, Concern: "x"}}},
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.v.Approves(); got != tc.want {
				t.Errorf("Approves() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestLoadSenator_AbsentReturnsNil covers the "no chamber yet" path —
// LoadSenator returns (nil, nil) so the router can skip the Senator
// without a hard failure.
func TestLoadSenator_AbsentReturnsNil(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	s, err := senate.LoadSenator(db, "no-such-repo")
	if err != nil {
		t.Fatalf("LoadSenator(absent): unexpected err %v", err)
	}
	if s != nil {
		t.Fatalf("LoadSenator(absent): got %+v, want nil", s)
	}
}

// TestLoadSenator_FullContext seeds a chamber + memory + active
// FleetRules row and asserts LoadSenator returns the assembled view.
func TestLoadSenator_FullContext(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	if err := store.UpsertSenateChamber(db, store.SenateChamber{
		SenatorName: "force-orchestrator",
		Scope:       "repo:force-orchestrator",
		Status:      "active",
	}); err != nil {
		t.Fatalf("UpsertSenateChamber: %v", err)
	}
	if _, err := store.InsertSenateMemory(db, store.SenateMemoryEntry{
		Senator: "force-orchestrator", Topic: "rejection", Summary: "past failure", Source: "rejection", Weight: 1.5,
	}); err != nil {
		t.Fatalf("InsertSenateMemory: %v", err)
	}
	// Insert one active senate-scoped rule to verify the rule loader
	// pulls it in.
	if _, err := db.Exec(`
		INSERT INTO FleetRules (rule_key, version, content, content_hash,
			category, agent_scope, render_to, enforced_by, created_by, active_until)
		VALUES ('senate-fo-test', 1, 'test rule body', 'h1',
			'senate', 'senate:force-orchestrator', 'senate-md-file', 'trust-only', 'test', '')`); err != nil {
		t.Fatalf("seed FleetRules: %v", err)
	}

	s, err := senate.LoadSenator(db, "force-orchestrator")
	if err != nil {
		t.Fatalf("LoadSenator: %v", err)
	}
	if s == nil {
		t.Fatal("LoadSenator: got nil, want loaded senator")
	}
	if s.Status != "active" {
		t.Errorf("Senator.Status = %q, want active", s.Status)
	}
	if len(s.Memory) != 1 || s.Memory[0].Summary != "past failure" {
		t.Errorf("Senator.Memory = %+v, want one rejection entry", s.Memory)
	}
	if len(s.RuleKeys) != 1 || s.RuleKeys[0] != "senate-fo-test" {
		t.Errorf("Senator.RuleKeys = %v, want [senate-fo-test]", s.RuleKeys)
	}
	if s.RuleBodies["senate-fo-test"] != "test rule body" {
		t.Errorf("Senator.RuleBodies = %v, want body", s.RuleBodies)
	}
}

// TestAffectedSenators_RoutesByRepo asserts the router picks up
// Senators whose name appears in the plan's repo set.
func TestAffectedSenators_RoutesByRepo(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	for _, name := range []string{"alpha", "beta", "gamma"} {
		if err := store.UpsertSenateChamber(db, store.SenateChamber{
			SenatorName: name, Scope: "repo:" + name, Status: "active",
		}); err != nil {
			t.Fatalf("UpsertSenateChamber(%s): %v", name, err)
		}
	}
	matched, err := senate.AffectedSenators(db, []string{"beta"}, "")
	if err != nil {
		t.Fatalf("AffectedSenators: %v", err)
	}
	if len(matched) != 1 || matched[0] != "beta" {
		t.Errorf("matched = %v, want [beta]", matched)
	}
}

// TestAffectedSenators_FeatureRepoFolded asserts the Feature's
// TargetRepo is included in the set even if the plan tasks don't
// reference it (a single-task feature on a fresh repo, for example).
func TestAffectedSenators_FeatureRepoFolded(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	if err := store.UpsertSenateChamber(db, store.SenateChamber{
		SenatorName: "delta", Scope: "repo:delta", Status: "active",
	}); err != nil {
		t.Fatalf("UpsertSenateChamber(delta): %v", err)
	}
	matched, err := senate.AffectedSenators(db, nil, "delta")
	if err != nil {
		t.Fatalf("AffectedSenators: %v", err)
	}
	if len(matched) != 1 || matched[0] != "delta" {
		t.Errorf("matched = %v, want [delta]", matched)
	}
}

// TestAffectedSenators_NoRepoFansOutToAll asserts a planless plan
// (very rare; e.g. a maintenance refresh) routes to every active
// Senator, not nobody.
func TestAffectedSenators_NoRepoFansOutToAll(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	for _, name := range []string{"alpha", "beta"} {
		if err := store.UpsertSenateChamber(db, store.SenateChamber{
			SenatorName: name, Scope: "repo:" + name, Status: "active",
		}); err != nil {
			t.Fatalf("UpsertSenateChamber(%s): %v", name, err)
		}
	}
	matched, err := senate.AffectedSenators(db, nil, "")
	if err != nil {
		t.Fatalf("AffectedSenators: %v", err)
	}
	if len(matched) != 2 {
		t.Errorf("matched = %v, want 2 senators", matched)
	}
}
