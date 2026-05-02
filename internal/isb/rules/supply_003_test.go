package rules

import (
	"context"
	"strings"
	"testing"
	"time"

	"force-orchestrator/internal/clients/codeartifact"
	"force-orchestrator/internal/isb"
	"force-orchestrator/internal/isb/scanners/manifests"
	"force-orchestrator/internal/isb/supplydeferral"
	"force-orchestrator/internal/store"
)

// TestSUPPLY003_FreshPackage_NoFinding — package published well within
// the threshold → no finding, single registry call.
func TestSUPPLY003_FreshPackage_NoFinding(t *testing.T) {
	stub := newStubCodeArtifact()
	stub.on(codeartifact.EcosystemNPM, "react", "18.2.0", func() (codeartifact.PackageVersionInfo, error) {
		return codeartifact.PackageVersionInfo{
			Name:        "react",
			Version:     "18.2.0",
			Status:      "Published",
			PublishedAt: time.Now().Add(-30 * 24 * time.Hour), // 30 days ago — fresh
		}, nil
	})
	rule := NewSUPPLY003(stub)

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	in := makeInput("feat/x", "deadbeef", "", "package.json", manifests.EcosystemNPM,
		dep(manifests.EcosystemNPM, "react", "18.2.0"))

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

// TestSUPPLY003_StalePackage_AdviseFinding — published earlier than the
// default 730-day cutoff → advise-severity finding whose message
// includes the published date and threshold.
func TestSUPPLY003_StalePackage_AdviseFinding(t *testing.T) {
	publishedAt := time.Now().Add(-1000 * 24 * time.Hour) // ~2.7 years ago
	stub := newStubCodeArtifact()
	stub.on(codeartifact.EcosystemPyPI, "abandonedpkg", "0.0.1", func() (codeartifact.PackageVersionInfo, error) {
		return codeartifact.PackageVersionInfo{
			Name:        "abandonedpkg",
			Version:     "0.0.1",
			Status:      "Published",
			PublishedAt: publishedAt,
		}, nil
	})
	rule := NewSUPPLY003(stub)

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	in := makeInput("feat/x", "deadbeef", "", "requirements.txt", manifests.EcosystemPyPI,
		dep(manifests.EcosystemPyPI, "abandonedpkg", "0.0.1"))

	findings, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d: %+v", len(findings), findings)
	}
	f := findings[0]
	if f.RuleID != "SUPPLY-003" {
		t.Errorf("rule id mismatch: %q", f.RuleID)
	}
	if f.Severity != isb.SeverityAdvise {
		t.Errorf("severity mismatch: %q (want advise)", f.Severity)
	}
	if f.Path != "requirements.txt" {
		t.Errorf("path mismatch: %q", f.Path)
	}
	if !strings.Contains(f.Message, "pypi") {
		t.Errorf("message missing ecosystem: %q", f.Message)
	}
	if !strings.Contains(f.Message, "abandonedpkg") {
		t.Errorf("message missing dep name: %q", f.Message)
	}
	if !strings.Contains(f.Message, "0.0.1") {
		t.Errorf("message missing version: %q", f.Message)
	}
	wantDate := publishedAt.UTC().Format("2006-01-02")
	if !strings.Contains(f.Message, wantDate) {
		t.Errorf("message missing published date %q: %q", wantDate, f.Message)
	}
	if !strings.Contains(f.Message, "730") {
		t.Errorf("message missing threshold (default 730): %q", f.Message)
	}
	if !strings.Contains(f.Message, "maintained alternative") {
		t.Errorf("message missing alternative-suggestion phrasing: %q", f.Message)
	}
}

