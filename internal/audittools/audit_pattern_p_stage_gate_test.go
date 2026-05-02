// Package audittools — Pattern P-StageGate (D5.5 P2 γ, full AST enforcer).
//
// P-StageGate enforces the "No astromech pre-staging" anti-cheat
// directive from docs/roadmap.md § Deliverable 5.5. The directive:
//
//	"Astromechs cannot hold a worktree on a `Pending` stage. Pattern
//	 P-StageGate (AST audit) walks the dispatch path and rejects any
//	 code path that would claim a Pending-stage task."
//
// This file ships the full enforcement (D5.5 P2 γ — Wave 2 slice γ).
// The skeleton from P1 (which only checked package wiring) has been
// promoted to a structural test that asserts:
//
//  1. internal/stagegate package wiring is in place (carried forward
//     from P1 — the convoy-stage-watch dog still needs to exist).
//  2. Every function in internal/store/tasks.go whose name starts with
//     "Claim" — i.e. every dispatch-time entry point — must include
//     the stage_id gating predicate. Concretely:
//     `stage_id IS NULL` (legacy / single-mode preserved) OR an
//     EXISTS subquery against ConvoyStages that filters out Pending.
//     This is the regression test that pins gating at the SQL level.
//  3. No production file under internal/ or cmd/ contains a fresh
//     SELECT that issues "claim-shaped" SQL against BountyBoard
//     (status = 'Pending' + UPDATE … status = 'Locked') without going
//     through the gated central path. Such a file would silently
//     re-introduce the pre-staging cheat.
//
// Scope decisions (documented for future maintainers):
//
//   - The audit is targeted at CLAIM-time paths. Read-only listings
//     (dashboard, GetBounty, ListConvoys, etc.) are not in scope. The
//     "claim shape" detector below requires both `status = 'Pending'`
//     AND a sibling `status = 'Locked'` exec, which only the dispatch
//     paths produce.
//
//   - Test files (*_test.go) are excluded — they intentionally exercise
//     the gating with hand-crafted queries.
//
//   - The skip-list mirrors the rest of audittools: .fix-worktrees,
//     .force-worktrees, .claude, .build-worktrees, vendor, .git,
//     node_modules, testdata.
//
//   - When a file legitimately needs to query BountyBoard at claim-time
//     without stage gating (e.g. a maintenance sweep that operates on
//     all rows regardless of stage), it must be added to
//     stageGateBypassAllowlist below WITH a truthful reason. Each entry
//     is reviewed at PR time; the bar is "this query is structurally
//     not a dispatch path."
package audittools

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// stageGateBypassAllowlist names files that contain a CLAIM-shaped SQL
// query (status = 'Pending' AND status = 'Locked' UPDATE) but are
// legitimately exempt from the stage_id gate. Each entry pairs a
// repo-relative path with a justification. The justification is
// printed in the test failure message if the entry ever drops out of
// sync with the file content, and is reviewed at PR time.
//
// At time of writing this allowlist is empty — the only claim-shaped
// SQL in the tree lives in internal/store/tasks.go, which IS gated.
var stageGateBypassAllowlist = map[string]string{
	// (path) : (reason)
}

// claimFunctionPrefix is the convention that marks a dispatch entry
// point in internal/store/tasks.go: any top-level func whose name
// starts with this prefix is a claim-time path and must contain the
// stage_id gating predicate.
const claimFunctionPrefix = "Claim"

// stageGateRequiredFragment is the literal SQL fragment a gated claim
// query must contain. The full expression is `stage_id IS NULL OR
// EXISTS (SELECT 1 FROM ConvoyStages cs WHERE cs.id =
// BountyBoard.stage_id AND cs.status != 'Pending')` but the fragment
// `stage_id IS NULL` is the load-bearing token: it short-circuits the
// JOIN for legacy rows AND signals that the author thought about the
// stage column at all. The audit checks for this exact fragment.
const stageGateRequiredFragment = "stage_id IS NULL"

