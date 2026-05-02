// Package npm parses JS/TS manifests: package.json (direct deps),
// package-lock.json v1/v2/v3 (transitive deps), yarn.lock, and
// pnpm-lock.yaml.
//
// JSON is the canonical shape for package.json + package-lock; we
// parse with encoding/json. yarn.lock + pnpm-lock are parsed with
// targeted regex — they have stable line shapes and a full grammar
// is overkill for the "extract name@version" goal.
package npm

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"force-orchestrator/internal/isb/scanners/manifests"
)

func init() {
	manifests.Register(&Parser{})
}

// Parser implements manifests.Parser for npm.
type Parser struct{}

// Ecosystem implements manifests.EcosystemAware.
func (Parser) Ecosystem() manifests.Ecosystem { return manifests.EcosystemNPM }

// Detect matches package.json, package-lock.json, yarn.lock, pnpm-lock.yaml.
func (Parser) Detect(path string) bool {
	switch filepath.Base(path) {
	case "package.json", "package-lock.json", "yarn.lock", "pnpm-lock.yaml":
		return true
	}
	return false
}

// Parse routes by basename.
func (p Parser) Parse(path string, content []byte) ([]manifests.Dependency, error) {
	switch filepath.Base(path) {
	case "package.json":
		return parsePackageJSON(content)
	case "package-lock.json":
		return parsePackageLock(content)
	case "yarn.lock":
		return parseYarnLock(content), nil
	case "pnpm-lock.yaml":
		return parsePnpmLock(content), nil
	}
	return nil, fmt.Errorf("npm: unsupported file %q", path)
}

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

type packageJSONShape struct {
	Dependencies         map[string]string `json:"dependencies"`
	DevDependencies      map[string]string `json:"devDependencies"`
	PeerDependencies     map[string]string `json:"peerDependencies"`
	OptionalDependencies map[string]string `json:"optionalDependencies"`
}

func parsePackageJSON(content []byte) ([]manifests.Dependency, error) {
	if len(bytes.TrimSpace(content)) == 0 {
		return nil, nil
	}
	var pj packageJSONShape
	if err := json.Unmarshal(content, &pj); err != nil {
		return nil, fmt.Errorf("npm: parse package.json: %w", err)
	}
	var out []manifests.Dependency
	for _, m := range []map[string]string{pj.Dependencies, pj.DevDependencies, pj.PeerDependencies, pj.OptionalDependencies} {
		for name, v := range m {
			out = append(out, manifests.Dependency{
				Ecosystem: manifests.EcosystemNPM,
				Name:      name,
				Version:   v,
				Source:    manifests.SourceDirect,
			})
		}
	}
	return out, nil
}

// parsePackageLock handles all three lockfileVersions:
//   - v1: top-level "dependencies" map (recursive)
//   - v2: both "dependencies" and "packages" populated
//   - v3: only "packages" populated
//
// We prefer "packages" when present (v2/v3) because it's flat. Fall
// back to "dependencies" for v1 — recursing through nested deps
// trees.
type packageLockShape struct {
	LockfileVersion int                          `json:"lockfileVersion"`
	Packages        map[string]packageLockEntry  `json:"packages"`
	Dependencies    map[string]packageLockV1Dep  `json:"dependencies"`
}

type packageLockEntry struct {
	Version string `json:"version"`
}

type packageLockV1Dep struct {
	Version      string                       `json:"version"`
	Dependencies map[string]packageLockV1Dep  `json:"dependencies"`
}

func parsePackageLock(content []byte) ([]manifests.Dependency, error) {
	if len(bytes.TrimSpace(content)) == 0 {
		return nil, nil
	}
	var pl packageLockShape
	if err := json.Unmarshal(content, &pl); err != nil {
		return nil, fmt.Errorf("npm: parse package-lock.json: %w", err)
	}
	seen := map[string]bool{}
	var out []manifests.Dependency

	// v2/v3 path: walk the flat packages map.
	for path, entry := range pl.Packages {
		if path == "" {
			continue // root package itself
		}
		// Path shape: "node_modules/<name>" or
		// "node_modules/<scope>/<name>" for nested deps. Strip the
		// "node_modules/" segments to recover the package name.
		name := stripNodeModules(path)
		if name == "" || entry.Version == "" {
			continue
		}
		key := name + "@" + entry.Version
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, manifests.Dependency{
			Ecosystem: manifests.EcosystemNPM,
			Name:      name,
			Version:   entry.Version,
			Source:    manifests.SourceTransitive,
		})
	}

	// v1 path: walk the recursive dependencies tree.
	walkV1Deps(pl.Dependencies, &out, seen)

	return out, nil
}

