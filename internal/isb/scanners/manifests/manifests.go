// Package manifests provides the top-level registry that maps a file
// path to the right per-ecosystem Parser. Used by the ISBReview
// manifest-gating dispatch (D5 Phase 0): SUPPLY-* rules only fire
// when a commit's diff includes at least one recognised manifest.
//
// Per-ecosystem subpackages each export a Parser implementation:
//
//   - manifests/gemfile  — Ruby (Gemfile, Gemfile.lock, *.gemspec)
//   - manifests/pip      — Python (requirements.txt, Pipfile,
//                          Pipfile.lock, pyproject.toml, poetry.lock,
//                          setup.py)
//   - manifests/npm      — JS/TS (package.json, package-lock.json,
//                          yarn.lock, pnpm-lock.yaml)
//   - manifests/maven    — Java/Kotlin (pom.xml, build.gradle,
//                          build.gradle.kts)
//   - manifests/gomod    — Go (go.mod, go.sum)
//
// The Dependency type is shared across ecosystems so the SUPPLY rules
// see one homogeneous shape regardless of source. Source = "direct"
// for direct dependencies, "transitive" for lock-file-only entries.
//
// Scope (per docs/roadmap.md § D5 P0): the parsers aim for "good
// enough to extract direct + transitive name@version from the most
// common shapes." They are NOT full dependency resolvers. Best-effort
// regex paths are documented per parser; malformed input must NOT
// panic and returns (nil, error) so the caller can route through the
// deferral path.
package manifests


// Ecosystem mirrors the codeartifact.Ecosystem enum but stays in this
// package so parsers don't have to import the AWS client. The values
// are string-equal to codeartifact.Ecosystem for trivial conversion.
type Ecosystem string

const (
	EcosystemPyPI     Ecosystem = "pypi"
	EcosystemNPM      Ecosystem = "npm"
	EcosystemRubyGems Ecosystem = "rubygems"
	EcosystemMaven    Ecosystem = "maven"
	EcosystemGo       Ecosystem = "go"
)

// SourceKind distinguishes direct deps (declared in the manifest) from
// transitive deps (resolved by the lock file). SUPPLY-001/002 rules
// typically focus on direct deps; SUPPLY-005 (CVE) checks both.
type SourceKind string

const (
	SourceDirect     SourceKind = "direct"
	SourceTransitive SourceKind = "transitive"
)

// Dependency is the unified shape every Parser produces.
type Dependency struct {
	Ecosystem Ecosystem
	Name      string
	Version   string // exact version when pinned; "" when unknown / range / VCS-ref
	Source    SourceKind
}

// Parser is the per-ecosystem contract.
type Parser interface {
	// Detect returns true if this parser handles the supplied file
	// path (case-sensitive on the basename — manifest filenames are
	// not Windows-cased in any production repo).
	Detect(path string) bool

	// Parse extracts dependencies from the manifest content.
	// Implementations MUST NOT panic on malformed input; return
	// (nil, error) instead so the caller can record the deferral.
	Parse(path string, content []byte) ([]Dependency, error)

	// ParseDiff extracts the (added, removed) dep delta between two
	// versions of the same manifest. Implementations may compute it
	// as (parse(after) − parse(before)) when a structural diff is
	// expensive — that gives the right answer for our use case
	// (deciding which deps are NEW in this commit).
	ParseDiff(path string, before, after []byte) (added, removed []Dependency, err error)
}

// Registry is the package-level dispatch table.
type Registry struct {
	parsers   []Parser
	ecosystem map[string]Ecosystem // basename → ecosystem (cache for IsManifest)
}

// NewRegistry returns the default registry seeded with every
// ecosystem's parser. Tests inject a custom Registry to unit-test
// dispatch in isolation.
func NewRegistry(parsers ...Parser) *Registry {
	r := &Registry{
		parsers:   parsers,
		ecosystem: map[string]Ecosystem{},
	}
	return r
}

// Detect returns the matching Parser for path (and true), or
// (nil, false) when no parser claims it.
func (r *Registry) Detect(path string) (Parser, bool) {
	for _, p := range r.parsers {
		if p.Detect(path) {
			return p, true
		}
	}
	return nil, false
}

// EcosystemFor returns the ecosystem for the given path (and true), or
// ("", false) when the path is not a recognised manifest.
func (r *Registry) EcosystemFor(path string) (Ecosystem, bool) {
	for _, p := range r.parsers {
		if p.Detect(path) {
			return ecosystemOf(p), true
		}
	}
	return "", false
}

// IsManifest is the manifest-gating helper used by ISBReview. Same as
// EcosystemFor but on the default fleet-wide registry.
func IsManifest(path string) (Ecosystem, bool) {
	return Default().EcosystemFor(path)
}

var defaultRegistry *Registry

// Default returns the package-level registry shared across the fleet.
// Lazy-init so import order doesn't matter; the per-ecosystem
// subpackages register themselves via init() the first time they're
// imported.
func Default() *Registry {
	if defaultRegistry == nil {
		defaultRegistry = NewRegistry()
	}
	return defaultRegistry
}

// Register adds a parser to the default registry. Per-ecosystem
// subpackages call this from their init().
func Register(p Parser) {
	d := Default()
	d.parsers = append(d.parsers, p)
}

// ecosystemOf inspects a parser to determine its ecosystem. Parsers
// that know their own ecosystem (recommended) implement the optional
// EcosystemAware interface; the fallback returns "" so test parsers
// without the interface are still routable through Detect/Parse.
func ecosystemOf(p Parser) Ecosystem {
	if a, ok := p.(EcosystemAware); ok {
		return a.Ecosystem()
	}
	return ""
}

// EcosystemAware lets a Parser self-declare its ecosystem. All
// production parsers implement this.
type EcosystemAware interface {
	Ecosystem() Ecosystem
}

// ResetForTests clears the package-level registry. ONLY for tests
// that need an isolated dispatch table — production callers must use
// the default registry.
func ResetForTests() {
	defaultRegistry = NewRegistry()
}
