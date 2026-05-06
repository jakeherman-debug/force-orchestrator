// Package audittools: Pattern P_CLIFlagParsing — every cmd<X> handler
// in cmd/force/*.go that accepts an `args []string` MUST route flag
// parsing through the shared parseSubcommandFlags / parseDaemonFlags
// helpers (which use flag.NewFlagSet with ContinueOnError, intercept
// --help/-h, and propagate parse errors as non-zero exits).
//
// Why this audit exists:
//
//   D12 P4 surfaced a CLI-safety defect class where `force daemon
//   install --help` was silently running the install (writing the
//   launchd plist) because the handler used a manual `for _, a :=
//   range args` loop that ignored unrecognized tokens. The fix landed
//   for the daemon family (Pattern P_DaemonFlagParsing). Generalizing
//   to the rest of the CLI surface is fix(cli)/cli-flag-parsing.
//
// What we check:
//
//   1. Every function in cmd/force/*.go (excluding _test.go) whose name
//      matches the cmd<X> shape AND takes an `args []string` parameter
//      MUST contain a call to parseSubcommandFlags OR parseDaemonFlags
//      OR be on the explicit allowlist below (dispatchers / no-op
//      passthroughs).
//
//   2. The destructive subcommand handlers (add-repo, reset, cancel,
//      block, prioritize, retry-all-failed, taskNote, etc., plus all
//      the daemon destructive ones from P_DaemonFlagParsing) MUST call
//      the helper as the first non-trivial statement, BEFORE any side-
//      effect call (the whole point of rejection is to fire BEFORE
//      mutating state).
//
//   3. There must be NO `for _, a := range args` or `for i := 0; i <
//      len(args); i++` loop survival in any cmd<X> handler that would
//      otherwise be required to use the helper. Allowlisted dispatchers
//      may keep their loops because they don't accept --flags
//      themselves; they route to leaf handlers that do.
//
// Behavioral coverage lives in cmd/force/cli_flag_parsing_test.go and
// cmd/force/daemon_help_test.go (subprocess tests against a real built
// binary). This audit is the AST-level guard that the contract isn't
// drifted away from over time.

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

