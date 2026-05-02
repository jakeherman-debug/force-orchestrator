package rules

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"force-orchestrator/internal/clients/codeartifact"
	"force-orchestrator/internal/isb"
	"force-orchestrator/internal/isb/scanners/manifests"
	"force-orchestrator/internal/isb/supplydeferral"
	"force-orchestrator/internal/store"
)

// supply004MakeInput is a small parallel of makeInput in
// supply_001_test.go, but it lets the caller override TargetRepo so
// the rule can look up the repo's license. (makeInput hard-codes
// "example/repo".)
func supply004MakeInput(branch, sha, manifestPath, targetRepo string, eco manifests.Ecosystem, deps ...manifests.Dependency) isb.ManifestGatedInput {
	return isb.ManifestGatedInput{
		SourceTaskID: 99,
		TargetRepo:   targetRepo,
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

// TestSUPPLY004_CompatibleLicenses_NoFinding — MIT repo + MIT dep →
// matrix says "allowed" → no finding.
func TestSUPPLY004_CompatibleLicenses_NoFinding(t *testing.T) {
	stub := newStubCodeArtifact()
	stub.on(codeartifact.EcosystemNPM, "tinyperm", "1.0.0", func() (codeartifact.PackageVersionInfo, error) {
		return codeartifact.PackageVersionInfo{Name: "tinyperm", Version: "1.0.0", License: "MIT", Status: "Published"}, nil
	})

	rule, err := NewSUPPLY004(stub)
	if err != nil {
		t.Fatalf("NewSUPPLY004: %v", err)
	}

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	if _, err := db.Exec(`INSERT INTO Repositories (name, local_path, mode, license) VALUES (?, '', 'write', ?)`, "myrepo", "MIT"); err != nil {
		t.Fatalf("insert repo: %v", err)
	}

	in := supply004MakeInput("feat/x", "abc", "package.json", "myrepo", manifests.EcosystemNPM,
		dep(manifests.EcosystemNPM, "tinyperm", "1.0.0"))

	findings, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected 0 findings, got %d: %+v", len(findings), findings)
	}
}

// TestSUPPLY004_IncompatibleLicenses_AdviseFinding — MIT repo +
// GPL-3.0 dep → matrix says "deny" → advise-mode finding.
func TestSUPPLY004_IncompatibleLicenses_AdviseFinding(t *testing.T) {
	stub := newStubCodeArtifact()
	stub.on(codeartifact.EcosystemNPM, "viral-pkg", "2.0.0", func() (codeartifact.PackageVersionInfo, error) {
		return codeartifact.PackageVersionInfo{Name: "viral-pkg", Version: "2.0.0", License: "GPL-3.0", Status: "Published"}, nil
	})

	rule, err := NewSUPPLY004(stub)
	if err != nil {
		t.Fatalf("NewSUPPLY004: %v", err)
	}

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	if _, err := db.Exec(`INSERT INTO Repositories (name, local_path, mode, license) VALUES (?, '', 'write', ?)`, "myrepo", "MIT"); err != nil {
		t.Fatalf("insert repo: %v", err)
	}

	in := supply004MakeInput("feat/x", "abc", "package.json", "myrepo", manifests.EcosystemNPM,
		dep(manifests.EcosystemNPM, "viral-pkg", "2.0.0"))

	findings, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d: %+v", len(findings), findings)
	}
	f := findings[0]
	if f.RuleID != "SUPPLY-004" {
		t.Errorf("rule id: %q", f.RuleID)
	}
	if f.Severity != isb.SeverityAdvise {
		t.Errorf("severity: %q (want advise)", f.Severity)
	}
	if !strings.Contains(f.Message, "MIT") || !strings.Contains(f.Message, "GPL-3.0") {
		t.Errorf("message missing licenses: %q", f.Message)
	}
	if !strings.Contains(f.Message, "incompatible") {
		t.Errorf("message missing incompatible marker: %q", f.Message)
	}
}

