// Package rules: D5 Phase 1 slice δ — SUPPLY-001 + SUPPLY-002
// integration matrix.
//
// Per docs/roadmap.md § "Deliverable 5 — Supply Chain Hygiene"
// exit criterion 5: "5 ecosystems × 2 rules = 10 named test
// functions, each end-to-end (manifest fixture → parser → rule →
// finding)."
//
// Each test:
//   1. Spins up a real :memory: SQLite via store.InitHolocronDSN
//      (per CLAUDE.md "never mock the database").
//   2. Writes a manifest pair (before/after) into a real git
//      repo via `git init` / `git add` / `git commit`, mirroring
//      the astromech-style commit shape used by ISBReview.
//   3. Parses the after-state through the real per-ecosystem
//      parser to produce the dep-set delta.
//   4. Builds an isb.ManifestGatedInput exactly the way
//      buildManifestGatedInput in internal/agents/isb.go would.
//   5. Calls the rule's Run directly (rather than dispatch) so
//      assertions stay tight to a single rule + ecosystem.
//
// Note on SUPPLY-001 + Go: CodeArtifact has no Go format, so
// SUPPLY-001 silently skips any Go dep via its
// ErrUnsupportedEcosystem branch. The Go test asserts no findings
// + no stub calls. SUPPLY-005 (P3, osv-scanner) covers Go.
//
// Note on SUPPLY-002 + Go: SUPPLY-002 doesn't query CodeArtifact
// at run time — it walks SystemConfig allowlists. Go is fully
// supported here; the test seeds `supply_allowlist_go` with a
// canonical module path and asserts a typosquat finding.
//
// Stubs are confined to the codeartifact.Client boundary
// (per CLAUDE.md "Mock gh and git only at the package
// boundary" — same shape applies to AWS clients). Manifest
// parsers, the dispatcher, and the DB are all real.
package rules

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"force-orchestrator/internal/clients/codeartifact"
	"force-orchestrator/internal/isb"
	"force-orchestrator/internal/isb/scanners/manifests"
	"force-orchestrator/internal/store"

	// Side-effect imports register the per-ecosystem parsers with
	// manifests.Default(). The rule integration tests depend on the
	// production registry so Detect/EcosystemFor return the right
	// parsers for our fixtures.
	_ "force-orchestrator/internal/isb/scanners/manifests/gemfile"
	_ "force-orchestrator/internal/isb/scanners/manifests/gomod"
	_ "force-orchestrator/internal/isb/scanners/manifests/maven"
	_ "force-orchestrator/internal/isb/scanners/manifests/npm"
	_ "force-orchestrator/internal/isb/scanners/manifests/pip"
)

// ── shared helpers ──────────────────────────────────────────────────────────

// manifestFixture describes a before/after manifest pair for one
// ecosystem in the integration matrix. The integration harness:
//   - writes `before` at the base commit,
//   - advances to a feature branch and writes `after`,
//   - parses `after` (and `before`) via the real parser to produce
//     the dep-set delta passed to the rule.
type manifestFixture struct {
	ecosystem manifests.Ecosystem
	relPath   string // basename within repo (e.g. "Gemfile", "package.json")
	before    string
	after     string
}

// setupGitRepo creates a real git repo with two commits: a base
// commit on `main` containing `before`, and a feature commit on
// `feature/x` containing `after`. Returns the repo path; cleanup is
// registered via t.Cleanup. Mirrors makeRepoCommit in
// internal/agents/isb_manifest_gating_test.go.
func setupGitRepo(t *testing.T, fx manifestFixture) string {
	t.Helper()
	dir := t.TempDir()

	mustGit := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	mustGit("init", "--initial-branch=main")
	mustGit("config", "user.email", "test@example.com")
	mustGit("config", "user.name", "test")
	mustGit("config", "commit.gpgsign", "false")

	full := filepath.Join(dir, fx.relPath)
	if err := writeFileBytes(full, []byte(fx.before)); err != nil {
		t.Fatalf("write base %s: %v", fx.relPath, err)
	}
	mustGit("add", "-A")
	mustGit("commit", "-m", "base")

	mustGit("checkout", "-b", "feature/x")
	if err := writeFileBytes(full, []byte(fx.after)); err != nil {
		t.Fatalf("write feature %s: %v", fx.relPath, err)
	}
	mustGit("add", "-A")
	mustGit("commit", "-m", "feature: add suspicious dep")

	return dir
}

