package rules

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"force-orchestrator/internal/clients/codeartifact"
	"force-orchestrator/internal/isb"
	"force-orchestrator/internal/isb/scanners/manifests"
	"force-orchestrator/internal/isb/supplydeferral"
	"force-orchestrator/internal/store"
)

// stubCodeArtifact is the test-only Client implementation. Each call
// to DescribePackageVersion is logged in calls and resolved via the
// (ecosystem, name, version) → handler map. Falls back to
// notFoundFallback when the triple isn't registered.
type stubCodeArtifact struct {
	handlers          map[supplyCacheKey]func() (codeartifact.PackageVersionInfo, error)
	notFoundFallback  bool
	calls             int32
	listPkgsResult    []codeartifact.Package
	listPkgsErr       error
	healthErr         error
	defaultHandler    func(ecosystem codeartifact.Ecosystem, name, version string) (codeartifact.PackageVersionInfo, error)
}

func newStubCodeArtifact() *stubCodeArtifact {
	return &stubCodeArtifact{handlers: map[supplyCacheKey]func() (codeartifact.PackageVersionInfo, error){}}
}

func (s *stubCodeArtifact) on(eco codeartifact.Ecosystem, name, version string, fn func() (codeartifact.PackageVersionInfo, error)) {
	s.handlers[supplyCacheKey{Ecosystem: string(eco), Name: name, Version: version}] = fn
}

func (s *stubCodeArtifact) DescribePackageVersion(ctx context.Context, eco codeartifact.Ecosystem, name, version string) (codeartifact.PackageVersionInfo, error) {
	atomic.AddInt32(&s.calls, 1)
	key := supplyCacheKey{Ecosystem: string(eco), Name: name, Version: version}
	if fn, ok := s.handlers[key]; ok {
		return fn()
	}
	if s.defaultHandler != nil {
		return s.defaultHandler(eco, name, version)
	}
	if s.notFoundFallback {
		return codeartifact.PackageVersionInfo{}, codeartifact.ErrPackageNotFound
	}
	// Default: success-empty, so unconfigured calls are obvious in
	// failure output but don't crash.
	return codeartifact.PackageVersionInfo{Ecosystem: eco, Name: name, Version: version, Status: "Published"}, nil
}

func (s *stubCodeArtifact) ListPackages(_ context.Context, _ codeartifact.Ecosystem) ([]codeartifact.Package, error) {
	return s.listPkgsResult, s.listPkgsErr
}

func (s *stubCodeArtifact) Health(_ context.Context) error { return s.healthErr }

// callCount returns the number of DescribePackageVersion calls made.
func (s *stubCodeArtifact) callCount() int32 {
	return atomic.LoadInt32(&s.calls)
}

// Compile-time assertion: stubCodeArtifact satisfies the Client
// interface (so a future signature drift surfaces here, not as a
// runtime panic).
var _ codeartifact.Client = (*stubCodeArtifact)(nil)

// makeInput constructs a one-manifest input with the given deps.
func makeInput(branch, sha, taskID, manifestPath string, eco manifests.Ecosystem, deps ...manifests.Dependency) isb.ManifestGatedInput {
	_ = taskID // kept in signature for future test readability
	return isb.ManifestGatedInput{
		SourceTaskID: 99,
		TargetRepo:   "example/repo",
		Branch:       branch,
		CommitSHA:    sha,
		ChangedManifests: []isb.ChangedManifest{
			{
				Path:      manifestPath,
				Ecosystem: eco,
				DepsAdded: deps,
			},
		},
	}
}

func dep(eco manifests.Ecosystem, name, version string) manifests.Dependency {
	return manifests.Dependency{
		Ecosystem: eco,
		Name:      name,
		Version:   version,
		Source:    manifests.SourceDirect,
	}
}