// TestSUPPLY004_LicensePairNotInMatrix_AdviseFinding — repo license
// "WTFPL" is not a matrix key → advise-mode (operator review). NEVER
// auto-allow.
func TestSUPPLY004_LicensePairNotInMatrix_AdviseFinding(t *testing.T) {
	stub := newStubCodeArtifact()
	stub.on(codeartifact.EcosystemNPM, "perm-pkg", "1.0.0", func() (codeartifact.PackageVersionInfo, error) {
		return codeartifact.PackageVersionInfo{Name: "perm-pkg", Version: "1.0.0", License: "MIT", Status: "Published"}, nil
	})

	rule, err := NewSUPPLY004(stub)
	if err != nil {
		t.Fatalf("NewSUPPLY004: %v", err)
	}

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	if _, err := db.Exec(`INSERT INTO Repositories (name, local_path, mode, license) VALUES (?, '', 'write', ?)`, "exotic-repo", "WTFPL"); err != nil {
		t.Fatalf("insert repo: %v", err)
	}

	in := supply004MakeInput("feat/x", "abc", "package.json", "exotic-repo", manifests.EcosystemNPM,
		dep(manifests.EcosystemNPM, "perm-pkg", "1.0.0"))

	findings, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding (advise), got %d: %+v", len(findings), findings)
	}
	if !strings.Contains(findings[0].Message, "operator review") {
		t.Errorf("expected operator-review marker, got: %q", findings[0].Message)
	}
	if !strings.Contains(findings[0].Message, "WTFPL") {
		t.Errorf("message missing repo license: %q", findings[0].Message)
	}
}

// TestSUPPLY004_RepoLicenseUnknown_AdviseFinding — Repositories.license
// is empty → advise-mode (operator review). The dep's license isn't
// even queried.
func TestSUPPLY004_RepoLicenseUnknown_AdviseFinding(t *testing.T) {
	stub := newStubCodeArtifact()
	stub.on(codeartifact.EcosystemNPM, "anything", "1.0.0", func() (codeartifact.PackageVersionInfo, error) {
		return codeartifact.PackageVersionInfo{Name: "anything", Version: "1.0.0", License: "MIT", Status: "Published"}, nil
	})

	rule, err := NewSUPPLY004(stub)
	if err != nil {
		t.Fatalf("NewSUPPLY004: %v", err)
	}

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// repo with empty license — pre-D5 backfill row shape.
	if _, err := db.Exec(`INSERT INTO Repositories (name, local_path, mode, license) VALUES (?, '', 'write', ?)`, "no-license-repo", ""); err != nil {
		t.Fatalf("insert repo: %v", err)
	}

	in := supply004MakeInput("feat/x", "abc", "package.json", "no-license-repo", manifests.EcosystemNPM,
		dep(manifests.EcosystemNPM, "anything", "1.0.0"))

	findings, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d: %+v", len(findings), findings)
	}
	if !strings.Contains(findings[0].Message, "repo license not declared") {
		t.Errorf("expected repo-license-not-declared marker, got: %q", findings[0].Message)
	}
	// Stub must NOT be called: with no repo license we short-circuit.
	if stub.callCount() != 0 {
		t.Errorf("expected 0 stub calls (short-circuit on empty repo license), got %d", stub.callCount())
	}
}

// TestSUPPLY004_DepLicenseUnknown_AdviseFinding — CodeArtifact returns
// license="" → advise-mode (operator review). NEVER auto-allow.
func TestSUPPLY004_DepLicenseUnknown_AdviseFinding(t *testing.T) {
	stub := newStubCodeArtifact()
	stub.on(codeartifact.EcosystemMaven, "org.example:silent", "1.0", func() (codeartifact.PackageVersionInfo, error) {
		return codeartifact.PackageVersionInfo{Name: "org.example:silent", Version: "1.0", License: "", Status: "Published"}, nil
	})

	rule, err := NewSUPPLY004(stub)
	if err != nil {
		t.Fatalf("NewSUPPLY004: %v", err)
	}

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	if _, err := db.Exec(`INSERT INTO Repositories (name, local_path, mode, license) VALUES (?, '', 'write', ?)`, "myrepo", "Apache-2.0"); err != nil {
		t.Fatalf("insert repo: %v", err)
	}

	in := supply004MakeInput("feat/x", "abc", "pom.xml", "myrepo", manifests.EcosystemMaven,
		dep(manifests.EcosystemMaven, "org.example:silent", "1.0"))

	findings, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d: %+v", len(findings), findings)
	}
	if !strings.Contains(findings[0].Message, "license unknown") {
		t.Errorf("expected license-unknown marker, got: %q", findings[0].Message)
	}
}