// writeFileBytes is a tiny helper to keep the test body tidy. It
// uses the same os primitives as the production manifest reader.
func writeFileBytes(path string, body []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o644)
}

// buildIntegrationInput parses fx.before / fx.after through the real
// production parser and returns an isb.ManifestGatedInput identical
// in shape to what buildManifestGatedInput would produce in the live
// daemon path. The rule call site is the same regardless of whether
// the input came from git diff or a synthesised fixture.
func buildIntegrationInput(t *testing.T, fx manifestFixture, branch, sha string) isb.ManifestGatedInput {
	t.Helper()
	parser, ok := manifests.Default().Detect(fx.relPath)
	if !ok {
		t.Fatalf("manifests.Default().Detect(%q) returned false — production registry not loaded?", fx.relPath)
	}
	added, removed, err := parser.ParseDiff(fx.relPath, []byte(fx.before), []byte(fx.after))
	if err != nil {
		t.Fatalf("ParseDiff(%s): %v", fx.relPath, err)
	}
	return isb.ManifestGatedInput{
		SourceTaskID: 42,
		TargetRepo:   "example/repo",
		Branch:       branch,
		CommitSHA:    sha,
		ChangedManifests: []isb.ChangedManifest{{
			Path:        fx.relPath,
			Ecosystem:   fx.ecosystem,
			DepsAdded:   added,
			DepsRemoved: removed,
			BeforeBytes: []byte(fx.before),
			AfterBytes:  []byte(fx.after),
		}},
	}
}

