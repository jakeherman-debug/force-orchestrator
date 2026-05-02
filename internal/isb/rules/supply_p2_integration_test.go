// Package rules: D5 Phase 2 slice δ — SUPPLY-003 + SUPPLY-004
// integration matrix.
//
// Per docs/roadmap.md § "Deliverable 5 — Supply Chain Hygiene"
// exit criterion 5: "5 ecosystems × 2 rules = 10 named test
// functions, each end-to-end (manifest fixture → parser → rule →
// finding)."
//
// This file is the P2 mirror of supply_integration_test.go (which
// covers SUPPLY-001 + SUPPLY-002 in P1). The shape is identical:
//
//   1. Spin up a real :memory: SQLite via store.InitHolocronDSN.
//   2. Write before/after manifest fixtures into a real git repo via
//      setupGitRepo (shared helper from supply_integration_test.go).
//   3. Parse the after-state through the real per-ecosystem parser
//      via buildIntegrationInput.
//   4. Build a stub codeartifact.Client at the Client interface
//      boundary (Pattern P16): SUPPLY-003 stubs return a stale or
//      fresh PublishedAt; SUPPLY-004 stubs return a License SPDX id.
//   5. Call the rule's Run directly and assert finding shape.
//
// Notes on Go ecosystem coverage:
//   - SUPPLY-003: silently skips Go (CodeArtifact has no Go format —
//     manifestsToCodeArtifact returns ok=false). Test asserts ZERO
//     findings + ZERO stub calls.
//   - SUPPLY-004: same — also silently skips Go via the same
//     manifestsToCodeArtifact branch. SUPPLY-005 (P3, osv-scanner)
//     covers Go in a later phase.
//
// SUPPLY-004 specifically depends on Repositories.license being
// populated for input.TargetRepo. Each SUPPLY-004 test seeds the
// repo row with `license = 'MIT'` and chooses a dep license from
// the matrix that lands in the deny list (e.g. GPL-3.0) so the rule
// emits an "incompatible" advise-mode finding.
//
// Stubs are confined to the codeartifact.Client boundary
// (per CLAUDE.md "Mock gh and git only at the package boundary" —
// same shape applies to AWS clients). Manifest parsers, the
// dispatcher, the matrix loader, and the DB are all real.
package rules

import (
	"context"
	"strings"
	"testing"
	"time"

	"force-orchestrator/internal/clients/codeartifact"
	"force-orchestrator/internal/isb"
	"force-orchestrator/internal/isb/scanners/manifests"
)

// ── shared P2 helpers ───────────────────────────────────────────────────────

// staleStub returns a codeartifact.Client stub that:
//   - records every DescribePackageVersion invocation in *calls,
//   - returns PackageVersionInfo with PublishedAt=staleAt for the
//     supplied (eco, name, version) triple,
//   - returns a Published 200 with PublishedAt=time.Now() for any
//     other triple (so unrelated existing deps in the manifest don't
//     accidentally trigger a finding).
//
// Mirrors notFoundStub in supply_integration_test.go but for the
// SUPPLY-003 outcome map.
func staleStub(calls *[]stubCallRecord, eco codeartifact.Ecosystem, name, version string, staleAt time.Time) *stubCodeArtifact {
	stub := newStubCodeArtifact()
	stub.defaultHandler = func(e codeartifact.Ecosystem, n, v string) (codeartifact.PackageVersionInfo, error) {
		*calls = append(*calls, stubCallRecord{ecosystem: e, name: n, version: v})
		if e == eco && n == name && v == version {
			return codeartifact.PackageVersionInfo{
				Ecosystem:   e,
				Name:        n,
				Version:     v,
				Status:      "Published",
				PublishedAt: staleAt,
			}, nil
		}
		return codeartifact.PackageVersionInfo{
			Ecosystem:   e,
			Name:        n,
			Version:     v,
			Status:      "Published",
			PublishedAt: time.Now(), // fresh — no SUPPLY-003 finding
		}, nil
	}
	return stub
}