// TestSUPPLY004_TokenExpired_DeferralLogged — auth error → deferral
// row written via supplydeferral.RecordDeferral, no finding emitted.
func TestSUPPLY004_TokenExpired_DeferralLogged(t *testing.T) {
	stub := newStubCodeArtifact()
	stub.on(codeartifact.EcosystemPyPI, "django", "4.2.7", func() (codeartifact.PackageVersionInfo, error) {
		return codeartifact.PackageVersionInfo{}, codeartifact.ErrTokenExpired
	})

	rule, err := NewSUPPLY004(stub)
	if err != nil {
		t.Fatalf("NewSUPPLY004: %v", err)
	}

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	if _, err := db.Exec(`INSERT INTO Repositories (name, local_path, mode, license) VALUES (?, '', 'write', ?)`, "myrepo", "MIT"); err != nil {
		t.Fatalf("insert repo: %v", err)
	}

	in := supply004MakeInput("feat/auth-x", "cafef00d", "requirements.txt", "myrepo", manifests.EcosystemPyPI,
		dep(manifests.EcosystemPyPI, "django", "4.2.7"))

	findings, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("Run: %v", err)
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
	if rows[0].Payload.RuleKey != "SUPPLY-004" {
		t.Errorf("rule key: %q", rows[0].Payload.RuleKey)
	}
	if rows[0].Payload.ManifestPath != "requirements.txt" {
		t.Errorf("manifest path: %q", rows[0].Payload.ManifestPath)
	}
	if len(rows[0].Payload.DepsAdded) != 1 || rows[0].Payload.DepsAdded[0].Name != "django" {
		t.Errorf("deps in payload: %+v", rows[0].Payload.DepsAdded)
	}
}

// TestSUPPLY004_Throttle_RetryOnce_ThenAdvise — first call returns
// ErrTransient, second also fails → no finding (advise-mode through),
// stub called twice.
func TestSUPPLY004_Throttle_RetryOnce_ThenAdvise(t *testing.T) {
	stub := newStubCodeArtifact()
	stub.on(codeartifact.EcosystemMaven, "org.example:flake", "1.0", func() (codeartifact.PackageVersionInfo, error) {
		return codeartifact.PackageVersionInfo{}, codeartifact.ErrTransient
	})

	rule, err := NewSUPPLY004(stub)
	if err != nil {
		t.Fatalf("NewSUPPLY004: %v", err)
	}

	prev := supply004TransientRetryDelay
	supply004TransientRetryDelay = 1 * time.Millisecond
	defer func() { supply004TransientRetryDelay = prev }()

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	if _, err := db.Exec(`INSERT INTO Repositories (name, local_path, mode, license) VALUES (?, '', 'write', ?)`, "myrepo", "MIT"); err != nil {
		t.Fatalf("insert repo: %v", err)
	}

	in := supply004MakeInput("feat/m", "feedbeef", "pom.xml", "myrepo", manifests.EcosystemMaven,
		dep(manifests.EcosystemMaven, "org.example:flake", "1.0"))

	findings, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected 0 findings on transient fallback, got %d: %+v", len(findings), findings)
	}
	if stub.callCount() != 2 {
		t.Errorf("expected 2 stub calls (retry once), got %d", stub.callCount())
	}
}

// TestSUPPLY004_PositiveCache_HitWithin24h — call twice with the same
// (eco, name, version); second call must NOT re-hit the stub and the
// finding outcome must be identical.
func TestSUPPLY004_PositiveCache_HitWithin24h(t *testing.T) {
	stub := newStubCodeArtifact()
	stub.on(codeartifact.EcosystemPyPI, "requests", "2.31.0", func() (codeartifact.PackageVersionInfo, error) {
		return codeartifact.PackageVersionInfo{Name: "requests", Version: "2.31.0", License: "Apache-2.0", Status: "Published"}, nil
	})

	rule, err := NewSUPPLY004(stub)
	if err != nil {
		t.Fatalf("NewSUPPLY004: %v", err)
	}

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	if _, err := db.Exec(`INSERT INTO Repositories (name, local_path, mode, license) VALUES (?, '', 'write', ?)`, "myrepo", "MIT"); err != nil {
		t.Fatalf("insert repo: %v", err)
	}

	in := supply004MakeInput("feat/c", "abc", "requirements.txt", "myrepo", manifests.EcosystemPyPI,
		dep(manifests.EcosystemPyPI, "requests", "2.31.0"))

	if _, err := rule.Run(context.Background(), db, in); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if _, err := rule.Run(context.Background(), db, in); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if stub.callCount() != 1 {
		t.Errorf("expected stub hit once (cached), got %d", stub.callCount())
	}
}