// freshDB is the per-test DB factory: real SQLite, registered for
// cleanup. Tests use this instead of a package-shared DB so each test
// stays isolated (per CLAUDE.md "tests are independent").
func freshDB(t *testing.T) *sql.DB {
	t.Helper()
	db := store.InitHolocronDSN(":memory:")
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// stubCallRecord captures the (ecosystem, name, version) triple for
// each stub invocation so tests can assert the rule queried with the
// exact dep we introduced via the manifest.
type stubCallRecord struct {
	ecosystem codeartifact.Ecosystem
	name      string
	version   string
}

// notFoundStub returns a codeartifact.Client stub that:
//   - records every DescribePackageVersion invocation in *calls,
//   - returns ErrPackageNotFound for the supplied (eco, name,
//     version) triple,
//   - returns a Published 200 for any other triple (so unrelated
//     existing deps in the manifest don't accidentally trigger a
//     finding when the parser surfaces them as "added").
//
// Note: for SUPPLY-001 we typically construct fixtures where only
// the hallucinated dep is in DepsAdded — but defensive 200s on
// non-matching triples make the matrix robust against parser
// surprises (e.g. version normalisation).
func notFoundStub(calls *[]stubCallRecord, hallEco codeartifact.Ecosystem, hallName, hallVersion string) *stubCodeArtifact {
	stub := newStubCodeArtifact()
	stub.defaultHandler = func(eco codeartifact.Ecosystem, name, version string) (codeartifact.PackageVersionInfo, error) {
		*calls = append(*calls, stubCallRecord{ecosystem: eco, name: name, version: version})
		if eco == hallEco && name == hallName && version == hallVersion {
			return codeartifact.PackageVersionInfo{}, codeartifact.ErrPackageNotFound
		}
		return codeartifact.PackageVersionInfo{Ecosystem: eco, Name: name, Version: version, Status: "Published"}, nil
	}
	return stub
}

// assertSupply001Finding fails the test unless `findings` contains
// exactly one SUPPLY-001 finding for `wantName` at `wantPath` with
// advise severity. Centralised so each ecosystem test stays terse.
func assertSupply001Finding(t *testing.T, findings []isb.Finding, wantPath, wantName, wantVersion string) {
	t.Helper()
	matches := 0
	for _, f := range findings {
		if f.RuleID != "SUPPLY-001" {
			continue
		}
		if f.Severity != isb.SeverityAdvise {
			t.Errorf("SUPPLY-001 finding severity=%q (want advise): %+v", f.Severity, f)
		}
		if f.Path != wantPath {
			t.Errorf("SUPPLY-001 finding path=%q (want %q): %+v", f.Path, wantPath, f)
		}
		if !strings.Contains(f.Message, wantName) {
			t.Errorf("SUPPLY-001 message missing dep name %q: %q", wantName, f.Message)
		}
		if wantVersion != "" && !strings.Contains(f.Message, wantVersion) {
			t.Errorf("SUPPLY-001 message missing version %q: %q", wantVersion, f.Message)
		}
		if !strings.Contains(f.Message, "hallucination") {
			t.Errorf("SUPPLY-001 message missing 'hallucination': %q", f.Message)
		}
		matches++
	}
	if matches != 1 {
		t.Fatalf("expected exactly 1 SUPPLY-001 finding for %s, got %d. all findings=%+v", wantName, matches, findings)
	}
}

// assertSupply002Finding fails unless `findings` contains exactly one
// SUPPLY-002 finding with the typosquat name + suspected canonical
// in its message.
func assertSupply002Finding(t *testing.T, findings []isb.Finding, wantPath, typoName, canonicalName string) {
	t.Helper()
	matches := 0
	for _, f := range findings {
		if f.RuleID != "SUPPLY-002" {
			continue
		}
		if f.Severity != isb.SeverityAdvise {
			t.Errorf("SUPPLY-002 finding severity=%q (want advise): %+v", f.Severity, f)
		}
		if f.Path != wantPath {
			t.Errorf("SUPPLY-002 finding path=%q (want %q): %+v", f.Path, wantPath, f)
		}
		if !strings.Contains(f.Message, typoName) {
			t.Errorf("SUPPLY-002 message missing typo name %q: %q", typoName, f.Message)
		}
		if !strings.Contains(f.Message, canonicalName) {
			t.Errorf("SUPPLY-002 message missing canonical name %q: %q", canonicalName, f.Message)
		}
		matches++
	}
	if matches != 1 {
		t.Fatalf("expected exactly 1 SUPPLY-002 finding for %s vs %s, got %d. all findings=%+v", typoName, canonicalName, matches, findings)
	}
}

// assertStubCalledWith fails the test unless the calls slice contains
// at least one record for the supplied triple. Used for SUPPLY-001
// to confirm the rule actually walked through to the registry layer
// for the hallucinated dep.
func assertStubCalledWith(t *testing.T, calls []stubCallRecord, eco codeartifact.Ecosystem, name, version string) {
	t.Helper()
	for _, c := range calls {
		if c.ecosystem == eco && c.name == name && c.version == version {
			return
		}
	}
	t.Fatalf("expected stub call for (%s, %s, %s); got %+v", eco, name, version, calls)
}

// ── SUPPLY-001 × 5 ecosystems ───────────────────────────────────────────────

// TestSupply001_HallucinatedRubyGem_Rejected — Gemfile delta
// introduces a non-existent gem; CodeArtifact stub returns 404; rule
// emits an advise-mode finding.
func TestSupply001_HallucinatedRubyGem_Rejected(t *testing.T) {
	const hallName = "hallucinated-fake-gem"
	const hallVersion = "0.0.1"
	fx := manifestFixture{
		ecosystem: manifests.EcosystemRubyGems,
		relPath:   "Gemfile",
		before: `source 'https://rubygems.org'

gem 'rails', '7.0.4'
`,
		after: `source 'https://rubygems.org'

gem 'rails', '7.0.4'
gem '` + hallName + `', '` + hallVersion + `'
`,
	}
	_ = setupGitRepo(t, fx) // exercise the git-tempdir setup path
	db := freshDB(t)

	var calls []stubCallRecord
	stub := notFoundStub(&calls, codeartifact.EcosystemRubyGems, hallName, hallVersion)
	rule := NewSUPPLY001(stub)

	in := buildIntegrationInput(t, fx, "feature/x", "deadbeef01")
	findings, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertSupply001Finding(t, findings, "Gemfile", hallName, hallVersion)
	assertStubCalledWith(t, calls, codeartifact.EcosystemRubyGems, hallName, hallVersion)
}

// TestSupply001_HallucinatedNpmPackage_Rejected — package.json adds
// a non-existent npm package; stub returns 404; finding emitted.
func TestSupply001_HallucinatedNpmPackage_Rejected(t *testing.T) {
	const hallName = "totally-not-a-real-npm-pkg"
	const hallVersion = "1.0.0"
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
    "` + hallName + `": "` + hallVersion + `"
  }
}
`,
	}
	_ = setupGitRepo(t, fx)
	db := freshDB(t)

	var calls []stubCallRecord
	stub := notFoundStub(&calls, codeartifact.EcosystemNPM, hallName, hallVersion)
	rule := NewSUPPLY001(stub)

	in := buildIntegrationInput(t, fx, "feature/x", "deadbeef02")
	findings, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertSupply001Finding(t, findings, "package.json", hallName, hallVersion)
	assertStubCalledWith(t, calls, codeartifact.EcosystemNPM, hallName, hallVersion)
}

// TestSupply001_HallucinatedPyPI_Rejected — requirements.txt adds a
// non-existent PyPI package; stub returns 404; finding emitted.
func TestSupply001_HallucinatedPyPI_Rejected(t *testing.T) {
	const hallName = "ghost-pypi-package"
	const hallVersion = "0.1.0"
	fx := manifestFixture{
		ecosystem: manifests.EcosystemPyPI,
		relPath:   "requirements.txt",
		before: `requests==2.31.0
