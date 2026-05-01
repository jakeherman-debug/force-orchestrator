// Package rules holds every Bureau of Standards rule body. Each rule
// is registered via init() into internal/bos's registry.
//
// The fileset.go indirection threads a *token.FileSet through to the
// position-resolution layer used by all rules without forcing every
// rule's Check signature to take one. Production callers (the BoS
// reviewer) call SetFileSet(fset) once per file before calling
// rule.Check; tests use the same indirection from testhelpers_test.go.
//
// This is the only mutable package-level state in the rules package
// and it is per-file (overwritten on each SetFileSet call). It is NOT
// safe for concurrent use across files; the BoS reviewer processes
// files sequentially within a single review.
package rules

import (
	"go/ast"
	"go/token"
	"sync"
)

var (
	fsetMu sync.RWMutex
	fset   *token.FileSet
)

// SetFileSet stores the FileSet for subsequent positionLine() calls.
// Production callers (internal/bos reviewer) call this once per file.
func SetFileSet(f *token.FileSet) {
	fsetMu.Lock()
	defer fsetMu.Unlock()
	fset = f
}

// setFset is the test-only alias used by testhelpers_test.go via the
// same package — it routes through SetFileSet so the production hook
// is the only place that mutates the package-level state.
func setFset(f *token.FileSet) { SetFileSet(f) }

// positionLine resolves an ast.Node's position to a 1-indexed line.
// Returns 0 if no FileSet has been registered (the production caller
// always sets one; this is a defensive guard for test code that forgot
// to use parse() helper).
func positionLine(n ast.Node) int {
	if n == nil {
		return 0
	}
	fsetMu.RLock()
	defer fsetMu.RUnlock()
	if fset == nil {
		return 0
	}
	return fset.Position(n.Pos()).Line
}
