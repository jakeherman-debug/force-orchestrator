// Package pip parses Python manifests:
//
//   - requirements.txt — line-oriented `name==version`, `name>=version`,
//     `name @ url`, `-r other.txt`, `-e .` etc. We extract direct deps
//     with exact pins; ranges are recorded with empty Version.
//   - Pipfile — TOML-shaped Pipenv manifest.
//   - Pipfile.lock — JSON-shaped lock with default + develop sections.
//   - pyproject.toml — PEP 621 (`[project] dependencies = [...]`) +
//     Poetry-style (`[tool.poetry.dependencies]`).
//   - poetry.lock — TOML-shaped lock from Poetry.
//   - setup.py — best-effort regex over `install_requires=[...]`.
package pip

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

type Parser struct{}

func (Parser) Ecosystem() manifests.Ecosystem { return manifests.EcosystemPyPI }

// Detect matches the standard Python manifest filenames.
func (Parser) Detect(path string) bool {
	switch filepath.Base(path) {
	case "requirements.txt", "Pipfile", "Pipfile.lock", "pyproject.toml", "poetry.lock", "setup.py":
		return true
	}
	return false
}

func (p Parser) Parse(path string, content []byte) ([]manifests.Dependency, error) {
	switch filepath.Base(path) {
	case "requirements.txt":
		return parseRequirementsTxt(content), nil
	case "Pipfile":
		return parsePipfile(content), nil
	case "Pipfile.lock":
		return parsePipfileLock(content)
	case "pyproject.toml":
		return parsePyProject(content), nil
	case "poetry.lock":
		return parsePoetryLock(content), nil
	case "setup.py":
		return parseSetupPy(content), nil
	}
	return nil, fmt.Errorf("pip: unsupported file %q", path)
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

// reqLineRegex matches a typical requirement line. PEP 508 is more
// expressive (env-markers, extras), but for our purposes a name and
// optional pinned version is the load-bearing data.
var reqLineRegex = regexp.MustCompile(`^\s*([A-Za-z0-9._\-]+)(?:\[[^\]]*\])?\s*(?:(==|>=|<=|~=|!=|>|<|===)\s*([A-Za-z0-9._\-]+))?`)

func parseRequirementsTxt(content []byte) []manifests.Dependency {
	var out []manifests.Dependency
	scanner := bufio.NewScanner(bytes.NewReader(content))
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024*16)
	for scanner.Scan() {
		line := scanner.Text()
		// Drop comments + blanks.
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "-") || strings.HasPrefix(line, "git+") {
			continue
		}
		m := reqLineRegex.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		ver := ""
		if len(m) >= 4 && m[2] == "==" {
			ver = m[3]
		}
		out = append(out, manifests.Dependency{
			Ecosystem: manifests.EcosystemPyPI,
			Name:      m[1],
			Version:   ver,
			Source:    manifests.SourceDirect,
		})
	}
	return out
}

// pipfileSectionRegex matches the [packages] / [dev-packages] header
// in a Pipfile.
var (
	pipfileSectionRegex   = regexp.MustCompile(`^\s*\[(packages|dev-packages)\]\s*$`)
	pipfileEntryRegex     = regexp.MustCompile(`^\s*([A-Za-z0-9._\-]+)\s*=\s*(?:"([^"]*)"|'([^']*)'|\{[^}]*version\s*=\s*"([^"]*)"[^}]*\}|\*)`)
	pipfileBoundaryRegex  = regexp.MustCompile(`^\s*\[`)
)

func parsePipfile(content []byte) []manifests.Dependency {
	var (
		out     []manifests.Dependency
		inPkgs  bool
		scanner = bufio.NewScanner(bytes.NewReader(content))
	)
	for scanner.Scan() {
		line := scanner.Text()
		if pipfileSectionRegex.MatchString(line) {
			inPkgs = true
			continue
		}
		if !inPkgs {
			continue
		}
		if pipfileBoundaryRegex.MatchString(line) {
			inPkgs = false
			continue
		}
		m := pipfileEntryRegex.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		ver := ""
		for _, v := range m[2:] {
			if v != "" && v != "*" {
				// Strip leading "==" if present (Pipfile shorthand).
				ver = strings.TrimPrefix(v, "==")
				if ver == "*" {
					ver = ""
				}
				break
			}
		}
		out = append(out, manifests.Dependency{
			Ecosystem: manifests.EcosystemPyPI,
			Name:      m[1],
			Version:   ver,
			Source:    manifests.SourceDirect,
		})
	}
	return out
}

