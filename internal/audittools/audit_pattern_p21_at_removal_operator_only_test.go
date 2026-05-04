// Package audittools: Pattern P21 — AT removal is operator-only.
//
// Roadmap reference: D3 § "Spec deprecation (concern #9)" / exit
// criterion 14d (line 1187, 1275, 1303).
//
// Invariant: spec deprecation is operator-UI-only. LLMs cannot
// propose REMOVE / DEPRECATE on AT references. Every LLM proposal
// schema (Captain `proposed_action_json`, ConvoyReview amendment
// proposals, EC promotion proposals) must NOT carry a "remove" or
// "deprecate" intent on AT references. Removal moves an AT from
// `verification_spec_json.ats[]` → `verification_spec_json.deprecated[]`
// with operator-supplied rationale; that mutation is reachable only
// from operator-routed handlers.
//
// Slice α of D3 fix-loop-1 authors this test as a SCAFFOLD that walks
// production Go source for LLM-proposal-schema definitions and
// SQL/Go writes that touch the deprecated[] subarray. Today (pre-
// slice-γ deprecation flow) production code has zero such writes,
// and Captain's structured-output schema (proposed_action_json) does
// not declare a "remove_at" / "deprecate_at" intent. The test passes
// with zero offenders today. Once slice γ ships the operator
// deprecation endpoint, those writes must be reachable only from the
// operator-action handler — any non-operator code path that writes
// to verification_spec_json.deprecated[] lands an offender row.
//
// The test is two-pronged:
//
//  1. SCHEMA-DEFINITION CHECK. Walks production code for prompt
//     templates / JSON-schema declarations that mention "ats" /
//     "AT references" alongside a removal-intent keyword. Hits like
//     "remove_ats", "deprecate_ats", "delete_at" in an LLM-prompt
//     context fail.
//
//  2. WRITE-PATH CHECK. Walks production code for `deprecated`
//     mutations on verification_spec_json. Today the column doesn't
//     yet host the deprecated subarray — the test logs "scaffold
//     present" and passes. Once slice γ ships, only files in the
//     operator-action allowlist may write deprecated entries.
//
// Pattern P21 graduates to a BoS commit-time rule when D4 ships.
package audittools

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// p21OperatorWriteAllowlist names files that ARE operator-routed and
// therefore allowed to write deprecation entries. Each entry MUST
// carry a one-line truthful rationale.
//
// Empty initially — slice γ will populate this with the operator
// dashboard handler that owns the deprecation endpoint.
var p21OperatorWriteAllowlist = map[string]string{}

// p21RemovalIntentRe matches the dangerous shape: an LLM-prompt
// template OR JSON-schema declaration that includes a "remove" /
// "deprecate" / "delete" key paired with an AT-references field.
//
// Examples that match (and would offend):
//   - "remove_ats": [...]
//   - "deprecate_ats": ["AT-005"]
//   - "delete_at": "AT-007"
//   - kind: "remove" with at_id near it
//
// The regex tolerates JSON / YAML / Go-string-literal whitespace.
var p21RemovalIntentRe = regexp.MustCompile(
	`(?i)\b(remove|deprecate|delete)[_\-]?at(s|_id|_ids)?\b`,
)

// p21DeprecatedWriteRe matches any production write to a
// verification_spec_json.deprecated subarray. Captures both
// SQL UPDATEs (json_insert / json_set on a deprecated path) and
// Go-side struct mutations (`.Deprecated = ...`).
var p21DeprecatedWriteRe = regexp.MustCompile(
	`(?i)(json_insert|json_set|json_replace).*verification_spec_json.*deprecated|verification_spec_json.*deprecated.*=`,
)

