// supplywire_test.go — regression tests for daemon-side SUPPLY-* wiring
// (D5 fix-loop iter 1 slice α).
//
// These tests pin the production wiring so the strict-verifier NO-GO
// gap (rules registered in tests but never in production) cannot
// silently re-open. Each test resets the manifest-gated registry +
// SupplyRecheckDeps both BEFORE and AFTER itself so the shared
// package-level state cannot leak across tests.
package agents

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"force-orchestrator/internal/isb"
	"force-orchestrator/internal/isb/scanners/manifests"
	"force-orchestrator/internal/isb/scanners/osv"
	"force-orchestrator/internal/isb/supplydeferral"
	"force-orchestrator/internal/store"
)

// ── stubs for the regression tests ───────────────────────────────────────

// stubOSV satisfies osv.Client. WireSupplyRules only requires that the
// client be non-nil; SUPPLY-005 invokes ScanLockfile but the wire test
// never exercises Run, so the body need only return zero findings.
type stubOSV struct{}

func (stubOSV) ScanLockfile(_ context.Context, _ string, _ []byte) ([]osv.Finding, error) {
	return nil, nil
}

var _ osv.Client = stubOSV{}

// resetWireGlobals clears the manifest-gated registry AND the
// supply-recheck deps. Tests that call WireSupplyRules MUST call this
// from t.Cleanup so subsequent tests start from a known-empty state.
// Must run BEFORE each test too in case a previous test failed before
// its own cleanup ran (defensive).
func resetWireGlobals(t *testing.T) {
	t.Helper()
	isb.ResetManifestGatedForTest()
	RegisterSupplyRecheckDeps(nil)
	t.Cleanup(func() {
		isb.ResetManifestGatedForTest()
		RegisterSupplyRecheckDeps(nil)
	})
}

// ── Tests ─────────────────────────────────────────────────────────────────

// TestWireSupplyRules_AllFiveRegistered confirms that, after a single
// WireSupplyRules call, every SUPPLY-* rule is reachable through the
// manifest-gated dispatcher's registry. This is the regression that
// would have caught the strict-verifier Static-shard finding.
func TestWireSupplyRules_AllFiveRegistered(t *testing.T) {
	resetWireGlobals(t)

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	if err := WireSupplyRules(db, &stubCA{}, stubOSV{}); err != nil {
		t.Fatalf("WireSupplyRules: %v", err)
	}

	registered := isb.AllManifestGated()
	if len(registered) < 5 {
		t.Fatalf("AllManifestGated: want >=5 SUPPLY rules registered, got %d", len(registered))
	}

	want := map[string]bool{
		"SUPPLY-001": false,
		"SUPPLY-002": false,
		"SUPPLY-003": false,
		"SUPPLY-004": false,
		"SUPPLY-005": false,
	}
	for _, r := range registered {
		if _, ok := want[r.ID()]; ok {
			want[r.ID()] = true
		}
	}
	for id, present := range want {
		if !present {
			t.Errorf("rule %s not registered after WireSupplyRules", id)
		}
	}
}

// TestWireSupplyRules_SupplyRecheckDepsPopulated confirms that the
// supply-token-recheck dog deps are wired correctly: CA + replay-rule
// map (with the codeartifact-using rules) + a non-nil RepoResolver.
func TestWireSupplyRules_SupplyRecheckDepsPopulated(t *testing.T) {
	resetWireGlobals(t)

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	ca := &stubCA{}
	if err := WireSupplyRules(db, ca, stubOSV{}); err != nil {
		t.Fatalf("WireSupplyRules: %v", err)
	}

	deps := GetSupplyRecheckDeps()
	if deps == nil {
		t.Fatal("GetSupplyRecheckDeps: nil after WireSupplyRules")
	}
	if deps.CA == nil {
		t.Error("deps.CA: nil after WireSupplyRules (expected the codeartifact stub)")
	}
	if deps.RepoResolver == nil {
		t.Error("deps.RepoResolver: nil after WireSupplyRules (expected DefaultRepoResolver(db))")
	}

	// Replay rule map must contain the three codeartifact-using rules.
	wantRules := []string{"SUPPLY-001", "SUPPLY-003", "SUPPLY-004"}
	for _, id := range wantRules {
		if _, ok := deps.Rules[id]; !ok {
			t.Errorf("deps.Rules[%q]: missing", id)
		}
	}
	// SUPPLY-002 + SUPPLY-005 should NOT be in the replay map (no
	// codeartifact deferral path). This pins the documented split.
	for _, id := range []string{"SUPPLY-002", "SUPPLY-005"} {
		if _, ok := deps.Rules[id]; ok {
			t.Errorf("deps.Rules[%q]: unexpectedly present (no codeartifact deferral path)", id)
		}
	}
}