// Pipfile.lock JSON shape: `default` and `develop` map name → entry,
// where entry has `"version": "==1.2.3"`.
type pipfileLockShape struct {
	Default map[string]struct {
		Version string `json:"version"`
	} `json:"default"`
	Develop map[string]struct {
		Version string `json:"version"`
	} `json:"develop"`
}

func parsePipfileLock(content []byte) ([]manifests.Dependency, error) {
	if len(bytes.TrimSpace(content)) == 0 {
		return nil, nil
	}
	var pl pipfileLockShape
	if err := json.Unmarshal(content, &pl); err != nil {
		return nil, fmt.Errorf("pip: parse Pipfile.lock: %w", err)
	}
	var out []manifests.Dependency
	for _, m := range []map[string]struct {
		Version string `json:"version"`
	}{pl.Default, pl.Develop} {
		for name, entry := range m {
			ver := strings.TrimPrefix(entry.Version, "==")
			out = append(out, manifests.Dependency{
				Ecosystem: manifests.EcosystemPyPI,
				Name:      name,
				Version:   ver,
				Source:    manifests.SourceTransitive,
			})
		}
	}
	return out, nil
}

// pyproject.toml has two shapes:
//
//   PEP 621:    [project]
//               dependencies = ["requests>=2.0", "click==8.1.3"]
//
//   Poetry:     [tool.poetry.dependencies]
//               requests = "^2.31.0"
//               click = "8.1.3"
//
// We handle both with regex sweeps over the file (no full TOML
// parser pulled in — keeps the dep tree small and is sufficient for
// the "extract name@version" goal). Lines outside the relevant
// section are ignored.
var (
	pep621Section          = regexp.MustCompile(`^\s*\[project\]\s*$`)
	pep621OptionalSection  = regexp.MustCompile(`^\s*\[project\.optional-dependencies\]\s*$`)
	pep621DepListStart     = regexp.MustCompile(`^\s*dependencies\s*=\s*\[`)
	pep621DepListItem      = regexp.MustCompile(`"\s*([A-Za-z0-9._\-]+)(?:\[[^\]]*\])?\s*(==|>=|<=|~=|!=|>|<|===)?\s*([A-Za-z0-9._\-]*)\s*"`)
	poetryDepsSection      = regexp.MustCompile(`^\s*\[tool\.poetry(?:\.[a-z\-]+)?-?dependencies\]\s*$`)
	poetryDepsAltSection   = regexp.MustCompile(`^\s*\[tool\.poetry\.dependencies\]\s*$`)
	poetryDepsDevSection   = regexp.MustCompile(`^\s*\[tool\.poetry\.dev-dependencies\]\s*$`)
	poetryEntryRegex       = regexp.MustCompile(`^\s*([A-Za-z0-9._\-]+)\s*=\s*(?:"([^"]*)"|'([^']*)'|\{[^}]*version\s*=\s*"([^"]*)"\})`)
	tomlSectionBoundary    = regexp.MustCompile(`^\s*\[`)
)

