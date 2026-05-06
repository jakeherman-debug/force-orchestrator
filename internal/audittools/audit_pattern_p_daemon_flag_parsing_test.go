// Package audittools: Pattern P_DaemonFlagParsing — every cmdDaemon<Name>
// handler in cmd/force/daemon_cmds.go MUST route flag parsing through
// the shared parseDaemonFlags helper (which uses flag.NewFlagSet with
// ContinueOnError, intercepts --help/-h, and propagates parse errors as
// non-zero exits).
//
// Why this audit exists:
//
//   D12 P4 surfaced a CLI-safety defect where `force daemon install
//   --help` was silently running the install (writing the launchd plist)
//   because the handler used a manual `for _, a := range args` loop that
//   ignored unrecognized tokens. A future regression — e.g. someone
//   adds `cmdDaemonRotateKeys` and copy-pastes the old manual loop —
//   would re-introduce the same class of bug. This AST audit catches
//   that at test time.
//
// What we check:
//
//   1. Every function in daemon_cmds.go whose name matches the
//      cmdDaemon<Capital><...> shape AND takes an `args []string`
//      parameter MUST contain a call to parseDaemonFlags.
//
//   2. The destructive subcommand handlers (install, uninstall, update,
//      rollback, clear-crash-budget) MUST call parseDaemonFlags as the
//      first non-trivial statement, BEFORE any side-effect call (the
//      whole point of the rejection is to fire before mutating state).
//
//   3. There must be NO `for _, a := range args` or `for i := 0; i <
//      len(args); i++` loop survival in any cmdDaemon<X> handler — the
//      old shape is the very thing this fix banishes.
//
// Behavioral coverage lives in cmd/force/daemon_help_test.go (subprocess
// tests against a real built binary). This audit is the AST-level guard
// that the contract isn't drifted away from over time.

package audittools

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

