// Package audittools — Pattern P-StagingPromotionConfirm (D5.5 P5 fix-iter1).
//
// P-StagingPromotionConfirm enforces the "No post-hoc single→multi
// promotion" anti-cheat directive from docs/roadmap.md § Deliverable 5.5:
//
//	"No Commander single-stage → multi-stage promotion post-hoc without
//	 explicit operator confirmation. Otherwise the Commander could re-plan
//	 to hide intent drift. Audit Pattern P-StagingPromotionConfirm
//	 enforces."
//
// The convoy's `staging_mode` column is mutated by exactly one production
// helper: `store.SetConvoyStaging` (defined in internal/store/convoy.go).
// The convoy's mode at *creation* time is set inside the constructor
// (`store.CreateConvoy` / `store.CreateStagedConvoy`) and never flows
// through SetConvoyStaging — the constructors INSERT the row with the
// final value already in place.
//
// That makes SetConvoyStaging a strictly post-hoc mutator. Any future
// production caller of it would, by definition, be promoting a
// single-stage convoy to staged (or vice versa) AFTER the convoy has
// already started running. The roadmap forbids that without operator
// confirmation; the cleanest enforcement is to assert no production code
// calls it AT ALL, gated by an explicit allowlist that demands a
// justification including the operator-confirm predicate site.
//
// Today the allowlist is empty: SetConvoyStaging is exported only because
// the migration tests need to reach it. If a future deliverable needs to
// promote a convoy mid-flight, the path is:
//
//  1. Implement an OperatorConfirm predicate (e.g. dashboard endpoint
//     `POST /api/convoys/<id>/staging-mode` requiring an `AUDIT-NNN`
//     reference like the stage-bypass path).
//  2. Add the new caller's repo-relative path to
//     stagingPromotionConfirmAllowlist below WITH a `reason:` line
//     pointing at the operator-confirm site.
//  3. Re-run this test green.
//
// The shape mirrors P-StageGate (audit_pattern_p_stage_gate_test.go):
// AST walk over production .go, find every CallExpr with selector
// `store.SetConvoyStaging`, fail with file:line + the allowlist
// instruction unless the call site is on the allowlist.
//
// Scope decisions:
//   - Test files (*_test.go) excluded — internal/store/convoy_staged_test.go
//     legitimately exercises the helper.
//   - Skip-list mirrors the rest of audittools: .fix-worktrees,
//     .force-worktrees, .claude, .build-worktrees, vendor, .git,
//     node_modules, testdata.
//   - Only `store.SetConvoyStaging` (qualified selector) matches.
//     Same-package callers (i.e. inside internal/store/...) would call
//     `SetConvoyStaging` unqualified; we currently have none, but the
//     unqualified form is also caught below to remain robust if the
//     helper moves.
package audittools

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// stagingPromotionConfirmAllowlist names production files that contain a
// CallExpr to `store.SetConvoyStaging` (or unqualified `SetConvoyStaging`
// if the caller is inside the store package) but are legitimately exempt
// because they are preceded by an operator-confirm predicate. Each
// entry's reason MUST name the predicate site.
//
// At time of writing this allowlist is empty — SetConvoyStaging has zero
// production callers. The constants ARE referenced in production
// (CreateConvoy initializes the column to StagingModeSingle / strict),
// but those references are not via SetConvoyStaging.
var stagingPromotionConfirmAllowlist = map[string]string{
	// (path) : (reason — must name the operator-confirm predicate site)
}

// stagingPromotionConfirmTargetFunc is the function name we hunt for.
// When the call is qualified (`store.SetConvoyStaging`), we match the
// SelectorExpr's selector identifier. Same-package callers would use
// the bare identifier; the AST walk below handles both forms.
const stagingPromotionConfirmTargetFunc = "SetConvoyStaging"

// TestPattern_PStagingPromotionConfirm_NoUngatedSetConvoyStaging walks
// every production .go under internal/ and cmd/ and rejects any call to
// `store.SetConvoyStaging` that is not on stagingPromotionConfirmAllowlist.
//
// Background: the roadmap demands an audit pattern named
// "P-StagingPromotionConfirm" enforcing operator-confirm before any
// post-hoc staging_mode flip. The simplest enforcement is "no production
// caller exists." Future callers MUST justify via the allowlist; the
// reviewer at PR time confirms the allowlist entry's named operator-
// confirm predicate is real.
func TestPattern_PStagingPromotionConfirm_NoUngatedSetConvoyStaging(t *testing.T) {
	root := moduleRoot(t)

	type offender struct {
		path string
		line int
		why  string
	}
	var offenders []offender

	walkTargets := []string{"internal", "cmd"}
	for _, sub := range walkTargets {
		walkRoot := filepath.Join(root, sub)
		err := filepath.WalkDir(walkRoot, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				name := d.Name()
				if name == ".fix-worktrees" || name == ".force-worktrees" ||
					name == ".claude" || name == ".build-worktrees" ||
					name == "vendor" || name == ".git" ||
					name == "node_modules" || name == "testdata" {
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
			fset := token.NewFileSet()
			f, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				// Best-effort: skip files we can't parse rather than
				// fail the audit. Real production files always parse.
				return nil
			}

			relPath := rel(root, path)
			if reason, ok := stagingPromotionConfirmAllowlist[relPath]; ok {
				t.Logf("Pattern P-StagingPromotionConfirm: %s — promotion allowed (%s)", relPath, reason)
				return nil
			}

			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch fn := call.Fun.(type) {
				case *ast.SelectorExpr:
					// `store.SetConvoyStaging(...)` shape.
					if fn.Sel == nil || fn.Sel.Name != stagingPromotionConfirmTargetFunc {
						return true
					}
					pkgIdent, ok := fn.X.(*ast.Ident)
					if !ok {
						return true
					}
					// Tighten match to the `store` package selector — a
					// future hypothetical `someLocal.SetConvoyStaging`
					// would not be the same target.
					if pkgIdent.Name != "store" {
						return true
					}
				case *ast.Ident:
					// Same-package call (would only fire from inside
					// internal/store/... if a helper is added there).
					if fn.Name != stagingPromotionConfirmTargetFunc {
						return true
					}
				default:
					return true
				}
				offenders = append(offenders, offender{
					path: relPath,
					line: fset.Position(call.Pos()).Line,
					why: "calls store.SetConvoyStaging (post-hoc staging_mode mutator) without operator-confirm gate. " +
						"Add to stagingPromotionConfirmAllowlist with a reason naming the operator-confirm predicate site.",
				})
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", walkRoot, err)
		}
	}

	if len(offenders) == 0 {
		return
	}
	sort.Slice(offenders, func(i, j int) bool {
		if offenders[i].path != offenders[j].path {
			return offenders[i].path < offenders[j].path
		}
		return offenders[i].line < offenders[j].line
	})
	t.Errorf("Pattern P-StagingPromotionConfirm: %d ungated SetConvoyStaging call site(s):", len(offenders))
	for _, o := range offenders {
		t.Errorf("  %s:%d — %s", o.path, o.line, o.why)
	}
	t.Errorf("\nFix: precede the SetConvoyStaging call with an operator-confirm predicate (e.g. dashboard endpoint requiring AUDIT-NNN), then add the file to stagingPromotionConfirmAllowlist with a `reason:` line naming the predicate site.")
}
