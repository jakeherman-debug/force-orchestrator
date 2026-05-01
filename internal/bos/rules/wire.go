package rules

import (
	"go/token"

	"force-orchestrator/internal/bos"
)

// init wires the rules-package SetFileSet helper into internal/bos's
// SetFileSetForRules indirection so production callers can do:
//
//	import _ "force-orchestrator/internal/bos/rules" // register rules
//	bos.ReviewFiles(gate, inputs)                    // walks rules
//
// without explicitly threading a fileset. The SetFileSetForRules hook
// is a function variable in internal/bos; the rules package's init
// (this file) overwrites it with a binding that calls the rules-package
// SetFileSet. Loading order: Go runs rules.init() before main().
func init() {
	bos.SetFileSetForRules = func(f *token.FileSet) { SetFileSet(f) }
}
