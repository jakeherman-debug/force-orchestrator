// Package maven parses Java/Kotlin manifests:
//
//   - pom.xml — Maven POM. We use encoding/xml to walk the
//     <dependencies>/<dependency> tree. Property substitution and
//     parent-pom inheritance are NOT resolved (out of P0 scope); we
//     surface ${var} versions verbatim with empty Version and let the
//     SUPPLY rules decide whether to advise on unresolved entries.
//
//   - build.gradle — Groovy DSL. Regex-shaped over the standard
//     `implementation 'group:name:version'` and
//     `implementation("group:name:version")` forms.
//
//   - build.gradle.kts — Kotlin DSL. Same regex shapes; Kotlin uses
//     parens, so the same matchers cover both.
package maven

import (
	"bufio"
	"bytes"
	"encoding/xml"
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

func (Parser) Ecosystem() manifests.Ecosystem { return manifests.EcosystemMaven }

func (Parser) Detect(path string) bool {
	switch filepath.Base(path) {
	case "pom.xml", "build.gradle", "build.gradle.kts":
		return true
	}
	return false
}

func (p Parser) Parse(path string, content []byte) ([]manifests.Dependency, error) {
	switch filepath.Base(path) {
	case "pom.xml":
		return parsePom(content)
	case "build.gradle", "build.gradle.kts":
		return parseGradle(content), nil
	}
	return nil, fmt.Errorf("maven: unsupported file %q", path)
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

// pomShape is the minimal slice of the Maven POM we need.
type pomShape struct {
	XMLName      xml.Name      `xml:"project"`
	Dependencies pomDepWrapper `xml:"dependencies"`
}

type pomDepWrapper struct {
	Deps []pomDep `xml:"dependency"`
}

type pomDep struct {
	GroupID    string `xml:"groupId"`
	ArtifactID string `xml:"artifactId"`
	Version    string `xml:"version"`
	Scope      string `xml:"scope"`
}

func parsePom(content []byte) ([]manifests.Dependency, error) {
	if len(bytes.TrimSpace(content)) == 0 {
		return nil, nil
	}
	var p pomShape
	if err := xml.Unmarshal(content, &p); err != nil {
		return nil, fmt.Errorf("maven: parse pom.xml: %w", err)
	}
	var out []manifests.Dependency
	for _, d := range p.Dependencies.Deps {
		if d.GroupID == "" || d.ArtifactID == "" {
			continue
		}
		ver := d.Version
		// Verbatim ${property} versions: leave Version="" so SUPPLY
		// rules can flag the unresolved entry without an inaccurate
		// version pin.
		if strings.HasPrefix(ver, "${") {
			ver = ""
		}
		out = append(out, manifests.Dependency{
			Ecosystem: manifests.EcosystemMaven,
			Name:      d.GroupID + ":" + d.ArtifactID,
			Version:   ver,
			Source:    manifests.SourceDirect,
		})
	}
	return out, nil
}

// gradleDepRegex matches the standard gradle-DSL forms:
//
//   implementation 'group:artifact:version'
//   implementation "group:artifact:version"
//   implementation('group:artifact:version')
//   implementation("group:artifact:version")
//   testImplementation 'group:artifact:version'
//   api  / compileOnly / runtimeOnly / annotationProcessor / kapt
//
// We accept the dependency-config keyword set most projects use and
// the typical surrounding shapes. Map / parameter-style declarations
// (`group: 'foo', name: 'bar', version: 'baz'`) are matched by the
// alternate gradleMapRegex below.
var (
	gradleDepRegex = regexp.MustCompile(`(?m)\b(?:implementation|api|compile|compileOnly|runtimeOnly|testImplementation|testCompile|annotationProcessor|kapt|kaptTest|kaptAndroidTest|debugImplementation|releaseImplementation)\s*\(?\s*['"]([^:'"]+):([^:'"]+):([^'"\s)]+)['"]`)
	gradleMapRegex = regexp.MustCompile(`(?m)\b(?:implementation|api|compile|compileOnly|runtimeOnly|testImplementation|testCompile|annotationProcessor|kapt)\s*\(?\s*group\s*:\s*['"]([^'"]+)['"]\s*,\s*name\s*:\s*['"]([^'"]+)['"]\s*,\s*version\s*:\s*['"]([^'"]+)['"]`)
)

func parseGradle(content []byte) []manifests.Dependency {
	var out []manifests.Dependency
	src := string(content)
	// Strip line and block comments to keep regex matches honest.
	src = stripGradleComments(src)
	seen := map[string]bool{}
	for _, m := range gradleDepRegex.FindAllStringSubmatch(src, -1) {
		name := m[1] + ":" + m[2]
		ver := m[3]
		key := name + "@" + ver
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, manifests.Dependency{
			Ecosystem: manifests.EcosystemMaven,
			Name:      name,
			Version:   ver,
			Source:    manifests.SourceDirect,
		})
	}
	for _, m := range gradleMapRegex.FindAllStringSubmatch(src, -1) {
		name := m[1] + ":" + m[2]
		ver := m[3]
		key := name + "@" + ver
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, manifests.Dependency{
			Ecosystem: manifests.EcosystemMaven,
			Name:      name,
			Version:   ver,
			Source:    manifests.SourceDirect,
		})
	}
	return out
}

// stripGradleComments removes // and /* ... */ comments. Kept simple
// — string-literal-aware comment stripping is overkill for the
// regex-based dep extraction.
func stripGradleComments(s string) string {
	var (
		out strings.Builder
		sc  = bufio.NewScanner(strings.NewReader(s))
	)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024*16)
	inBlock := false
	for sc.Scan() {
		line := sc.Text()
		if inBlock {
			if i := strings.Index(line, "*/"); i >= 0 {
				line = line[i+2:]
				inBlock = false
			} else {
				continue
			}
		}
		if i := strings.Index(line, "/*"); i >= 0 {
			j := strings.Index(line[i+2:], "*/")
			if j >= 0 {
				line = line[:i] + line[i+2+j+2:]
			} else {
				line = line[:i]
				inBlock = true
			}
		}
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return out.String()
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