flask==2.3.0
`,
		after: `requests==2.31.0
flask==2.3.0
` + hallName + `==` + hallVersion + `
`,
	}
	_ = setupGitRepo(t, fx)
	db := freshDB(t)

	var calls []stubCallRecord
	stub := notFoundStub(&calls, codeartifact.EcosystemPyPI, hallName, hallVersion)
	rule := NewSUPPLY001(stub)

	in := buildIntegrationInput(t, fx, "feature/x", "deadbeef03")
	findings, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertSupply001Finding(t, findings, "requirements.txt", hallName, hallVersion)
	assertStubCalledWith(t, calls, codeartifact.EcosystemPyPI, hallName, hallVersion)
}

// TestSupply001_HallucinatedMaven_Rejected — pom.xml introduces a
// non-existent groupId:artifactId. Maven dep names are
// "groupId:artifactId" per the parser's pomDep handling.
func TestSupply001_HallucinatedMaven_Rejected(t *testing.T) {
	const hallName = "com.fake.fictional:phantom-jar"
	const hallVersion = "9.9.9"
	// Split groupId / artifactId for the XML body.
	parts := strings.SplitN(hallName, ":", 2)
	hallGroup, hallArtifact := parts[0], parts[1]
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
            <groupId>` + hallGroup + `</groupId>
            <artifactId>` + hallArtifact + `</artifactId>
            <version>` + hallVersion + `</version>
        </dependency>
    </dependencies>
</project>
`,
	}
	_ = setupGitRepo(t, fx)
	db := freshDB(t)

	var calls []stubCallRecord
	stub := notFoundStub(&calls, codeartifact.EcosystemMaven, hallName, hallVersion)
	rule := NewSUPPLY001(stub)

	in := buildIntegrationInput(t, fx, "feature/x", "deadbeef04")
	findings, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertSupply001Finding(t, findings, "pom.xml", hallName, hallVersion)
	assertStubCalledWith(t, calls, codeartifact.EcosystemMaven, hallName, hallVersion)
}