// TestPattern_PStageGate_PackageWiringPresent is the surface check
// carried forward from the P1 skeleton. The full claim-time enforcer
// lives in TestPattern_PStageGate_ClaimBountyHasStageFilter and
// TestPattern_PStageGate_NoUngatedClaimSQL below.
func TestPattern_PStageGate_PackageWiringPresent(t *testing.T) {
	root := moduleRoot(t)

	// ── 1) stagegate package files ────────────────────────────────────
	stagegateDir := filepath.Join(root, "internal", "stagegate")
	for _, want := range []string{
		"gate.go",
		"soak_minutes.go",
		"operator_confirm.go",
		"null_gate.go",
		"compound.go",
		"baseline.go",
	} {
		if _, err := os.Stat(filepath.Join(stagegateDir, want)); err != nil {
			t.Errorf("Pattern P-StageGate: missing %s in internal/stagegate", want)
		}
	}

	// ── 2) Required exported names from internal/stagegate/gate.go ───
	gateBody := mustReadFile(t, filepath.Join(stagegateDir, "gate.go"))
	for _, want := range []string{
		"type Gate interface",
		"type Registry struct",
		"func NewRegistry()",
		"func (r *Registry) Register(",
		"func (r *Registry) Lookup(",
		"func (r *Registry) EvaluateGateConfig(",
		"var ErrPending",
		"const MaxNestingDepth",
		"type StageContext struct",
	} {
		if !strings.Contains(gateBody, want) {
			t.Errorf("Pattern P-StageGate: gate.go missing required symbol %q", want)
		}
	}

	// ── 3) Required leaf gates ──────────────────────────────────────
	for _, pair := range []struct {
		file    string
		typeStr string
	}{
		{"soak_minutes.go", `"soak_minutes"`},
		{"operator_confirm.go", `"operator_confirm"`},
		{"null_gate.go", `"null"`},
		{"compound.go", `"all_of"`},
		{"compound.go", `"any_of"`},
	} {
		body := mustReadFile(t, filepath.Join(stagegateDir, pair.file))
		if !strings.Contains(body, pair.typeStr) {
			t.Errorf("Pattern P-StageGate: %s missing gate Type() %s", pair.file, pair.typeStr)
		}
	}

	// ── 4) RegisterBaselineGates wires all five ────────────────────
	baselineBody := mustReadFile(t, filepath.Join(stagegateDir, "baseline.go"))
	for _, want := range []string{"SoakMinutes", "OperatorConfirm", "NullGate", "AllOf", "AnyOf"} {
		if !strings.Contains(baselineBody, want) {
			t.Errorf("Pattern P-StageGate: RegisterBaselineGates missing %s", want)
		}
	}

	// ── 5) Dog file present + dispatched ────────────────────────────
	dogFile := filepath.Join(root, "internal", "agents", "dogs_convoy_stage_watch.go")
	if _, err := os.Stat(dogFile); err != nil {
		t.Errorf("Pattern P-StageGate: missing %s", dogFile)
	}
	dogsBody := mustReadFile(t, filepath.Join(root, "internal", "agents", "dogs.go"))
	if !strings.Contains(dogsBody, `"convoy-stage-watch"`) {
		t.Error("Pattern P-StageGate: dogs.go missing convoy-stage-watch registration")
	}
	if !strings.Contains(dogsBody, "dogConvoyStageWatch(") {
		t.Error("Pattern P-StageGate: dogs.go runDog dispatch missing dogConvoyStageWatch call")
	}
}