// TestSUPPLY004_PartialFailure_OtherDepsStillProcessed — three deps,
// mixed outcomes. Confirms no short-circuit on other-error paths.
func TestSUPPLY004_PartialFailure_OtherDepsStillProcessed(t *testing.T) {
	weird := errors.New("registry hiccup of unknown classification")
	stub := newStubCodeArtifact()
	stub.on(codeartifact.EcosystemNPM, "ok-pkg", "1.0.0", func() (codeartifact.PackageVersionInfo, error) {
		return codeartifact.PackageVersionInfo{Name: "ok-pkg", Version: "1.0.0", License: "MIT", Status: "Published"}, nil
	})
	stub.on(codeartifact.EcosystemNPM, "viral", "9.9.9", func() (codeartifact.PackageVersionInfo, error) {
		return codeartifact.PackageVersionInfo{Name: "viral", Version: "9.9.9", License: "GPL-3.0", Status: "Published"}, nil
	})
	stub.on(codeartifact.EcosystemNPM, "weird", "0.0.0", func() (codeartifact.PackageVersionInfo, error) {
		return codeartifact.PackageVersionInfo{}, weird
	})

	rule, err := NewSUPPLY004(stub)
	if err != nil {
		t.Fatalf("NewSUPPLY004: %v", err)
	}

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	if _, err := db.Exec(`INSERT INTO Repositories (name, local_path, mode, license) VALUES (?, '', 'write', ?)`, "myrepo", "MIT"); err != nil {
		t.Fatalf("insert repo: %v", err)
	}

	in := supply004MakeInput("feat/multi", "12345abc", "package.json", "myrepo", manifests.EcosystemNPM,
		dep(manifests.EcosystemNPM, "ok-pkg", "1.0.0"),
		dep(manifests.EcosystemNPM, "viral", "9.9.9"),
		dep(manifests.EcosystemNPM, "weird", "0.0.0"),
	)

	findings, err := rule.Run(context.Background(), db, in)
	if err == nil {
		t.Fatalf("expected wrapped error from Run, got nil")
	}
	// Partial-progress: the GPL-3.0 dep must still surface as a finding
	// even though "weird" produced a Run-level error.
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding (the GPL-3.0 dep), got %d: %+v", len(findings), findings)
	}
	if !strings.Contains(findings[0].Message, "viral") {
		t.Errorf("expected viral finding, got: %q", findings[0].Message)
	}
	if !strings.Contains(err.Error(), "weird") {
		t.Errorf("expected error to mention 'weird', got: %v", err)
	}
}

// TestSUPPLY004_GoEcosystem_SilentlySkipped — Go is declared but
// CodeArtifact has no Go format; Run must not call the stub or emit
// findings for Go deps.
func TestSUPPLY004_GoEcosystem_SilentlySkipped(t *testing.T) {
	stub := newStubCodeArtifact()
	rule, err := NewSUPPLY004(stub)
	if err != nil {
		t.Fatalf("NewSUPPLY004: %v", err)
	}

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	if _, err := db.Exec(`INSERT INTO Repositories (name, local_path, mode, license) VALUES (?, '', 'write', ?)`, "myrepo", "MIT"); err != nil {
		t.Fatalf("insert repo: %v", err)
	}

	in := supply004MakeInput("feat/go", "abc", "go.mod", "myrepo", manifests.EcosystemGo,
		dep(manifests.EcosystemGo, "github.com/example/pkg", "v1.2.3"),
	)

	findings, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected 0 findings for Go (skipped), got %d: %+v", len(findings), findings)
	}
	if stub.callCount() != 0 {
		t.Errorf("expected 0 stub calls for Go deps, got %d", stub.callCount())
	}
}

