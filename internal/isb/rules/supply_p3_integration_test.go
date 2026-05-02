// Package rules: D5 Phase 3 slice δ — SUPPLY-005 integration matrix.
//
// Per docs/roadmap.md § "Deliverable 5 — Supply Chain Hygiene"
// exit criterion 5: "5 ecosystems × 1 rule = 5 named test functions,
// each end-to-end (lockfile fixture → parser → rule → finding)."
//
// This file is the P3 mirror of supply_integration_test.go (P1
// SUPPLY-001/002) and supply_p2_integration_test.go (P2
// SUPPLY-003/004). The shape is identical:
//
//   1. Spin up a real :memory: SQLite via store.InitHolocronDSN
//      (per CLAUDE.md "never mock the database").
//   2. Write before/after lockfile fixtures into a real git repo via
//      setupGitRepo (shared helper from supply_integration_test.go).
//   3. Parse the after-state through the real per-ecosystem parser
//      via buildIntegrationInput (so the dispatcher boundary is
//      exercised end-to-end).
//   4. Build a stub osv.Client at the Client interface boundary
//      (Pattern P16): the stub fires a synthetic CVE Finding for the
//      targeted (ecosystem, name, version) triple, and empty for any
//      other path.
//   5. Call the rule's Run directly and assert finding shape.
//
// Notes on Go ecosystem coverage:
//   - SUPPLY-005 supports Go natively (osv-scanner has go.mod / go.sum
//     extractors). Unlike SUPPLY-001..004 (which silently skip Go
//     because CodeArtifact has no Go format), the Go test here asserts
//     a NON-zero finding from the stub.
//
// Severity at launch: per the SUPPLY-* anti-cheat directive ("no
// block-default for new rules"), every SUPPLY rule ships at advise.
// HIGH/CRITICAL findings are surfaced in the message but the rule's
// Severity field is isb.SeverityAdvise. Block-vs-advise becomes a
// FleetRules promotion later (paired-run mechanism).
//
// Stubs are confined to the osv.Client boundary (per CLAUDE.md "Mock
// gh and git only at the package boundary" — the same shape applies
// here). Manifest parsers, the dispatcher, and the DB are all real.
package rules

import (
	"context"
	"strings"
	"testing"

	"force-orchestrator/internal/isb"
	"force-orchestrator/internal/isb/scanners/manifests"
	"force-orchestrator/internal/isb/scanners/osv"
)

// ── shared P3 helpers ───────────────────────────────────────────────────────

// vulnStub returns an osv.Client stub that:
//   - fires a synthetic Finding (CVE-style) for the targeted lockfile
//     path only, populated with the supplied (ecosystem, name,
//     version, severity, osvID) tuple,
//   - returns empty findings + nil error for any other path so
//     unrelated lockfiles in a multi-manifest input don't accidentally
//     trigger findings.
//
// Mirrors notFoundStub / staleStub / licenseStub from earlier slices,
// but for the SUPPLY-005 outcome map (one Finding per matched path).
func vulnStub(targetPath, targetEcosystem, targetName, targetVersion, severity, osvID string) *stubOSV {
	stub := newStubOSV()
	stub.on(targetPath, func() ([]osv.Finding, error) {
		return []osv.Finding{{
			OSVID:          osvID,
			Severity:       severity,
			PackageName:    targetName,
			PackageVersion: targetVersion,
			Ecosystem:      targetEcosystem,
			Summary:        "test fake CVE for integration matrix",
			URL:            "https://osv.dev/vulnerability/" + osvID,
		}}, nil
	})
	return stub
}

// assertSupply005Finding fails the test unless `findings` contains
// exactly one SUPPLY-005 finding at `wantPath` whose message includes
// the OSVID, OSV ecosystem, dep name, version, severity bucket, and
// OSV URL substring. Severity (Finding.Severity) must be advise per
// launch policy (HIGH/CRITICAL still ship at advise — see file
// header).
func assertSupply005Finding(t *testing.T, findings []isb.Finding, wantPath, wantEcosystem, wantName, wantVersion, wantOSVID, wantSeverity string) {
	t.Helper()
	matches := 0
	for _, f := range findings {
		if f.RuleID != "SUPPLY-005" {
			continue
		}
		if f.Severity != isb.SeverityAdvise {
			t.Errorf("SUPPLY-005 finding severity=%q (want advise — launch policy): %+v", f.Severity, f)
		}
		if f.Path != wantPath {
			t.Errorf("SUPPLY-005 finding path=%q (want %q): %+v", f.Path, wantPath, f)
		}
		// Message must include OSVID, ecosystem, dep name, version,
		// severity bucket, OSV URL substring.
		for _, want := range []string{wantOSVID, wantEcosystem, wantName, wantVersion, wantSeverity, "https://osv.dev/vulnerability/" + wantOSVID} {
			if !strings.Contains(f.Message, want) {
				t.Errorf("SUPPLY-005 message missing %q: %q", want, f.Message)
			}
		}
		matches++
	}
	if matches != 1 {
		t.Fatalf("expected exactly 1 SUPPLY-005 finding for %s@%s, got %d. all findings=%+v",
			wantName, wantVersion, matches, findings)
	}
}

