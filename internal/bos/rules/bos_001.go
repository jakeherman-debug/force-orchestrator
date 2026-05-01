package rules

import (
	"go/ast"
	"go/types"
	"strings"

	"force-orchestrator/internal/bos"
)

// BOS-001 — VoidStoreNewMutator
//
// CLAUDE.md anchor: "No silent failures". Every new store mutator MUST
// return error so the caller can route through FailBounty / escalate.
// This rule flags func declarations in internal/store/ whose name starts
// with a mutator verb (Update/Insert/Delete/Set/Add/Remove/Mark/Bump/...
// /Clear/Reset/Bulk*/Append/Apply) AND that return nothing or only
// non-error types.
//
// Anti-cheat: severity=advise at launch (per docs/roadmap.md § D4).
type bos001 struct{}

func (bos001) ID() string             { return "BOS-001" }
func (bos001) CLAUDEMDAnchor() string { return "No silent failures" }
func (bos001) Severity() bos.Severity { return bos.SeverityAdvise }

// mutatorVerbs is the set of name prefixes that indicate a function
// performs a mutation. The Insert/Update/Delete/Set/... canonical set
// keeps the surface narrow enough that false positives stay rare; if a
// new prefix proves load-bearing we add it here AND pair the rule
// addition with first-30-firings precision tracking.
var mutatorVerbs = []string{
	"Insert", "Update", "Delete", "Set", "Add", "Remove",
	"Mark", "Bump", "Clear", "Reset", "Bulk", "Append", "Apply",
}

func (bos001) Check(file *ast.File, path string, _ *types.Info) []bos.Finding {
	if !strings.Contains(path, "internal/store") {
		return nil
	}
	var out []bos.Finding
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil { // methods are not store-mutators in our shape
			continue
		}
		name := fn.Name.Name
		if !startsWithMutatorVerb(name) {
			continue
		}
		if returnsError(fn) {
			continue
		}
		out = append(out, bos.Finding{
			RuleID:   "BOS-001",
			Severity: bos.SeverityAdvise,
			Path:     path,
			Line:     positionLine(fn),
			Message:  "store mutator " + name + " returns no error — every new store mutator MUST return error per CLAUDE.md No silent failures",
		})
	}
	return out
}

func startsWithMutatorVerb(name string) bool {
	for _, v := range mutatorVerbs {
		if strings.HasPrefix(name, v) {
			return true
		}
	}
	return false
}

// returnsError returns true iff the function's return list includes a
// type whose textual representation is "error". The check is
// intentionally lexical (not types-info-aware) because the BoS reviewer
// runs without a full type-check pass.
func returnsError(fn *ast.FuncDecl) bool {
	if fn.Type == nil || fn.Type.Results == nil {
		return false
	}
	for _, field := range fn.Type.Results.List {
		ident, ok := field.Type.(*ast.Ident)
		if ok && ident.Name == "error" {
			return true
		}
	}
	return false
}

func init() { bos.Register(bos001{}) }
