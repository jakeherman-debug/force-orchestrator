package manifests

import (
	"testing"
)

// stubParser satisfies the manifests.Parser interface for the
// dispatch-table tests.
type stubParser struct {
	eco       Ecosystem
	matches   map[string]bool
	parseRes  []Dependency
	parseErr  error
}

func (s *stubParser) Ecosystem() Ecosystem { return s.eco }

func (s *stubParser) Detect(path string) bool {
	return s.matches[path]
}

func (s *stubParser) Parse(path string, content []byte) ([]Dependency, error) {
	return s.parseRes, s.parseErr
}

func (s *stubParser) ParseDiff(path string, before, after []byte) ([]Dependency, []Dependency, error) {
	return s.parseRes, nil, s.parseErr
}

func TestRegistry_Detect(t *testing.T) {
	r := NewRegistry()
	r.parsers = append(r.parsers,
		&stubParser{eco: EcosystemPyPI, matches: map[string]bool{"requirements.txt": true}},
		&stubParser{eco: EcosystemNPM, matches: map[string]bool{"package.json": true}},
	)

	if _, ok := r.Detect("requirements.txt"); !ok {
		t.Errorf("expected pypi parser to claim requirements.txt")
	}
	if _, ok := r.Detect("package.json"); !ok {
		t.Errorf("expected npm parser to claim package.json")
	}
	if _, ok := r.Detect("unrelated.txt"); ok {
		t.Errorf("unrelated.txt must not match")
	}
}

func TestRegistry_EcosystemFor(t *testing.T) {
	r := NewRegistry()
	r.parsers = append(r.parsers,
		&stubParser{eco: EcosystemMaven, matches: map[string]bool{"pom.xml": true}},
	)
	got, ok := r.EcosystemFor("pom.xml")
	if !ok || got != EcosystemMaven {
		t.Errorf("expected EcosystemMaven, got %q ok=%v", got, ok)
	}
	if _, ok := r.EcosystemFor("README.md"); ok {
		t.Errorf("README.md must not be a manifest")
	}
}

