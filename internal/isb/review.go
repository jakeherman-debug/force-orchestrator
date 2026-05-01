package isb

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
// 'isb' that is currently active (active_until = ''). Anti-cheat: a
// rule body without a FleetRules row is NOT active by design (per
// docs/roadmap.md § D4).
type FleetRulesGate func(ruleID string) (active bool, severityOverride Severity, ok bool)

// ReviewInput describes one file to be reviewed.
type ReviewInput struct {
	Path   string // file path, used for both Finding.Path and rule scope checks
	Source string // raw source text
}

// ReviewResult is the verdict surface for one ISB scan.
type ReviewResult struct {
	Findings  []Finding         // every finding (active rule + bypass-applied)
	Malformed []MalformedBypass // any malformed bypass comments
	HasBlock  bool              // true iff any finding has SeverityBlock and is not overridden
}

// ReviewFiles runs every active rule against every input and returns
// the aggregated result. Bypass comments are matched per file: a
// // ISB-BYPASS comment downgrades a finding from block→advisory and
// the finding's Severity is rewritten in-place to SeverityAdvise.
//
// The gate function decides which rules are active. ALL findings from
// inactive rules are dropped — the rule body is technically running
// but the verdict is suppressed; per docs/roadmap.md § D4 anti-cheat.
func ReviewFiles(gate FleetRulesGate, inputs []ReviewInput) ReviewResult {
	var res ReviewResult
	rules := All()

	for _, in := range inputs {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, in.Path, in.Source, parser.ParseComments)
		if err != nil {
			res.Findings = append(res.Findings, Finding{
				RuleID:   "ISB-PARSE-ERROR",
				Severity: SeverityAdvise,
				Path:     in.Path,
				Line:     0,
				Message:  fmt.Sprintf("ISB could not parse %s: %v", in.Path, err),
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
			findings := r.Check(file, in.Path, in.Source, nil)
			for _, f := range findings {
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

	// Malformed bypasses are themselves a hard reject (ISB-BYPASS-MALFORMED).
	for _, m := range res.Malformed {
		res.Findings = append(res.Findings, Finding{
			RuleID:   "ISB-BYPASS-MALFORMED",
			Severity: SeverityBlock,
			Path:     "",
			Line:     m.Line,
			Message:  fmt.Sprintf("malformed ISB-BYPASS comment: %s — bypass requires `// ISB-BYPASS: AUDIT-NNN <reason>` with reason >= 10 chars", m.Detail),
		})
		res.HasBlock = true
	}
	return res
}

// SetFileSetForRules is the production hook that threads the parser's
// fileset down to internal/isb/rules so positionLine() resolves.
// Mirror of internal/bos's SetFileSetForRules; the rules package's
// init() overrides the default no-op.
var SetFileSetForRules = func(_ *token.FileSet) {
	// Default no-op — overridden by the rules package's init() so the
	// rule library's positionLine() helper can resolve.
}

// LoadFromDisk reads a list of file paths and returns ReviewInputs.
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
// in FleetRules under category='isb'; a rule is active iff its row
// exists with active_until = ''.
func DBFleetRulesGate(db *sql.DB) FleetRulesGate {
	return func(ruleID string) (bool, Severity, bool) {
		var sev string
		err := db.QueryRow(`
			SELECT IFNULL(content, '')
			FROM FleetRules
			WHERE rule_key = ? AND category = 'isb' AND active_until = ''
			ORDER BY version DESC LIMIT 1`, ruleID).Scan(&sev)
		if err != nil {
			return false, "", true
		}
		// content is the rule metadata blob; for Phase 2 we don't
		// override severity from the row — the Go-side rule's
		// Severity() is authoritative until a future phase plumbs
		// severity-on-promotion.
		return true, "", true
	}
}

// AbsPath rebases a relative path against root if root is non-empty.
func AbsPath(root, p string) string {
	if root == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(root, p)
}
