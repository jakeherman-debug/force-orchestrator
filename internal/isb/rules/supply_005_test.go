package rules

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"force-orchestrator/internal/isb"
	"force-orchestrator/internal/isb/scanners/manifests"
	"force-orchestrator/internal/isb/scanners/osv"
	"force-orchestrator/internal/store"
)

// stubOSV is the test-only osv.Client used to drive SUPPLY-005's
// per-manifest dispatch. We stub at the interface boundary so the
// rule's tests never reach the lockfile parser or the OSV API; the
// scanner-side wiring is exercised in osv_test.go.
type stubOSV struct {
	// handlers map manifest path → response. Each call to
	// ScanLockfile increments calls and resolves via the path key
	// (NOT the content) — tests register expected paths.
	handlers map[string]func() ([]osv.Finding, error)
	calls    int32
}

func newStubOSV() *stubOSV {
	return &stubOSV{handlers: map[string]func() ([]osv.Finding, error){}}
}

func (s *stubOSV) on(path string, fn func() ([]osv.Finding, error)) {
	s.handlers[path] = fn
}

func (s *stubOSV) ScanLockfile(_ context.Context, path string, _ []byte) ([]osv.Finding, error) {
	atomic.AddInt32(&s.calls, 1)
	if fn, ok := s.handlers[path]; ok {
		return fn()
	}
	// Default: return no findings + no error so unconfigured paths
	// produce a clean empty pass.
	return nil, nil
}

func (s *stubOSV) callCount() int32 {
	return atomic.LoadInt32(&s.calls)
}

// Compile-time assertion: stubOSV satisfies the Client interface.
var _ osv.Client = (*stubOSV)(nil)

// makeS5Input builds a one-or-more-manifest input for SUPPLY-005
// tests. AfterBytes is set to a non-empty placeholder so the rule
// passes the empty-content short-circuit; tests stub the scanner so
// the bytes themselves don't matter.
func makeS5Input(manifestPath string, eco manifests.Ecosystem) isb.ManifestGatedInput {
	return isb.ManifestGatedInput{
		SourceTaskID: 17,
		TargetRepo:   "example/repo",
		Branch:       "feat/cve-x",
		CommitSHA:    "deadbeef",
		ChangedManifests: []isb.ChangedManifest{
			{
				Path:       manifestPath,
				Ecosystem:  eco,
				AfterBytes: []byte("placeholder-content"),
			},
		},
	}
}

// makeS5InputMulti builds an input with several ChangedManifests in
// one go.
func makeS5InputMulti(items ...isb.ChangedManifest) isb.ManifestGatedInput {
	return isb.ManifestGatedInput{
		SourceTaskID:     17,
		TargetRepo:       "example/repo",
		Branch:           "feat/multi",
		CommitSHA:        "cafef00d",
		ChangedManifests: items,
	}
}

// TestSUPPLY005_NoVulns_NoFinding — scanner returns empty → 0 findings.
func TestSUPPLY005_NoVulns_NoFinding(t *testing.T) {
	stub := newStubOSV()
	stub.on("Gemfile.lock", func() ([]osv.Finding, error) { return nil, nil })

	rule := NewSUPPLY005(stub)
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	in := makeS5Input("Gemfile.lock", manifests.EcosystemRubyGems)

	findings, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected 0 findings, got %d: %+v", len(findings), findings)
	}
	if stub.callCount() != 1 {
		t.Errorf("expected 1 stub call, got %d", stub.callCount())
	}
}

