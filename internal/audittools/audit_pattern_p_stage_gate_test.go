// Package audittools — Pattern P-StageGate (D5.5 Phase 1, skeleton).
//
// P-StageGate enforces the "No astromech pre-staging" anti-cheat
// directive from docs/roadmap.md § Deliverable 5.5. The directive:
//
//   "Astromechs cannot hold a worktree on a `Pending` stage. Pattern
//    P-StageGate (AST audit) walks the dispatch path and rejects any
//    code path that would claim a Pending-stage task."
//
// P1 ships a SKELETON audit:
//
//  1. TestPattern_PStageGate_PackageWiringPresent asserts that the
//     stagegate package + the convoy-stage-watch dog file exist and
//     export the load-bearing surface (Gate interface, Registry,
//     baseline gates, dog dispatch). This is the "wiring is in
//     place" gate so future phases can layer on the real claim-path
//     enforcement without re-discovering the layout.
//
//  2. TestPattern_PStageGate_ClaimPathSurfaceProbed walks the known
//     astromech-claim entry points (store.ClaimBounty + the agents/
//     spawn-loop callers) and confirms they exist. P2 will extend
//     this with: "every claim site joins on ConvoyStages.status and
//     refuses Pending-stage rows." For P1 we just establish the
//     probe — failing this test means the dispatch surface moved
//     and the stage-gating logic that lands in P2 needs to track
//     the new shape.
//
// The stricter form (rejection of any code path that claims a
// Pending-stage task) requires schema-level coupling between
// BountyBoard and ConvoyStages that doesn't exist until P2 lands the
// stage_id wiring on tasks. P1 documents the placeholder; P2 swaps
// the skeleton for the real AST regression.
package audittools

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

func TestPattern_PStageGate_ClaimPathSurfaceProbed(t *testing.T) {
	// Probe: store.ClaimBounty exists + lives where we expect. Phase 2
	// will extend this to assert the claim SQL JOINs ConvoyStages and
	// refuses Pending stages. For P1 we just lock down the location so
	// the P2 work has a stable target.
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
		t.Errorf("Pattern P-StageGate: store.ClaimBounty not found via AST walk — claim surface moved? P2 stage-gating will need to retarget.")
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