// TestSUPPLY001_ExistingPackage_NoFinding — 200 from stub → no finding.
func TestSUPPLY001_ExistingPackage_NoFinding(t *testing.T) {
	stub := newStubCodeArtifact()
	stub.on(codeartifact.EcosystemRubyGems, "redis", "5.0.0", func() (codeartifact.PackageVersionInfo, error) {
		return codeartifact.PackageVersionInfo{Name: "redis", Version: "5.0.0", Status: "Published"}, nil
	})
	rule := NewSUPPLY001(stub)

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	in := makeInput("feat/x", "deadbeef", "", "Gemfile", manifests.EcosystemRubyGems,
		dep(manifests.EcosystemRubyGems, "redis", "5.0.0"))

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

// TestSUPPLY001_HallucinatedPackage_AdviseFinding — 404 from stub →
// advise-severity finding with ecosystem + name@version in message.
func TestSUPPLY001_HallucinatedPackage_AdviseFinding(t *testing.T) {
	stub := newStubCodeArtifact()
	stub.on(codeartifact.EcosystemNPM, "totally-fake-pkg", "0.1.0", func() (codeartifact.PackageVersionInfo, error) {
		return codeartifact.PackageVersionInfo{}, codeartifact.ErrPackageNotFound
	})
	rule := NewSUPPLY001(stub)

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	in := makeInput("feat/x", "deadbeef", "", "package.json", manifests.EcosystemNPM,
		dep(manifests.EcosystemNPM, "totally-fake-pkg", "0.1.0"))

	findings, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d: %+v", len(findings), findings)
	}
	f := findings[0]
	if f.RuleID != "SUPPLY-001" {
		t.Errorf("rule id mismatch: %q", f.RuleID)
	}
	if f.Severity != isb.SeverityAdvise {
		t.Errorf("severity mismatch: %q (want advise)", f.Severity)
	}
	if f.Path != "package.json" {
		t.Errorf("path mismatch: %q", f.Path)
	}
	if !strings.Contains(f.Message, "npm") {
		t.Errorf("message missing ecosystem: %q", f.Message)
	}
	if !strings.Contains(f.Message, "totally-fake-pkg") {
		t.Errorf("message missing dep name: %q", f.Message)
	}
	if !strings.Contains(f.Message, "0.1.0") {
		t.Errorf("message missing version: %q", f.Message)
	}
	if !strings.Contains(f.Message, "hallucination") {
		t.Errorf("message missing hallucination marker: %q", f.Message)
	}
}

// TestSUPPLY001_TokenExpired_DeferralLogged — auth error → deferral
// row written via supplydeferral.RecordDeferral, no finding emitted.
func TestSUPPLY001_TokenExpired_DeferralLogged(t *testing.T) {
	stub := newStubCodeArtifact()
	stub.on(codeartifact.EcosystemPyPI, "requests", "2.31.0", func() (codeartifact.PackageVersionInfo, error) {
		return codeartifact.PackageVersionInfo{}, codeartifact.ErrTokenExpired
	})
	rule := NewSUPPLY001(stub)

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	in := makeInput("feat/auth-x", "cafef00d", "", "requirements.txt", manifests.EcosystemPyPI,
		dep(manifests.EcosystemPyPI, "requests", "2.31.0"))

	findings, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected 0 findings (deferred), got %d: %+v", len(findings), findings)
	}

	// Deferral row should be present + the dedup digest is recorded as
	// bypass_reason (per supplydeferral.RecordDeferral).
	rows, err := supplydeferral.ListPendingDeferrals(db, "feat/auth-x")
	if err != nil {
		t.Fatalf("ListPendingDeferrals: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 deferral row, got %d", len(rows))
	}
	row := rows[0]
	if row.Payload.RuleKey != "SUPPLY-001" {
		t.Errorf("rule key in payload: %q", row.Payload.RuleKey)
	}
	if row.Payload.ManifestPath != "requirements.txt" {
		t.Errorf("manifest path in payload: %q", row.Payload.ManifestPath)
	}
	if row.Payload.CommitSHA != "cafef00d" {
		t.Errorf("commit sha in payload: %q", row.Payload.CommitSHA)
	}
	if len(row.Payload.DepsAdded) != 1 || row.Payload.DepsAdded[0].Name != "requests" {
		t.Errorf("deps in payload: %+v", row.Payload.DepsAdded)
	}

	// Confirm the dedup digest is non-empty (RecordDeferral wrote it
	// to bypass_reason). We can't read bypass_reason via
	// ListPendingDeferrals directly — but we can verify the row's
	// finding id is non-zero.
	if row.FindingID == 0 {
		t.Errorf("expected non-zero finding id")
	}
}

// TestSUPPLY001_Throttle_RetryOnce_ThenAdvise — first call returns
// ErrTransient, second also fails → no finding (advise-mode), but
// the stub was called twice.
func TestSUPPLY001_Throttle_RetryOnce_ThenAdvise(t *testing.T) {
	stub := newStubCodeArtifact()
	stub.on(codeartifact.EcosystemMaven, "org.example:flake", "1.0", func() (codeartifact.PackageVersionInfo, error) {
		return codeartifact.PackageVersionInfo{}, codeartifact.ErrTransient
	})
	rule := NewSUPPLY001(stub)
	// Tighten the retry delay so the test runs fast. The rule reads
	// the package-level supplyTransientRetryDelay.
	prev := supplyTransientRetryDelay
	supplyTransientRetryDelay = 1 * time.Millisecond
	defer func() { supplyTransientRetryDelay = prev }()

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	in := makeInput("feat/m", "feedbeef", "", "pom.xml", manifests.EcosystemMaven,
		dep(manifests.EcosystemMaven, "org.example:flake", "1.0"))

	findings, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected 0 findings on transient fallback, got %d: %+v", len(findings), findings)
	}
	if stub.callCount() != 2 {
		t.Errorf("expected 2 stub calls (retry once), got %d", stub.callCount())
	}
}

