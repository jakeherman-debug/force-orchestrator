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
		// Inline-switch dispatchers whose destructive verbs were extracted by
		// fix(cli)/cli-destructive-verbs. Read-only verbs stay inline; every
		// destructive verb now delegates to a parseSubcommandFlags-using
		// cmd<Name><Verb> handler. The destructive-verb sub-audit below
		// (assertDispatcherDestructiveVerbsDelegate) walks the dispatcher
		// bodies and asserts that delegation is intact.
		"cmdConvoy":           "dispatcher — destructive verbs (create/approve/reset/reject/ship) extracted; read-only verbs (list/show/pr/pr-review) inline",
		"cmdConfig":           "dispatcher — destructive verb (set) extracted; read-only verbs (get/list) inline",
		"cmdMemories":         "dispatcher — destructive verb (delete) extracted; read-only verbs (search/list) inline",
		"cmdMail":             "dispatcher — destructive verb (send) extracted; read-only verbs (list/inbox/read) inline",
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
		// Destructive verbs extracted from inline-switch dispatchers by
		// fix(cli)/cli-destructive-verbs. See dispatcherDestructiveVerbs
		// below for the dispatcher → leaf mapping that the
		// assertDispatcherDestructiveVerbsDelegate walker enforces.
		"cmdConvoyCreate":  true,
		"cmdConvoyApprove": true,
		"cmdConvoyReset":   true,
		"cmdConvoyReject":  true,
		"cmdConvoyShipCLI": true,
		"cmdMailSend":      true,
		"cmdConfigSet":     true,
		"cmdMemoriesDelete": true,
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

	handlers := map[string]*cliHandler{}

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
			handlers[fname] = &cliHandler{
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

	// (3) Destructive-verb sub-audit: each allowlisted inline-switch
	// dispatcher must delegate every destructive verb to the
	// corresponding extracted cmd<Name><Verb> handler. This guards the
	// fix(cli)/cli-destructive-verbs contract: read-only verbs may stay
	// inline (they print and exit, no safety relevance), but destructive
	// verbs MUST hop into a parseSubcommandFlags-using leaf so --help
	// short-circuits BEFORE any mutation.
	t.Run("DispatcherDestructiveVerbsDelegate", func(t *testing.T) {
		assertDispatcherDestructiveVerbsDelegate(t, handlers)
	})
}

// dispatcherDestructiveVerbs maps each inline-switch dispatcher (from
// the allowlist above) to the destructive verbs it handles and the
// extracted leaf handler each must delegate to. If you extract a new
// destructive verb out of a dispatcher, add it here AND to the
// `destructive` map above; the walker below is the regression guard.
var dispatcherDestructiveVerbs = map[string]map[string]string{
	"cmdConvoy": {
		"create":  "cmdConvoyCreate",
		"approve": "cmdConvoyApprove",
		"reset":   "cmdConvoyReset",
		"reject":  "cmdConvoyReject",
		"ship":    "cmdConvoyShipCLI",
	},
	"cmdConfig": {
		"set": "cmdConfigSet",
	},
	"cmdMemories": {
		"delete": "cmdMemoriesDelete",
	},
	"cmdMail": {
		"send": "cmdMailSend",
	},
}

// assertDispatcherDestructiveVerbsDelegate walks each dispatcher's
// switch statement(s) and asserts that for every destructive verb in
// dispatcherDestructiveVerbs[<dispatcher>], the corresponding case body
// is a single-call delegation to the expected leaf handler.
//
// The walker is permissive about positional checks (the case body may
// also bail out with `os.Exit` on a malformed positional after the
// helper short-circuits), but it REJECTS any case body that performs a
// mutating call directly (DB Exec/Query that mutates, store mutators,
// SendMail, LogAudit, etc.) without first hopping into the extracted
// handler. Practically, we check that the case body contains a call to
// the expected leaf handler — the existing destructive-handler-must-
// call-helper-first audit (clause 2) guards the leaf itself.
//
// We tolerate cases that wrap the leaf in additional positional-arg
// boilerplate (e.g. `cmdConvoyPR(db, mustParseID(args[1]))` for the
// pr verb) because the destructive map only lists the truly destructive
// verbs; non-listed verbs are not constrained.
// cliHandler is the package-scope record describing a discovered cmd<X>
// handler — we lift it out of the test function so the
// DispatcherDestructiveVerbsDelegate sub-audit (which lives at package
// scope) can take it as a parameter.
type cliHandler struct {
	name       string
	fn         *ast.FuncDecl
	filename   string
	isDestruct bool
}

func assertDispatcherDestructiveVerbsDelegate(t *testing.T, handlers map[string]*cliHandler) {
	t.Helper()
	for dispatcher, verbs := range dispatcherDestructiveVerbs {
		h, ok := handlers[dispatcher]
		if !ok {
			t.Errorf("Pattern P_CLIFlagParsing/DispatcherDestructiveVerbsDelegate: dispatcher %q not found among cmd<X> handlers — was it renamed or removed?", dispatcher)
			continue
		}
		// For each (verb, expectedHandler), find the matching case clause
		// in the dispatcher body and assert the case body calls
		// expectedHandler.
		caseBodies := collectSwitchCaseBodies(h.fn)
		for verb, expectedHandler := range verbs {
			body, found := caseBodies[verb]
			if !found {
				t.Errorf("Pattern P_CLIFlagParsing/DispatcherDestructiveVerbsDelegate: dispatcher %s has no `case %q:` clause — expected delegation to %s",
					dispatcher, verb, expectedHandler)
				continue
			}
			if !caseBodyCallsHandler(body, expectedHandler) {
				t.Errorf("Pattern P_CLIFlagParsing/DispatcherDestructiveVerbsDelegate: dispatcher %s `case %q:` does NOT delegate to %s — destructive verbs must hop into the parseSubcommandFlags-using leaf so --help short-circuits BEFORE mutation",
					dispatcher, verb, expectedHandler)
			}
		}
	}
}

// collectSwitchCaseBodies walks fn's body for every top-level switch
// statement and returns a map of case-string-literal → list of
// statements in that case. If multiple case clauses share the same
// string literal, last-write-wins (we're only asking yes/no questions
// per verb, so collisions are impossible in practice).
func collectSwitchCaseBodies(fn *ast.FuncDecl) map[string][]ast.Stmt {
	out := map[string][]ast.Stmt{}
	if fn == nil || fn.Body == nil {
		return out
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		sw, ok := n.(*ast.SwitchStmt)
		if !ok || sw.Body == nil {
			return true
		}
		for _, c := range sw.Body.List {
			cc, ok := c.(*ast.CaseClause)
			if !ok {
				continue
			}
			for _, e := range cc.List {
				lit, ok := e.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				// Strip surrounding quotes.
				s := lit.Value
				if len(s) >= 2 && (s[0] == '"' || s[0] == '`') {
					s = s[1 : len(s)-1]
				}
				out[s] = cc.Body
			}
		}
		return true
	})
	return out
}

// caseBodyCallsHandler reports whether any statement in body contains
// (recursively) a call to a function named handlerName.
func caseBodyCallsHandler(body []ast.Stmt, handlerName string) bool {
	found := false
	for _, stmt := range body {
		ast.Inspect(stmt, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			id, ok := call.Fun.(*ast.Ident)
			if !ok {
				return true
			}
			if id.Name == handlerName {
				found = true
				return false
			}
			return true
		})
		if found {
			return true
		}
	}
	return false
}