// stripNodeModules turns "node_modules/foo/node_modules/@bar/baz"
// into "@bar/baz" — the deepest segment after the final
// "node_modules/".
func stripNodeModules(path string) string {
	parts := strings.Split(path, "node_modules/")
	if len(parts) < 2 {
		return ""
	}
	return parts[len(parts)-1]
}

func walkV1Deps(deps map[string]packageLockV1Dep, out *[]manifests.Dependency, seen map[string]bool) {
	for name, d := range deps {
		if d.Version != "" {
			key := name + "@" + d.Version
			if !seen[key] {
				seen[key] = true
				*out = append(*out, manifests.Dependency{
					Ecosystem: manifests.EcosystemNPM,
					Name:      name,
					Version:   d.Version,
					Source:    manifests.SourceTransitive,
				})
			}
		}
		if d.Dependencies != nil {
			walkV1Deps(d.Dependencies, out, seen)
		}
	}
}

// yarnLockEntryRegex matches the spec line at the top of each yarn
// entry, e.g. `"@babel/core@^7.0.0":` or `lodash@4.17.21:`. We then
// look for the `version "X.Y.Z"` line that follows.
var (
	yarnSpecRegex    = regexp.MustCompile(`^"?([^@\s"]+(?:@[^@\s"]+)?)@[^"]+"?:\s*$`)
	yarnVersionRegex = regexp.MustCompile(`^\s*version\s+"([^"]+)"`)
)

// parseYarnLock walks yarn.lock looking for spec/version pairs.
func parseYarnLock(content []byte) []manifests.Dependency {
	var (
		out      []manifests.Dependency
		seen     = map[string]bool{}
		scanner  = bufio.NewScanner(bytes.NewReader(content))
		curName  string
	)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024*16)
	for scanner.Scan() {
		line := scanner.Text()

		// Try to match spec line (start of a new entry).
		if m := yarnSpecRegex.FindStringSubmatch(line); m != nil {
			// Yarn entries can list multiple specs separated by ", " —
			// "lodash@^4.17.0, lodash@^4.17.21:". We capture the FIRST
			// name; the version line below applies to all.
			curName = strings.TrimPrefix(m[1], "\"")
			// Strip trailing range chars if any leaked in (defensive).
			if idx := strings.IndexAny(curName, ","); idx >= 0 {
				curName = curName[:idx]
			}
			continue
		}
		// Try to match version line under the active spec.
		if curName != "" {
			if m := yarnVersionRegex.FindStringSubmatch(line); m != nil {
				key := curName + "@" + m[1]
				if !seen[key] {
					seen[key] = true
					out = append(out, manifests.Dependency{
						Ecosystem: manifests.EcosystemNPM,
						Name:      curName,
						Version:   m[1],
						Source:    manifests.SourceTransitive,
					})
				}
				curName = "" // wait for next spec
			}
		}
	}
	return out
}

// pnpmPkgRegex matches the "/package@version" keys in pnpm-lock.yaml.
// Two shapes: classic "/foo/1.2.3" and v6+ "/foo@1.2.3". We accept
// both. The (?:\([^)]+\))? trailing tail captures "(peer-deps...)"
// suffixes that don't change the resolved version.
var (
	pnpmKeyRegexA = regexp.MustCompile(`^\s+/([^/@:\s]+(?:/[^/@:\s]+)?)/([^/(:\s]+)(?:\(.*\))?:\s*$`)
	pnpmKeyRegexB = regexp.MustCompile(`^\s+/?([^@:/\s][^@:\s]*)@([^(:\s]+)(?:\(.*\))?:\s*$`)
)

// parsePnpmLock walks pnpm-lock.yaml. Best-effort regex parser.
func parsePnpmLock(content []byte) []manifests.Dependency {
	var (
		out     []manifests.Dependency
		seen    = map[string]bool{}
		scanner = bufio.NewScanner(bytes.NewReader(content))
	)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024*16)
	for scanner.Scan() {
		line := scanner.Text()
		var name, version string
		if m := pnpmKeyRegexA.FindStringSubmatch(line); m != nil {
			name, version = m[1], m[2]
		} else if m := pnpmKeyRegexB.FindStringSubmatch(line); m != nil {
			name, version = m[1], m[2]
		}
		if name == "" {
			continue
		}
		key := name + "@" + version
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, manifests.Dependency{
			Ecosystem: manifests.EcosystemNPM,
			Name:      name,
			Version:   version,
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