// TestSUPPLY005_HighSeverityVuln_AdviseFinding — scanner returns one
// HIGH finding → exactly one isb.Finding emitted at advise severity
// (per launch policy), with OSVID + name + version + severity + URL
// in the message.
func TestSUPPLY005_HighSeverityVuln_AdviseFinding(t *testing.T) {
	stub := newStubOSV()
	stub.on("Gemfile.lock", func() ([]osv.Finding, error) {
		return []osv.Finding{{
			PackageName:    "rails",
			PackageVersion: "5.0.0",
			Ecosystem:      "RubyGems",
			OSVID:          "CVE-2023-99999",
			Severity:       "HIGH",
			Summary:        "rails RCE",
			URL:            "https://osv.dev/vulnerability/CVE-2023-99999",
		}}, nil
	})

	rule := NewSUPPLY005(stub)
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	in := makeS5Input("Gemfile.lock", manifests.EcosystemRubyGems)

	findings, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d: %+v", len(findings), findings)
	}
	f := findings[0]
	if f.RuleID != "SUPPLY-005" {
		t.Errorf("rule id mismatch: %q", f.RuleID)
	}
	if f.Severity != isb.SeverityAdvise {
		t.Errorf("severity mismatch: %q (want advise — launch policy)", f.Severity)
	}
	if f.Path != "Gemfile.lock" {
		t.Errorf("path mismatch: %q", f.Path)
	}
	for _, want := range []string{"CVE-2023-99999", "rails", "5.0.0", "HIGH", "RubyGems", "https://osv.dev/vulnerability/CVE-2023-99999", "rails RCE"} {
		if !strings.Contains(f.Message, want) {
			t.Errorf("message missing %q: %q", want, f.Message)
		}
	}
}

// TestSUPPLY005_CriticalSeverityVuln_AdviseFinding — CRITICAL still
// ships at advise (launch policy: no block-default for new rules).
func TestSUPPLY005_CriticalSeverityVuln_AdviseFinding(t *testing.T) {
	stub := newStubOSV()
	stub.on("package-lock.json", func() ([]osv.Finding, error) {
		return []osv.Finding{{
			PackageName:    "lodash",
			PackageVersion: "4.17.0",
			Ecosystem:      "npm",
			OSVID:          "CVE-2020-8203",
			Severity:       "CRITICAL",
			Summary:        "prototype pollution",
			URL:            "https://osv.dev/vulnerability/CVE-2020-8203",
		}}, nil
	})

	rule := NewSUPPLY005(stub)
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	in := makeS5Input("package-lock.json", manifests.EcosystemNPM)

	findings, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d: %+v", len(findings), findings)
	}
	if findings[0].Severity != isb.SeverityAdvise {
		t.Errorf("CRITICAL must still be advise at launch, got %q", findings[0].Severity)
	}
	if !strings.Contains(findings[0].Message, "CRITICAL") {
		t.Errorf("CRITICAL severity bucket should appear in message, got: %q", findings[0].Message)
	}
}

// TestSUPPLY005_MediumSeverityVuln_AdviseFinding — MEDIUM is advise
// (matches spec).
func TestSUPPLY005_MediumSeverityVuln_AdviseFinding(t *testing.T) {
	stub := newStubOSV()
	stub.on("requirements.txt", func() ([]osv.Finding, error) {
		return []osv.Finding{{
			PackageName:    "requests",
			PackageVersion: "2.28.0",
			Ecosystem:      "PyPI",
			OSVID:          "GHSA-x99x-x99x-x99x",
			Severity:       "MEDIUM",
			Summary:        "info disclosure",
			URL:            "https://osv.dev/vulnerability/GHSA-x99x-x99x-x99x",
		}}, nil
	})

	rule := NewSUPPLY005(stub)
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	in := makeS5Input("requirements.txt", manifests.EcosystemPyPI)
	findings, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].Severity != isb.SeverityAdvise {
		t.Errorf("MEDIUM should be advise, got %q", findings[0].Severity)
	}
	if !strings.Contains(findings[0].Message, "MEDIUM") {
		t.Errorf("expected 'MEDIUM' in message, got: %q", findings[0].Message)
	}
}

