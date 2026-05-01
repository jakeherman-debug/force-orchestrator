package rules

import (
	"go/token"

	"force-orchestrator/internal/isb"
)

// init wires the rules-package SetFileSet helper into internal/isb's
// SetFileSetForRules indirection so production callers can do:
//
//	import _ "force-orchestrator/internal/isb/rules" // register rules
//	isb.ReviewFiles(gate, inputs)                    // walks rules
//
// without explicitly threading a fileset. Loading order: Go runs
// rules.init() before main().
func init() {
	isb.SetFileSetForRules = func(f *token.FileSet) { SetFileSet(f) }
}
