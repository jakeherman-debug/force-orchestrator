// Package audittools: pattern test for the per-agent capability profile
// invariant introduced in D1 Track T0-1 (CLAUDE.md "Per-agent capability
// profiles"). Pattern P13 enforces, at CI time, that:
//
//  1. Every AskClaudeCLI / AskClaudeCLIContext / RunCLI /
//     RunCLIStreaming / RunCLIStreamingContext call site in production
//     code sources its tool args from a *capabilities.Profile (via
//     AllowedToolsArg / DisallowedToolsArg / MCPConfigArg), not from a
//     hardcoded string literal or a const reference.
//
//  2. Every agent name passed to capabilities.LoadProfile in production
//     code has a corresponding YAML file in agents/capabilities/.
//
//  3. Every YAML profile in agents/capabilities/ validates against the
//     registry + blocklist (delegates to the loader's own validator).
//
//  4. The .forceblocklist.yaml entries are NOT granted by any per-agent
//     profile (delegates to the loader's blocklist enforcement).
//
//  5. The REGISTRY.yaml is internally consistent (every namespace
//     expansion is present in mcp_tools).
//
// Invariants 3-5 are largely covered by the loader's own unit tests,
// but P13 re-verifies them at the CI layer so the contract is visible
// in the same place a reviewer would look for "does this regression
// stand up?"
//
// Pattern P13 graduates to a BoS commit-time rule when D4 ships,
// alongside the cross-agent service-interface rule (Pattern P16).
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

	"force-orchestrator/internal/agents/capabilities"
)

// p13ClaudeCallNames is the set of LLM-invocation entry points whose
// tool args MUST come from a profile. Any new entry point that calls
// `claude` MUST be added to this list AND have its call sites
// audited.
var p13ClaudeCallNames = map[string]struct{}{
	"AskClaudeCLI":           {},
	"AskClaudeCLIContext":    {},
	"RunCLI":                 {},
	"RunCLIStreaming":        {},
	"RunCLIStreamingContext": {},
}

// p13Allowlist names files where a Claude call site is allowed to
// pass non-profile tool args. The loader itself (internal/agents/
// capabilities/) and the claude package's own internals are the only
// legitimate cases.
//
// Each entry MUST carry a one-line rationale per the CLAUDE.md
// allowlist-truthfulness invariant — adding a third entry without a
// rationale is rejected by TestPattern_P13_AllowlistReasonsTruthful.
var p13Allowlist = map[string]string{
	// internal/claude/claude.go is the LLM call layer itself — its
	// AskClaudeCLI / RunCLI helpers are the implementation, not call
	// sites of, the profile-sourced contract. Its internal helper
	// (ClassifyTaskType) takes the profile-sourced args from the
	// caller (Inquisitor), so even though there's an AskClaudeCLI
	// call in this file, its tool-arg sources are the function's own
	// parameters.
	"internal/claude/claude.go": "claude package internals: AskClaudeCLI / RunCLI helpers ARE the contract; ClassifyTaskType threads tool args from the caller's profile",

	// internal/clients/librarian/summarize_call.go wraps
	// AskClaudeCLIContext for the SummarizeForContextOverflow path
	// (D2 T1-2). The summarize is pure-reasoning — empty allowed/
	// disallowed/mcpConfig args ARE the contract. Routing through a
	// per-agent capability profile would add a YAML for an internal
	// client helper that has no tool surface, defeating the purpose
	// of the profile system.
	"internal/clients/librarian/summarize_call.go": "librarian client internals: SummarizeForContextOverflow is pure-reasoning, the empty-tool args ARE the contract — a profile here would carve no surface and add maintenance",
}