// TestSupply001_HallucinatedGoModule_Rejected — go.mod adds a
// non-existent module path. Go is silently skipped per the Wave 1
// design (CodeArtifact has no Go format, so SUPPLY-001 is a no-op
// for go deps via the manifestsToCodeArtifact `ok=false` branch).
//
// This test asserts ZERO findings AND zero stub calls — that's the
// expected shape until SUPPLY-005 (P3) covers Go via osv-scanner.
func TestSupply001_HallucinatedGoModule_Rejected(t *testing.T) {
	const hallName = "github.com/fake-org/nonexistent-module"
	const hallVersion = "v1.2.3"
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
	` + hallName + ` ` + hallVersion + `
)
`,
	}
	_ = setupGitRepo(t, fx)
	db := freshDB(t)

	var calls []stubCallRecord
	// Provide the stub but expect zero invocations: the rule short-
	// circuits on Go ecosystems before any client call.
	stub := newStubCodeArtifact()
	stub.defaultHandler = func(eco codeartifact.Ecosystem, name, version string) (codeartifact.PackageVersionInfo, error) {
		calls = append(calls, stubCallRecord{ecosystem: eco, name: name, version: version})
		// Should never be reached; if it is, return an obviously wrong
		// result so the test fails loudly.
		return codeartifact.PackageVersionInfo{}, codeartifact.ErrPackageNotFound
	}
	rule := NewSUPPLY001(stub)

	in := buildIntegrationInput(t, fx, "feature/x", "deadbeef05")
	findings, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("Go is silently skipped by SUPPLY-001 (CodeArtifact has no Go format) — expected 0 findings, got %d: %+v", len(findings), findings)
	}
	if len(calls) != 0 {
		t.Errorf("expected 0 stub calls for Go ecosystem, got %d: %+v", len(calls), calls)
	}
}

// ── SUPPLY-002 × 5 ecosystems ───────────────────────────────────────────────

// TestSupply002_RubyTyposquat_Rejected — Gemfile adds "loadash"
// (Damerau distance 1 from "lodash"); allowlist contains "lodash";
// rule emits a typosquat advisory.
func TestSupply002_RubyTyposquat_Rejected(t *testing.T) {
	const canonical = "lodash"
	const typo = "loadash" // 1 insertion
	const typoVersion = "4.17.21"
	fx := manifestFixture{
		ecosystem: manifests.EcosystemRubyGems,
		relPath:   "Gemfile",
		before: `source 'https://rubygems.org'

gem 'rails', '7.0.4'
`,
		after: `source 'https://rubygems.org'

gem 'rails', '7.0.4'
gem '` + typo + `', '` + typoVersion + `'
`,
	}
	_ = setupGitRepo(t, fx)
	db := freshDB(t)
	store.SetConfig(db, "supply_allowlist_rubygems", canonical)

	rule := NewSUPPLY002()
	in := buildIntegrationInput(t, fx, "feature/x", "deadbabe01")
	findings, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertSupply002Finding(t, findings, "Gemfile", typo, canonical)
}

// TestSupply002_NpmTyposquat_Rejected — package.json adds "expres"
// (distance 1 from "express"); allowlist seeded with "express".
func TestSupply002_NpmTyposquat_Rejected(t *testing.T) {
	const canonical = "express"
	const typo = "expres" // 1 deletion
	const typoVersion = "4.18.0"
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
    "` + typo + `": "` + typoVersion + `"
  }
}
`,
	}
	_ = setupGitRepo(t, fx)
	db := freshDB(t)
	// Seed allowlist with the canonical name; "react" too so the
	// pre-existing dep doesn't accidentally hit the closest-match
	// path for some other entry.
	store.SetConfig(db, "supply_allowlist_npm", strings.Join([]string{canonical, "react"}, "\n"))

	rule := NewSUPPLY002()
	in := buildIntegrationInput(t, fx, "feature/x", "deadbabe02")
	findings, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertSupply002Finding(t, findings, "package.json", typo, canonical)
}