// TestSUPPLY005_LowSeverityVuln_AdviseFinding — LOW is advise.
func TestSUPPLY005_LowSeverityVuln_AdviseFinding(t *testing.T) {
	stub := newStubOSV()
	stub.on("Gemfile.lock", func() ([]osv.Finding, error) {
		return []osv.Finding{{
			PackageName:    "tiny-gem",
			PackageVersion: "0.1.0",
			Ecosystem:      "RubyGems",
			OSVID:          "GHSA-low-low-low",
			Severity:       "LOW",
			Summary:        "minor",
			URL:            "https://osv.dev/vulnerability/GHSA-low-low-low",
		}}, nil
	})

	rule := NewSUPPLY005(stub)
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	in := makeS5Input("Gemfile.lock", manifests.EcosystemRubyGems)
	findings, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].Severity != isb.SeverityAdvise {
		t.Errorf("LOW should be advise, got %q", findings[0].Severity)
	}
}

// TestSUPPLY005_ScannerError_PartialFindings — three manifests; the
// middle one errors. The other two still produce findings, and the
// returned error mentions the failing path.
func TestSUPPLY005_ScannerError_PartialFindings(t *testing.T) {
	stub := newStubOSV()
	stub.on("Gemfile.lock", func() ([]osv.Finding, error) {
		return []osv.Finding{{
			PackageName: "rails", PackageVersion: "5.0.0", Ecosystem: "RubyGems",
			OSVID: "CVE-rails-1", Severity: "HIGH", Summary: "x", URL: "u1",
		}}, nil
	})
	stub.on("package-lock.json", func() ([]osv.Finding, error) {
		return nil, fmt.Errorf("boom: networking down for npm scan")
	})
	stub.on("requirements.txt", func() ([]osv.Finding, error) {
		return []osv.Finding{{
			PackageName: "requests", PackageVersion: "2.0.0", Ecosystem: "PyPI",
			OSVID: "CVE-req-1", Severity: "MEDIUM", Summary: "y", URL: "u2",
		}}, nil
	})

	rule := NewSUPPLY005(stub)
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	in := makeS5InputMulti(
		isb.ChangedManifest{Path: "Gemfile.lock", Ecosystem: manifests.EcosystemRubyGems, AfterBytes: []byte("x")},
		isb.ChangedManifest{Path: "package-lock.json", Ecosystem: manifests.EcosystemNPM, AfterBytes: []byte("x")},
		isb.ChangedManifest{Path: "requirements.txt", Ecosystem: manifests.EcosystemPyPI, AfterBytes: []byte("x")},
	)

	findings, err := rule.Run(context.Background(), db, in)
	if err == nil {
		t.Fatal("expected wrapped error from Run")
	}
	if !strings.Contains(err.Error(), "package-lock.json") {
		t.Errorf("expected error to mention failing manifest, got: %v", err)
	}
	if len(findings) != 2 {
		t.Fatalf("expected 2 findings (rails + requests), got %d: %+v", len(findings), findings)
	}
}

// TestSUPPLY005_AllEcosystems_RouteCorrectly — five ChangedManifests,
// one per ecosystem. The stub records which paths were called; we
// confirm all five reached the scanner.
func TestSUPPLY005_AllEcosystems_RouteCorrectly(t *testing.T) {
	stub := newStubOSV()
	called := map[string]bool{}
	for _, p := range []string{"Gemfile.lock", "requirements.txt", "package-lock.json", "pom.xml", "go.sum"} {
		path := p
		stub.on(path, func() ([]osv.Finding, error) {
			called[path] = true
			return nil, nil
		})
	}

	rule := NewSUPPLY005(stub)
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	in := makeS5InputMulti(
		isb.ChangedManifest{Path: "Gemfile.lock", Ecosystem: manifests.EcosystemRubyGems, AfterBytes: []byte("x")},
		isb.ChangedManifest{Path: "requirements.txt", Ecosystem: manifests.EcosystemPyPI, AfterBytes: []byte("x")},
		isb.ChangedManifest{Path: "package-lock.json", Ecosystem: manifests.EcosystemNPM, AfterBytes: []byte("x")},
		isb.ChangedManifest{Path: "pom.xml", Ecosystem: manifests.EcosystemMaven, AfterBytes: []byte("x")},
		isb.ChangedManifest{Path: "go.sum", Ecosystem: manifests.EcosystemGo, AfterBytes: []byte("x")},
	)

	if _, err := rule.Run(context.Background(), db, in); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	for _, p := range []string{"Gemfile.lock", "requirements.txt", "package-lock.json", "pom.xml", "go.sum"} {
		if !called[p] {
			t.Errorf("expected scanner to be called for %s", p)
		}
	}
	if stub.callCount() != 5 {
		t.Errorf("expected 5 scanner calls (one per ecosystem), got %d", stub.callCount())
	}
}

