// Package gomod is the Go manifest parser. Handles `go.mod` (direct +
// indirect deps via the require block) and `go.sum` (transitive
// resolution). go.mod parsing routes through golang.org/x/mod/modfile
// — the canonical AST — so version comments, retract directives, and
// replace blocks don't trip us.
package gomod

import (
	"bufio"
	"bytes"
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/mod/modfile"

	"force-orchestrator/internal/isb/scanners/manifests"
)

func init() {
	manifests.Register(&Parser{})
}

// Parser implements manifests.Parser for Go.
type Parser struct{}

// Ecosystem implements manifests.EcosystemAware.
func (Parser) Ecosystem() manifests.Ecosystem { return manifests.EcosystemGo }

// Detect matches go.mod and go.sum.
func (Parser) Detect(path string) bool {
	base := filepath.Base(path)
	return base == "go.mod" || base == "go.sum"
}

// Parse extracts deps from go.mod or go.sum.
func (p Parser) Parse(path string, content []byte) ([]manifests.Dependency, error) {
	base := filepath.Base(path)
	switch base {
	case "go.mod":
		return parseGoMod(path, content)
	case "go.sum":
		return parseGoSum(content), nil
	default:
		return nil, fmt.Errorf("gomod: unsupported file %q", path)
	}
}

// ParseDiff returns the (added, removed) delta. Implementation is the
// canonical "parse(after) − parse(before)" set difference.
func (p Parser) ParseDiff(path string, before, after []byte) ([]manifests.Dependency, []manifests.Dependency, error) {
	beforeDeps, err := p.Parse(path, before)
	if err != nil {
		// Best-effort: if `before` is empty/missing the file was
		// new, treat parse errors there as zero baseline.
		beforeDeps = nil
	}
	afterDeps, err := p.Parse(path, after)
	if err != nil {
		return nil, nil, err
	}
	return diffDeps(afterDeps, beforeDeps), diffDeps(beforeDeps, afterDeps), nil
}

// parseGoMod uses modfile so replace / retract / version comments
// are handled correctly.
func parseGoMod(path string, content []byte) ([]manifests.Dependency, error) {
	if len(bytes.TrimSpace(content)) == 0 {
		return nil, nil
	}
	mf, err := modfile.Parse(path, content, nil)
	if err != nil {
		return nil, fmt.Errorf("gomod: parse %s: %w", path, err)
	}
	var out []manifests.Dependency
	for _, r := range mf.Require {
		if r == nil {
			continue
		}
		src := manifests.SourceDirect
		if r.Indirect {
			src = manifests.SourceTransitive
		}
		out = append(out, manifests.Dependency{
			Ecosystem: manifests.EcosystemGo,
			Name:      r.Mod.Path,
			Version:   r.Mod.Version,
			Source:    src,
		})
	}
	return out, nil
}

// parseGoSum extracts (module, version) pairs from go.sum lines. Two
// lines per dep (module + go.mod). We dedup on (name, version) to
// surface each once. Source is always Transitive — go.sum captures
// the resolved transitive set.
func parseGoSum(content []byte) []manifests.Dependency {
	seen := map[string]bool{}
	var out []manifests.Dependency
	scanner := bufio.NewScanner(bytes.NewReader(content))
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024*16)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		// go.sum line shape: "<module> <version>[/go.mod] h1:<hash>"
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		name := fields[0]
		ver := strings.TrimSuffix(fields[1], "/go.mod")
		key := name + "\x00" + ver
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, manifests.Dependency{
			Ecosystem: manifests.EcosystemGo,
			Name:      name,
			Version:   ver,
			Source:    manifests.SourceTransitive,
		})
	}
	return out
}

// diffDeps returns deps that are in `a` but not in `b` (set
// difference, keyed on (name, version)).
func diffDeps(a, b []manifests.Dependency) []manifests.Dependency {
	bSet := map[string]bool{}
	for _, d := range b {
		bSet[depKey(d)] = true
	}
	var out []manifests.Dependency
	for _, d := range a {
		if !bSet[depKey(d)] {
			out = append(out, d)
		}
	}
	return out
}

func depKey(d manifests.Dependency) string {
	return string(d.Ecosystem) + "\x00" + d.Name + "\x00" + d.Version
}