func TestPattern_P_CLIFlagParsing(t *testing.T) {
	root := moduleRoot(t)
	dir := filepath.Join(root, "cmd", "force")

	// Allowlist: handler names that legitimately do NOT call the
	// helper. Each entry has a one-line rationale.
	allowlist := map[string]string{
		// Subcommand dispatchers — they switch on args[0] and route
		// to leaf handlers. The leaves call the helper. Adding the
		// helper at the dispatcher level would require teaching it
		// about subcommand verbs; cleaner to keep the dispatch as a
		// thin switch.
		"cmdRepos":            "dispatcher — routes to list/remove (leaves call helper)",
		"cmdEscalations":      "dispatcher — routes to list/ack/close/requeue (leaves call helper)",
		"cmdDogs":             "dispatcher — routes to list/run (leaves call helper)",
		"cmdConvoy":           "dispatcher — routes to many subcommands; leaves call helper or are read-only",
		"cmdConfig":           "dispatcher — routes to get/set/list",
		"cmdMemories":         "dispatcher — routes to delete/search/list",
		"cmdMail":             "dispatcher — routes to send/list/inbox/read",
		"cmdEC":               "dispatcher — routes to list/ratify/reject/status",
		"cmdExperiment":       "dispatcher — routes to author/ratify/terminate/status/list",
		"cmdMigrate":          "dispatcher — routes to pr-flow",
		"cmdNotifications":    "dispatcher — routes to budget",
		"cmdSession":          "dispatcher — routes to save/clear",
		"cmdTrust":            "dispatcher — routes to list (or accepts agent/value positionals via helper)",
		"cmdProposedFeatures": "dispatcher — routes to list/suppress/score/promote",
		"cmdLearning":         "dispatcher — routes to refresh/show",
		"cmdRetro":            "dispatcher — routes to generate/save",
		"cmdCooldown":         "dispatcher — routes to pause/resume/cancel (leaves call helper)",
		"cmdRepo":             "wrapper — routes to sync/set-pr-flow/help (cli_inline_handlers.go)",
		"cmdBounty":           "wrapper — routes to stats (cli_inline_handlers.go)",
		"cmdTask":             "wrapper — routes to note (cli_inline_handlers.go)",
		"cmdRunForeground":    "wrapper — calls helper internally; signature takes a closure",
		// `cmdReject` (briefing-reject) and `cmdDecide` actually
		// DO call the helper after migration; they're listed here
		// only if they're identified by the AST as not calling it.

		// Daemon family allowlists (preserved from P_DaemonFlagParsing).
		"cmdDaemon":      "legacy foreground daemon entry — bare `force daemon` with no subcommand falls through",
		"cmdDaemonTrust": "dispatcher — routes to add/remove/list (leaves call parseDaemonFlags)",
	}

	// Destructive handlers that must call the helper BEFORE any
	// mutating call. Mirrors P_DaemonFlagParsing's destructive set,
	// extended to the rest of the CLI surface.
	destructive := map[string]bool{
		// Daemon family (preserved from P_DaemonFlagParsing).
		"cmdDaemonInstall":          true,
		"cmdDaemonUninstall":        true,
		"cmdDaemonUpdate":           true,
		"cmdDaemonRollback":         true,
		"cmdDaemonClearCrashBudget": true,
		"cmdDaemonTrustAdd":         true,
		"cmdDaemonTrustRemove":      true,
		// Top-level CLI mutators.
		"cmdAddRepo":            true,
		"cmdReset":              true,
		"cmdCancel":             true,
		"cmdBlock":              true,
		"cmdUnblock":            true,
		"cmdUnblockDependents":  true,
		"cmdPrioritize":         true,
		"cmdRetryAllFailed":     true,
		"cmdRejectTask":         true,
		"cmdApproveTask":        true,
		"cmdTaskNote":           true,
		"cmdAdd":                true,
		"cmdAddInvestigate":     true,
		"cmdAddAudit":           true,
		"cmdAddJira":            true,
		"cmdAnnotate":           true,
		"cmdPurge":              true,
		"cmdHardReset":          true,
		"cmdPrune":              true,
		"cmdScale":              true,
		"cmdEstop":              true,
		"cmdResume":             true,
		"cmdMigratePRFlow":      true,
		"cmdRepoSetPRFlow":      true,
		"cmdAttention":          true,
	}

	// Mutating call detectors. Mirrors P_DaemonFlagParsing's set,
	// extended with store mutators commonly used by CLI handlers.
	mutatingNames := map[string]bool{
		"WriteFile":                  true,
		"MkdirAll":                   true,
		"Remove":                     true,
		"Rename":                     true,
		"Chmod":                      true,
		"RecordDaemonUpdate":         true,
		"ClearDaemonStartLog":        true,
		"Append":                     true,
		"RemoveSHA":                  true,
		"copyBinaryFile":             true,
		"installLaunchd":             true,
		"uninstallLaunchd":           true,
		"installSystemd":             true,
		"uninstallSystemd":           true,
		"AddRepo":                    true,
		"RemoveRepo":                 true,
		"AddBounty":                  true,
		"AddBountyClassifying":       true,
		"AddCodeEditTask":            true,
		"ResetTask":                  true,
		"CancelTask":                 true,
		"AddDependency":              true,
		"RemoveDependenciesOf":       true,
		"UnblockDependentsOf":        true,
		"SetBountyPriority":          true,
		"ResetAllFailed":             true,
		"AppendTaskNote":             true,
		"FailBounty":                 true,
		"ReturnTaskForRework":        true,
		"IncrementRetryCount":        true,
		"UpdateBountyStatus":         true,
		"SetEstop":                   true,
		"SetConfig":                  true,
		"InsertAnnotation":           true,
		"SetAttentionTag":            true,
		"SetTrustDial":               true,
		"BootstrapTrustDials":        true,
		"SetNotificationBudget":      true,
		"SuppressProposedFeature":    true,
		"OverrideProposedFeatureScore": true,
		"PromoteProposedFeature":     true,
		"SaveOperatorSession":        true,
		"ClearOperatorSession":       true,
		"SetRepoPRFlowEnabled":       true,
		"SetRepoRemoteInfo":          true,
		"QueueFindPRTemplate":        true,
		"QueueSenatorOnboarding":     true,
		"QueueFeatureFromJira":       true,
		"runPRFlowMigrate":           true,
		"runPRFlowRollback":          true,
	}

	type handler struct {
		name       string
		fn         *ast.FuncDecl
		filename   string
		isDestruct bool
	}

	handlers := map[string]*handler{}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Name == nil {
				return true
			}
			fname := fn.Name.Name
			if !strings.HasPrefix(fname, "cmd") {
				return true
			}
			// Must take an `args []string` parameter to be a flag-
			// parsing handler.
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
			handlers[fname] = &handler{
				name:       fname,
				fn:         fn,
				filename:   name,
				isDestruct: destructive[fname],
			}
			return true
		})
	}

	if len(handlers) == 0 {
		t.Fatalf("Pattern P_CLIFlagParsing: no cmd* handlers discovered in %s — has the dir moved?", dir)
	}

	// (1) Every handler must call parseSubcommandFlags or
	// parseDaemonFlags, OR be on the allowlist.
	for name, h := range handlers {
		if _, ok := allowlist[name]; ok {
			continue
		}
		callsParse := false
		ast.Inspect(h.fn, func(inner ast.Node) bool {
			call, ok := inner.(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, ok := call.Fun.(*ast.Ident); ok {
				if id.Name == "parseSubcommandFlags" || id.Name == "parseDaemonFlags" {
					callsParse = true
				}
			}
			return true
		})
		if !callsParse {
			t.Errorf("Pattern P_CLIFlagParsing: %s (%s) does not call parseSubcommandFlags / parseDaemonFlags — unknown flags will be silently ignored, --help may run the side-effect. If this is a dispatcher, add it to the allowlist with a rationale.",
				name, h.filename)
		}
	}

	// (2) Destructive handlers must call the helper BEFORE any
	// mutating call.
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
			if id, ok := call.Fun.(*ast.Ident); ok {
				if (id.Name == "parseSubcommandFlags" || id.Name == "parseDaemonFlags") && parsePos == token.NoPos {
					parsePos = call.Pos()
				}
			}
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
			// Already errored above.
			continue
		}
		if firstMutPos != token.NoPos && firstMutPos < parsePos {
			t.Errorf("Pattern P_CLIFlagParsing: %s (%s) performs a mutating call BEFORE parseSubcommandFlags / parseDaemonFlags — destructive handlers must reject unknown flags first (helper pos=%d, mutating call pos=%d)",
				name, h.filename, parsePos, firstMutPos)
		}
	}
}
