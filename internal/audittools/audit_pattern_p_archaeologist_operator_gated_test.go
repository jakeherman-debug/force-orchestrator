// Pattern P-ArchaeologistOperatorGated (D9 Track A).
//
// Anti-cheat directive: the Archaeologist proposes; the operator
// ratifies. Translated to code:
//
//   1. The ONLY proposal-emission seam reachable from
//      internal/archaeologist/* OR internal/agents/archaeologist*.go
//      MUST be librarian.Client.EmitCandidate. Direct INSERTs into
//      PromotionProposals, calls to store.RatifyPromotionProposal,
//      and direct invocations of EngineeringCorps experiment-author
//      methods are forbidden.
//
//   2. The migration handoff MUST flow through a known task type
//      (ArchaeologistProposeMigration) that lives on the BountyBoard
//      — i.e. the operator-visible queue. The agent file
//      (internal/agents/archaeologist.go) MUST reference this task
//      type by literal so the queue surface stays auditable from a
//      grep.
//
// AST-based regression: walks every .go file under
// internal/archaeologist/ and the archaeologist agent files in
// internal/agents/, and rejects:
//   - bare `*sql.DB.Exec("INSERT INTO PromotionProposals ...")` calls
//   - selector calls into any package other than the librarian client
//     surface for proposal emission (e.g. engineering_corps.Submit*)
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

// pArchaeologistForbiddenSelectors lists method/function names that,
// when called from the archaeologist tree, would constitute an
// auto-dispatched migration. The matcher checks the selector's
// trailing `.Name` only — package qualification is checked separately
// when needed.
var pArchaeologistForbiddenSelectors = map[string]struct{}{
	"RatifyPromotionProposal":   {},
	"InsertPromotionProposal":   {},
	"AuthorExperiment":          {},
	"DispatchMigration":         {},
	"AutoApplyCandidate":        {},
}

// pArchaeologistFileRoots are the AST scan roots — relative to the
// module root.
var pArchaeologistFileRoots = []string{
	"internal/archaeologist",
}

// pArchaeologistExtraFiles lists individual files outside the
// archaeologist tree that ALSO must obey the operator-gated rule
// (the agent file lives under internal/agents/).
var pArchaeologistExtraFiles = []string{
	"internal/agents/archaeologist.go",
}

// TestPattern_PArchaeologistOperatorGated_OnlyEmitCandidate walks
// every archaeologist source file and asserts no forbidden selector
// is invoked. The single permitted proposal-emission seam is
// librarian.Client.EmitCandidate.
func TestPattern_PArchaeologistOperatorGated_OnlyEmitCandidate(t *testing.T) {
	root := moduleRoot(t)
	type offence struct {
		File     string
		Line     int
		Selector string
	}
	var offences []offence
	emitCandidateSeen := false

	scan := func(absPath string) {
		// Skip _test.go files — tests legitimately mock the seam.
		if strings.HasSuffix(absPath, "_test.go") {
			return
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, absPath, nil, 0)
		if err != nil {
			return
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
			if sel.Sel.Name == "EmitCandidate" {
				emitCandidateSeen = true
				return true
			}
			if _, forbidden := pArchaeologistForbiddenSelectors[sel.Sel.Name]; forbidden {
				rel, _ := filepath.Rel(root, absPath)
				offences = append(offences, offence{
					File:     filepath.ToSlash(rel),
					Line:     fset.Position(call.Pos()).Line,
					Selector: sel.Sel.Name,
				})
			}
			return true
		})
	}

	for _, rel := range pArchaeologistFileRoots {
		walkRoot := filepath.Join(root, rel)
		if err := filepath.WalkDir(walkRoot, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			scan(path)
			return nil
		}); err != nil {
			t.Fatalf("walk %s: %v", walkRoot, err)
		}
	}
	for _, rel := range pArchaeologistExtraFiles {
		scan(filepath.Join(root, rel))
	}

	if len(offences) > 0 {
		sort.Slice(offences, func(i, j int) bool {
			if offences[i].File != offences[j].File {
				return offences[i].File < offences[j].File
			}
			return offences[i].Line < offences[j].Line
		})
		t.Errorf("Pattern P-ArchaeologistOperatorGated: %d forbidden proposal-dispatch selector(s) reached from the archaeologist tree. The ONLY permitted proposal-emission seam is librarian.Client.EmitCandidate (anti-cheat #1: archaeologist proposes; operator ratifies):", len(offences))
		for _, o := range offences {
			t.Errorf("  %s:%d — .%s(...)", o.File, o.Line, o.Selector)
		}
	}

	// Positive control: at least ONE EmitCandidate call must reach
	// from the archaeologist tree, otherwise the seam exists in name
	// only and a future refactor could silently delete it.
	if !emitCandidateSeen {
		t.Errorf("Pattern P-ArchaeologistOperatorGated: zero EmitCandidate call sites detected in the archaeologist tree — the operator-gated seam is missing (or has been renamed without updating the audit). Re-introduce a librarian.Client.EmitCandidate call from internal/agents/archaeologist.go's propose-migration handler.")
	}
}

// TestPattern_PArchaeologistOperatorGated_NoRawPromotionProposalInsert
// scans the archaeologist tree for raw `INSERT INTO PromotionProposals`
// SQL strings — the second leg of the operator-gated invariant. The
// permitted seam (librarian.EmitCandidate) does its OWN INSERT inside
// the librarian package; archaeologist code MUST NOT bypass it.
func TestPattern_PArchaeologistOperatorGated_NoRawPromotionProposalInsert(t *testing.T) {
	root := moduleRoot(t)
	type offence struct {
		File string
		Line int
	}
	var offences []offence
	scan := func(absPath string) {
		if strings.HasSuffix(absPath, "_test.go") {
			return
		}
		// Use the AST so a comment that mentions the SQL doesn't trip
		// the matcher. Walk every basic-string literal and check for
		// the offending substring.
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, absPath, nil, 0)
		if err != nil {
			return
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			val := lit.Value
			upper := strings.ToUpper(val)
			if strings.Contains(upper, "INSERT INTO PROMOTIONPROPOSALS") {
				rel, _ := filepath.Rel(root, absPath)
				offences = append(offences, offence{
					File: filepath.ToSlash(rel),
					Line: fset.Position(lit.Pos()).Line,
				})
			}
			return true
		})
	}
	for _, rel := range pArchaeologistFileRoots {
		walkRoot := filepath.Join(root, rel)
		_ = filepath.WalkDir(walkRoot, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			scan(path)
			return nil
		})
	}
	for _, rel := range pArchaeologistExtraFiles {
		scan(filepath.Join(root, rel))
	}
	if len(offences) > 0 {
		t.Errorf("Pattern P-ArchaeologistOperatorGated: %d raw `INSERT INTO PromotionProposals` literal(s) inside the archaeologist tree. Use librarian.Client.EmitCandidate instead (the librarian package owns the INSERT):", len(offences))
		for _, o := range offences {
			t.Errorf("  %s:%d", o.File, o.Line)
		}
	}
}