// TestPattern_PStageGate_ClaimBountyHasStageFilter is the AST-level
// regression that pins the stage_id gate inside ClaimBounty (and every
// sibling Claim* dispatch entry point). It walks tasks.go, finds every
// top-level FuncDecl whose name starts with "Claim", extracts the
// string-literal argument to db.QueryRow (the SQL), and asserts the
// SQL contains the required stage-gate fragment.
//
// This is the "is the gate AT the SQL?" test. If a future edit
// accidentally drops the gate from one Claim* function, this test
// fails and names the offender.
func TestPattern_PStageGate_ClaimBountyHasStageFilter(t *testing.T) {
	root := moduleRoot(t)
	tasksPath := filepath.Join(root, "internal", "store", "tasks.go")

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, tasksPath, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", tasksPath, err)
	}

	type checked struct {
		name       string
		sawSelect  bool // did we find a SELECT against BountyBoard?
		sawGate    bool // does the SELECT contain stage_id IS NULL?
		sawPending bool // does the SELECT filter status = 'Pending'?
		hasUpdate  bool // does the func also issue UPDATE … 'Locked'?
	}
	var results []checked

	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		name := fn.Name.Name
		if !strings.HasPrefix(name, claimFunctionPrefix) {
			continue
		}
		// Skip the helper that has nothing to do with claim dispatch.
		// Right now there are none; future-proof against e.g.
		// "ClaimMetadataPreview".
		if fn.Body == nil {
			continue
		}
		c := checked{name: name}
		ast.Inspect(fn, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			// SQL string literals in this codebase use backticks.
			if !strings.HasPrefix(lit.Value, "`") {
				return true
			}
			body := lit.Value
			if strings.Contains(body, "FROM BountyBoard") && strings.Contains(body, "SELECT") {
				c.sawSelect = true
				if strings.Contains(body, stageGateRequiredFragment) {
					c.sawGate = true
				}
				if strings.Contains(body, "status = 'Pending'") {
					c.sawPending = true
				}
			}
			if strings.Contains(body, "UPDATE BountyBoard") && strings.Contains(body, "'Locked'") {
				c.hasUpdate = true
			}
			return true
		})
		results = append(results, c)
	}

	if len(results) == 0 {
		t.Fatalf("Pattern P-StageGate: no Claim* functions found in %s — file moved or convention changed; retarget the audit",
			rel(root, tasksPath))
	}

	// Every Claim* function that issues a Pending-status SELECT against
	// BountyBoard MUST also include the stage_id gate. Functions that
	// claim non-Pending rows (ClaimForReview, ClaimForCaptainReview)
	// are unaffected — their queries filter by 'AwaitingCouncilReview'
	// or 'AwaitingCaptainReview', which are post-astromech states and
	// outside the scope of "no pre-staging".
	for _, c := range results {
		if !c.sawSelect {
			// Not a SQL-issuing Claim function — skip silently. Could be
			// a wrapper that delegates to one we already covered.
			continue
		}
		if !c.sawPending {
			// Claim* function whose SELECT does not target Pending rows
			// — out of scope (e.g. ClaimForReview claims AwaitingCouncilReview).
			continue
		}
		if !c.sawGate {
			t.Errorf("Pattern P-StageGate: %s in tasks.go issues a Pending-status SELECT against BountyBoard but does NOT include the stage_id gate (looking for fragment %q). Astromechs would be able to claim Pending-stage tasks.",
				c.name, stageGateRequiredFragment)
		}
	}
}