// TestSUPPLY004_PackageNotFound_SilentSkip — ErrPackageNotFound is
// SUPPLY-001's domain (existence) — SUPPLY-004 silently skips so we
// don't double-flag.
func TestSUPPLY004_PackageNotFound_SilentSkip(t *testing.T) {
	stub := newStubCodeArtifact()
	stub.on(codeartifact.EcosystemNPM, "ghost-pkg", "0.0.1", func() (codeartifact.PackageVersionInfo, error) {
		return codeartifact.PackageVersionInfo{}, codeartifact.ErrPackageNotFound
	})

	rule, err := NewSUPPLY004(stub)
	if err != nil {
		t.Fatalf("NewSUPPLY004: %v", err)
	}

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	if _, err := db.Exec(`INSERT INTO Repositories (name, local_path, mode, license) VALUES (?, '', 'write', ?)`, "myrepo", "MIT"); err != nil {
		t.Fatalf("insert repo: %v", err)
	}

	in := supply004MakeInput("feat/n", "abc", "package.json", "myrepo", manifests.EcosystemNPM,
		dep(manifests.EcosystemNPM, "ghost-pkg", "0.0.1"))

	findings, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected 0 findings (SUPPLY-004 doesn't own existence), got %d: %+v", len(findings), findings)
	}
}

// TestSUPPLY004_NilClient_Errors — a rule constructed with nil client
// must return an error from Run rather than panicking.
func TestSUPPLY004_NilClient_Errors(t *testing.T) {
	rule, err := NewSUPPLY004(nil)
	if err != nil {
		t.Fatalf("NewSUPPLY004: %v", err)
	}
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	in := supply004MakeInput("feat/x", "abc", "package.json", "myrepo", manifests.EcosystemNPM,
		dep(manifests.EcosystemNPM, "ok-pkg", "1.0.0"))
	if _, err := rule.Run(context.Background(), db, in); err == nil {
		t.Fatal("expected error from nil-client Run")
	}
}

// TestSUPPLY004_EmptyDepsSkipped — deps without a version pin → no
// stub call, no finding (mirrors SUPPLY-001).
func TestSUPPLY004_EmptyDepsSkipped(t *testing.T) {
	stub := newStubCodeArtifact()
	rule, err := NewSUPPLY004(stub)
	if err != nil {
		t.Fatalf("NewSUPPLY004: %v", err)
	}

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	if _, err := db.Exec(`INSERT INTO Repositories (name, local_path, mode, license) VALUES (?, '', 'write', ?)`, "myrepo", "MIT"); err != nil {
		t.Fatalf("insert repo: %v", err)
	}

	in := supply004MakeInput("feat/x", "abc", "Gemfile", "myrepo", manifests.EcosystemRubyGems,
		manifests.Dependency{Ecosystem: manifests.EcosystemRubyGems, Name: "rails", Version: "", Source: manifests.SourceDirect},
	)

	findings, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for unversioned dep, got %d", len(findings))
	}
	if stub.callCount() != 0 {
		t.Errorf("expected 0 stub calls for unversioned deps, got %d", stub.callCount())
	}
}

// TestLoadLicenseMatrix_Embedded — the matrix YAML loads + has the
// keys this slice ships. Catches a malformed YAML or a renamed embed
// path before runtime.
func TestLoadLicenseMatrix_Embedded(t *testing.T) {
	m, err := LoadLicenseMatrix()
	if err != nil {
		t.Fatalf("LoadLicenseMatrix: %v", err)
	}
	if m == nil {
		t.Fatal("nil matrix")
	}

	wantKeys := []string{"MIT", "Apache-2.0", "BSD-3-Clause", "BSD-2-Clause", "ISC", "GPL-3.0", "GPL-2.0", "AGPL-3.0", "LGPL-3.0", "LGPL-2.1", "MPL-2.0", "Unlicense", "CC0-1.0"}
	for _, k := range wantKeys {
		if _, ok := m[k]; !ok {
			t.Errorf("matrix missing key %q", k)
		}
	}

	// Sanity-check a couple of rows for shape.
	mit, ok := m["MIT"]
	if !ok {
		t.Fatal("MIT key missing")
	}
	if len(mit.Allowed) == 0 {
		t.Errorf("MIT.allowed empty")
	}
	if len(mit.Deny) == 0 {
		t.Errorf("MIT.deny empty (expected GPL family)")
	}
}