// TestSUPPLY003_BoundaryAtThreshold — exactly at the threshold is NOT
// stale; one second past is. This pins the half-open interval semantics
// (stale := published.Before(now − threshold), strictly less than).
func TestSUPPLY003_BoundaryAtThreshold(t *testing.T) {
	thresholdDays := 730
	threshold := time.Duration(thresholdDays) * 24 * time.Hour

	// Case A: exactly at the threshold (within microseconds). Not stale.
	atBoundary := time.Now().Add(-threshold + 1*time.Second)
	// Case B: just past the threshold by 2 seconds. Stale.
	pastBoundary := time.Now().Add(-threshold - 2*time.Second)

	stub := newStubCodeArtifact()
	stub.on(codeartifact.EcosystemNPM, "atedge", "1.0.0", func() (codeartifact.PackageVersionInfo, error) {
		return codeartifact.PackageVersionInfo{
			Name: "atedge", Version: "1.0.0", Status: "Published",
			PublishedAt: atBoundary,
		}, nil
	})
	stub.on(codeartifact.EcosystemNPM, "pastedge", "1.0.0", func() (codeartifact.PackageVersionInfo, error) {
		return codeartifact.PackageVersionInfo{
			Name: "pastedge", Version: "1.0.0", Status: "Published",
			PublishedAt: pastBoundary,
		}, nil
	})
	rule := NewSUPPLY003(stub)

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	in := makeInput("feat/edge", "abc", "", "package.json", manifests.EcosystemNPM,
		dep(manifests.EcosystemNPM, "atedge", "1.0.0"),
		dep(manifests.EcosystemNPM, "pastedge", "1.0.0"))

	findings, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding (only pastedge is stale), got %d: %+v", len(findings), findings)
	}
	if !strings.Contains(findings[0].Message, "pastedge") {
		t.Errorf("expected pastedge finding, got: %q", findings[0].Message)
	}
}

// TestSUPPLY003_TokenExpired_DeferralLogged — auth error → deferral
// row written via supplydeferral.RecordDeferral, no finding emitted.
// Mirrors SUPPLY-001's behaviour but writes RuleKey=SUPPLY-003.
func TestSUPPLY003_TokenExpired_DeferralLogged(t *testing.T) {
	stub := newStubCodeArtifact()
	stub.on(codeartifact.EcosystemPyPI, "requests", "2.31.0", func() (codeartifact.PackageVersionInfo, error) {
		return codeartifact.PackageVersionInfo{}, codeartifact.ErrTokenExpired
	})
	rule := NewSUPPLY003(stub)

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

	rows, err := supplydeferral.ListPendingDeferrals(db, "feat/auth-x")
	if err != nil {
		t.Fatalf("ListPendingDeferrals: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 deferral row, got %d", len(rows))
	}
	row := rows[0]
	if row.Payload.RuleKey != "SUPPLY-003" {
		t.Errorf("rule key in payload: %q (want SUPPLY-003)", row.Payload.RuleKey)
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
	if row.FindingID == 0 {
		t.Errorf("expected non-zero finding id")
	}
}

// TestSUPPLY003_Throttle_RetryOnce_ThenAdvise — first call returns
// ErrTransient, second also fails → no finding (advise-mode), but
// the stub was called twice.
func TestSUPPLY003_Throttle_RetryOnce_ThenAdvise(t *testing.T) {
	stub := newStubCodeArtifact()
	stub.on(codeartifact.EcosystemMaven, "org.example:slow", "1.0", func() (codeartifact.PackageVersionInfo, error) {
		return codeartifact.PackageVersionInfo{}, codeartifact.ErrTransient
	})
	rule := NewSUPPLY003(stub)
	prev := supplyTransientRetryDelay
	supplyTransientRetryDelay = 1 * time.Millisecond
	defer func() { supplyTransientRetryDelay = prev }()

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	in := makeInput("feat/m", "feedbeef", "", "pom.xml", manifests.EcosystemMaven,
		dep(manifests.EcosystemMaven, "org.example:slow", "1.0"))

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

// TestSUPPLY003_NotFound_NoFinding — ErrPackageNotFound is SUPPLY-001's
// territory. SUPPLY-003 must silently skip — no finding, no error.
func TestSUPPLY003_NotFound_NoFinding(t *testing.T) {
	stub := newStubCodeArtifact()
	stub.on(codeartifact.EcosystemRubyGems, "ghost-gem", "0.0.1", func() (codeartifact.PackageVersionInfo, error) {
		return codeartifact.PackageVersionInfo{}, codeartifact.ErrPackageNotFound
	})
	rule := NewSUPPLY003(stub)

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	in := makeInput("feat/notfound", "abc", "", "Gemfile", manifests.EcosystemRubyGems,
		dep(manifests.EcosystemRubyGems, "ghost-gem", "0.0.1"))

	findings, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected 0 findings (404 is SUPPLY-001's job), got %d: %+v", len(findings), findings)
	}
}

// TestSUPPLY003_PositiveCache_HitWithin24h — call twice with the same
// (ecosystem, name, version) and a fresh PublishedAt; the second call
// must NOT re-hit the stub.
func TestSUPPLY003_PositiveCache_HitWithin24h(t *testing.T) {
	stub := newStubCodeArtifact()
	stub.on(codeartifact.EcosystemPyPI, "django", "4.2.7", func() (codeartifact.PackageVersionInfo, error) {
		return codeartifact.PackageVersionInfo{
			Name: "django", Version: "4.2.7", Status: "Published",
			PublishedAt: time.Now().Add(-7 * 24 * time.Hour),
		}, nil
	})
	rule := NewSUPPLY003(stub)

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

// TestSUPPLY003_StaleNotCached — anti-cheat: a stale-detected dep MUST
// be re-checked on subsequent Runs (no negative cache). This mirrors
// SUPPLY-001's "404 not cached" rule and is justified by the same
// reasoning: a new release tomorrow flips the verdict.
func TestSUPPLY003_StaleNotCached(t *testing.T) {
	stub := newStubCodeArtifact()
	stub.on(codeartifact.EcosystemNPM, "rusted", "0.1.0", func() (codeartifact.PackageVersionInfo, error) {
		return codeartifact.PackageVersionInfo{
			Name: "rusted", Version: "0.1.0", Status: "Published",
			PublishedAt: time.Now().Add(-1500 * 24 * time.Hour),
		}, nil
	})
	rule := NewSUPPLY003(stub)

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	in := makeInput("feat/c2", "abc123", "", "package.json", manifests.EcosystemNPM,
		dep(manifests.EcosystemNPM, "rusted", "0.1.0"))

	findings1, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("first call err: %v", err)
	}
	if len(findings1) != 1 {
		t.Fatalf("first call: expected 1 finding, got %d", len(findings1))
	}

	findings2, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("second call err: %v", err)
	}
	if len(findings2) != 1 {
		t.Fatalf("second call: expected 1 finding (re-checked), got %d", len(findings2))
	}
	if stub.callCount() != 2 {
		t.Errorf("expected 2 stub hits (no negative cache for stale), got %d", stub.callCount())
	}
}

// TestSUPPLY003_CustomThreshold_FromSystemConfig — operator override
// via SystemConfig key supply_stale_threshold_days. Set to 30; a dep
// published 60 days ago must trigger a finding (would be fresh under
// the default 730).
func TestSUPPLY003_CustomThreshold_FromSystemConfig(t *testing.T) {
	stub := newStubCodeArtifact()
	publishedAt := time.Now().Add(-60 * 24 * time.Hour)
	stub.on(codeartifact.EcosystemNPM, "agedpkg", "1.0.0", func() (codeartifact.PackageVersionInfo, error) {
		return codeartifact.PackageVersionInfo{
			Name: "agedpkg", Version: "1.0.0", Status: "Published",
			PublishedAt: publishedAt,
		}, nil
	})
	rule := NewSUPPLY003(stub)

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.SetConfig(db, supplyStaleConfigKey, "30")

	in := makeInput("feat/cfg", "abc123", "", "package.json", manifests.EcosystemNPM,
		dep(manifests.EcosystemNPM, "agedpkg", "1.0.0"))

	findings, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding (60d > 30d threshold), got %d: %+v", len(findings), findings)
	}
	if !strings.Contains(findings[0].Message, "30") {
		t.Errorf("message missing custom threshold (30): %q", findings[0].Message)
	}
}