func parsePyProject(content []byte) []manifests.Dependency {
	var (
		out          []manifests.Dependency
		inProject    bool
		inPep621Deps bool
		inPoetry     bool
		scanner      = bufio.NewScanner(bytes.NewReader(content))
		seen         = map[string]bool{}
	)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024*16)
	for scanner.Scan() {
		line := scanner.Text()

		switch {
		case pep621Section.MatchString(line):
			inProject = true
			inPoetry = false
			continue
		case pep621OptionalSection.MatchString(line):
			inProject = true
			inPoetry = false
			continue
		case poetryDepsSection.MatchString(line) || poetryDepsAltSection.MatchString(line) || poetryDepsDevSection.MatchString(line):
			inPoetry = true
			inProject = false
			continue
		case tomlSectionBoundary.MatchString(line) && !pep621Section.MatchString(line) && !pep621OptionalSection.MatchString(line) &&
			!poetryDepsSection.MatchString(line) && !poetryDepsAltSection.MatchString(line) && !poetryDepsDevSection.MatchString(line):
			// Some other section header — leave both modes.
			inProject = false
			inPoetry = false
			inPep621Deps = false
		}

		if inProject && pep621DepListStart.MatchString(line) {
			inPep621Deps = true
		}
		if inPep621Deps {
			for _, m := range pep621DepListItem.FindAllStringSubmatch(line, -1) {
				ver := ""
				if len(m) >= 4 && m[2] == "==" {
					ver = m[3]
				}
				addDep(&out, seen, m[1], ver, manifests.SourceDirect)
			}
			if strings.Contains(line, "]") {
				inPep621Deps = false
			}
		}

		if inPoetry {
			if m := poetryEntryRegex.FindStringSubmatch(line); m != nil {
				if strings.EqualFold(m[1], "python") {
					continue // skip the python-version pin
				}
				ver := ""
				for _, v := range m[2:] {
					if v != "" {
						ver = strings.TrimLeft(v, "^~>=<! ")
						break
					}
				}
				addDep(&out, seen, m[1], ver, manifests.SourceDirect)
			}
		}
	}
	return out
}

// poetry.lock: TOML with repeated [[package]] tables. Each has
// `name = "..."` and `version = "..."`.
var (
	poetryLockPkgHeader = regexp.MustCompile(`^\s*\[\[package\]\]\s*$`)
	poetryLockNameLine  = regexp.MustCompile(`^\s*name\s*=\s*"([^"]+)"`)
	poetryLockVerLine   = regexp.MustCompile(`^\s*version\s*=\s*"([^"]+)"`)
)

func parsePoetryLock(content []byte) []manifests.Dependency {
	var (
		out      []manifests.Dependency
		inPkg    bool
		curName  string
		scanner  = bufio.NewScanner(bytes.NewReader(content))
	)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024*16)
	for scanner.Scan() {
		line := scanner.Text()
		if poetryLockPkgHeader.MatchString(line) {
			inPkg = true
			curName = ""
			continue
		}
		if !inPkg {
			continue
		}
		if m := poetryLockNameLine.FindStringSubmatch(line); m != nil {
			curName = m[1]
			continue
		}
		if curName != "" {
			if m := poetryLockVerLine.FindStringSubmatch(line); m != nil {
				out = append(out, manifests.Dependency{
					Ecosystem: manifests.EcosystemPyPI,
					Name:      curName,
					Version:   m[1],
					Source:    manifests.SourceTransitive,
				})
				inPkg = false
				curName = ""
			}
		}
	}
	return out
}

// setup.py: regex over the install_requires list. Best-effort.
var (
	setupPyInstallRegex = regexp.MustCompile(`(?s)install_requires\s*=\s*\[([^\]]*)\]`)
	setupPyEntryRegex   = regexp.MustCompile(`['"]([A-Za-z0-9._\-]+)(?:\[[^\]]*\])?\s*(?:(==|>=|<=|~=|!=|>|<|===)\s*([A-Za-z0-9._\-]+))?['"]`)
)

func parseSetupPy(content []byte) []manifests.Dependency {
	m := setupPyInstallRegex.FindSubmatch(content)
	if m == nil {
		return nil
	}
	body := string(m[1])
	var out []manifests.Dependency
	for _, e := range setupPyEntryRegex.FindAllStringSubmatch(body, -1) {
		ver := ""
		if len(e) >= 4 && e[2] == "==" {
			ver = e[3]
		}
		out = append(out, manifests.Dependency{
			Ecosystem: manifests.EcosystemPyPI,
			Name:      e[1],
			Version:   ver,
			Source:    manifests.SourceDirect,
		})
	}
	return out
}

func addDep(out *[]manifests.Dependency, seen map[string]bool, name, version string, src manifests.SourceKind) {
	key := name + "@" + version
	if seen[key] {
		return
	}
	seen[key] = true
	*out = append(*out, manifests.Dependency{
		Ecosystem: manifests.EcosystemPyPI,
		Name:      name,
		Version:   version,
		Source:    src,
	})
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