// TestPattern_P13_CapabilityProfiles is the D1 / Pattern P13 regression.
// Walks production code (cmd/, internal/) and fails if any allowed
// LLM call site:
//   - has a string-literal tool arg
//   - references a deleted constant (CouncilTools, CommanderTools, etc.)
//   - references any constant that does not source from a
//     *capabilities.Profile
func TestPattern_P13_CapabilityProfiles(t *testing.T) {
	root := moduleRoot(t)

	// Phase 1: every production Claude call site is profile-sourced.
	type offender struct {
		file string
		line int
		call string
		why  string
	}
	var offenders []offender

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".build-worktrees" || name == ".force-worktrees" ||
				name == "vendor" || name == ".git" || name == "node_modules" ||
				name == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// Production files only — skip cmd/* and internal/* tests above.
		relPath := rel(root, path)
		if !strings.HasPrefix(relPath, "cmd/") && !strings.HasPrefix(relPath, "internal/") {
			return nil
		}
		// Allowlist: claude package internals and the loader itself.
		if _, ok := p13Allowlist[relPath]; ok {
			return nil
		}
		// The capabilities loader itself doesn't call AskClaudeCLI; skip
		// fully so a test against it doesn't get fooled by stray refs.
		if strings.HasPrefix(relPath, "internal/agents/capabilities/") {
			return nil
		}
		// agents/capabilities/ embed shim — also irrelevant.
		if strings.HasPrefix(relPath, "agents/capabilities/") {
			return nil
		}

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
			fnName := claudeCallName(call.Fun)
			if fnName == "" {
				return true
			}
			if _, watched := p13ClaudeCallNames[fnName]; !watched {
				return true
			}

			pos := fset.Position(call.Pos())
			toolArgs := profileToolArgs(call, fnName)
			for _, ta := range toolArgs {
				if !p13ArgIsProfileSourced(ta) {
					offenders = append(offenders, offender{
						file: relPath, line: pos.Line, call: fnName,
						why: "non-profile tool arg in " + fnName + ": " + describeArg(ta),
					})
					break
				}
			}
			return true
		})
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk: %v", walkErr)
	}

	// Phase 2: every LoadProfile("<name>") call references an existing
	// YAML profile. Walk both production code and the test layer for
	// LoadProfile invocations — tests that pass a profile-name to a
	// runner should be referencing real profiles too.
	loadProfileNames := collectLoadProfileNames(t, root)
	availableProfiles, err := capabilities.ListProfiles()
	if err != nil {
		t.Fatalf("ListProfiles: %v", err)
	}
	avail := map[string]struct{}{}
	for _, n := range availableProfiles {
		avail[n] = struct{}{}
	}
	var missing []string
	for name := range loadProfileNames {
		if _, ok := avail[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("Pattern P13 (D1 T0-1): %d agent name(s) referenced via LoadProfile have NO YAML in agents/capabilities/:", len(missing))
		for _, m := range missing {
			t.Errorf("  %s — add agents/capabilities/%s.yaml", m, m)
		}
	}

	// Phase 3: every YAML profile validates (LoadProfile returns no
	// error). Delegates to the loader's own validator; if any profile
	// granted a blocklisted tool or referenced an unknown namespace,
	// LoadProfile would error here.
	for _, name := range availableProfiles {
		if _, err := capabilities.LoadProfile(name); err != nil {
			t.Errorf("Pattern P13: profile %q failed validation: %v", name, err)
		}
	}

	if len(offenders) == 0 {
		return
	}
	sort.Slice(offenders, func(i, j int) bool {
		if offenders[i].file != offenders[j].file {
			return offenders[i].file < offenders[j].file
		}
		return offenders[i].line < offenders[j].line
	})
	t.Errorf("Pattern P13 (D1 T0-1): %d production Claude call site(s) source tool args from a non-profile expression. Use *capabilities.Profile.AllowedToolsArg() / .DisallowedToolsArg() / .MCPConfigArg() instead:", len(offenders))
	for _, o := range offenders {
		t.Errorf("  %s:%d — %s\n      %s", o.file, o.line, o.call, o.why)
	}
	t.Errorf("\nFix: load the profile via capabilities.LoadProfile(\"<agent>\") at Spawn time, then pass profile.AllowedToolsArg() / profile.DisallowedToolsArg() / mcpConfig (from profile.MCPConfigArg()) to the Claude call.")
}

// claudeCallName returns the unqualified function name of a call
// expression if it matches `<pkg>.AskClaudeCLI` etc., or "" otherwise.
func claudeCallName(fn ast.Expr) string {
	switch e := fn.(type) {
	case *ast.SelectorExpr:
		return e.Sel.Name
	case *ast.Ident:
		return e.Name
	}
	return ""
}