// licenseStub returns a codeartifact.Client stub that:
//   - records every DescribePackageVersion invocation in *calls,
//   - returns PackageVersionInfo with License=depLicense for the
//     supplied (eco, name, version) triple,
//   - returns a Published 200 with License="MIT" (compatible with
//     the test repo's MIT license) for any other triple, so unrelated
//     existing deps don't trigger findings.
//
// Mirrors notFoundStub in supply_integration_test.go but for the
// SUPPLY-004 outcome map.
func licenseStub(calls *[]stubCallRecord, eco codeartifact.Ecosystem, name, version, depLicense string) *stubCodeArtifact {
	stub := newStubCodeArtifact()
	stub.defaultHandler = func(e codeartifact.Ecosystem, n, v string) (codeartifact.PackageVersionInfo, error) {
		*calls = append(*calls, stubCallRecord{ecosystem: e, name: n, version: v})
		if e == eco && n == name && v == version {
			return codeartifact.PackageVersionInfo{
				Ecosystem: e,
				Name:      n,
				Version:   v,
				Status:    "Published",
				License:   depLicense,
			}, nil
		}
		return codeartifact.PackageVersionInfo{
			Ecosystem: e,
			Name:      n,
			Version:   v,
			Status:    "Published",
			License:   "MIT",
		}, nil
	}
	return stub
}

// assertSupply003Finding fails unless `findings` contains exactly
// one SUPPLY-003 finding for `wantName@wantVersion` at `wantPath`
// with advise severity. The message must include the published date
// (YYYY-MM-DD) and threshold day count.
func assertSupply003Finding(t *testing.T, findings []isb.Finding, wantPath, wantName, wantVersion string, publishedAt time.Time, thresholdDays int) {
	t.Helper()
	matches := 0
	for _, f := range findings {
		if f.RuleID != "SUPPLY-003" {
			continue
		}
		if f.Severity != isb.SeverityAdvise {
			t.Errorf("SUPPLY-003 finding severity=%q (want advise): %+v", f.Severity, f)
		}
		if f.Path != wantPath {
			t.Errorf("SUPPLY-003 finding path=%q (want %q): %+v", f.Path, wantPath, f)
		}
		if !strings.Contains(f.Message, wantName) {
			t.Errorf("SUPPLY-003 message missing dep name %q: %q", wantName, f.Message)
		}
		if !strings.Contains(f.Message, wantVersion) {
			t.Errorf("SUPPLY-003 message missing version %q: %q", wantVersion, f.Message)
		}
		wantDate := publishedAt.UTC().Format("2006-01-02")
		if !strings.Contains(f.Message, wantDate) {
			t.Errorf("SUPPLY-003 message missing published date %q: %q", wantDate, f.Message)
		}
		if !strings.Contains(f.Message, "staleness threshold") {
			t.Errorf("SUPPLY-003 message missing 'staleness threshold' marker: %q", f.Message)
		}
		matches++
	}
	if matches != 1 {
		t.Fatalf("expected exactly 1 SUPPLY-003 finding for %s@%s, got %d. all findings=%+v",
			wantName, wantVersion, matches, findings)
	}
}

// assertSupply004IncompatibleFinding fails unless `findings`
// contains exactly one SUPPLY-004 finding at `wantPath` whose
// message names both repo and dep licenses and tags the pair as
// incompatible.
func assertSupply004IncompatibleFinding(t *testing.T, findings []isb.Finding, wantPath, wantName, wantVersion, repoLicense, depLicense string) {
	t.Helper()
	matches := 0
	for _, f := range findings {
		if f.RuleID != "SUPPLY-004" {
			continue
		}
		if f.Severity != isb.SeverityAdvise {
			t.Errorf("SUPPLY-004 finding severity=%q (want advise): %+v", f.Severity, f)
		}
		if f.Path != wantPath {
			t.Errorf("SUPPLY-004 finding path=%q (want %q): %+v", f.Path, wantPath, f)
		}
		if !strings.Contains(f.Message, wantName) {
			t.Errorf("SUPPLY-004 message missing dep name %q: %q", wantName, f.Message)
		}
		if !strings.Contains(f.Message, wantVersion) {
			t.Errorf("SUPPLY-004 message missing version %q: %q", wantVersion, f.Message)
		}
		if !strings.Contains(f.Message, repoLicense) {
			t.Errorf("SUPPLY-004 message missing repo license %q: %q", repoLicense, f.Message)
		}
		if !strings.Contains(f.Message, depLicense) {
			t.Errorf("SUPPLY-004 message missing dep license %q: %q", depLicense, f.Message)
		}
		if !strings.Contains(f.Message, "incompatible") {
			t.Errorf("SUPPLY-004 message missing 'incompatible' marker: %q", f.Message)
		}
		matches++
	}
	if matches != 1 {
		t.Fatalf("expected exactly 1 SUPPLY-004 finding for %s@%s, got %d. all findings=%+v",
			wantName, wantVersion, matches, findings)
	}
}