// TestCheckLicenseCompatibility_KnownPairs — table-driven matrix
// lookup unit test. Doesn't go through the rule, just the helper.
func TestCheckLicenseCompatibility_KnownPairs(t *testing.T) {
	m, err := LoadLicenseMatrix()
	if err != nil {
		t.Fatalf("LoadLicenseMatrix: %v", err)
	}

	cases := []struct {
		name        string
		repo, dep   string
		wantAllowed bool
		wantDenied  bool
	}{
		{"MIT+MIT allowed", "MIT", "MIT", true, false},
		{"MIT+Apache-2.0 allowed", "MIT", "Apache-2.0", true, false},
		{"MIT+GPL-3.0 denied", "MIT", "GPL-3.0", false, true},
		{"MIT+AGPL-3.0 denied", "MIT", "AGPL-3.0", false, true},
		{"Apache-2.0+MIT allowed", "Apache-2.0", "MIT", true, false},
		{"Apache-2.0+GPL-3.0 denied", "Apache-2.0", "GPL-3.0", false, true},
		{"GPL-3.0+MIT allowed", "GPL-3.0", "MIT", true, false},
		{"GPL-3.0+anything denied is empty (absorbs)", "GPL-3.0", "Apache-2.0", true, false},
		{"BSD-3-Clause+MIT allowed", "BSD-3-Clause", "MIT", true, false},
		{"BSD-3-Clause+GPL-2.0 denied", "BSD-3-Clause", "GPL-2.0", false, true},
		{"unknown repo license falls through to advise", "WTFPL", "MIT", false, false},
		{"unknown dep license under MIT falls through to advise", "MIT", "Made-Up-1.0", false, false},
		{"MPL-2.0 absent under MIT row → advise", "MIT", "MPL-2.0", false, false},
		{"empty repo license → advise", "", "MIT", false, false},
		{"empty dep license → advise", "MIT", "", false, false},
		{"both empty → advise", "", "", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			allowed, denied := CheckLicenseCompatibility(m, c.repo, c.dep)
			if allowed != c.wantAllowed || denied != c.wantDenied {
				t.Errorf("CheckLicenseCompatibility(%q, %q) = (%v, %v); want (%v, %v)",
					c.repo, c.dep, allowed, denied, c.wantAllowed, c.wantDenied)
			}
		})
	}
}

// TestSUPPLY004_NotInMatrix_NotCachedNegatively — even when the dep's
// license lookup returns a license that's not in the matrix, the
// positive license cache stores the license itself; on a re-run the
// stub is not called again but the same advise-mode finding is
// re-emitted (the matrix lookup is deterministic, so re-checking is
// fine — we still don't re-fetch the license).
func TestSUPPLY004_NotInMatrix_NotCachedNegatively(t *testing.T) {
	stub := newStubCodeArtifact()
	stub.on(codeartifact.EcosystemNPM, "weird-license-pkg", "1.0.0", func() (codeartifact.PackageVersionInfo, error) {
		return codeartifact.PackageVersionInfo{Name: "weird-license-pkg", Version: "1.0.0", License: "Made-Up-1.0", Status: "Published"}, nil
	})

	rule, err := NewSUPPLY004(stub)
	if err != nil {
		t.Fatalf("NewSUPPLY004: %v", err)
	}

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	if _, err := db.Exec(`INSERT INTO Repositories (name, local_path, mode, license) VALUES (?, '', 'write', ?)`, "myrepo", "MIT"); err != nil {
		t.Fatalf("insert repo: %v", err)
	}

	in := supply004MakeInput("feat/x", "abc", "package.json", "myrepo", manifests.EcosystemNPM,
		dep(manifests.EcosystemNPM, "weird-license-pkg", "1.0.0"))

	findings, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if !strings.Contains(findings[0].Message, "not in license_matrix") {
		t.Errorf("expected matrix-absence marker, got: %q", findings[0].Message)
	}

	// Second run: stub should NOT be called again (positive license
	// cache hit), and the finding should re-emit deterministically.
	findings2, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if len(findings2) != 1 {
		t.Fatalf("second run: expected 1 finding, got %d", len(findings2))
	}
	if stub.callCount() != 1 {
		t.Errorf("expected 1 stub call across 2 runs (cached), got %d", stub.callCount())
	}
}