// TestSUPPLY005_NoChangedManifests_NoCalls — empty input → zero
// scanner calls + zero findings + nil error.
func TestSUPPLY005_NoChangedManifests_NoCalls(t *testing.T) {
	stub := newStubOSV()
	rule := NewSUPPLY005(stub)
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	in := isb.ManifestGatedInput{
		SourceTaskID: 1, TargetRepo: "x/y", Branch: "f", CommitSHA: "abc",
		ChangedManifests: nil,
	}
	findings, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("expected 0 findings, got %d", len(findings))
	}
	if stub.callCount() != 0 {
		t.Errorf("expected 0 scanner calls, got %d", stub.callCount())
	}
}

// TestSUPPLY005_MultipleVulnsPerLockfile_MultipleFindings — one lock
// file with three vulns → three findings.
func TestSUPPLY005_MultipleVulnsPerLockfile_MultipleFindings(t *testing.T) {
	stub := newStubOSV()
	stub.on("Gemfile.lock", func() ([]osv.Finding, error) {
		return []osv.Finding{
			{PackageName: "rails", PackageVersion: "5.0.0", Ecosystem: "RubyGems", OSVID: "CVE-1", Severity: "HIGH", Summary: "a", URL: "u1"},
			{PackageName: "puma", PackageVersion: "3.0.0", Ecosystem: "RubyGems", OSVID: "CVE-2", Severity: "MEDIUM", Summary: "b", URL: "u2"},
			{PackageName: "nokogiri", PackageVersion: "1.0.0", Ecosystem: "RubyGems", OSVID: "GHSA-3", Severity: "CRITICAL", Summary: "c", URL: "u3"},
		}, nil
	})

	rule := NewSUPPLY005(stub)
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	in := makeS5Input("Gemfile.lock", manifests.EcosystemRubyGems)
	findings, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(findings) != 3 {
		t.Fatalf("expected 3 findings, got %d: %+v", len(findings), findings)
	}
	// Spot-check that each OSV ID appears in some message.
	wantIDs := map[string]bool{"CVE-1": false, "CVE-2": false, "GHSA-3": false}
	for _, f := range findings {
		for id := range wantIDs {
			if strings.Contains(f.Message, id) {
				wantIDs[id] = true
			}
		}
	}
	for id, seen := range wantIDs {
		if !seen {
			t.Errorf("expected OSV id %q to appear in some finding message", id)
		}
	}
}

// TestSUPPLY005_NilScanner_Errors — defensive: a rule constructed with
// nil scanner must error rather than panic.
func TestSUPPLY005_NilScanner_Errors(t *testing.T) {
	rule := NewSUPPLY005(nil)
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	in := makeS5Input("Gemfile.lock", manifests.EcosystemRubyGems)
	_, err := rule.Run(context.Background(), db, in)
	if err == nil {
		t.Fatal("expected error from nil-scanner Run")
	}
}

