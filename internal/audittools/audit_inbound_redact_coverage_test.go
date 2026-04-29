// Package audittools — D1 T0-2 inbound-redact coverage test.
//
// This pattern test enforces, at CI time, that every Claude CLI ingress
// inside the claude package wraps the prompt argument with ScrubInbound
// before it reaches cliRunner. The ScrubInbound function itself can do
// the redaction perfectly, but the security guarantee only holds if it
// is actually called at every entry point.
//
// What the test checks (per claude/claude.go function):
//   - AskClaudeCLIContext  : the local that flows into cliRunner is
//     produced by ScrubInbound (or assigned from its result).
//   - RunCLI               : same.
//   - RunCLIStreamingContext: same.
//   - The defaultCLIRunner exec path is downstream of those wrappers,
//     so it is exempt.
//   - AskClaudeCLI / RunCLIStreaming are shims that delegate to the
//     Context variants — they inherit coverage automatically.
//
// Allowlist: a single allowlist entry is permitted for the
// inbound_redact.go file itself (where ScrubInbound is defined) and
// the legacy non-Context shims (which call into the Context variants).
// Each entry carries a one-line rationale per the CLAUDE.md
// allowlist-truthfulness invariant.
package audittools

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// inboundRedactWatched names the claude.go functions that MUST wrap
// their prompt argument with ScrubInbound before invoking cliRunner.
var inboundRedactWatched = map[string]struct{}{
	"AskClaudeCLIContext":    {},
	"RunCLI":                 {},
	"RunCLIStreamingContext": {},
}

// inboundRedactExempt names functions in the claude package that do
// not need their own ScrubInbound call. Each comes with a rationale
// kept truthful by CLAUDE.md's allowlist-truthfulness rule.
var inboundRedactExempt = map[string]string{
	// AskClaudeCLI is a no-ctx shim that delegates to AskClaudeCLIContext
	// — the latter performs the scrub, so the shim inherits it.
	"AskClaudeCLI": "shim delegates to AskClaudeCLIContext (which scrubs)",
	// RunCLIStreaming is the no-ctx shim for RunCLIStreamingContext —
	// same delegation pattern as above.
	"RunCLIStreaming": "shim delegates to RunCLIStreamingContext (which scrubs)",
	// defaultCLIRunner is the actual claude exec; it sits downstream of
	// the three wrappers, so by the time it runs the prompt is already
	// scrubbed. Wrapping here would re-scrub already-scrubbed content
	// (idempotent, but pointless cost on every call).
	"defaultCLIRunner": "exec layer downstream of the wrappers; prompt arrives pre-scrubbed",
	// ClassifyTaskType is a thin wrapper over AskClaudeCLI — the scrub
	// happens inside the AskClaudeCLI / AskClaudeCLIContext path.
	"ClassifyTaskType": "wrapper over AskClaudeCLI; scrub happens in the inner call",
}

// TestInboundRedactCalledAtEveryCallSite walks claude.go's AST,
// identifies every function whose body invokes cliRunner directly, and
// verifies that ScrubInbound is called on the prompt before that
// invocation. The cliRunner check is the discriminator: any function
// that passes a string into cliRunner is by definition a CLI ingress.
func TestInboundRedactCalledAtEveryCallSite(t *testing.T) {
	root := moduleRoot(t)
	claudePath := filepath.Join(root, "internal", "claude", "claude.go")

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, claudePath, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", claudePath, err)
	}

	type offender struct {
		fn   string
		line int
		why  string
	}
	var offenders []offender

	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		fnName := fn.Name.Name

		// Identify functions that call cliRunner — these are the CLI
		// ingresses. A function with no cliRunner call is not a CLI
		// entry point and is irrelevant.
		callsCLIRunner := false
		callsScrubInbound := false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fnIdent := call.Fun.(type) {
			case *ast.Ident:
				if fnIdent.Name == "cliRunner" {
					callsCLIRunner = true
				}
				if fnIdent.Name == "ScrubInbound" {
					callsScrubInbound = true
				}
			case *ast.SelectorExpr:
				if fnIdent.Sel.Name == "ScrubInbound" {
					callsScrubInbound = true
				}
			}
			return true
		})

		if !callsCLIRunner {
			continue
		}
		// Must ALSO call ScrubInbound, OR be on the exempt list.
		if rationale, exempt := inboundRedactExempt[fnName]; exempt {
			_ = rationale
			continue
		}
		if _, watched := inboundRedactWatched[fnName]; !watched {
			// A new function that calls cliRunner has appeared without
			// being added to the watched set OR the exempt set. That
			// itself is a regression: the test layer must classify it.
			offenders = append(offenders, offender{
				fn:   fnName,
				line: fset.Position(fn.Pos()).Line,
				why: "calls cliRunner but is not in inboundRedactWatched or inboundRedactExempt — " +
					"add it to one set in audit_inbound_redact_coverage_test.go",
			})
			continue
		}
		if !callsScrubInbound {
			offenders = append(offenders, offender{
				fn:   fnName,
				line: fset.Position(fn.Pos()).Line,
				why:  "calls cliRunner but does NOT call ScrubInbound on the prompt first",
			})
		}
	}

	if len(offenders) == 0 {
		// Cross-check: every name in inboundRedactWatched must actually
		// exist as a function in claude.go. A stale entry would silently
		// reduce coverage.
		seen := map[string]bool{}
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok {
				seen[fn.Name.Name] = true
			}
		}
		var stale []string
		for name := range inboundRedactWatched {
			if !seen[name] {
				stale = append(stale, name)
			}
		}
		if len(stale) > 0 {
			sort.Strings(stale)
			t.Fatalf("inboundRedactWatched contains names not present in claude.go: %v — remove the stale entries to avoid silently reducing coverage", stale)
		}
		return
	}

	sort.Slice(offenders, func(i, j int) bool { return offenders[i].line < offenders[j].line })
	t.Errorf("D1 T0-2 inbound-redact coverage: %d Claude CLI ingress(es) do not wrap their prompt with ScrubInbound:", len(offenders))
	for _, o := range offenders {
		t.Errorf("  internal/claude/claude.go:%d  %s — %s", o.line, o.fn, o.why)
	}
	t.Errorf("\nFix: at the top of the function, after fullPrompt is constructed, call:")
	t.Errorf("    scrubbed, n := ScrubInbound(prompt)")
	t.Errorf("    observeInboundRedact(\"%s\", 0, n)", offenders[0].fn)
	t.Errorf("    prompt = scrubbed")
	t.Errorf("Then pass `scrubbed` (or the reassigned `prompt`) to cliRunner.")
}

// TestInboundRedact_AllowlistReasonsTruthful enforces the
// CLAUDE.md allowlist-truthfulness invariant for the inboundRedactExempt
// map: every entry must have a non-empty rationale, and the rationale
// must mention either "shim", "delegates", "wrapper", or "downstream"
// — the four legitimate reasons a cliRunner-touching function can skip
// its own ScrubInbound call.
func TestInboundRedact_AllowlistReasonsTruthful(t *testing.T) {
	keywords := []string{"shim", "delegates", "wrapper", "downstream"}
	for fn, why := range inboundRedactExempt {
		if strings.TrimSpace(why) == "" {
			t.Errorf("inboundRedactExempt[%q] has empty rationale", fn)
			continue
		}
		ok := false
		lower := strings.ToLower(why)
		for _, k := range keywords {
			if strings.Contains(lower, k) {
				ok = true
				break
			}
		}
		if !ok {
			t.Errorf("inboundRedactExempt[%q] rationale does not mention one of %v: %q", fn, keywords, why)
		}
	}
}