// ── SUPPLY-003 × 5 ecosystems ───────────────────────────────────────────────

// TestSupply003_StaleRubyGem_Rejected — Gemfile delta introduces a
// gem whose CodeArtifact PublishedAt is older than the default
// 730-day threshold; rule emits an advise-mode finding.
func TestSupply003_StaleRubyGem_Rejected(t *testing.T) {
	const depName = "ancientgem"
	const depVersion = "0.1.0"
	publishedAt := time.Now().Add(-1000 * 24 * time.Hour) // ~2.7y
	fx := manifestFixture{
		ecosystem: manifests.EcosystemRubyGems,
		relPath:   "Gemfile",
		before: `source 'https://rubygems.org'

gem 'rails', '7.0.4'
`,
		after: `source 'https://rubygems.org'

gem 'rails', '7.0.4'
gem '` + depName + `', '` + depVersion + `'
`,
	}
	_ = setupGitRepo(t, fx)
	db := freshDB(t)

	var calls []stubCallRecord
	stub := staleStub(&calls, codeartifact.EcosystemRubyGems, depName, depVersion, publishedAt)
	rule := NewSUPPLY003(stub)

	in := buildIntegrationInput(t, fx, "feature/x", "stale01")
	findings, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertSupply003Finding(t, findings, "Gemfile", depName, depVersion, publishedAt, supplyStaleDefaultDays)
	assertStubCalledWith(t, calls, codeartifact.EcosystemRubyGems, depName, depVersion)
}

// TestSupply003_StaleNpmPackage_Rejected — package.json adds an npm
// package with a stale PublishedAt; rule emits a finding.
func TestSupply003_StaleNpmPackage_Rejected(t *testing.T) {
	const depName = "stale-npm-pkg"
	const depVersion = "0.0.1"
	publishedAt := time.Now().Add(-900 * 24 * time.Hour)
	fx := manifestFixture{
		ecosystem: manifests.EcosystemNPM,
		relPath:   "package.json",
		before: `{
  "name": "myapp",
  "version": "1.0.0",
  "dependencies": {
    "react": "18.2.0"
  }
}
`,
		after: `{
  "name": "myapp",
  "version": "1.0.0",
  "dependencies": {
    "react": "18.2.0",
    "` + depName + `": "` + depVersion + `"
  }
}
`,
	}
	_ = setupGitRepo(t, fx)
	db := freshDB(t)

	var calls []stubCallRecord
	stub := staleStub(&calls, codeartifact.EcosystemNPM, depName, depVersion, publishedAt)
	rule := NewSUPPLY003(stub)

	in := buildIntegrationInput(t, fx, "feature/x", "stale02")
	findings, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertSupply003Finding(t, findings, "package.json", depName, depVersion, publishedAt, supplyStaleDefaultDays)
	assertStubCalledWith(t, calls, codeartifact.EcosystemNPM, depName, depVersion)
}

// TestSupply003_StalePyPI_Rejected — requirements.txt adds a pypi
// package with a stale PublishedAt.
func TestSupply003_StalePyPI_Rejected(t *testing.T) {
	const depName = "abandoned-pypi"
	const depVersion = "0.0.1"
	publishedAt := time.Now().Add(-1500 * 24 * time.Hour) // ~4y
	fx := manifestFixture{
		ecosystem: manifests.EcosystemPyPI,
		relPath:   "requirements.txt",
		before: `requests==2.31.0
flask==2.3.0
`,
		after: `requests==2.31.0
flask==2.3.0
` + depName + `==` + depVersion + `
`,
	}
	_ = setupGitRepo(t, fx)
	db := freshDB(t)

	var calls []stubCallRecord
	stub := staleStub(&calls, codeartifact.EcosystemPyPI, depName, depVersion, publishedAt)
	rule := NewSUPPLY003(stub)

	in := buildIntegrationInput(t, fx, "feature/x", "stale03")
	findings, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertSupply003Finding(t, findings, "requirements.txt", depName, depVersion, publishedAt, supplyStaleDefaultDays)
	assertStubCalledWith(t, calls, codeartifact.EcosystemPyPI, depName, depVersion)
}

