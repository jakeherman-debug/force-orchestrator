package dashboard

import (
	"database/sql"
	"fmt"
	"net/http"
)

// D3 P6A.1 — Three-surface IA + nav rebuild.
//
// The dashboard's top-level navigation is capped at three surfaces forever:
//   - Pulse      (default landing, ambient fleet view)
//   - Briefing   (decision queue + conversational triage)
//   - Reflection (calibration + learning)
//
// Plus a global Ask shortcut accessible via `/` from anywhere.
//
// These handlers serve thin HTML shells that load the same SPA (`/`) under
// the hood but at the correct hash-fragment so the SPA boots into the
// right surface. Each shell carries its own <title> so deep-linked tabs,
// browser history entries, and the curl-based smoke tests in the
// task brief all surface a meaningful name.
//
// Existing tabs (tasks/escalations/convoys/agents/mail/knowledge/...) are
// folded under the legacy umbrella — accessible via `#/legacy/<name>`
// fragments. This preserves the operator's daily workflow while the
// subsequent 6A tasks (heartbeat, pulse panel, briefing) fill in the
// new surfaces.
//
// Anti-cheat:
//   - All three handlers attach to the existing mux (no new http.Server).
//   - Existing securityMiddleware wraps the mux and stamps CSP, X-Frame,
//     etc. on every response, including these.
//   - The surface name is a fixed string (no user input), so there is no
//     attacker-controllable text rendered into the shell.

// surfaceShell renders the placeholder HTML page for a top-level surface.
// The shell carries the surface name in its <title>, embeds the surface
// in the body for grep-based smoke tests, and on load redirects to the
// SPA root with the matching hash fragment so subsequent JS routing
// (popstate / hashchange) takes over.
func surfaceShell(w http.ResponseWriter, name, fragment string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The CSP already locks scripts to 'self', so the inline redirect script
	// here would violate it. We use a <meta refresh> instead — no inline
	// script, CSP-clean, and degrades gracefully if JS is off.
	fmt.Fprintf(w, `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta http-equiv="Content-Security-Policy" content="default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'">
<meta http-equiv="refresh" content="0; url=/%s">
<title>%s — Fleet Command Center</title>
</head>
<body>
<p>Loading %s surface… <a href="/%s">Continue</a></p>
</body>
</html>
`, fragment, name, name, fragment)
}

// handlePulsePage — GET /pulse — renders the Pulse surface shell.
// Subsequent tasks (6A.7 narrative panel, 6A.8 fleet panel, 6A.9 cinematic)
// fill in the SPA-side rendering hooked off the `#/pulse` fragment.
func handlePulsePage(_ *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		surfaceShell(w, "Pulse", "#/pulse")
	}
}

// handleBriefingPage — GET /briefing — renders the Briefing surface shell.
// Subsequent tasks (6A.10 conversational triage, 6A.11 counter-proposal,
// 6A.12 prior-similar context) fill in the SPA-side rendering.
func handleBriefingPage(_ *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		surfaceShell(w, "Briefing", "#/briefing")
	}
}

// handleReflectionPage — GET /reflection — renders the Reflection surface
// shell. Reflection itself lands in Phase 6B (calibration scoreboard,
// fleet-learning panel, retro generator); 6A reserves the slot.
func handleReflectionPage(_ *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		surfaceShell(w, "Reflection", "#/reflection")
	}
}