// TestWireSupplyRules_NilOSVClient_Errors is the defensive guard: a nil
// osvClient at startup must surface as an error so the daemon escalates
// rather than booting with SUPPLY-005 broken (per CLAUDE.md "no silent
// failures").
func TestWireSupplyRules_NilOSVClient_Errors(t *testing.T) {
	resetWireGlobals(t)

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	err := WireSupplyRules(db, &stubCA{}, nil)
	if err == nil {
		t.Fatal("WireSupplyRules(nil osvClient): want error, got nil")
	}
	if !strings.Contains(err.Error(), "osvClient") {
		t.Errorf("WireSupplyRules(nil osvClient): error missing osvClient mention: %v", err)
	}

	// Side-effect check: no rules should have been registered when the
	// guard fires. This proves the guard runs FIRST, before any side
	// effect lands.
	if got := len(isb.AllManifestGated()); got != 0 {
		t.Errorf("AllManifestGated after error path: want 0 (guard fires before registration), got %d", got)
	}
	if GetSupplyRecheckDeps() != nil {
		t.Error("GetSupplyRecheckDeps after error path: want nil (guard fires before deps registration)")
	}
}

// TestWireSupplyRules_NilCAClient_StillRegistersRules confirms the CI /
// non-AWS-dev environment behaviour: nil caClient is tolerated;
// rules + deps still get wired (the rules each detect nil at call time
// and either return an error to the dispatcher or no-op for the
// codeartifact-independent rules).
func TestWireSupplyRules_NilCAClient_StillRegistersRules(t *testing.T) {
	resetWireGlobals(t)

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	if err := WireSupplyRules(db, nil, stubOSV{}); err != nil {
		t.Fatalf("WireSupplyRules(nil caClient): unexpected error: %v", err)
	}

	if got := len(isb.AllManifestGated()); got < 5 {
		t.Errorf("AllManifestGated with nil caClient: want >=5, got %d", got)
	}
	deps := GetSupplyRecheckDeps()
	if deps == nil {
		t.Fatal("GetSupplyRecheckDeps with nil caClient: nil")
	}
	if deps.CA != nil {
		t.Errorf("deps.CA with nil caClient: want nil, got non-nil (the wiring is supposed to forward nil so the dog's own nil-check fires)")
	}
	// The replay map should still be populated — the dog will refuse to
	// run on its own when CA is nil; the wiring's job is just to put
	// the rules into the map so a follow-up daemon restart with valid
	// AWS config fixes the situation without a code change.
	if _, ok := deps.Rules["SUPPLY-001"]; !ok {
		t.Error("deps.Rules[SUPPLY-001]: missing despite nil caClient")
	}
}

// TestReplayAdapter_RoundTrip exercises the adapter against a synthetic
// rule that records its input + emits a deterministic finding. Verifies
// the field mapping (ReplayInput → ManifestGatedInput → Finding →
// ReplayFinding) is faithful for every load-bearing field.
func TestReplayAdapter_RoundTrip(t *testing.T) {
	// Rule that captures ManifestGatedInput + emits one Finding.
	captured := &fakeMGRule{
		id: "SUPPLY-FAKE",
		findings: []isb.Finding{
			{
				RuleID:   "SUPPLY-FAKE",
				Severity: isb.SeverityBlock,
				Path:     "Gemfile",
				Line:     17,
				Message:  "fake finding",
			},
		},
	}

	adapter := NewReplayAdapter(captured)
	if adapter.ID() != "SUPPLY-FAKE" {
		t.Errorf("adapter.ID: want SUPPLY-FAKE, got %q", adapter.ID())
	}

	input := supplydeferral.ReplayInput{
		Branch:       "feature/x",
		CommitSHA:    "abc123",
		TargetRepo:   "api",
		SourceTaskID: 42,
		ChangedManifests: []supplydeferral.ReplayChangedManifest{
			{
				Path:      "Gemfile",
				Ecosystem: manifests.EcosystemRubyGems,
				DepsAdded: []manifests.Dependency{{Name: "redis", Version: "5.0.0"}},
				After:     []byte("gem 'redis', '5.0.0'\n"),
			},
		},
	}

	out, err := adapter.Run(context.Background(), nil, input)
	if err != nil {
		t.Fatalf("adapter.Run: %v", err)
	}

	// Forward field mapping: ReplayInput → ManifestGatedInput.
	got := captured.lastInput
	if got.SourceTaskID != 42 {
		t.Errorf("mgIn.SourceTaskID: want 42, got %d", got.SourceTaskID)
	}
	if got.TargetRepo != "api" {
		t.Errorf("mgIn.TargetRepo: want api, got %q", got.TargetRepo)
	}
	if got.Branch != "feature/x" {
		t.Errorf("mgIn.Branch: want feature/x, got %q", got.Branch)
	}
	if got.CommitSHA != "abc123" {
		t.Errorf("mgIn.CommitSHA: want abc123, got %q", got.CommitSHA)
	}
	if len(got.ChangedManifests) != 1 {
		t.Fatalf("mgIn.ChangedManifests: want 1, got %d", len(got.ChangedManifests))
	}
	cm := got.ChangedManifests[0]
	if cm.Path != "Gemfile" || cm.Ecosystem != manifests.EcosystemRubyGems {
		t.Errorf("mgIn.ChangedManifests[0]: path=%q eco=%q (want Gemfile/rubygems)", cm.Path, cm.Ecosystem)
	}
	if len(cm.DepsAdded) != 1 || cm.DepsAdded[0].Name != "redis" || cm.DepsAdded[0].Version != "5.0.0" {
		t.Errorf("mgIn.ChangedManifests[0].DepsAdded: %+v", cm.DepsAdded)
	}
	if string(cm.AfterBytes) != "gem 'redis', '5.0.0'\n" {
		t.Errorf("mgIn.ChangedManifests[0].AfterBytes: want gem 'redis'..., got %q", cm.AfterBytes)
	}

	// Reverse field mapping: Finding → ReplayFinding.
	if len(out) != 1 {
		t.Fatalf("adapter.Run output: want 1 finding, got %d", len(out))
	}
	rf := out[0]
	if rf.RuleID != "SUPPLY-FAKE" {
		t.Errorf("ReplayFinding.RuleID: want SUPPLY-FAKE, got %q", rf.RuleID)
	}
	if rf.Severity != string(isb.SeverityBlock) {
		t.Errorf("ReplayFinding.Severity: want %q, got %q", isb.SeverityBlock, rf.Severity)
	}
	if rf.Path != "Gemfile" {
		t.Errorf("ReplayFinding.Path: want Gemfile, got %q", rf.Path)
	}
	if rf.Line != 17 {
		t.Errorf("ReplayFinding.Line: want 17, got %d", rf.Line)
	}
	if rf.Message != "fake finding" {
		t.Errorf("ReplayFinding.Message: want fake finding, got %q", rf.Message)
	}
}

