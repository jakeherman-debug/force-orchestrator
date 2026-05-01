// Package rules holds every Imperial Security Bureau rule body. Each
// rule is registered via init() into internal/isb's registry.
//
// The fileset.go indirection threads a *token.FileSet through to the
// position-resolution layer used by all rules without forcing every
// rule's Check signature to take one. Mirror of internal/bos/rules's
// shape.
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
func SetFileSet(f *token.FileSet) {
	fsetMu.Lock()
	defer fsetMu.Unlock()
	fset = f
}

// setFset is the test-only alias used by testhelpers_test.go via the
// same package.
func setFset(f *token.FileSet) { SetFileSet(f) }

// positionLine resolves an ast.Node's position to a 1-indexed line.
// Returns 0 if no FileSet has been registered.
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

// positionLineAt returns the 1-indexed line for an arbitrary
// token.Pos (used by rules that walk subexpressions and need a Line
// per call site rather than the enclosing FuncDecl).
func positionLineAt(p token.Pos) int {
	fsetMu.RLock()
	defer fsetMu.RUnlock()
	if fset == nil {
		return 0
	}
	return fset.Position(p).Line
}