// TestSUPPLY003_PartialFailure_OtherDepsStillProcessed — three deps,
// mixed outcomes. Confirms no short-circuit: dep #1's stale finding
// lands even though dep #3's token-expired triggers a deferral.
func TestSUPPLY003_PartialFailure_OtherDepsStillProcessed(t *testing.T) {
	stub := newStubCodeArtifact()
	stub.on(codeartifact.EcosystemNPM, "stale-dep", "1.0.0", func() (codeartifact.PackageVersionInfo, error) {
		return codeartifact.PackageVersionInfo{
			Name: "stale-dep", Version: "1.0.0", Status: "Published",
			PublishedAt: time.Now().Add(-1500 * 24 * time.Hour),
		}, nil
	})
	stub.on(codeartifact.EcosystemNPM, "fresh-dep", "2.0.0", func() (codeartifact.PackageVersionInfo, error) {
		return codeartifact.PackageVersionInfo{
			Name: "fresh-dep", Version: "2.0.0", Status: "Published",
			PublishedAt: time.Now().Add(-30 * 24 * time.Hour),
		}, nil
	})
	stub.on(codeartifact.EcosystemNPM, "deferred-dep", "3.0.0", func() (codeartifact.PackageVersionInfo, error) {
		return codeartifact.PackageVersionInfo{}, codeartifact.ErrTokenExpired
	})
	rule := NewSUPPLY003(stub)

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	in := makeInput("feat/multi", "12345abc", "", "package.json", manifests.EcosystemNPM,
		dep(manifests.EcosystemNPM, "stale-dep", "1.0.0"),
		dep(manifests.EcosystemNPM, "fresh-dep", "2.0.0"),
		dep(manifests.EcosystemNPM, "deferred-dep", "3.0.0"),
	)

	findings, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding (for stale-dep), got %d: %+v", len(findings), findings)
	}
	if !strings.Contains(findings[0].Message, "stale-dep") {
		t.Errorf("expected finding for stale-dep, got: %q", findings[0].Message)
	}

	defs, err := supplydeferral.ListPendingDeferrals(db, "feat/multi")
	if err != nil {
		t.Fatalf("ListPendingDeferrals: %v", err)
	}
	if len(defs) != 1 {
		t.Fatalf("expected 1 deferral for deferred-dep, got %d", len(defs))
	}
	if defs[0].Payload.DepsAdded[0].Name != "deferred-dep" {
		t.Errorf("deferral dep mismatch: %+v", defs[0].Payload.DepsAdded)
	}
	if defs[0].Payload.RuleKey != "SUPPLY-003" {
		t.Errorf("deferral RuleKey mismatch: %q", defs[0].Payload.RuleKey)
	}

	if stub.callCount() != 3 {
		t.Errorf("expected 3 stub calls (one per dep), got %d", stub.callCount())
	}
}

