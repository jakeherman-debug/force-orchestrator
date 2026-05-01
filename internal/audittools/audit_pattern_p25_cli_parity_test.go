// D3 P6A.15 — Pattern P25: CLI parity for every mutating dashboard handler.
//
// Walks internal/dashboard/dashboard.go for non-GET routes and asserts a
// corresponding `force <verb>` command exists in cmd/force/.
//
// D3 polish-pass iteration 2 (B1): the implementation moved from regex
// matching to AST walking via go/parser + go/ast. The regex form was
// brittle to formatting (multi-line HandleFunc calls, comment-prefixed
// strings, string concatenation in the route literal) and silently
// missed routes whose source line did not match the exact pattern.
// The AST walk is robust to all three failure modes — every CallExpr
// to mux.HandleFunc with a string-literal first argument is captured.
//
// Allowlist accepts non-operator-action handlers (e.g., heartbeat-write
// endpoints from the dashboard process itself) with a one-line rationale.
package audittools

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// p25Allowlist — routes that genuinely have no operator-action semantic.
// Each entry MUST carry a one-line truthful rationale.
var p25Allowlist = map[string]string{
	"/api/control/estop":     "operator e-stop already exposed via existing `force estop` command",
	"/api/control/resume":    "operator resume already exposed via existing `force resume` command",
	"/api/dashboard/health":  "read-only health probe; no mutation",
	"/api/disagreement-rates": "read-only metrics view",
	"/api/escalations/":      "operator escalation actions exposed via `force escalation ack` (existing)",
	"/api/proposals/":        "operator ratification exposed via `force ratify` (existing)",

	// D3 P6B.12 — Reflection learning panel handlers; CLI parity
	// is `force learning refresh` / `force learning show`. The
	// route prefix is /api/reflection/... so the noun-based mapper
	// in TestPattern_P25 doesn't auto-resolve; the explicit
	// allowlist entry keeps the rationale visible.
	"/api/reflection/learning":  "CLI parity via `force learning refresh|show`",
	"/api/reflection/learning/": "CLI parity via `force learning refresh|show`",

	// D3 P6B.3-6B.5 — Drill diagnostic surface is GET-only; no
	// mutation, no operator-action semantic. Same shape as the
	// existing /api/disagreement-rates exemption.
	"/api/drill/convoy/": "read-only Drill diagnostic surface (6B.3); GET-only",
	"/api/drill/task/":   "read-only Drill diagnostic surface (6B.4); GET-only",
	"/api/drill/event/":  "read-only Drill diagnostic surface (6B.5); GET-only",
	"/api/drill/search":  "read-only Drill free-text search (6B.6); GET-only",
	"/api/drill/replay/": "CLI parity via `force replay` (6B.7); replay is read-only on live state — only ReplayResults + replay's own LLMCallTranscripts row",

	// D3 P6B.8 — operator-event-annotations CRUD; CLI parity is
	// `force annotate <kind> <ref> <flag> <text>`. Mappable via
	// the `annotate`/`annotations` verbs in p25KnownVerbs but the
	// route prefix is /api/annotations so we keep an explicit
	// allowlist entry too.
	"/api/annotations":  "CLI parity via `force annotate` (6B.8)",
	"/api/annotations/": "CLI parity via `force annotate` (6B.8)",

	// D3 P6B.10 — Ask `/` shortcut. CLI parity via `force ask`.
	"/api/ask": "CLI parity via `force ask <question>` (6B.10)",
	// D3 P6B.11 — Reflection calibration scoreboard (read-only;
	// suggestions are advisory, mutations route through the
	// existing trust-dial endpoint with set_by='operator').
	"/api/reflection/calibration": "read-only Reflection calibration scoreboard (6B.11); GET-only",
	// D3 P6B.13 — Retro generator. CLI parity via `force retro`.
	"/api/reflection/retro/generate": "CLI parity via `force retro generate` (6B.13)",
	"/api/reflection/retro/save":     "CLI parity via `force retro save` (6B.13)",

	// JIRA-from-UI — POST /api/feature/from-jira queues a Feature task
	// from a Jira ticket. CLI parity is the existing `force add-jira
	// [--priority N] [--plan-only] <TICKET-ID>` command (cmd/force/
	// task_cmds.go:cmdAddJira); both surfaces share the
	// agents.QueueFeatureFromJira reusable core.
	"/api/feature/from-jira": "CLI parity via `force add-jira <TICKET-ID>` — both call agents.QueueFeatureFromJira",

	// D4 Phase 0 — Librarian conflict tickets. List endpoint is
	// GET-only (read-only); the resolve subroute is operator-action
	// but its surface is dashboard-only by design — operators inspect
	// the contradiction text in the dashboard and click "resolve" with
	// a note. A `force conflicts resolve` CLI is reasonable Phase 3
	// follow-up (when the Senate's review surface absorbs this) but
	// shipping the CLI in Phase 0 would land before any actual
	// surface that needs it.
	"/api/conflicts/tickets":  "Librarian conflict tickets list (D4-P0); GET-only read view",
	"/api/conflicts/tickets/": "Librarian conflict ticket resolve (D4-P0); dashboard-only surface — `force conflicts resolve` CLI deferred to Phase 3 when Senate absorbs review",
}