// TestSupply003_StaleMaven_Rejected — pom.xml introduces a stale
// groupId:artifactId. Maven dep names are "groupId:artifactId" per
// the parser's pomDep handling.
func TestSupply003_StaleMaven_Rejected(t *testing.T) {
	const depName = "com.fake.legacy:dustyjar"
	const depVersion = "1.0.0"
	publishedAt := time.Now().Add(-2000 * 24 * time.Hour) // ~5.5y
	parts := strings.SplitN(depName, ":", 2)
	depGroup, depArtifact := parts[0], parts[1]
	fx := manifestFixture{
		ecosystem: manifests.EcosystemMaven,
		relPath:   "pom.xml",
		before: `<?xml version="1.0" encoding="UTF-8"?>
<project xmlns="http://maven.apache.org/POM/4.0.0">
    <modelVersion>4.0.0</modelVersion>
    <groupId>com.example</groupId>
    <artifactId>myapp</artifactId>
    <version>1.0.0</version>
    <dependencies>
        <dependency>
            <groupId>org.springframework.boot</groupId>
            <artifactId>spring-boot-starter-web</artifactId>
            <version>3.2.0</version>
        </dependency>
    </dependencies>
</project>
`,
		after: `<?xml version="1.0" encoding="UTF-8"?>
<project xmlns="http://maven.apache.org/POM/4.0.0">
    <modelVersion>4.0.0</modelVersion>
    <groupId>com.example</groupId>
    <artifactId>myapp</artifactId>
    <version>1.0.0</version>
    <dependencies>
        <dependency>
            <groupId>org.springframework.boot</groupId>
            <artifactId>spring-boot-starter-web</artifactId>
            <version>3.2.0</version>
        </dependency>
        <dependency>
            <groupId>` + depGroup + `</groupId>
            <artifactId>` + depArtifact + `</artifactId>
            <version>` + depVersion + `</version>
        </dependency>
    </dependencies>
</project>
`,
	}
	_ = setupGitRepo(t, fx)
	db := freshDB(t)

	var calls []stubCallRecord
	stub := staleStub(&calls, codeartifact.EcosystemMaven, depName, depVersion, publishedAt)
	rule := NewSUPPLY003(stub)

	in := buildIntegrationInput(t, fx, "feature/x", "stale04")
	findings, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertSupply003Finding(t, findings, "pom.xml", depName, depVersion, publishedAt, supplyStaleDefaultDays)
	assertStubCalledWith(t, calls, codeartifact.EcosystemMaven, depName, depVersion)
}

// TestSupply003_StaleGoModule_Rejected — go.mod adds a module that
// would be stale, but Go is silently skipped per the rule's design
// (CodeArtifact has no Go format, so SUPPLY-003 short-circuits via
// the manifestsToCodeArtifact ok=false branch). Asserts ZERO findings
// AND zero stub calls. SUPPLY-005 (P3) covers Go staleness via
// osv-scanner.
func TestSupply003_StaleGoModule_Rejected(t *testing.T) {
	const depName = "github.com/fake-org/abandoned-module"
	const depVersion = "v0.1.0"
	fx := manifestFixture{
		ecosystem: manifests.EcosystemGo,
		relPath:   "go.mod",
		before: `module example.com/myservice

go 1.22

require (
	github.com/stretchr/testify v1.9.0
)
`,
		after: `module example.com/myservice

go 1.22

require (
	github.com/stretchr/testify v1.9.0
	` + depName + ` ` + depVersion + `
)
`,
	}
	_ = setupGitRepo(t, fx)
	db := freshDB(t)

	var calls []stubCallRecord
	// Wire a defaultHandler that records calls but returns a stale
	// PublishedAt — if Go were NOT silently skipped this would emit a
	// finding, so the assertion below catches a regression.
	stub := newStubCodeArtifact()
	stub.defaultHandler = func(e codeartifact.Ecosystem, n, v string) (codeartifact.PackageVersionInfo, error) {
		calls = append(calls, stubCallRecord{ecosystem: e, name: n, version: v})
		return codeartifact.PackageVersionInfo{
			Ecosystem:   e,
			Name:        n,
			Version:     v,
			Status:      "Published",
			PublishedAt: time.Now().Add(-1500 * 24 * time.Hour),
		}, nil
	}
	rule := NewSUPPLY003(stub)

	in := buildIntegrationInput(t, fx, "feature/x", "stale05")
	findings, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("Go is silently skipped by SUPPLY-003 (CodeArtifact has no Go format) — expected 0 findings, got %d: %+v", len(findings), findings)
	}
	if len(calls) != 0 {
		t.Errorf("expected 0 stub calls for Go ecosystem, got %d: %+v", len(calls), calls)
	}
}

