package bos

import (
	"database/sql"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

// FleetRulesGate is the function the reviewer calls to decide whether
// a registered Rule's body actually counts as "active." A rule is
// active iff its ID has a corresponding FleetRules row of category
// 'bos' that is currently active (active_until = ''). Anti-cheat: a
// rule body without a FleetRules row is NOT active by design (per
// docs/roadmap.md § D4).
//
// Production callers use the default DBFleetRulesGate; tests can
// inject a stub.
type FleetRulesGate func(ruleID string) (active bool, severityOverride Severity, ok bool)

// ReviewInput describes one file to be reviewed.
type ReviewInput struct {
	Path     string // file path, used for both Finding.Path and rule scope checks
	Source   string // raw source text
}

// ReviewResult is the verdict surface for one BoS scan.
type ReviewResult struct {
	Findings  []Finding         // every finding (active rule + bypass-applied)
	Malformed []MalformedBypass // any malformed bypass comments
	HasBlock  bool              // true iff any finding has SeverityBlock and is not overridden
}

// ReviewFiles runs every active rule against every input and returns
// the aggregated result. Bypass comments are matched per file: a
// // BOS-BYPASS comment downgrades a finding from block→advisory and
// the finding's Severity is rewritten in-place to SeverityAdvise.
//
// The gate function decides which rules are active. ALL findings from
// inactive rules are dropped — the rule body is technically running
// but the verdict is suppressed; per docs/roadmap.md § D4 anti-cheat
// "a rule whose check body exists but has no FleetRules row is NOT
// active; this is by design."
func ReviewFiles(gate FleetRulesGate, inputs []ReviewInput) ReviewResult {
	var res ReviewResult
	rules := All()

	for _, in := range inputs {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, in.Path, in.Source, parser.ParseComments)
		if err != nil {
			// Parse failure — surface as a synthetic finding so the
			// reviewer's verdict cannot silently green-light an
			// unparseable diff. This is an advise-severity finding;
			// callers can promote to block via a separate gate.
			res.Findings = append(res.Findings, Finding{
				RuleID:   "BOS-PARSE-ERROR",
				Severity: SeverityAdvise,
				Path:     in.Path,
				Line:     0,
				Message:  fmt.Sprintf("BoS could not parse %s: %v", in.Path, err),
			})
			continue
		}

		// Bypass comments must be threaded through the rules' fset
		// so positionLine() resolves correctly.
		SetFileSetForRules(fset)
		bypasses, malformed := ParseBypasses(fset, file)
		res.Malformed = append(res.Malformed, malformed...)

		for _, r := range rules {
			active, sevOverride, ok := gate(r.ID())
			if !ok || !active {
				continue
			}
			findings := r.Check(file, in.Path, nil)
			for _, f := range findings {
				// Apply bypass: a matching // BOS-BYPASS on the line
				// directly above downgrades to advise + records the
				// audit trail in the message.
				if bp := MatchBypass(bypasses, f.Line); bp != nil {
					f.Severity = SeverityAdvise
					f.Message = fmt.Sprintf("[BYPASSED %s: %s] %s", bp.AuditID, bp.Reason, f.Message)
				} else if sevOverride != "" {
					f.Severity = sevOverride
				}
				if f.Severity == SeverityBlock {
					res.HasBlock = true
				}
				res.Findings = append(res.Findings, f)
			}
		}
	}

	// Malformed bypasses are themselves a hard reject (BOS-BYPASS-MALFORMED).
	for _, m := range res.Malformed {
		res.Findings = append(res.Findings, Finding{
			RuleID:   "BOS-BYPASS-MALFORMED",
			Severity: SeverityBlock,
			Path:     "",
			Line:     m.Line,
			Message:  fmt.Sprintf("malformed BOS-BYPASS comment: %s — bypass requires `// BOS-BYPASS: AUDIT-NNN <reason>` with reason >= 10 chars", m.Detail),
		})
		res.HasBlock = true
	}
	return res
}

// SetFileSetForRules is the production hook that threads the parser's
// fileset down to internal/bos/rules so positionLine() resolves. Mirrors
// the test-only setFset in internal/bos/rules.
//
// Implemented in a separate file (review_fset.go) to avoid an import
// cycle: internal/bos/rules imports internal/bos for the Rule
// interface, so internal/bos cannot directly reach into rules; the
// indirection function lives at the rules-package level and is
// imported here as a function variable set during init.
var SetFileSetForRules = func(_ *token.FileSet) {
	// Default no-op — overridden by the rules package's init() so the
	// rule library's positionLine() helper can resolve. Tests that
	// only exercise the bos package without the rules package see
	// this default and get Line=0 in findings, which is acceptable.
}

// LoadFromDisk reads a list of file paths and returns ReviewInputs.
// Convenience for production callers; tests use ReviewInput literals.
func LoadFromDisk(paths []string) ([]ReviewInput, error) {
	inputs := make([]ReviewInput, 0, len(paths))
	for _, p := range paths {
		if !strings.HasSuffix(p, ".go") {
			continue
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", p, err)
		}
		inputs = append(inputs, ReviewInput{Path: p, Source: string(body)})
	}
	return inputs, nil
}

// DBFleetRulesGate is the production gate. It looks up the rule_id
// in FleetRules under category='bos'; a rule is active iff its row
// exists with active_until = ''.
func DBFleetRulesGate(db *sql.DB) FleetRulesGate {
	return func(ruleID string) (bool, Severity, bool) {
		var sev string
		err := db.QueryRow(`
			SELECT IFNULL(content, '')
			FROM FleetRules
			WHERE rule_key = ? AND category = 'bos' AND active_until = ''
			ORDER BY version DESC LIMIT 1`, ruleID).Scan(&sev)
		if err != nil {
			return false, "", true
		}
		// content is the rule metadata blob (we store severity as a
		// JSON-ish field). For Phase 1 we don't override severity from
		// the row — the Go-side rule's Severity() is authoritative
		// until D4 Phase 2 plumbs through severity-on-promotion. We
		// simply gate active vs. inactive.
		return true, "", true
	}
}

// AbsPath rebases a relative path against root if root is non-empty.
// Used so production callers can hand the reviewer absolute paths.
func AbsPath(root, p string) string {
	if root == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(root, p)
}
