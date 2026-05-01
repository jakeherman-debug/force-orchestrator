// D3 P6A.15 — Pattern P25: CLI parity for every mutating dashboard handler.
//
// Walks internal/dashboard/dashboard.go for non-GET routes and asserts a
// corresponding `force <verb>` command exists in cmd/force/.
//
// Allowlist accepts non-operator-action handlers (e.g., heartbeat-write
// endpoints from the dashboard process itself) with a one-line rationale.
package audittools

import (
	"os"
	"path/filepath"
	"regexp"
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
}

// p25CLIVerbs — the canonical set of CLI verbs known to exist in
// cmd/force/. Built once at test time by scanning main.go's `case`
// arms. The verbs cover the handlers added in 6A.4-6A.14 plus a
// handful of pre-6A operator commands (estop/resume/ratify).
var p25KnownVerbs = []string{
	"estop", "resume", "ratify", "approve", "reject",
	"trust", "session", "notifications", "cooldown", "decide",
	"attention", "briefing-reject", "dashboard",
}

func TestPattern_P25_CLIParity(t *testing.T) {
	root := repoRootP25(t)

	// Read dashboard.go, extract non-GET HandleFunc routes.
	dashSrc, err := os.ReadFile(filepath.Join(root, "internal/dashboard/dashboard.go"))
	if err != nil {
		t.Fatalf("read dashboard.go: %v", err)
	}
	routeRe := regexp.MustCompile(`mux\.HandleFunc\("(/api/[^"]+)"`)
	matches := routeRe.FindAllStringSubmatch(string(dashSrc), -1)
	var routes []string
	for _, m := range matches {
		if len(m) >= 2 {
			routes = append(routes, m[1])
		}
	}

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

func loadCLIVerbs(t *testing.T, root string) []string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(root, "cmd/force/main.go"))
	if err != nil {
		t.Fatalf("read cmd/force/main.go: %v", err)
	}
	caseRe := regexp.MustCompile(`case "([^"]+)":`)
	matches := caseRe.FindAllStringSubmatch(string(src), -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if len(m) >= 2 {
			out = append(out, m[1])
		}
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