// ── SUPPLY-004 × 5 ecosystems ───────────────────────────────────────────────

// TestSupply004_IncompatibleLicenseRubyGem_Rejected — MIT repo
// pulls in a GPL-3.0 RubyGem; matrix denies the pair → finding.
func TestSupply004_IncompatibleLicenseRubyGem_Rejected(t *testing.T) {
	const depName = "viral-gem"
	const depVersion = "1.0.0"
	const repoLicense = "MIT"
	const depLicense = "GPL-3.0"
	const targetRepo = "myorg/myrubyrepo"
	fx := manifestFixture{
		ecosystem: manifests.EcosystemRubyGems,
		relPath:   "Gemfile",
		before: `source 'https://rubygems.org'

gem 'rails', '7.0.4'
`,
		after: `source 'https://rubygems.org'

gem 'rails', '7.0.4'
gem '` + depName + `', '` + depVersion + `'
`,
	}
	_ = setupGitRepo(t, fx)
	db := freshDB(t)
	if _, err := db.Exec(`INSERT INTO Repositories (name, local_path, mode, license) VALUES (?, '', 'write', ?)`, targetRepo, repoLicense); err != nil {
		t.Fatalf("seed repo: %v", err)
	}

	var calls []stubCallRecord
	stub := licenseStub(&calls, codeartifact.EcosystemRubyGems, depName, depVersion, depLicense)
	rule, err := NewSUPPLY004(stub)
	if err != nil {
		t.Fatalf("NewSUPPLY004: %v", err)
	}

	in := buildIntegrationInput(t, fx, "feature/x", "lic01")
	in.TargetRepo = targetRepo
	findings, runErr := rule.Run(context.Background(), db, in)
	if runErr != nil {
		t.Fatalf("Run: %v", runErr)
	}
	assertSupply004IncompatibleFinding(t, findings, "Gemfile", depName, depVersion, repoLicense, depLicense)
	assertStubCalledWith(t, calls, codeartifact.EcosystemRubyGems, depName, depVersion)
}

// TestSupply004_IncompatibleLicenseNpmPackage_Rejected — MIT repo
// + AGPL-3.0 npm dep → finding.
func TestSupply004_IncompatibleLicenseNpmPackage_Rejected(t *testing.T) {
	const depName = "viral-npm-pkg"
	const depVersion = "2.0.0"
	const repoLicense = "MIT"
	const depLicense = "AGPL-3.0"
	const targetRepo = "myorg/mynoderepo"
	fx := manifestFixture{
		ecosystem: manifests.EcosystemNPM,
		relPath:   "package.json",
		before: `{
  "name": "myapp",
  "version": "1.0.0",
  "dependencies": {
    "react": "18.2.0"
  }
}
`,
		after: `{
  "name": "myapp",
  "version": "1.0.0",
  "dependencies": {
    "react": "18.2.0",
    "` + depName + `": "` + depVersion + `"
  }
}
`,
	}
	_ = setupGitRepo(t, fx)
	db := freshDB(t)
	if _, err := db.Exec(`INSERT INTO Repositories (name, local_path, mode, license) VALUES (?, '', 'write', ?)`, targetRepo, repoLicense); err != nil {
		t.Fatalf("seed repo: %v", err)
	}

	var calls []stubCallRecord
	stub := licenseStub(&calls, codeartifact.EcosystemNPM, depName, depVersion, depLicense)
	rule, err := NewSUPPLY004(stub)
	if err != nil {
		t.Fatalf("NewSUPPLY004: %v", err)
	}

	in := buildIntegrationInput(t, fx, "feature/x", "lic02")
	in.TargetRepo = targetRepo
	findings, runErr := rule.Run(context.Background(), db, in)
	if runErr != nil {
		t.Fatalf("Run: %v", runErr)
	}
	assertSupply004IncompatibleFinding(t, findings, "package.json", depName, depVersion, repoLicense, depLicense)
	assertStubCalledWith(t, calls, codeartifact.EcosystemNPM, depName, depVersion)
}