// TestSUPPLY001_PartialFailure_OtherDepsStillProcessed — three deps,
// mixed outcomes. Confirms no short-circuit: dep #2's 404 lands as a
// finding even though dep #3's token-expired triggers a deferral.
func TestSUPPLY001_PartialFailure_OtherDepsStillProcessed(t *testing.T) {
	stub := newStubCodeArtifact()
	stub.on(codeartifact.EcosystemNPM, "react", "18.2.0", func() (codeartifact.PackageVersionInfo, error) {
		return codeartifact.PackageVersionInfo{Name: "react", Version: "18.2.0", Status: "Published"}, nil
	})
	stub.on(codeartifact.EcosystemNPM, "halucinated-pkg", "1.0.0", func() (codeartifact.PackageVersionInfo, error) {
		return codeartifact.PackageVersionInfo{}, codeartifact.ErrPackageNotFound
	})
	stub.on(codeartifact.EcosystemNPM, "vue", "3.4.0", func() (codeartifact.PackageVersionInfo, error) {
		return codeartifact.PackageVersionInfo{}, codeartifact.ErrTokenExpired
	})
	rule := NewSUPPLY001(stub)

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	in := makeInput("feat/multi", "12345abc", "", "package.json", manifests.EcosystemNPM,
		dep(manifests.EcosystemNPM, "react", "18.2.0"),
		dep(manifests.EcosystemNPM, "halucinated-pkg", "1.0.0"),
		dep(manifests.EcosystemNPM, "vue", "3.4.0"),
	)

	findings, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding (for halucinated-pkg), got %d: %+v", len(findings), findings)
	}
	if !strings.Contains(findings[0].Message, "halucinated-pkg") {
		t.Errorf("expected finding for halucinated-pkg, got: %q", findings[0].Message)
	}

	// Deferral row for vue must exist.
	defs, err := supplydeferral.ListPendingDeferrals(db, "feat/multi")
	if err != nil {
		t.Fatalf("ListPendingDeferrals: %v", err)
	}
	if len(defs) != 1 {
		t.Fatalf("expected 1 deferral for vue, got %d", len(defs))
	}
	if defs[0].Payload.DepsAdded[0].Name != "vue" {
		t.Errorf("deferral dep mismatch: %+v", defs[0].Payload.DepsAdded)
	}

	if stub.callCount() != 3 {
		t.Errorf("expected 3 stub calls (one per dep), got %d", stub.callCount())
	}
}

// TestSUPPLY001_PositiveCache_HitWithin24h — call twice with the
// same (ecosystem, name, version); second call must NOT re-hit the
// stub.
func TestSUPPLY001_PositiveCache_HitWithin24h(t *testing.T) {
	stub := newStubCodeArtifact()
	stub.on(codeartifact.EcosystemPyPI, "django", "4.2.7", func() (codeartifact.PackageVersionInfo, error) {
		return codeartifact.PackageVersionInfo{Name: "django", Version: "4.2.7", Status: "Published"}, nil
	})
	rule := NewSUPPLY001(stub)

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	in := makeInput("feat/c", "abc123", "", "requirements.txt", manifests.EcosystemPyPI,
		dep(manifests.EcosystemPyPI, "django", "4.2.7"))

	if _, err := rule.Run(context.Background(), db, in); err != nil {
		t.Fatalf("first call err: %v", err)
	}
	if _, err := rule.Run(context.Background(), db, in); err != nil {
		t.Fatalf("second call err: %v", err)
	}
	if stub.callCount() != 1 {
		t.Errorf("expected stub hit only once (cached), got %d", stub.callCount())
	}
}

// TestSUPPLY001_NotFound_NotCached — call twice with a 404 dep; the
// stub MUST be hit twice. Anti-cheat enforcement: a package that
// 404'd today might be real tomorrow.
func TestSUPPLY001_NotFound_NotCached(t *testing.T) {
	stub := newStubCodeArtifact()
	stub.on(codeartifact.EcosystemRubyGems, "ghost-gem", "0.0.1", func() (codeartifact.PackageVersionInfo, error) {
		return codeartifact.PackageVersionInfo{}, codeartifact.ErrPackageNotFound
	})
	rule := NewSUPPLY001(stub)

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	in := makeInput("feat/c2", "abc123", "", "Gemfile", manifests.EcosystemRubyGems,
		dep(manifests.EcosystemRubyGems, "ghost-gem", "0.0.1"))

	findings1, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("first call err: %v", err)
	}
	if len(findings1) != 1 {
		t.Fatalf("first call: expected 1 finding, got %d", len(findings1))
	}

	// Second call MUST hit the stub again — anti-cheat: no negative
	// cache.
	findings2, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("second call err: %v", err)
	}
	if len(findings2) != 1 {
		t.Fatalf("second call: expected 1 finding (re-checked), got %d", len(findings2))
	}
	if stub.callCount() != 2 {
		t.Errorf("expected 2 stub hits (no negative cache), got %d", stub.callCount())
	}
}