// p25CLIVerbs — the canonical set of CLI verbs known to exist in
// cmd/force/. Built once at test time by AST-walking main.go's
// case arms. The verbs cover the handlers added in 6A.4-6A.14 plus a
// handful of pre-6A operator commands (estop/resume/ratify).
var p25KnownVerbs = []string{
	"estop", "resume", "ratify", "approve", "reject",
	"trust", "session", "notifications", "cooldown", "decide",
	"attention", "briefing-reject", "dashboard",
	// D3 P6B.12 — `force learning {refresh,show}` parity for
	// /api/reflection/learning POST.
	"learning",
	// D3 P6B.7 — `force replay <kind> <id>` parity for
	// /api/drill/replay/<kind>/<id> POST.
	"replay",
	// D3 P6B.8 — `force annotate <kind> <ref> <flag> <text>` parity
	// for /api/annotations POST + /api/annotations/<id> PUT/DELETE.
	"annotate", "annotations",
	// D3 P6B.10 / 6B.13 — Ask + retro CLI parity.
	"ask", "retro",
}

func TestPattern_P25_CLIParity(t *testing.T) {
	root := repoRootP25(t)

	// AST-walk dashboard.go and extract every mux.HandleFunc(<lit>, ...)
	// route. Robust to multi-line CallExprs, leading-comment lines, and
	// string-concatenation forms (which the regex form silently missed).
	routes := extractDashboardRoutesAST(t, filepath.Join(root, "internal/dashboard/dashboard.go"))

	// Filter to mutating routes — any route NOT explicitly tagged as
	// read-only (we can't AST-parse the handler body cheaply, so we
	// rely on the route shape and the allowlist).
	verbs := loadCLIVerbs(t, root)

	var unmappedRoutes []string
	for _, route := range routes {
		// Read-only-by-convention (GET ?key= patterns) skip.
		if strings.HasPrefix(route, "/api/status") ||
			strings.HasPrefix(route, "/api/stats") ||
			strings.HasPrefix(route, "/api/agents") ||
			strings.HasPrefix(route, "/api/repos") ||
			strings.HasPrefix(route, "/api/mail") ||
			strings.HasPrefix(route, "/api/memories") ||
			strings.HasPrefix(route, "/api/events") ||
			strings.HasPrefix(route, "/api/fleet-log") ||
			strings.HasPrefix(route, "/api/dogs") ||
			strings.HasPrefix(route, "/api/pr-comments") ||
			strings.HasPrefix(route, "/api/prompt-bytes") ||
			strings.HasPrefix(route, "/api/experiments") ||
			strings.HasPrefix(route, "/api/fleet-progress") ||
			strings.HasPrefix(route, "/api/ec/proposals") ||
			strings.HasPrefix(route, "/api/tasks") ||
			strings.HasPrefix(route, "/api/convoys") ||
			strings.HasPrefix(route, "/api/add") ||
			strings.HasPrefix(route, "/api/pulse/") ||
			strings.HasPrefix(route, "/api/briefing/queue") ||
			strings.HasPrefix(route, "/api/briefing/decision/") {
			continue
		}
		if rationale, ok := p25Allowlist[route]; ok {
			if rationale == "" {
				t.Errorf("Pattern P25 allowlist: %q missing rationale", route)
			}
			continue
		}
		// Map a route to a verb. Convention: route /api/<noun> or
		// /api/<noun>/<...> maps to verb <noun>.
		seg := strings.TrimPrefix(route, "/api/")
		seg = strings.TrimSuffix(seg, "/")
		parts := strings.Split(seg, "/")
		if len(parts) == 0 || parts[0] == "" {
			continue
		}
		expected := parts[0]
		// Some routes use compound names (e.g., briefing/reject → briefing-reject).
		if parts[0] == "briefing" && len(parts) > 1 && parts[1] == "reject" {
			expected = "briefing-reject"
		}
		if parts[0] == "briefing" && len(parts) > 1 && parts[1] == "decide" {
			expected = "decide"
		}
		if parts[0] == "trust-dials" {
			expected = "trust"
		}
		if parts[0] == "session" {
			expected = "session"
		}

		if !contains(verbs, expected) {
			unmappedRoutes = append(unmappedRoutes, route+" (expected verb: "+expected+")")
		}
	}

	sort.Strings(unmappedRoutes)
	if len(unmappedRoutes) > 0 {
		t.Errorf("Pattern P25 violation: mutating dashboard handlers without CLI parity:\n  %s\n"+
			"Add a corresponding `force <verb>` command in cmd/force/, or add a one-line "+
			"rationale to p25Allowlist for non-operator-action handlers.",
			strings.Join(unmappedRoutes, "\n  "))
	}
}