// TestSUPPLY005_UnsupportedLockfile_SilentSkip — when the wrapper
// returns ErrUnsupportedLockfile (e.g. for a plain `Gemfile`), the
// rule must skip silently rather than wrap into errs. The companion
// `Gemfile.lock` in the same input still produces findings.
func TestSUPPLY005_UnsupportedLockfile_SilentSkip(t *testing.T) {
	stub := newStubOSV()
	stub.on("Gemfile", func() ([]osv.Finding, error) {
		return nil, fmt.Errorf("wrap: %w", osv.ErrUnsupportedLockfile)
	})
	stub.on("Gemfile.lock", func() ([]osv.Finding, error) {
		return []osv.Finding{{
			PackageName: "rails", PackageVersion: "5.0.0", Ecosystem: "RubyGems",
			OSVID: "CVE-X", Severity: "HIGH", Summary: "x", URL: "u",
		}}, nil
	})

	rule := NewSUPPLY005(stub)
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	in := makeS5InputMulti(
		isb.ChangedManifest{Path: "Gemfile", Ecosystem: manifests.EcosystemRubyGems, AfterBytes: []byte("source 'rubygems'")},
		isb.ChangedManifest{Path: "Gemfile.lock", Ecosystem: manifests.EcosystemRubyGems, AfterBytes: []byte("GEM\n")},
	)

	findings, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("unexpected err on supported sibling: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding (from Gemfile.lock), got %d: %+v", len(findings), findings)
	}
}

// TestSUPPLY005_GoEcosystem_Supported — unlike SUPPLY-001 (where Go is
// silently skipped because CodeArtifact has no Go format), SUPPLY-005
// SHOULD scan Go lock files. Test that go.sum doesn't trigger any
// silent-skip path.
func TestSUPPLY005_GoEcosystem_Supported(t *testing.T) {
	stub := newStubOSV()
	stub.on("go.sum", func() ([]osv.Finding, error) {
		return []osv.Finding{{
			PackageName: "github.com/example/badpkg", PackageVersion: "v1.2.3",
			Ecosystem: "Go", OSVID: "CVE-go-1", Severity: "HIGH",
			Summary: "x", URL: "https://osv.dev/vulnerability/CVE-go-1",
		}}, nil
	})

	rule := NewSUPPLY005(stub)
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	in := makeS5Input("go.sum", manifests.EcosystemGo)
	findings, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 Go finding, got %d", len(findings))
	}
	if !strings.Contains(findings[0].Message, "github.com/example/badpkg") {
		t.Errorf("expected Go pkg name in message: %q", findings[0].Message)
	}
}

// TestSUPPLY005_OutOfScopeEcosystem_NotCalled — the rule should not
// invoke the scanner for ChangedManifests whose Ecosystem isn't in
// its declared set. (Defensive: the dispatcher already filters this
// way, but we guarantee the rule body does too.)
func TestSUPPLY005_OutOfScopeEcosystem_NotCalled(t *testing.T) {
	stub := newStubOSV()
	rule := NewSUPPLY005(stub)
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	in := makeS5InputMulti(
		isb.ChangedManifest{
			Path:       "weird.lock",
			Ecosystem:  manifests.Ecosystem("perl"), // not in declared set
			AfterBytes: []byte("x"),
		},
	)
	findings, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for out-of-scope ecosystem, got %d", len(findings))
	}
	if stub.callCount() != 0 {
		t.Errorf("expected 0 scanner calls, got %d", stub.callCount())
	}
}

// TestSUPPLY005_Identity — sanity: ID() and Ecosystems() return what
// the dispatcher expects.
func TestSUPPLY005_Identity(t *testing.T) {
	rule := NewSUPPLY005(newStubOSV())
	if got := rule.ID(); got != "SUPPLY-005" {
		t.Errorf("ID(): got %q want SUPPLY-005", got)
	}
	ecos := rule.Ecosystems()
	want := map[manifests.Ecosystem]bool{
		manifests.EcosystemRubyGems: false, manifests.EcosystemPyPI: false,
		manifests.EcosystemNPM: false, manifests.EcosystemMaven: false,
		manifests.EcosystemGo: false,
	}
	for _, e := range ecos {
		want[e] = true
	}
	for e, seen := range want {
		if !seen {
			t.Errorf("expected ecosystem %s in declared set", e)
		}
	}
}

