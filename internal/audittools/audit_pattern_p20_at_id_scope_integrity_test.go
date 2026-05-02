// Package audittools: Pattern P20 — cross-convoy AT-id scope integrity.
//
// Roadmap reference: D3 § "Cross-convoy AT-id collisions (concern #8)" /
// exit criterion 14c (line 1182, 1274).
//
// Invariant: every code path that resolves an AT (Acceptance Test) must
// scope the lookup by the compound key (convoy_id, at_id), never by bare
// at_id. AT-ids are namespaced PER CONVOY — convoy 47's AT-005 is a
// different row than convoy 48's AT-005. A query with a bare
// `WHERE at_id = ?` predicate (no co-occurring convoy_id constraint) is
// a cross-convoy collision waiting to happen.
//
// Slice α of D3 fix-loop-1 authors this test as a SCAFFOLD that walks
// production Go source for SQL string literals containing the at_id
// column. Today (pre-slice-β AT resolver) production code has zero
// at_id query sites — the only at_id reference in the codebase is the
// schema definition itself (BountyBoard.spawning_at_id column + the
// matching ALTER). The test passes with zero offenders today. Once
// slice β / γ ships the AT resolver, every new query must be scoped
// or it lands an offender row in this test's failure output.
//
// The test is deliberately permissive about JSON-payload references
// (verification_spec_json contains AT entries, but those are JSON
// queries, not WHERE-clause predicates on a column called at_id). It
// fires only on `at_id` column-style predicates without a
// `convoy_id` neighbor in the same SQL statement.
//
// Pattern P20 graduates to a BoS commit-time rule when D4 ships,
// composing with the existing pattern test inventory.
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

// p20Allowlist names "<file>:<line>" entries where a bare-at_id query
// is acceptable. Each entry MUST carry a one-line truthful rationale
// per the CLAUDE.md allowlist-truthfulness invariant.
//
// Empty initially — slice β/γ resolver code MUST scope by
// (convoy_id, at_id). If a legitimate fleet-wide AT lookup arrives
// (e.g. v2 fleet-wide AT namespace), document it here with a
// reviewer-visible rationale.
var p20Allowlist = map[string]string{}

// p20BareAtIDRe matches the dangerous shape: `at_id` appearing as a
// SQL predicate (`= ?`, `IN (?)`, `= '...'`) without an immediate
// table-qualifier. The regex is intentionally lenient — false
// positives surface for human review, false negatives are the bug.
//
// Examples that match (and would offend if not co-scoped):
//   - WHERE at_id = ?
//   - AND at_id IN (?)
//   - WHERE at_id = ? AND status = 'active'
//
// Examples that don't match (legitimate non-predicate uses):
//   - bountyboard.spawning_at_id  (column qualifier; not the AT
//     resolver)
//   - "spawning_at_id"            (different column)
//   - // at_id is …               (comment text, no SQL syntax)
var p20BareAtIDRe = regexp.MustCompile(`(?i)\bat_id\s*(=|IN\s*\()`)

// p20ConvoyScopeRe matches a co-occurring convoy_id predicate in the
// same SQL string. Order doesn't matter — we just need both to appear
// in the same string literal.
var p20ConvoyScopeRe = regexp.MustCompile(`(?i)\bconvoy_id\s*=`)

// TestPattern_P20_ATIdScopeIntegrity walks production Go source under
// internal/ and cmd/, parses every string literal, and fails if any
// literal contains an at_id predicate without a co-occurring
// convoy_id predicate. The test is structurally a "compound-key
// scope" guardrail — it doesn't enforce HOW the resolver is built,
// only that AT lookups don't land cross-convoy by accident.
func TestPattern_P20_ATIdScopeIntegrity(t *testing.T) {
	root := moduleRoot(t)

	type offender struct {
		file string
		line int
		text string
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
		// The store schema defines BountyBoard.spawning_at_id as a column
		// — that's a column declaration, not a query. Skip.
		if relPath == "internal/store/schema.go" {
			return nil
		}
		// audittools/ is the test layer; skipping its own files keeps
		// the test self-referent and stable.
		if strings.HasPrefix(relPath, "internal/audittools/") {
			return nil
		}

		fset := token.NewFileSet()
		f, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return nil
		}

		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			val := lit.Value
			// Strip Go string-literal quotes.
			val = strings.TrimPrefix(strings.TrimSuffix(val, "`"), "`")
			val = strings.TrimPrefix(strings.TrimSuffix(val, `"`), `"`)
			if !p20BareAtIDRe.MatchString(val) {
				return true
			}
			// Allow column-qualifier shapes like `bountyboard.spawning_at_id`
			// — those are columns, not the AT resolver predicate. The
			// regex already excludes `spawning_at_id` (no `at_id\s*(=|IN`)
			// because the column name is `spawning_at_id`, but be defensive.
			if !p20ConvoyScopeRe.MatchString(val) {
				pos := fset.Position(lit.Pos())
				key := relPath
				if _, ok := p20Allowlist[key]; ok {
					return true
				}
				// Surface a single-line preview for the failure message.
				preview := val
				if len(preview) > 200 {
					preview = preview[:200] + "…"
				}
				preview = strings.ReplaceAll(preview, "\n", " ")
				preview = strings.ReplaceAll(preview, "\t", " ")
				offenders = append(offenders, offender{
					file: relPath, line: pos.Line, text: preview,
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
		// Today's expected state: zero offenders. The slice β/γ AT
		// resolver isn't shipped yet; once it lands, every query must
		// scope by (convoy_id, at_id).
		t.Logf("Pattern P20 (D3 14c): zero bare-at_id query sites. AT-id scope integrity invariant holds.")
		return
	}
	sort.Slice(offenders, func(i, j int) bool {
		if offenders[i].file != offenders[j].file {
			return offenders[i].file < offenders[j].file
		}
		return offenders[i].line < offenders[j].line
	})
	t.Errorf("Pattern P20 (D3 14c): %d production query site(s) reference at_id without a co-occurring convoy_id constraint. Cross-convoy AT collisions waiting to happen — scope by (convoy_id, at_id):", len(offenders))
	for _, o := range offenders {
		t.Errorf("  %s:%d — %s", o.file, o.line, o.text)
	}
	t.Errorf("\nFix: every AT lookup MUST scope by `WHERE convoy_id = ? AND at_id = ?`. Bare `WHERE at_id = ?` is forbidden — AT-005 in convoy 47 is a DIFFERENT row than AT-005 in convoy 48.")
}

// TestPattern_P20_AllowlistReasonsTruthful asserts every p20Allowlist
// entry carries a rationale longer than 20 chars. Mirrors the
// truthfulness check applied to P11 / P13 / P25.
func TestPattern_P20_AllowlistReasonsTruthful(t *testing.T) {
	missing := []string{}
	for path, reason := range p20Allowlist {
		if len(reason) < 20 {
			missing = append(missing, path+": rationale too short ("+reason+")")
		}
	}
	if len(missing) == 0 {
		return
	}
	sort.Strings(missing)
	t.Errorf("Pattern P20: %d allowlist entry(ies) lack a truthful rationale:", len(missing))
	for _, m := range missing {
		t.Errorf("  %s", m)
	}
	t.Errorf("\nA reason MUST name the file's role and explain why this query site is structurally exempt from compound-key scoping.")
}