// ── SUPPLY-005 × 5 ecosystems ───────────────────────────────────────────────

// TestSupply005_KnownCVERubyGem_Rejected — Gemfile.lock delta adds a
// known-CVE RubyGem; osv stub returns a HIGH finding for the targeted
// (Gemfile.lock, RubyGems, name, version) triple; rule emits an
// advise-mode finding (per launch policy).
func TestSupply005_KnownCVERubyGem_Rejected(t *testing.T) {
	const depName = "rails"
	const depVersion = "5.0.0"
	const osvID = "CVE-2023-FAKE-001"
	const severity = "HIGH"
	// Realistic Gemfile.lock shape: GEM section with `name (version)`
	// indent. Parser's lockNameVersionRegex matches this exact form.
	fx := manifestFixture{
		ecosystem: manifests.EcosystemRubyGems,
		relPath:   "Gemfile.lock",
		before: `GEM
  remote: https://rubygems.org/
  specs:
    actionpack (7.0.4)

PLATFORMS
  ruby

DEPENDENCIES
  actionpack

BUNDLED WITH
   2.3.0
`,
		after: `GEM
  remote: https://rubygems.org/
  specs:
    actionpack (7.0.4)
    ` + depName + ` (` + depVersion + `)

PLATFORMS
  ruby

DEPENDENCIES
  actionpack
  ` + depName + `

BUNDLED WITH
   2.3.0
`,
	}
	_ = setupGitRepo(t, fx)
	db := freshDB(t)

	stub := vulnStub("Gemfile.lock", "RubyGems", depName, depVersion, severity, osvID)
	rule := NewSUPPLY005(stub)

	in := buildIntegrationInput(t, fx, "feature/x", "cve01")
	findings, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertSupply005Finding(t, findings, "Gemfile.lock", "RubyGems", depName, depVersion, osvID, severity)
	if stub.callCount() < 1 {
		t.Errorf("expected stub to be called at least once, got %d", stub.callCount())
	}
}

// TestSupply005_KnownCVENpmPackage_Rejected — package-lock.json delta
// adds a known-CVE npm package; stub returns a CRITICAL finding;
// advise-mode finding emitted.
func TestSupply005_KnownCVENpmPackage_Rejected(t *testing.T) {
	const depName = "lodash"
	const depVersion = "4.17.0"
	const osvID = "CVE-2023-FAKE-002"
	const severity = "CRITICAL"
	// package-lock.json v3 shape (flat "packages" map). Parser walks
	// stripNodeModules(path) to recover the dep name.
	fx := manifestFixture{
		ecosystem: manifests.EcosystemNPM,
		relPath:   "package-lock.json",
		before: `{
  "name": "myapp",
  "version": "1.0.0",
  "lockfileVersion": 3,
  "requires": true,
  "packages": {
    "": {
      "name": "myapp",
      "version": "1.0.0"
    },
    "node_modules/react": {
      "version": "18.2.0"
    }
  }
}
`,
		after: `{
  "name": "myapp",
  "version": "1.0.0",
  "lockfileVersion": 3,
  "requires": true,
  "packages": {
    "": {
      "name": "myapp",
      "version": "1.0.0"
    },
    "node_modules/react": {
      "version": "18.2.0"
    },
    "node_modules/` + depName + `": {
      "version": "` + depVersion + `"
    }
  }
}
`,
	}
	_ = setupGitRepo(t, fx)
	db := freshDB(t)

	stub := vulnStub("package-lock.json", "npm", depName, depVersion, severity, osvID)
	rule := NewSUPPLY005(stub)

	in := buildIntegrationInput(t, fx, "feature/x", "cve02")
	findings, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertSupply005Finding(t, findings, "package-lock.json", "npm", depName, depVersion, osvID, severity)
	if stub.callCount() < 1 {
		t.Errorf("expected stub to be called at least once, got %d", stub.callCount())
	}
}