func TestPattern_P_DaemonFlagParsing(t *testing.T) {
	root := moduleRoot(t)
	target := filepath.Join(root, "cmd", "force", "daemon_cmds.go")

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, target, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", target, err)
	}

	// Set of handler functions to audit. We discover them by walking the
	// file rather than hard-coding a list, so newly-added cmdDaemon*
	// handlers are caught automatically.
	type handler struct {
		fn          *ast.FuncDecl
		isDestruct  bool
		isLoopOnly  bool // false until proven; default to "rejects" until we see otherwise
	}
	destructive := map[string]bool{
		"cmdDaemonInstall":           true,
		"cmdDaemonUninstall":         true,
		"cmdDaemonUpdate":            true,
		"cmdDaemonRollback":          true,
		"cmdDaemonClearCrashBudget":  true,
		// Trust mutators are also destructive (write the trust file).
		"cmdDaemonTrustAdd":    true,
		"cmdDaemonTrustRemove": true,
	}
	handlers := map[string]*handler{}
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name == nil {
			return true
		}
		name := fn.Name.Name
		if !strings.HasPrefix(name, "cmdDaemon") {
			return true
		}
		// Must take an `args []string` parameter to be a flag-parsing handler.
		hasArgsParam := false
		if fn.Type.Params != nil {
			for _, p := range fn.Type.Params.List {
				for _, n := range p.Names {
					if n.Name == "args" {
						hasArgsParam = true
					}
				}
			}
		}
		if !hasArgsParam {
			return true
		}
		// cmdDaemonHistoryFromTrustFile receives a `limit int`, not args —
		// caught by the hasArgsParam gate above. cmdDaemonTrust dispatches
		// to its sub-handlers without using flag.FlagSet itself (it walks
		// args[0] as a verb), so we exclude it explicitly.
		if name == "cmdDaemonTrust" {
			return true
		}
		handlers[name] = &handler{fn: fn, isDestruct: destructive[name]}
		return true
	})

	if len(handlers) == 0 {
		t.Fatalf("Pattern P_DaemonFlagParsing: no cmdDaemon* handlers discovered — has the file moved?")
	}

	// (1) Every handler must call parseDaemonFlags.
	// (3) No handler may contain the legacy `for _, a := range args` or
	//     `for i := 0; i < len(args); i++` loops.
	for name, h := range handlers {
		callsParse := false
		hasLegacyLoop := false
		ast.Inspect(h.fn, func(inner ast.Node) bool {
			if call, ok := inner.(*ast.CallExpr); ok {
				if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "parseDaemonFlags" {
					callsParse = true
				}
			}
			// Legacy shape: `for _, a := range args` or
			// `for i := 0; i < len(args); i++`.
			if rs, ok := inner.(*ast.RangeStmt); ok {
				if id, ok := rs.X.(*ast.Ident); ok && id.Name == "args" {
					hasLegacyLoop = true
				}
			}
			if fs, ok := inner.(*ast.ForStmt); ok && fs.Cond != nil {
				// Heuristic: condition is `i < len(args)`.
				if be, ok := fs.Cond.(*ast.BinaryExpr); ok {
					if call, ok := be.Y.(*ast.CallExpr); ok {
						if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "len" {
							for _, arg := range call.Args {
								if aid, ok := arg.(*ast.Ident); ok && aid.Name == "args" {
									hasLegacyLoop = true
								}
							}
						}
					}
				}
			}
			return true
		})
		if !callsParse {
			t.Errorf("Pattern P_DaemonFlagParsing: %s does not call parseDaemonFlags — unknown flags will be silently ignored, --help may run the side-effect", name)
		}
		if hasLegacyLoop {
			t.Errorf("Pattern P_DaemonFlagParsing: %s contains a legacy `for ... args` parsing loop — replace with parseDaemonFlags so unknown flags are rejected", name)
		}
	}

	// (2) Destructive handlers must call parseDaemonFlags BEFORE any
	//     mutating call. We approximate "mutating" with: os.WriteFile,
	//     os.MkdirAll, os.Remove, os.Rename, os.Chmod, store.RecordDaemonUpdate,
	//     store.ClearDaemonStartLog, trust.Append, trust.RemoveSHA,
	//     copyBinaryFile, installLaunchd, uninstallLaunchd, installSystemd,
	//     uninstallSystemd. The check: in a top-down traversal of the
	//     function body, the first parseDaemonFlags call must precede the
	//     first mutating call.
	mutatingNames := map[string]bool{
		"WriteFile":             true,
		"MkdirAll":              true,
		"Remove":                true,
		"Rename":                true,
		"Chmod":                 true,
		"RecordDaemonUpdate":    true,
		"ClearDaemonStartLog":   true,
		"Append":                true, // trust.Append
		"RemoveSHA":             true,
		"copyBinaryFile":        true,
		"installLaunchd":        true,
		"uninstallLaunchd":      true,
		"installSystemd":        true,
		"uninstallSystemd":      true,
	}
	for name, h := range handlers {
		if !h.isDestruct {
			continue
		}
		var (
			parsePos    token.Pos
			firstMutPos token.Pos
		)
		ast.Inspect(h.fn, func(inner ast.Node) bool {
			call, ok := inner.(*ast.CallExpr)
			if !ok {
				return true
			}
			// parseDaemonFlags
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "parseDaemonFlags" && parsePos == token.NoPos {
				parsePos = call.Pos()
			}
			// Mutating call detection: a SelectorExpr (pkg.Fn) OR an
			// Ident (locally-defined helper).
			var calledName string
			switch fn := call.Fun.(type) {
			case *ast.SelectorExpr:
				calledName = fn.Sel.Name
			case *ast.Ident:
				calledName = fn.Name
			}
			if mutatingNames[calledName] && firstMutPos == token.NoPos {
				firstMutPos = call.Pos()
			}
			return true
		})
		if parsePos == token.NoPos {
			// Already errored above; skip ordering check to avoid noise.
			continue
		}
		if firstMutPos != token.NoPos && firstMutPos < parsePos {
			t.Errorf("Pattern P_DaemonFlagParsing: %s performs a mutating call BEFORE parseDaemonFlags — destructive handlers must reject unknown flags first (parseDaemonFlags pos=%d, mutating call pos=%d)",
				name, parsePos, firstMutPos)
		}
	}
}