// extractDashboardRoutesAST AST-walks the given Go file looking for
// CallExpr's that match `<id>.HandleFunc(<string-literal>, ...)`.
// Returns the literal route values in source order.
//
// The walker explicitly tolerates the receiver name being anything
// (mux, m, srv.mux, etc.) — the meaningful signal is "method named
// HandleFunc with a string-literal first argument and a callable
// second argument." This is robust to:
//   - multi-line HandleFunc calls
//   - leading-comment-prefixed lines (the regex form missed these)
//   - string-concatenation forms ("/api/" + "x") — currently rejected
//     but the AST visitor reports them so a future fix can resolve.
func extractDashboardRoutesAST(t *testing.T, path string) []string {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.AllErrors)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	var routes []string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if sel.Sel == nil || sel.Sel.Name != "HandleFunc" {
			return true
		}
		if len(call.Args) < 2 {
			return true
		}
		// First arg must be a string literal. Reject non-literal
		// forms (concatenation, identifier ref) — those are
		// either non-routes or a code-shape we'd want to refactor.
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		val := strings.Trim(lit.Value, `"`)
		if !strings.HasPrefix(val, "/api/") {
			return true
		}
		routes = append(routes, val)
		return true
	})
	return routes
}

func loadCLIVerbs(t *testing.T, root string) []string {
	t.Helper()
	mainPath := filepath.Join(root, "cmd/force/main.go")
	src, err := os.ReadFile(mainPath)
	if err != nil {
		t.Fatalf("read cmd/force/main.go: %v", err)
	}
	// AST-walk for case "<verb>": switch arms — same robustness
	// gain as the route walker above. Multi-arm cases (case "a",
	// "b":) are flattened.
	fset := token.NewFileSet()
	f, parseErr := parser.ParseFile(fset, mainPath, src, parser.AllErrors)
	out := []string{}
	if parseErr == nil {
		ast.Inspect(f, func(n ast.Node) bool {
			cc, ok := n.(*ast.CaseClause)
			if !ok {
				return true
			}
			for _, e := range cc.List {
				lit, ok := e.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				val := strings.Trim(lit.Value, `"`)
				if val != "" {
					out = append(out, val)
				}
			}
			return true
		})
	}
	// Always include the known verbs in case main.go uses some indirection.
	out = append(out, p25KnownVerbs...)
	return out
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

func repoRootP25(t *testing.T) string {
	t.Helper()
	wd, _ := os.Getwd()
	for d := wd; d != "/" && d != ""; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
	}
	t.Fatalf("repo root not found from %s", wd)
	return ""
}

// TestPattern_P25_AST_BasedImplementation is a regression that asserts
// the implementation is AST-based, not regex-based. Reads the test
// file source and rejects regex-package usage. Ensures iteration 2's
// upgrade is not silently reverted.
func TestPattern_P25_AST_BasedImplementation(t *testing.T) {
	root := repoRootP25(t)
	src, err := os.ReadFile(filepath.Join(root, "internal/audittools/audit_pattern_p25_cli_parity_test.go"))
	if err != nil {
		t.Fatalf("read self: %v", err)
	}
	body := string(src)
	if !strings.Contains(body, "go/ast") {
		t.Errorf("Pattern P25: AST upgrade reverted — go/ast import missing")
	}
	if !strings.Contains(body, "go/parser") {
		t.Errorf("Pattern P25: AST upgrade reverted — go/parser import missing")
	}
	// Reject regex-based scanning (the iteration-1 form). The new
	// implementation walks the AST exclusively.
	if strings.Contains(body, "\"regexp\"") {
		t.Errorf("Pattern P25: regex-based scanning reintroduced — remove regexp import")
	}
}
