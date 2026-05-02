// Package rules: license_matrix loader for SUPPLY-004.
//
// The matrix lives in license_matrix.yaml as a hand-authored,
// PR-reviewable table keyed by the repo's declared SPDX id. We embed
// it via Go's `embed` package so the matrix ships with the binary —
// reading from disk at runtime would be a deployment-fragility hazard
// (the matrix could drift between the binary and its working tree).
//
// Anti-cheat (per docs/roadmap.md § D5 SUPPLY-004): NO LLM decides
// license compatibility. The matrix is the only authority. Pairs not
// declared in the matrix → advise-mode + operator review. We never
// auto-allow or auto-deny a pair that's not explicitly listed.
package rules

import (
	"embed"
	"fmt"

	"gopkg.in/yaml.v3"
)

//go:embed license_matrix.yaml
var licenseMatrixFS embed.FS

// licenseEntry is one row of the license matrix: the deps' SPDX ids
// allowed (no finding) or denied (advise-mode finding) under the
// keyed repo license. Both lists are case-sensitive — SPDX ids have
// canonical casing (e.g. "Apache-2.0", not "APACHE-2.0").
type licenseEntry struct {
	Allowed []string `yaml:"allowed"`
	Deny    []string `yaml:"deny"`
}

// LoadLicenseMatrix parses the embedded license_matrix.yaml and
// returns a map keyed by the repo license SPDX id. The matrix is
// shipped with the binary (embed.FS) so this never touches disk at
// runtime. Errors only on a malformed YAML — a successful parse always
// returns a non-nil (possibly empty) map.
func LoadLicenseMatrix() (map[string]licenseEntry, error) {
	body, err := licenseMatrixFS.ReadFile("license_matrix.yaml")
	if err != nil {
		return nil, fmt.Errorf("LoadLicenseMatrix: read embedded yaml: %w", err)
	}
	out := map[string]licenseEntry{}
	if err := yaml.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("LoadLicenseMatrix: parse yaml: %w", err)
	}
	return out, nil
}

// CheckLicenseCompatibility resolves a (repoLicense, depLicense) pair
// against the matrix. Returns three logical outcomes:
//
//	allowed=true,  denied=false  — dep's license is in repo's `allowed`
//	                               list. No finding.
//	allowed=false, denied=true   — dep's license is in repo's `deny`
//	                               list. Caller emits an advise-mode
//	                               finding citing the matrix entry.
//	allowed=false, denied=false  — pair NOT in matrix (either repo
//	                               license unknown, or dep license not
//	                               listed under repo's row). Caller
//	                               emits an advise-mode finding asking
//	                               for operator review. NEVER
//	                               auto-allow.
//
// Both arguments are matched verbatim against the matrix keys (SPDX
// canonical casing). Empty inputs always fall through to the
// advise-mode path — see the rule for the empty-handling logic.
func CheckLicenseCompatibility(matrix map[string]licenseEntry, repoLicense, depLicense string) (allowed, denied bool) {
	if repoLicense == "" || depLicense == "" {
		return false, false
	}
	entry, ok := matrix[repoLicense]
	if !ok {
		return false, false
	}
	for _, a := range entry.Allowed {
		if a == depLicense {
			return true, false
		}
	}
	for _, d := range entry.Deny {
		if d == depLicense {
			return false, true
		}
	}
	return false, false
}
