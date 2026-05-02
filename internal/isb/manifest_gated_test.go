package isb

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"
	"testing"

	"force-orchestrator/internal/isb/scanners/manifests"
)

// realStubRule satisfies ManifestGatedRule so tests can register
// counter-tracking rules and verify dispatch behaviour without
// pulling in the production SUPPLY-* implementations.
type realStubRule struct {
	id       string
	ecos     []manifests.Ecosystem
	findings []Finding
	err      error
	calls    *int32
}

func (r *realStubRule) ID() string                        { return r.id }
func (r *realStubRule) Ecosystems() []manifests.Ecosystem { return r.ecos }
func (r *realStubRule) Run(_ context.Context, _ *sql.DB, _ ManifestGatedInput) ([]Finding, error) {
	if r.calls != nil {
		atomic.AddInt32(r.calls, 1)
	}
	return r.findings, r.err
}

func withRegistryReset(t *testing.T, fn func()) {
	t.Helper()
	ResetManifestGatedForTest()
	defer ResetManifestGatedForTest()
	fn()
}

func gateAlwaysOn() FleetRulesGate {
	return func(_ string) (bool, Severity, bool) {
		return true, "", true
	}
}

// TestDispatch_NoChangedManifests_NoCalls confirms zero rule
// invocations when no recognised manifests changed (the "manifest
// gating" point).
func TestDispatch_NoChangedManifests_NoCalls(t *testing.T) {
	withRegistryReset(t, func() {
		var calls int32
		RegisterManifestGated(&realStubRule{
			id:    "X",
			ecos:  []manifests.Ecosystem{manifests.EcosystemPyPI},
			calls: &calls,
		})
		findings, errs := DispatchManifestGated(context.Background(), nil, gateAlwaysOn(), ManifestGatedInput{})
		if len(findings) != 0 {
			t.Errorf("expected zero findings, got %d", len(findings))
		}
		if len(errs) != 0 {
			t.Errorf("expected zero errors, got %d", len(errs))
		}
		if calls != 0 {
			t.Errorf("expected zero rule invocations, got %d", calls)
		}
	})
}

// TestDispatch_FiresOnEcosystemMatch confirms a rule fires when a
// changed manifest matches its ecosystem.
func TestDispatch_FiresOnEcosystemMatch(t *testing.T) {
	withRegistryReset(t, func() {
		var calls int32
		RegisterManifestGated(&realStubRule{
			id:       "MG-1",
			ecos:     []manifests.Ecosystem{manifests.EcosystemNPM},
			findings: []Finding{{RuleID: "MG-1", Severity: SeverityAdvise, Path: "package.json"}},
			calls:    &calls,
		})
		input := ManifestGatedInput{
			ChangedManifests: []ChangedManifest{
				{Path: "package.json", Ecosystem: manifests.EcosystemNPM},
			},
		}
		findings, errs := DispatchManifestGated(context.Background(), nil, gateAlwaysOn(), input)
		if calls != 1 {
			t.Errorf("expected 1 invocation, got %d", calls)
		}
		if len(findings) != 1 || findings[0].RuleID != "MG-1" {
			t.Errorf("expected MG-1 finding, got %+v", findings)
		}
		if len(errs) != 0 {
			t.Errorf("no errors expected: %v", errs)
		}
	})
}

// TestDispatch_DoesNotFireOnEcosystemMismatch confirms a rule scoped
// to npm does NOT fire when only Maven manifests changed.
func TestDispatch_DoesNotFireOnEcosystemMismatch(t *testing.T) {
	withRegistryReset(t, func() {
		var calls int32
		RegisterManifestGated(&realStubRule{
			id:    "MG-NPM",
			ecos:  []manifests.Ecosystem{manifests.EcosystemNPM},
			calls: &calls,
		})
		input := ManifestGatedInput{
			ChangedManifests: []ChangedManifest{
				{Path: "pom.xml", Ecosystem: manifests.EcosystemMaven},
			},
		}
		findings, errs := DispatchManifestGated(context.Background(), nil, gateAlwaysOn(), input)
		if calls != 0 {
			t.Errorf("expected 0 invocations on ecosystem mismatch, got %d", calls)
		}
		if len(findings) != 0 || len(errs) != 0 {
			t.Errorf("expected empty result: findings=%v errs=%v", findings, errs)
		}
	})
}

// TestDispatch_InactiveRulesAreSkipped confirms the FleetRules gate
// short-circuits inactive rules.
func TestDispatch_InactiveRulesAreSkipped(t *testing.T) {
	withRegistryReset(t, func() {
		var calls int32
		RegisterManifestGated(&realStubRule{
			id:    "MG-OFF",
			ecos:  []manifests.Ecosystem{manifests.EcosystemNPM},
			calls: &calls,
		})
		gate := func(_ string) (bool, Severity, bool) { return false, "", true }
		input := ManifestGatedInput{
			ChangedManifests: []ChangedManifest{{Path: "package.json", Ecosystem: manifests.EcosystemNPM}},
		}
		_, _ = DispatchManifestGated(context.Background(), nil, gate, input)
		if calls != 0 {
			t.Errorf("expected gate to skip inactive rule, got %d calls", calls)
		}
	})
}

// TestDispatch_RuleErrorsDoNotShortCircuit confirms one rule's error
// does not stop other rules from running.
func TestDispatch_RuleErrorsDoNotShortCircuit(t *testing.T) {
	withRegistryReset(t, func() {
		var c1, c2 int32
		RegisterManifestGated(&realStubRule{
			id:    "MG-ERR",
			ecos:  []manifests.Ecosystem{manifests.EcosystemNPM},
			err:   errors.New("oops"),
			calls: &c1,
		})
		RegisterManifestGated(&realStubRule{
			id:       "MG-OK",
			ecos:     []manifests.Ecosystem{manifests.EcosystemNPM},
			findings: []Finding{{RuleID: "MG-OK", Severity: SeverityAdvise, Path: "package.json"}},
			calls:    &c2,
		})
		input := ManifestGatedInput{
			ChangedManifests: []ChangedManifest{{Path: "package.json", Ecosystem: manifests.EcosystemNPM}},
		}
		findings, errs := DispatchManifestGated(context.Background(), nil, gateAlwaysOn(), input)
		if c1 != 1 || c2 != 1 {
			t.Errorf("both rules should run: c1=%d c2=%d", c1, c2)
		}
		if errs["MG-ERR"] == nil {
			t.Errorf("MG-ERR error should be in map: %v", errs)
		}
		if len(findings) != 1 || findings[0].RuleID != "MG-OK" {
			t.Errorf("expected MG-OK finding even after MG-ERR failed: %+v", findings)
		}
	})
}

// TestEcosystemSet_Dedups confirms the helper returns each ecosystem
// once even when multiple manifests of the same ecosystem changed.
func TestEcosystemSet_Dedups(t *testing.T) {
	in := ManifestGatedInput{
		ChangedManifests: []ChangedManifest{
			{Ecosystem: manifests.EcosystemNPM},
			{Ecosystem: manifests.EcosystemNPM},
			{Ecosystem: manifests.EcosystemPyPI},
		},
	}
	got := in.EcosystemSet()
	if len(got) != 2 || !got[manifests.EcosystemNPM] || !got[manifests.EcosystemPyPI] {
		t.Errorf("unexpected ecosystem set: %v", got)
	}
}