// TestSUPPLY001_OtherError_Wrapped — an unrelated error → Run returns
// a wrapped error, but partial findings still come through.
func TestSUPPLY001_OtherError_Wrapped(t *testing.T) {
	weird := errors.New("registry hiccup of unknown classification")
	stub := newStubCodeArtifact()
	stub.on(codeartifact.EcosystemNPM, "ok-pkg", "1.0.0", func() (codeartifact.PackageVersionInfo, error) {
		return codeartifact.PackageVersionInfo{Name: "ok-pkg", Version: "1.0.0", Status: "Published"}, nil
	})
	stub.on(codeartifact.EcosystemNPM, "missing", "9.9.9", func() (codeartifact.PackageVersionInfo, error) {
		return codeartifact.PackageVersionInfo{}, codeartifact.ErrPackageNotFound
	})
	stub.on(codeartifact.EcosystemNPM, "weird", "0.0.0", func() (codeartifact.PackageVersionInfo, error) {
		return codeartifact.PackageVersionInfo{}, weird
	})
	rule := NewSUPPLY001(stub)

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	in := makeInput("feat/err", "abc123", "", "package.json", manifests.EcosystemNPM,
		dep(manifests.EcosystemNPM, "ok-pkg", "1.0.0"),
		dep(manifests.EcosystemNPM, "missing", "9.9.9"),
		dep(manifests.EcosystemNPM, "weird", "0.0.0"),
	)

	findings, err := rule.Run(context.Background(), db, in)
	if err == nil {
		t.Fatal("expected wrapped error from Run, got nil")
	}
	// Partial-progress: the 404 must still surface as a finding even
	// though "weird" produced a Run-level error.
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding (the 404), got %d: %+v", len(findings), findings)
	}
	if !strings.Contains(findings[0].Message, "missing") {
		t.Errorf("expected 'missing' finding, got: %q", findings[0].Message)
	}
	// The wrapped error should reference the dep that produced it.
	if !strings.Contains(err.Error(), "weird") {
		t.Errorf("expected error to mention 'weird', got: %v", err)
	}
}

// TestSUPPLY001_GoEcosystem_SilentlySkipped — Go is declared as an
// ecosystem but CodeArtifact has no Go format. Run must not call the
// stub for Go deps and must not emit findings or errors.
func TestSUPPLY001_GoEcosystem_SilentlySkipped(t *testing.T) {
	stub := newStubCodeArtifact()
	rule := NewSUPPLY001(stub)

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	in := makeInput("feat/go", "abc123", "", "go.mod", manifests.EcosystemGo,
		dep(manifests.EcosystemGo, "github.com/example/pkg", "v1.2.3"),
	)

	findings, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected 0 findings for Go (skipped), got %d: %+v", len(findings), findings)
	}
	if stub.callCount() != 0 {
		t.Errorf("expected 0 stub calls for Go deps, got %d", stub.callCount())
	}
}

// TestSUPPLY001_NilClient_Errors — a rule constructed with nil client
// must return an error from Run rather than panicking.
func TestSUPPLY001_NilClient_Errors(t *testing.T) {
	rule := NewSUPPLY001(nil)
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	in := makeInput("feat/x", "abc", "", "Gemfile", manifests.EcosystemRubyGems,
		dep(manifests.EcosystemRubyGems, "redis", "5.0.0"))
	_, err := rule.Run(context.Background(), db, in)
	if err == nil {
		t.Fatal("expected error from nil-client Run")
	}
}

// TestSUPPLY001_EmptyDepsSkipped — deps without a version pin (range
// / unknown) must NOT trigger registry calls.
func TestSUPPLY001_EmptyDepsSkipped(t *testing.T) {
	stub := newStubCodeArtifact()
	rule := NewSUPPLY001(stub)
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	in := makeInput("feat/x", "abc", "", "Gemfile", manifests.EcosystemRubyGems,
		manifests.Dependency{Ecosystem: manifests.EcosystemRubyGems, Name: "rails", Version: "", Source: manifests.SourceDirect},
	)
	findings, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for unversioned dep, got %d", len(findings))
	}
	if stub.callCount() != 0 {
		t.Errorf("expected 0 stub calls for unversioned deps, got %d", stub.callCount())
	}
}
