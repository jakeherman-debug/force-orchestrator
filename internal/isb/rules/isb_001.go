package rules

import (
	"context"
	"go/ast"
	"go/types"
	"strings"

	"force-orchestrator/internal/isb"
	"force-orchestrator/internal/isb/scanners"
)

// ISB-001 — HardcodedSecretPatterns
//
// AUDIT-055 anchor. Detects credential-shaped literals (GitHub PATs,
// basic-auth URLs, AWS keys, ...) committed inline in source. The
// primary detector is the vendored gitleaks library
// (internal/isb/scanners.RunGitleaks); the regex fallback in
// scanners/regex_patterns.go is belt-and-suspenders for the most
// common ghp_/gho_/basic-auth shapes.
//
// Anti-cheat: severity=advise at launch (no block-default for new
// rules per docs/roadmap.md § D4). Promotion to block happens via
// FleetRules promotion after 30 clean firings.
//
// Deterministic-fallback note: this rule does NOT use the LLM layer.
// Both gitleaks (cached default-config detector) and the regex
// fallback are pure in-memory checks; the per-task cost is sub-ms.
type isb001 struct{}

func (isb001) ID() string             { return "ISB-001" }
func (isb001) CLAUDEMDAnchor() string { return "AUDIT-055 hardcoded secrets" }
func (isb001) Severity() isb.Severity { return isb.SeverityAdvise }

func (isb001) Check(file *ast.File, path, source string, _ *types.Info) []isb.Finding {
	if source == "" {
		return nil
	}
	var out []isb.Finding

	// Primary: gitleaks library scan.
	hits, err := scanners.RunGitleaks(context.Background(), map[string]string{path: source})
	if err == nil {
		for _, h := range hits {
			out = append(out, isb.Finding{
				RuleID:   "ISB-001",
				Severity: isb.SeverityAdvise,
				Path:     path,
				Line:     h.Line,
				Message:  "ISB-001: hardcoded secret pattern detected (" + h.RuleID + "): " + h.Message,
			})
		}
	}

	// Belt-and-suspenders: regex fallback for the highest-leverage
	// shapes (GitHub PATs, basic-auth URLs).
	for _, p := range scanners.HardcodedSecretLikePatterns {
		// Find all line-precise matches.
		matches := p.Re.FindAllStringIndex(source, -1)
		for _, m := range matches {
			line := lineFromOffset(source, m[0])
			// Avoid duplicating a gitleaks hit on the same line.
			if alreadyReportedAt(out, line) {
				continue
			}
			out = append(out, isb.Finding{
				RuleID:   "ISB-001",
				Severity: isb.SeverityAdvise,
				Path:     path,
				Line:     line,
				Message:  "ISB-001: " + p.Message,
			})
		}
	}

	// Defensive: even with no source-text hits, surface a hint if a
	// `var` decl includes a string literal whose name contains
	// "token"/"secret" — operators occasionally inline test fixtures.
	// Skipped if file is a _test.go (false-positive heavy).
	if !strings.HasSuffix(path, "_test.go") {
		ast.Inspect(file, func(n ast.Node) bool {
			vs, ok := n.(*ast.ValueSpec)
			if !ok {
				return true
			}
			for _, name := range vs.Names {
				lower := strings.ToLower(name.Name)
				if !(strings.Contains(lower, "token") || strings.Contains(lower, "secret") || strings.Contains(lower, "password") || strings.Contains(lower, "apikey")) {
					continue
				}
				for _, val := range vs.Values {
					bl, ok := val.(*ast.BasicLit)
					if !ok || bl.Kind.String() != "STRING" {
						continue
					}
					if len(bl.Value) < 12 { // exclude trivial values like ""
						continue
					}
					out = append(out, isb.Finding{
						RuleID:   "ISB-001",
						Severity: isb.SeverityAdvise,
						Path:     path,
						Line:     positionLine(name),
						Message:  "ISB-001: variable named like a credential (" + name.Name + ") has an inline string literal — store secrets in env/keystore, never in source",
					})
				}
			}
			return true
		})
	}

	return out
}

// lineFromOffset returns the 1-indexed line number of the byte offset
// in the source string.
func lineFromOffset(source string, off int) int {
	if off < 0 || off > len(source) {
		return 0
	}
	line := 1
	for i := 0; i < off; i++ {
		if source[i] == '\n' {
			line++
		}
	}
	return line
}

// alreadyReportedAt skips dup findings on the same line within one
// rule pass.
func alreadyReportedAt(findings []isb.Finding, line int) bool {
	for _, f := range findings {
		if f.Line == line {
			return true
		}
	}
	return false
}

func init() { isb.Register(isb001{}) }