// profileToolArgs returns the (allowedTools, disallowedTools, mcpConfig)
// argument expressions for a Claude call. The position of the tool args
// depends on the function:
//   - AskClaudeCLI(systemPrompt, userPrompt, allowedTools, disallowedTools, mcpConfig, maxTurns)
//     → args[2..4]
//   - AskClaudeCLIContext(ctx, systemPrompt, userPrompt, allowedTools, disallowedTools, mcpConfig, maxTurns)
//     → args[3..5]
//   - RunCLI(ctx, prompt, allowedTools, disallowedTools, mcpConfig, dir, maxTurns, timeout)
//     → args[2..4]
//   - RunCLIStreaming(prompt, allowedTools, disallowedTools, mcpConfig, dir, maxTurns, timeout, w)
//     → args[1..3]
//   - RunCLIStreamingContext(ctx, prompt, allowedTools, disallowedTools, mcpConfig, dir, maxTurns, timeout, w)
//     → args[2..4]
func profileToolArgs(call *ast.CallExpr, fnName string) []ast.Expr {
	args := call.Args
	var lo int
	switch fnName {
	case "AskClaudeCLI":
		// args = (sys, user, allowed, disallowed, mcp, maxTurns)
		lo = 2
	case "AskClaudeCLIContext":
		// args = (ctx, sys, user, allowed, disallowed, mcp, maxTurns)
		lo = 3
	case "RunCLI":
		// args = (ctx, prompt, allowed, disallowed, mcp, dir, maxTurns, timeout)
		lo = 2
	case "RunCLIStreamingContext":
		// args = (ctx, prompt, allowed, disallowed, mcp, dir, maxTurns, timeout, w)
		lo = 2
	case "RunCLIStreaming":
		// args = (prompt, allowed, disallowed, mcp, dir, maxTurns, timeout, w)
		lo = 1
	default:
		return nil
	}
	if len(args) < lo+3 {
		return nil
	}
	return args[lo : lo+3]
}

// p13ArgIsProfileSourced reports whether a tool-arg expression is
// either (a) a method call on a *capabilities.Profile that returns
// the profile's tool args, or (b) a local variable assigned from
// such a method call (the production wrapper for MCPConfigArg
// pattern: `mcpConfig, _ := profile.MCPConfigArg()` then pass
// mcpConfig).
func p13ArgIsProfileSourced(arg ast.Expr) bool {
	switch e := arg.(type) {
	case *ast.CallExpr:
		// Methods we treat as profile-sourced:
		//   profile.AllowedToolsArg()
		//   profile.DisallowedToolsArg()
		//   profile.MCPConfigArg()
		if sel, ok := e.Fun.(*ast.SelectorExpr); ok {
			switch sel.Sel.Name {
			case "AllowedToolsArg", "DisallowedToolsArg", "MCPConfigArg":
				return true
			}
		}
		return false
	case *ast.Ident:
		// Local variables — we trust that the production code wraps
		// the profile call result into a local before passing it
		// (common pattern: `mcpConfig, _ := profile.MCPConfigArg()`).
		// Locals are accepted iff their name matches a recognised
		// profile-sourced shape.
		return p13LocalNameIsProfileSourced(e.Name)
	}
	return false
}

// p13LocalNameIsProfileSourced returns true for variable names that
// are conventionally bound to profile-derived tool args. The list is
// short on purpose — tests cannot inspect actual data flow without
// a full analysis pass; we lean on naming convention to keep the
// surface manageable. The convention:
//   - mcpConfig (only valid binding for the MCPConfigArg slot)
//   - allowedTools / disallowedTools (one-step shims, rare in production)
//
// Anything else (`""`, `"Edit,Write"`, `claude.CouncilTools`, `tools`,
// etc.) is rejected.
func p13LocalNameIsProfileSourced(name string) bool {
	switch name {
	case "mcpConfig", "allowedTools", "disallowedTools":
		return true
	}
	return false
}