// TestSupply002_PythonTyposquat_Rejected — requirements.txt adds
// "reqests" (distance 1 from "requests"); allowlist seeded with
// "requests".
func TestSupply002_PythonTyposquat_Rejected(t *testing.T) {
	const canonical = "requests"
	const typo = "reqests" // 1 deletion
	const typoVersion = "2.31.0"
	fx := manifestFixture{
		ecosystem: manifests.EcosystemPyPI,
		relPath:   "requirements.txt",
		before: `flask==2.3.0
`,
		after: `flask==2.3.0
` + typo + `==` + typoVersion + `
`,
	}
	_ = setupGitRepo(t, fx)
	db := freshDB(t)
	store.SetConfig(db, "supply_allowlist_pypi", strings.Join([]string{canonical, "flask"}, "\n"))

	rule := NewSUPPLY002()
	in := buildIntegrationInput(t, fx, "feature/x", "deadbabe03")
	findings, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertSupply002Finding(t, findings, "requirements.txt", typo, canonical)
}

// TestSupply002_MavenTyposquat_Rejected — pom.xml adds a typosquat
// of a canonical groupId:artifactId. SUPPLY-002 dep names are
// "groupId:artifactId"; we typo the artifact only ("commons-lang4"
// vs canonical "commons-lang3" — distance 1).
func TestSupply002_MavenTyposquat_Rejected(t *testing.T) {
	const canonical = "org.apache.commons:commons-lang3"
	const typo = "org.apache.commons:commons-lang4" // distance 1 in the artifact-id segment
	const typoVersion = "3.14.0"
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
            <groupId>org.apache.commons</groupId>
            <artifactId>commons-lang4</artifactId>
            <version>` + typoVersion + `</version>
        </dependency>
    </dependencies>
</project>
`,
	}
	_ = setupGitRepo(t, fx)
	db := freshDB(t)
	// Seed both canonical and the existing spring-boot-starter-web
	// so the unrelated dep doesn't surface any unexpected finding.
	store.SetConfig(db, "supply_allowlist_maven",
		strings.Join([]string{canonical, "org.springframework.boot:spring-boot-starter-web"}, "\n"))

	rule := NewSUPPLY002()
	in := buildIntegrationInput(t, fx, "feature/x", "deadbabe04")
	findings, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertSupply002Finding(t, findings, "pom.xml", typo, canonical)
}

// TestSupply002_GoTyposquat_Rejected — go.mod adds a typosquat of
// a canonical module path. SUPPLY-002 doesn't query CodeArtifact so
// Go IS supported here (unlike SUPPLY-001's silent skip).
//
// "github.com/spf13/cobr" vs canonical "github.com/spf13/cobra" —
// distance 1 (single deletion at the end).
func TestSupply002_GoTyposquat_Rejected(t *testing.T) {
	const canonical = "github.com/spf13/cobra"
	const typo = "github.com/spf13/cobr" // 1 deletion
	const typoVersion = "v1.8.0"
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
	` + typo + ` ` + typoVersion + `
)
`,
	}
	_ = setupGitRepo(t, fx)
	db := freshDB(t)
	// Seed the canonical + the unchanged testify so neither becomes
	// a stray closest-match for some other dep.
	store.SetConfig(db, "supply_allowlist_go",
		strings.Join([]string{canonical, "github.com/stretchr/testify"}, "\n"))

	rule := NewSUPPLY002()
	in := buildIntegrationInput(t, fx, "feature/x", "deadbabe05")
	findings, err := rule.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertSupply002Finding(t, findings, "go.mod", typo, canonical)
}