// TestSUPPLY003_GoEcosystem_SilentlySkipped — Go is declared as an
// ecosystem but CodeArtifact has no Go format. Run must not call the
// stub for Go deps and must not emit findings or errors.
func TestSUPPLY003_GoEcosystem_SilentlySkipped(t *testing.T) {
	stub := newStubCodeArtifact()
	rule := NewSUPPLY003(stub)

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	in := makeInput("feat/go", "abc123", "", "go.mod", manifests.EcosystemGo,
		dep(manifests.EcosystemGo, "github.com/example/pkg", "v1.2.3"))

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

// TestSUPPLY003_NilClient_Errors — a rule constructed with nil client
// must return an error from Run rather than panicking.
func TestSUPPLY003_NilClient_Errors(t *testing.T) {
	rule := NewSUPPLY003(nil)
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	in := makeInput("feat/x", "abc", "", "Gemfile", manifests.EcosystemRubyGems,
		dep(manifests.EcosystemRubyGems, "redis", "5.0.0"))
	_, err := rule.Run(context.Background(), db, in)
	if err == nil {
		t.Fatal("expected error from nil-client Run")
	}
}

// TestSUPPLY003_ZeroPublishedAt_NoFinding — when CodeArtifact returns
// a zero PublishedAt the rule must silently skip rather than guess
// (treats the publish-time as unknown).
func TestSUPPLY003_ZeroPublishedAt_NoFinding(t *testing.T) {
	stub := newStubCodeArtifact()
	stub.on(codeartifact.EcosystemPyPI, "mystery", "1.0.0", func() (codeartifact.PackageVersionInfo, error) {
		return codeartifact.PackageVersionInfo{
			Name: "mystery", Version: "1.0.0", Status: "Published",
			// PublishedAt zero on purpose
		}, nil
	})
	rule := NewSUPPLY003(stub)

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	in := makeInput("feat/zero", "abc", "", "requirements.txt", manifests.EcosystemPyPI,
		dep(manifests.EcosystemPyPI, "mystery", "1.0.0"))

	findings, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected 0 findings on zero PublishedAt, got %d: %+v", len(findings), findings)
	}
}

// TestSUPPLY003_InvalidThreshold_FallsBackToDefault — operator typo'd
// the SystemConfig value; the rule logs+ignores and uses the 730 default.
func TestSUPPLY003_InvalidThreshold_FallsBackToDefault(t *testing.T) {
	stub := newStubCodeArtifact()
	// 60 days ago — fresh under default 730, stale under 30.
	stub.on(codeartifact.EcosystemNPM, "borderline", "1.0.0", func() (codeartifact.PackageVersionInfo, error) {
		return codeartifact.PackageVersionInfo{
			Name: "borderline", Version: "1.0.0", Status: "Published",
			PublishedAt: time.Now().Add(-60 * 24 * time.Hour),
		}, nil
	})
	rule := NewSUPPLY003(stub)

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.SetConfig(db, supplyStaleConfigKey, "not-a-number")

	in := makeInput("feat/bad", "abc", "", "package.json", manifests.EcosystemNPM,
		dep(manifests.EcosystemNPM, "borderline", "1.0.0"))

	findings, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected 0 findings under default 730, got %d: %+v", len(findings), findings)
	}
}