// describeArg pretty-prints an arg expression for an error message.
func describeArg(arg ast.Expr) string {
	switch e := arg.(type) {
	case *ast.BasicLit:
		return "literal " + e.Value
	case *ast.Ident:
		return "identifier " + e.Name
	case *ast.SelectorExpr:
		var sb strings.Builder
		writeSelector(&sb, e)
		return "selector " + sb.String()
	case *ast.BinaryExpr:
		return "binary expression (concatenation)"
	}
	return "non-profile expression"
}

func writeSelector(sb *strings.Builder, sel *ast.SelectorExpr) {
	if inner, ok := sel.X.(*ast.SelectorExpr); ok {
		writeSelector(sb, inner)
		sb.WriteString(".")
		sb.WriteString(sel.Sel.Name)
		return
	}
	if id, ok := sel.X.(*ast.Ident); ok {
		sb.WriteString(id.Name)
		sb.WriteString(".")
		sb.WriteString(sel.Sel.Name)
		return
	}
	sb.WriteString(sel.Sel.Name)
}

// collectLoadProfileNames walks the entire repo (production AND test
// files) and returns every string literal passed to
// capabilities.LoadProfile(...). Test references count: a test that
// passes a non-existent profile name to a runner is a P13 violation
// just as much as production code would be.
func collectLoadProfileNames(t *testing.T, root string) map[string]struct{} {
	t.Helper()
	names := map[string]struct{}{}
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".build-worktrees" || name == ".force-worktrees" ||
				name == "vendor" || name == ".git" || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		// Skip the loader package's own tests — they exercise negative
		// paths (LoadProfile of a deliberately-nonexistent agent).
		// Counting those as P13 references would mean inventing a
		// "definitely-not-a-real-agent.yaml" stub.
		relPath := rel(root, path)
		if strings.HasPrefix(relPath, "internal/agents/capabilities/") {
			return nil
		}
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
			fnName := claudeCallName(call.Fun)
			if fnName != "LoadProfile" && fnName != "mustLoadCapProfile" {
				return true
			}
			// LoadProfile takes the name as the only arg (non-helper) or
			// the second arg (mustLoadCapProfile(t, "<name>")).
			var litExpr ast.Expr
			if fnName == "LoadProfile" && len(call.Args) >= 1 {
				litExpr = call.Args[0]
			}
			if fnName == "mustLoadCapProfile" && len(call.Args) >= 2 {
				litExpr = call.Args[1]
			}
			if lit, ok := litExpr.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				name := strings.Trim(lit.Value, `"`)
				if name != "" {
					names[name] = struct{}{}
				}
			}
			return true
		})
		return nil
	})
	return names
}

// TestPattern_P13_AllowlistReasonsTruthful asserts every p13Allowlist
// entry carries a rationale longer than 20 chars and references either
// the file's role (helper / classifier / loader) OR why the call site
// can't take a profile. Mirrors the truthfulness check Pattern P11
// applies to its allowlist.
func TestPattern_P13_AllowlistReasonsTruthful(t *testing.T) {
	descriptors := []string{
		"loader", "internals", "helper", "classifier",
		"contract", "implementation", "wraps", "ARE the",
		"profile-sourced", "from the caller",
	}
	missing := []string{}
	for path, reason := range p13Allowlist {
		if len(reason) < 20 {
			missing = append(missing, path+": rationale too short ("+reason+")")
			continue
		}
		lower := strings.ToLower(reason)
		hit := false
		for _, d := range descriptors {
			if strings.Contains(lower, strings.ToLower(d)) {
				hit = true
				break
			}
		}
		if !hit {
			missing = append(missing, path+": "+reason)
		}
	}
	if len(missing) == 0 {
		return
	}
	sort.Strings(missing)
	t.Errorf("Pattern P13: %d allowlist entry(ies) lack a truthful rationale:", len(missing))
	for _, m := range missing {
		t.Errorf("  %s", m)
	}
	t.Errorf("\nA reason MUST name the file's role (e.g. loader internals, classifier helper, contract implementation) AND why this call site is structurally exempt from profile-sourcing.")
}
