// Package audittools — Pattern P34 (D4 Phase 3).
//
// P34 enforces the "No Senator auto-editing its own rules" anti-cheat
// directive from docs/roadmap.md § Deliverable 4 (line 1428). The
// Senate must NEVER mutate FleetRules directly; rule changes route
// through the operator-ratified PromotionProposal pipeline (Librarian
// emits candidates → Engineering Corps experiments → operator
// ratifies → rule lands in FleetRules).
//
// The check walks every production .go file under:
//
//   - internal/senate/
//   - internal/agents/senate*.go (the Senate agent + its hook)
//
// and rejects any direct call to a FleetRules-mutating helper:
//
//   - store.SetActiveFleetRule        (set rule active in some scope)
//   - store.UpsertFleetRule
//   - store.InsertFleetRule
//   - store.DeleteFleetRule
//   - store.UpdateFleetRule
//   - store.DeactivateFleetRule
//   - store.RatifyPromotionProposal   (gateway to FleetRules from EC pipeline)
//   - store.BootstrapFleetRules       (seed-time only; not a runtime call)
//
// The legitimate path — emitting a candidate via the Librarian Client's
// EmitCandidate — is allowed.
//
// Allowlist is empty by design. Adding an entry requires explicit
// review-time justification (the spec is unambiguous: "No Senator
// auto-editing own rules").
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

// p34ForbiddenStoreFuncs is the set of FleetRules-mutating store
// helpers the Senate package + Senate agent files MUST NOT call.
var p34ForbiddenStoreFuncs = map[string]struct{}{
	"SetActiveFleetRule":       {},
	"UpsertFleetRule":          {},
	"InsertFleetRule":          {},
	"DeleteFleetRule":          {},
	"UpdateFleetRule":          {},
	"DeactivateFleetRule":      {},
	"RatifyPromotionProposal":  {},
	"BootstrapFleetRules":      {},
}

// p34Allowlist names files exempted from the Pattern P34 check. Empty
// at landing; future entries MUST carry a one-line truthful rationale
// (mirrors P33's allowlist shape).
var p34Allowlist = map[string]string{}

// p34Offence is the offending-call-site shape (named so the helper
// scanFileForP34 can return a typed slice without re-declaring it).
type p34Offence struct {
	File string
	Line int
	Func string
}

// TestPattern_P34_SenateNoSelfPromote walks the Senate package +
// Senate agent files and reports any direct FleetRules-mutating call.
func TestPattern_P34_SenateNoSelfPromote(t *testing.T) {
	root := moduleRoot(t)
	var offences []p34Offence

	scanDirs := []string{
		filepath.Join(root, "internal", "senate"),
	}
	for _, dir := range scanDirs {
		walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if isNotExistErr(err) {
					return nil
				}
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel := relForP34(root, path)
			if _, allowed := p34Allowlist[rel]; allowed {
				return nil
			}
			offences = append(offences, scanFileForP34(rel, path)...)
			return nil
		})
		if walkErr != nil {
			t.Fatalf("walk %s: %v", dir, walkErr)
		}
	}

	// Senate agent files: scan internal/agents/senate*.go (non-test).
	agentDir := filepath.Join(root, "internal", "agents")
	entries, err := filepath.Glob(filepath.Join(agentDir, "senate*.go"))
	if err != nil {
		t.Fatalf("glob senate*.go: %v", err)
	}
	for _, path := range entries {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		rel := relForP34(root, path)
		if _, allowed := p34Allowlist[rel]; allowed {
			continue
		}
		offences = append(offences, scanFileForP34(rel, path)...)
	}

	if len(offences) == 0 {
		return
	}
	sort.Slice(offences, func(i, j int) bool {
		if offences[i].File != offences[j].File {
			return offences[i].File < offences[j].File
		}
		return offences[i].Line < offences[j].Line
	})
	t.Errorf("Pattern P34 (D4-P3): %d Senate file(s) call a forbidden FleetRules-mutating helper. Senate's only path to FleetRules is via Librarian.EmitCandidate → operator ratification (per docs/roadmap.md § D4 anti-cheat \"No Senator auto-editing own rules\"):", len(offences))
	for _, o := range offences {
		t.Errorf("  %s:%d — store.%s(...)", o.File, o.Line, o.Func)
	}
}

// scanFileForP34 returns every offending call site in path. Bound to a
// helper so the two scan loops above stay readable.
func scanFileForP34(rel, path string) []p34Offence {
	var hits []p34Offence
	fset := token.NewFileSet()
	f, parseErr := parser.ParseFile(fset, path, nil, 0)
	if parseErr != nil {
		return nil
	}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "store" {
			return true
		}
		if _, forbidden := p34ForbiddenStoreFuncs[sel.Sel.Name]; !forbidden {
			return true
		}
		pos := fset.Position(call.Pos())
		hits = append(hits, p34Offence{File: rel, Line: pos.Line, Func: sel.Sel.Name})
		return true
	})
	return hits
}

// TestPattern_P34_AllowlistReasonsTruthful ensures every allowlist
// entry has a real rationale.
func TestPattern_P34_AllowlistReasonsTruthful(t *testing.T) {
	for path, reason := range p34Allowlist {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("p34Allowlist[%q] empty rationale — every allowlist entry MUST carry a one-line truthful reason", path)
		}
	}
}

func relForP34(root, path string) string {
	r, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(r)
}

// isNotExistErr maps fs.ErrNotExist (no internal/senate during a fresh
// pre-P3 checkout, for instance) to a tolerable skip. Defence in depth.
func isNotExistErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such file")
}