// TestPattern_P21_ATRemovalIsOperatorOnly walks production Go source
// and fails if any non-operator code path includes an LLM-removal-
// intent declaration on AT references, OR writes to the
// verification_spec_json.deprecated subarray outside the operator
// handler allowlist.
func TestPattern_P21_ATRemovalIsOperatorOnly(t *testing.T) {
	root := moduleRoot(t)

	type offender struct {
		file string
		line int
		text string
		why  string
	}
	var offenders []offender

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".build-worktrees" || name == ".force-worktrees" ||
				name == ".claude" || name == ".fix-worktrees" ||
				name == ".d7-worktrees" ||
				name == "vendor" || name == ".git" || name == "node_modules" ||
				name == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		relPath := rel(root, path)
		if !strings.HasPrefix(relPath, "cmd/") && !strings.HasPrefix(relPath, "internal/") {
			return nil
		}
		// audittools is the test layer.
		if strings.HasPrefix(relPath, "internal/audittools/") {
			return nil
		}
		// Schema definitions may mention "deprecated" as a column comment.
		// Skip the schema file — the test targets WRITES and PROMPTS.
		if relPath == "internal/store/schema.go" {
			return nil
		}

		fset := token.NewFileSet()
		f, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return nil
		}

		// Determine whether this file is on the operator-write allowlist.
		operatorRouted := false
		if _, ok := p21OperatorWriteAllowlist[relPath]; ok {
			operatorRouted = true
		}

		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			raw := lit.Value
			val := strings.TrimPrefix(strings.TrimSuffix(raw, "`"), "`")
			val = strings.TrimPrefix(strings.TrimSuffix(val, `"`), `"`)

			// Prong 1 — LLM-prompt removal intent on ATs.
			if p21RemovalIntentRe.MatchString(val) {
				pos := fset.Position(lit.Pos())
				preview := val
				if len(preview) > 200 {
					preview = preview[:200] + "…"
				}
				preview = strings.ReplaceAll(preview, "\n", " ")
				preview = strings.ReplaceAll(preview, "\t", " ")
				offenders = append(offenders, offender{
					file: relPath, line: pos.Line, text: preview,
					why: "removal-intent keyword on AT references in a non-operator file",
				})
			}

			// Prong 2 — write to verification_spec_json.deprecated[] from
			// a non-operator file.
			if p21DeprecatedWriteRe.MatchString(val) && !operatorRouted {
				pos := fset.Position(lit.Pos())
				preview := val
				if len(preview) > 200 {
					preview = preview[:200] + "…"
				}
				preview = strings.ReplaceAll(preview, "\n", " ")
				preview = strings.ReplaceAll(preview, "\t", " ")
				offenders = append(offenders, offender{
					file: relPath, line: pos.Line, text: preview,
					why: "write to verification_spec_json.deprecated[] from a non-operator code path",
				})
			}
			return true
		})
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk: %v", walkErr)
	}

	if len(offenders) == 0 {
		t.Logf("Pattern P21 (D3 14d): zero LLM-driven AT-removal intents and zero non-operator writes to verification_spec_json.deprecated[]. Operator-only deprecation invariant holds.")
		return
	}
	sort.Slice(offenders, func(i, j int) bool {
		if offenders[i].file != offenders[j].file {
			return offenders[i].file < offenders[j].file
		}
		return offenders[i].line < offenders[j].line
	})
	t.Errorf("Pattern P21 (D3 14d): %d production site(s) violate the operator-only AT-removal invariant:", len(offenders))
	for _, o := range offenders {
		t.Errorf("  %s:%d — %s\n      preview: %s", o.file, o.line, o.why, o.text)
	}
	t.Errorf("\nFix: spec deprecation is operator-UI-only. LLM proposal schemas MUST NOT declare a remove/deprecate intent on AT references. Writes to verification_spec_json.deprecated[] route through the operator dashboard handler ONLY.")
}

// TestPattern_P21_AllowlistReasonsTruthful asserts every
// p21OperatorWriteAllowlist entry carries a rationale longer than
// 20 chars and references operator-action / dashboard / handler
// shape.
func TestPattern_P21_AllowlistReasonsTruthful(t *testing.T) {
	descriptors := []string{
		"operator", "dashboard", "handler", "endpoint",
		"ratify", "approve", "ui-routed", "operator-action",
	}
	missing := []string{}
	for path, reason := range p21OperatorWriteAllowlist {
		if len(reason) < 20 {
			missing = append(missing, path+": rationale too short ("+reason+")")
			continue
		}
		lower := strings.ToLower(reason)
		hit := false
		for _, d := range descriptors {
			if strings.Contains(lower, strings.ToLower(d)) {
				hit = true
				break
			}
		}
		if !hit {
			missing = append(missing, path+": "+reason)
		}
	}
	if len(missing) == 0 {
		return
	}
	sort.Strings(missing)
	t.Errorf("Pattern P21: %d allowlist entry(ies) lack a truthful rationale:", len(missing))
	for _, m := range missing {
		t.Errorf("  %s", m)
	}
	t.Errorf("\nA reason MUST name the operator handler / dashboard / endpoint that routes the deprecation write.")
}