// TestSupply004_IncompatibleLicensePyPI_Rejected — MIT repo + GPL-2.0
// pypi dep → finding.
func TestSupply004_IncompatibleLicensePyPI_Rejected(t *testing.T) {
	const depName = "viral-pypi-pkg"
	const depVersion = "1.2.3"
	const repoLicense = "MIT"
	const depLicense = "GPL-2.0"
	const targetRepo = "myorg/mypythonrepo"
	fx := manifestFixture{
		ecosystem: manifests.EcosystemPyPI,
		relPath:   "requirements.txt",
		before: `flask==2.3.0
`,
		after: `flask==2.3.0
` + depName + `==` + depVersion + `
`,
	}
	_ = setupGitRepo(t, fx)
	db := freshDB(t)
	if _, err := db.Exec(`INSERT INTO Repositories (name, local_path, mode, license) VALUES (?, '', 'write', ?)`, targetRepo, repoLicense); err != nil {
		t.Fatalf("seed repo: %v", err)
	}

	var calls []stubCallRecord
	stub := licenseStub(&calls, codeartifact.EcosystemPyPI, depName, depVersion, depLicense)
	rule, err := NewSUPPLY004(stub)
	if err != nil {
		t.Fatalf("NewSUPPLY004: %v", err)
	}

	in := buildIntegrationInput(t, fx, "feature/x", "lic03")
	in.TargetRepo = targetRepo
	findings, runErr := rule.Run(context.Background(), db, in)
	if runErr != nil {
		t.Fatalf("Run: %v", runErr)
	}
	assertSupply004IncompatibleFinding(t, findings, "requirements.txt", depName, depVersion, repoLicense, depLicense)
	assertStubCalledWith(t, calls, codeartifact.EcosystemPyPI, depName, depVersion)
}

// TestSupply004_IncompatibleLicenseMaven_Rejected — MIT repo +
// LGPL-3.0 maven dep → matrix denies → finding.
func TestSupply004_IncompatibleLicenseMaven_Rejected(t *testing.T) {
	const depName = "com.viral.copyleft:gpljar"
	const depVersion = "9.9.9"
	const repoLicense = "MIT"
	const depLicense = "LGPL-3.0"
	const targetRepo = "myorg/myjavarepo"
	parts := strings.SplitN(depName, ":", 2)
	depGroup, depArtifact := parts[0], parts[1]
	fx := manifestFixture{
		ecosystem: manifests.EcosystemMaven,
		relPath:   "pom.xml",
		before: `<?xml version="1.0" encoding="UTF-8"?>
<project xmlns="http://maven.apache.org/POM/4.0.0">
    <modelVersion>4.0.0</modelVersion>
    <groupId>com.example</groupId>
    <artifactId>myapp</artifactId>
    <version>1.0.0</version>
    <dependencies>
        <dependency>
            <groupId>org.springframework.boot</groupId>
            <artifactId>spring-boot-starter-web</artifactId>
            <version>3.2.0</version>
        </dependency>
    </dependencies>
</project>
`,
		after: `<?xml version="1.0" encoding="UTF-8"?>
<project xmlns="http://maven.apache.org/POM/4.0.0">
    <modelVersion>4.0.0</modelVersion>
    <groupId>com.example</groupId>
    <artifactId>myapp</artifactId>
    <version>1.0.0</version>
    <dependencies>
        <dependency>
            <groupId>org.springframework.boot</groupId>
            <artifactId>spring-boot-starter-web</artifactId>
            <version>3.2.0</version>
        </dependency>
        <dependency>
            <groupId>` + depGroup + `</groupId>
            <artifactId>` + depArtifact + `</artifactId>
            <version>` + depVersion + `</version>
        </dependency>
    </dependencies>
</project>
`,
	}
	_ = setupGitRepo(t, fx)
	db := freshDB(t)
	if _, err := db.Exec(`INSERT INTO Repositories (name, local_path, mode, license) VALUES (?, '', 'write', ?)`, targetRepo, repoLicense); err != nil {
		t.Fatalf("seed repo: %v", err)
	}

	var calls []stubCallRecord
	stub := licenseStub(&calls, codeartifact.EcosystemMaven, depName, depVersion, depLicense)
	rule, err := NewSUPPLY004(stub)
	if err != nil {
		t.Fatalf("NewSUPPLY004: %v", err)
	}

	in := buildIntegrationInput(t, fx, "feature/x", "lic04")
	in.TargetRepo = targetRepo
	findings, runErr := rule.Run(context.Background(), db, in)
	if runErr != nil {
		t.Fatalf("Run: %v", runErr)
	}
	assertSupply004IncompatibleFinding(t, findings, "pom.xml", depName, depVersion, repoLicense, depLicense)
	assertStubCalledWith(t, calls, codeartifact.EcosystemMaven, depName, depVersion)
}