// TestPattern_PStageGate_NoUngatedClaimSQL walks every production .go
// file under internal/ and cmd/ and rejects any SQL string literal
// that issues a claim-shaped SELECT query against BountyBoard without
// the stage_id gate.
//
// Definition of "claim-shaped" SELECT (single-literal scope):
//
//	A SQL string literal whose body contains
//	  - "FROM BountyBoard"
//	  - "status = 'Pending'"
//	  - "SELECT" + a sibling "ORDER BY"/"LIMIT" — the dispatch shape:
//	    one Pending row gets picked up at a time.
//	The SELECT must ALSO sit in a file that issues an UPDATE
//	BountyBoard SET status = 'Locked' literal — that's what closes
//	the loop on "claim-time SQL". Files with only one of the two are
//	not dispatch paths (e.g. inquisitor.go releases stuck Locked
//	rows BACK to Pending; that's a maintenance sweep, not a claim).
//
// Crucially the per-literal check (rather than per-file substring
// match) avoids the false-positive shape where a file contains
// Pending and Locked tokens in unrelated queries.
//
// If a literal matches but the file is on stageGateBypassAllowlist,
// the entry's reason is printed but the test passes for that file.
func TestPattern_PStageGate_NoUngatedClaimSQL(t *testing.T) {
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

			// Phase 1: collect every backtick SQL literal in the file.
			type sqlLit struct {
				body string
				pos  token.Position
			}
			var lits []sqlLit
			ast.Inspect(f, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				if !strings.HasPrefix(lit.Value, "`") {
					return true
				}
				lits = append(lits, sqlLit{body: lit.Value, pos: fset.Position(lit.Pos())})
				return true
			})

			// Phase 2: does ANY literal in the file issue an
			// UPDATE BountyBoard … 'Locked' write? Required for
			// "claim-shaped" — without it, the file isn't a dispatch
			// path even if a Pending SELECT exists.
			fileHasLockedUpdate := false
			for _, l := range lits {
				if strings.Contains(l.body, "UPDATE BountyBoard") &&
					strings.Contains(l.body, "'Locked'") {
					fileHasLockedUpdate = true
					break
				}
			}
			if !fileHasLockedUpdate {
				return nil
			}

			relPath := rel(root, path)
			if reason, ok := stageGateBypassAllowlist[relPath]; ok {
				t.Logf("Pattern P-StageGate: %s — bypass allowed (%s)", relPath, reason)
				return nil
			}

			// Phase 3: check each Pending-SELECT literal individually.
			// A claim-shaped SELECT that lacks the gate is the offender.
			for _, l := range lits {
				body := l.body
				if !strings.Contains(body, "SELECT") {
					continue
				}
				if !strings.Contains(body, "FROM BountyBoard") {
					continue
				}
				if !strings.Contains(body, "status = 'Pending'") {
					continue
				}
				// Dispatch SELECTs always pick a single row at a time
				// (LIMIT 1) and order it deterministically. Maintenance
				// SELECTs typically iterate. This is the discriminator
				// that excludes inquisitor's "rows = stuck-Pending"
				// re-sweep query if one ever emerges.
				if !strings.Contains(body, "LIMIT 1") {
					continue
				}
				if strings.Contains(body, stageGateRequiredFragment) {
					continue
				}
				offenders = append(offenders, offender{
					path: relPath,
					line: l.pos.Line,
					why: "Pending-status SELECT against BountyBoard (LIMIT 1) lacks the stage_id gate (" +
						stageGateRequiredFragment + "). Astromechs would be able to claim Pending-stage tasks.",
				})
			}
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
	t.Errorf("Pattern P-StageGate: %d ungated claim-shaped SQL site(s):", len(offenders))
	for _, o := range offenders {
		t.Errorf("  %s:%d — %s", o.path, o.line, o.why)
	}
	t.Errorf("\nFix: add the stage gate predicate `(stage_id IS NULL OR EXISTS (SELECT 1 FROM ConvoyStages cs WHERE cs.id = BountyBoard.stage_id AND cs.status != 'Pending'))` to the WHERE clause, OR add the file to stageGateBypassAllowlist with a justification (only for non-dispatch maintenance queries).")
}

// TestPattern_PStageGate_ClaimPathSurfaceProbed is carried forward from
// the P1 skeleton — it confirms store.ClaimBounty still exists and lives
// where the gating audit expects it. If the function moves, the
// per-function audit above must be re-pointed.
func TestPattern_PStageGate_ClaimPathSurfaceProbed(t *testing.T) {
	root := moduleRoot(t)
	storeDir := filepath.Join(root, "internal", "store")
	matches, err := filepath.Glob(filepath.Join(storeDir, "*.go"))
	if err != nil {
		t.Fatalf("glob %s: %v", storeDir, err)
	}

	fset := token.NewFileSet()
	var found bool
	for _, m := range matches {
		if strings.HasSuffix(m, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, m, nil, 0)
		if err != nil {
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok {
				return true
			}
			if fn.Name.Name == "ClaimBounty" {
				found = true
				return false
			}
			return true
		})
		if found {
			break
		}
	}
	if !found {
		t.Errorf("Pattern P-StageGate: store.ClaimBounty not found via AST walk — claim surface moved? Update the per-function audit target.")
	}
}

func mustReadFile(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}