// TestSupply005_KnownCVEPyPI_Rejected — requirements.txt adds a
// known-CVE PyPI package; stub returns a HIGH finding; advise-mode
// finding emitted.
//
// Note: requirements.txt is not strictly a lock file, but osv-scanner
// does parse it (and the production rule feeds whatever the dispatcher
// hands it). The stub keys on path so semantic distinction doesn't
// matter for this matrix slice.
func TestSupply005_KnownCVEPyPI_Rejected(t *testing.T) {
	const depName = "django"
	const depVersion = "2.2.0"
	const osvID = "CVE-2023-FAKE-003"
	const severity = "HIGH"
	fx := manifestFixture{
		ecosystem: manifests.EcosystemPyPI,
		relPath:   "requirements.txt",
		before: `flask==2.3.0
requests==2.31.0
`,
		after: `flask==2.3.0
requests==2.31.0
` + depName + `==` + depVersion + `
`,
	}
	_ = setupGitRepo(t, fx)
	db := freshDB(t)

	stub := vulnStub("requirements.txt", "PyPI", depName, depVersion, severity, osvID)
	rule := NewSUPPLY005(stub)

	in := buildIntegrationInput(t, fx, "feature/x", "cve03")
	findings, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertSupply005Finding(t, findings, "requirements.txt", "PyPI", depName, depVersion, osvID, severity)
	if stub.callCount() < 1 {
		t.Errorf("expected stub to be called at least once, got %d", stub.callCount())
	}
}

// TestSupply005_KnownCVEMaven_Rejected — pom.xml introduces a
// known-CVE groupId:artifactId; stub returns a CRITICAL finding;
// advise-mode finding emitted.
//
// Maven dep names are "groupId:artifactId" per the parser's pomDep
// handling; we use a real-ish coordinate (log4j-core 2.14.0 is the
// canonical Log4Shell-era example, kept here as a known-shape vuln
// fixture).
func TestSupply005_KnownCVEMaven_Rejected(t *testing.T) {
	const depName = "org.apache.logging.log4j:log4j-core"
	const depVersion = "2.14.0"
	const osvID = "CVE-2023-FAKE-004"
	const severity = "CRITICAL"
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

	stub := vulnStub("pom.xml", "Maven", depName, depVersion, severity, osvID)
	rule := NewSUPPLY005(stub)

	in := buildIntegrationInput(t, fx, "feature/x", "cve04")
	findings, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertSupply005Finding(t, findings, "pom.xml", "Maven", depName, depVersion, osvID, severity)
	if stub.callCount() < 1 {
		t.Errorf("expected stub to be called at least once, got %d", stub.callCount())
	}
}

// TestSupply005_KnownCVEGoModule_Rejected — go.sum adds a known-CVE
// Go module; stub returns a HIGH finding; advise-mode finding emitted.
//
// Crucial divergence from P1+P2 Go tests: SUPPLY-005 supports Go
// (osv-scanner has go.mod / go.sum extractors), so unlike SUPPLY-001,
// 002, 003, 004 (which silently skip Go via the
// manifestsToCodeArtifact ok=false branch), SUPPLY-005 SHOULD fire
// findings for Go deps. This test asserts NON-zero findings.
func TestSupply005_KnownCVEGoModule_Rejected(t *testing.T) {
	const depName = "github.com/dgrijalva/jwt-go"
	const depVersion = "v3.2.0+incompatible"
	const osvID = "CVE-2023-FAKE-005"
	const severity = "HIGH"
	// go.sum line shape: "<module> <version>[/go.mod] h1:<hash>". The
	// parser dedups on (name, version) and surfaces each once.
	fx := manifestFixture{
		ecosystem: manifests.EcosystemGo,
		relPath:   "go.sum",
		before: `github.com/stretchr/testify v1.9.0 h1:abc123==
github.com/stretchr/testify v1.9.0/go.mod h1:def456==
`,
		after: `github.com/stretchr/testify v1.9.0 h1:abc123==
github.com/stretchr/testify v1.9.0/go.mod h1:def456==
` + depName + ` ` + depVersion + ` h1:ghi789==
` + depName + ` ` + depVersion + `/go.mod h1:jkl012==
`,
	}
	_ = setupGitRepo(t, fx)
	db := freshDB(t)

	stub := vulnStub("go.sum", "Go", depName, depVersion, severity, osvID)
	rule := NewSUPPLY005(stub)

	in := buildIntegrationInput(t, fx, "feature/x", "cve05")
	findings, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertSupply005Finding(t, findings, "go.sum", "Go", depName, depVersion, osvID, severity)
	if stub.callCount() < 1 {
		t.Errorf("expected stub to be called at least once for Go (osv-scanner supports Go natively), got %d", stub.callCount())
	}
}
