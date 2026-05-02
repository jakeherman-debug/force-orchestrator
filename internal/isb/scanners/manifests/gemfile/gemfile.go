// Package gemfile is the Ruby manifest parser. Handles Gemfile,
// Gemfile.lock, and *.gemspec files via line-oriented regex matchers.
//
// Bundler's Gemfile DSL is Ruby code — a complete parser would
// require an actual Ruby evaluator. We instead match the conventional
// `gem 'name', 'version'` shape that covers ~99% of real Gemfile
// declarations. Programmatic Gemfiles (loops, conditional gem calls)
// are best-effort: missed deps are caught downstream by Gemfile.lock
// (which is the resolved truth and ALWAYS regex-shaped).
package gemfile

import (
	"bufio"
	"bytes"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"force-orchestrator/internal/isb/scanners/manifests"
)

func init() {
	manifests.Register(&Parser{})
}

// Parser implements manifests.Parser for Ruby.
type Parser struct{}

// Ecosystem implements manifests.EcosystemAware.
func (Parser) Ecosystem() manifests.Ecosystem { return manifests.EcosystemRubyGems }

// Detect matches Gemfile, Gemfile.lock, and *.gemspec.
func (Parser) Detect(path string) bool {
	base := filepath.Base(path)
	if base == "Gemfile" || base == "Gemfile.lock" {
		return true
	}
	return strings.HasSuffix(base, ".gemspec")
}

// gemRegex matches `gem 'name'[, 'version'][, options...]` in Gemfile.
// Quotes may be either single or double; version arg is optional.
// We accept range operators (~> 1.0, >= 2) and capture the verbatim
// constraint string — exact pinning is the caller's concern.
var gemRegex = regexp.MustCompile(`^\s*gem\s+['"]([^'"]+)['"](?:\s*,\s*['"]([^'"]+)['"])?`)

// gemspecRegex matches add_dependency / add_runtime_dependency /
// add_development_dependency calls inside a *.gemspec.
var gemspecRegex = regexp.MustCompile(`\.add(?:_runtime|_development)?_dependency\(?\s*['"]([^'"]+)['"](?:\s*,\s*['"]([^'"]+)['"])?`)

// lockNameVersionRegex matches Gemfile.lock specs lines, which look
// like `    rails (7.0.4)` (4-space indent, name, space, parenthesised
// version). Indent isn't strictly required so we tolerate any.
var lockNameVersionRegex = regexp.MustCompile(`^\s+([a-zA-Z0-9._\-]+)\s+\(([^)]+)\)`)

// Parse extracts deps from the supplied manifest. Direct deps come
// from Gemfile / *.gemspec; transitive deps come from Gemfile.lock.
func (p Parser) Parse(path string, content []byte) ([]manifests.Dependency, error) {
	base := filepath.Base(path)
	switch {
	case base == "Gemfile":
		return parseGemfile(content), nil
	case base == "Gemfile.lock":
		return parseGemfileLock(content), nil
	case strings.HasSuffix(base, ".gemspec"):
		return parseGemspec(content), nil
	}
	return nil, fmt.Errorf("gemfile: unsupported file %q", path)
}

// ParseDiff is the "parse(after) − parse(before)" shape.
func (p Parser) ParseDiff(path string, before, after []byte) ([]manifests.Dependency, []manifests.Dependency, error) {
	beforeDeps, err := p.Parse(path, before)
	if err != nil {
		beforeDeps = nil
	}
	afterDeps, err := p.Parse(path, after)
	if err != nil {
		return nil, nil, err
	}
	return diffDeps(afterDeps, beforeDeps), diffDeps(beforeDeps, afterDeps), nil
}

func parseGemfile(content []byte) []manifests.Dependency {
	var out []manifests.Dependency
	scanner := bufio.NewScanner(bytes.NewReader(content))
	for scanner.Scan() {
		line := scanner.Text()
		// Strip inline comments before matching so `gem 'foo' # …` works.
		if idx := strings.Index(line, "#"); idx >= 0 {
			line = line[:idx]
		}
		m := gemRegex.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		out = append(out, manifests.Dependency{
			Ecosystem: manifests.EcosystemRubyGems,
			Name:      m[1],
			Version:   m[2],
			Source:    manifests.SourceDirect,
		})
	}
	return out
}

func parseGemspec(content []byte) []manifests.Dependency {
	var out []manifests.Dependency
	scanner := bufio.NewScanner(bytes.NewReader(content))
	for scanner.Scan() {
		line := scanner.Text()
		if idx := strings.Index(line, "#"); idx >= 0 {
			line = line[:idx]
		}
		m := gemspecRegex.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		out = append(out, manifests.Dependency{
			Ecosystem: manifests.EcosystemRubyGems,
			Name:      m[1],
			Version:   m[2],
			Source:    manifests.SourceDirect,
		})
	}
	return out
}

// parseGemfileLock walks the GEM/specs section. A spec line is
// "    name (version)"; following lines indented further are the
// spec's transitive deps (without versions). Direct deps come from
// the DEPENDENCIES block at the bottom — but every entry there also
// appears in specs, so reading specs alone gives us the complete
// resolved set.
func parseGemfileLock(content []byte) []manifests.Dependency {
	var (
		out      []manifests.Dependency
		inSpecs  bool
		seen     = map[string]bool{}
		scanner  = bufio.NewScanner(bytes.NewReader(content))
	)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024*16)
	for scanner.Scan() {
		line := scanner.Text()
		trim := strings.TrimSpace(line)

		switch {
		case trim == "specs:":
			inSpecs = true
			continue
		case trim == "DEPENDENCIES" || trim == "PLATFORMS" || trim == "BUNDLED WITH" ||
			trim == "GEM" || trim == "GIT" || trim == "PATH":
			inSpecs = false
			continue
		}
		if !inSpecs {
			continue
		}

		m := lockNameVersionRegex.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		// Heuristic: a spec line has 4 leading spaces; a transitive-
		// of-spec line has 6+. Both are deps in our model — we record
		// them all as Transitive so SUPPLY-001/002 still gets the
		// names. Direct vs transitive distinction is recovered from
		// Gemfile parsing, not Gemfile.lock.
		key := m[1] + "@" + m[2]
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, manifests.Dependency{
			Ecosystem: manifests.EcosystemRubyGems,
			Name:      m[1],
			Version:   m[2],
			Source:    manifests.SourceTransitive,
		})
	}
	return out
}

func diffDeps(a, b []manifests.Dependency) []manifests.Dependency {
	bSet := map[string]bool{}
	for _, d := range b {
		bSet[d.Name+"@"+d.Version] = true
	}
	var out []manifests.Dependency
	for _, d := range a {
		if !bSet[d.Name+"@"+d.Version] {
			out = append(out, d)
		}
	}
	return out
}