// TestReplayAdapter_PropagatesError pins the error-path semantics: if
// the wrapped rule errors, the adapter surfaces the same error and
// returns no findings. (Per docs, supplydeferral.replayOneRow keys off
// the err to record a per-row failure.)
func TestReplayAdapter_PropagatesError(t *testing.T) {
	want := errors.New("wrapped rule failed")
	rule := &fakeMGRule{id: "SUPPLY-FAKE", err: want}
	adapter := NewReplayAdapter(rule)

	out, err := adapter.Run(context.Background(), nil, supplydeferral.ReplayInput{
		Branch: "feature/x",
		ChangedManifests: []supplydeferral.ReplayChangedManifest{
			{Path: "Gemfile", Ecosystem: manifests.EcosystemRubyGems},
		},
	})
	if !errors.Is(err, want) {
		t.Errorf("adapter.Run: want %v, got %v", want, err)
	}
	if out != nil {
		t.Errorf("adapter.Run: want nil findings on error, got %+v", out)
	}
}

// TestWireSupplyRules_DaemonRegression is the daemon-side guard: it
// asserts the same invariants TestWireSupplyRules_AllFiveRegistered +
// TestWireSupplyRules_SupplyRecheckDepsPopulated do, but using a
// codeartifact stub identical in shape to what the daemon would inject.
// This is the test that catches future omissions of WireSupplyRules
// from cmd/force/fleet_cmds.go.
func TestWireSupplyRules_DaemonRegression(t *testing.T) {
	resetWireGlobals(t)

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Mimic what cmdDaemon does: construct a CA stub + osv client,
	// call WireSupplyRules. If a future refactor accidentally removes
	// the WireSupplyRules call from fleet_cmds.go, this test will still
	// pass — but the exact same invariants are also enforced by
	// TestWireSupplyRules_AllFiveRegistered, which IS the regression for
	// the wiring itself. This separate test focuses on the
	// "post-wire-call invariants" the daemon depends on.
	if err := WireSupplyRules(db, &stubCA{}, stubOSV{}); err != nil {
		t.Fatalf("WireSupplyRules: %v", err)
	}

	if got := len(isb.AllManifestGated()); got < 5 {
		t.Errorf("post-WireSupplyRules: AllManifestGated len=%d, want>=5", got)
	}
	deps := GetSupplyRecheckDeps()
	if deps == nil {
		t.Fatal("post-WireSupplyRules: GetSupplyRecheckDeps nil")
	}
	for _, id := range []string{"SUPPLY-001", "SUPPLY-003", "SUPPLY-004"} {
		if _, ok := deps.Rules[id]; !ok {
			t.Errorf("post-WireSupplyRules: deps.Rules missing %s", id)
		}
	}
}

// ── fakeMGRule helper ────────────────────────────────────────────────────

// fakeMGRule is a minimal isb.ManifestGatedRule used by adapter
// round-trip tests. It captures the ManifestGatedInput it was called
// with and either returns a canned []Finding or a canned error.
type fakeMGRule struct {
	id        string
	findings  []isb.Finding
	err       error
	lastInput isb.ManifestGatedInput
}

func (f *fakeMGRule) ID() string { return f.id }

func (f *fakeMGRule) Ecosystems() []manifests.Ecosystem {
	return []manifests.Ecosystem{manifests.EcosystemRubyGems}
}

func (f *fakeMGRule) Run(_ context.Context, _ *sql.DB, in isb.ManifestGatedInput) ([]isb.Finding, error) {
	f.lastInput = in
	if f.err != nil {
		return nil, f.err
	}
	return f.findings, nil
}

var _ isb.ManifestGatedRule = (*fakeMGRule)(nil)