// TestSupply004_IncompatibleLicenseGoModule_Rejected — Go is
// silently skipped per the SUPPLY-004 design (CodeArtifact has no
// Go format → manifestsToCodeArtifact ok=false branch). Asserts
// ZERO findings AND zero stub calls. SUPPLY-005 (P3) eventually
// covers Go via osv-scanner.
func TestSupply004_IncompatibleLicenseGoModule_Rejected(t *testing.T) {
	const depName = "github.com/fake-org/copyleft-module"
	const depVersion = "v1.0.0"
	const targetRepo = "myorg/mygorepo"
	fx := manifestFixture{
		ecosystem: manifests.EcosystemGo,
		relPath:   "go.mod",
		before: `module example.com/myservice

go 1.22

require (
	github.com/stretchr/testify v1.9.0
)
`,
		after: `module example.com/myservice

go 1.22

require (
	github.com/stretchr/testify v1.9.0
	` + depName + ` ` + depVersion + `
)
`,
	}
	_ = setupGitRepo(t, fx)
	db := freshDB(t)
	if _, err := db.Exec(`INSERT INTO Repositories (name, local_path, mode, license) VALUES (?, '', 'write', ?)`, targetRepo, "MIT"); err != nil {
		t.Fatalf("seed repo: %v", err)
	}

	var calls []stubCallRecord
	// defaultHandler returns GPL-3.0 — would force a finding if Go
	// were NOT silently skipped, so the assertion catches a regression.
	stub := newStubCodeArtifact()
	stub.defaultHandler = func(e codeartifact.Ecosystem, n, v string) (codeartifact.PackageVersionInfo, error) {
		calls = append(calls, stubCallRecord{ecosystem: e, name: n, version: v})
		return codeartifact.PackageVersionInfo{
			Ecosystem: e,
			Name:      n,
			Version:   v,
			Status:    "Published",
			License:   "GPL-3.0",
		}, nil
	}
	rule, err := NewSUPPLY004(stub)
	if err != nil {
		t.Fatalf("NewSUPPLY004: %v", err)
	}

	in := buildIntegrationInput(t, fx, "feature/x", "lic05")
	in.TargetRepo = targetRepo
	findings, runErr := rule.Run(context.Background(), db, in)
	if runErr != nil {
		t.Fatalf("Run: %v", runErr)
	}
	if len(findings) != 0 {
		t.Fatalf("Go is silently skipped by SUPPLY-004 (CodeArtifact has no Go format) — expected 0 findings, got %d: %+v", len(findings), findings)
	}
	if len(calls) != 0 {
		t.Errorf("expected 0 stub calls for Go ecosystem, got %d: %+v", len(calls), calls)
	}
}

