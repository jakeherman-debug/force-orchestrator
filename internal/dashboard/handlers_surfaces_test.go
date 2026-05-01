package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// TestDashboardThreeSurfaceNav — D3 P6A.1 — verifies the three top-level
// surface page handlers are wired and render the expected <title> + body.
//
// The brief's smoke test is curl-based:
//
//	curl /pulse      | grep -q "<title>Pulse"
//	curl /briefing   | grep -q "<title>Briefing"
//	curl /reflection | grep -q "<title>Reflection"
//	curl /           | grep -q "Pulse"
//
// We exercise the handlers directly via httptest so the test does not need
// to bind a port (CLAUDE.md: tests must run in CI without network).
func TestDashboardThreeSurfaceNav(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	cases := []struct {
		name        string
		handler     http.HandlerFunc
		wantTitleIn string
		wantBodyIn  string
	}{
		{"pulse", handlePulsePage(db), "<title>Pulse", "Pulse"},
		{"briefing", handleBriefingPage(db), "<title>Briefing", "Briefing"},
		{"reflection", handleReflectionPage(db), "<title>Reflection", "Reflection"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/"+tc.name, nil)
			rr := httptest.NewRecorder()
			tc.handler(rr, req)

			if rr.Code != http.StatusOK {
				t.Fatalf("got status %d, want %d", rr.Code, http.StatusOK)
			}
			body := rr.Body.String()
			if !strings.Contains(body, tc.wantTitleIn) {
				t.Fatalf("body missing %q; got:\n%s", tc.wantTitleIn, body)
			}
			if !strings.Contains(body, tc.wantBodyIn) {
				t.Fatalf("body missing %q; got:\n%s", tc.wantBodyIn, body)
			}
			ct := rr.Header().Get("Content-Type")
			if !strings.Contains(ct, "text/html") {
				t.Fatalf("got Content-Type %q, want text/html", ct)
			}
		})
	}
}

// TestDashboardThreeSurfaceNav_RootMentionsPulse — verifies the SPA shell
// (index.html, served at `/` by the static FileServer) mentions Pulse so
// the curl-based smoke test grep `curl / | grep -q Pulse` passes.
func TestDashboardThreeSurfaceNav_RootMentionsPulse(t *testing.T) {
	// We don't have an easy way to exercise the full mux from outside
	// RunDashboard without binding a port. Instead, read the embedded
	// static index.html and assert it contains "Pulse" in the body —
	// which is what the FileServer will serve at `/`.
	sub, err := staticFiles.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("read embedded index.html: %v", err)
	}
	body := string(sub)
	for _, want := range []string{"Pulse", "Briefing", "Reflection", "surface-nav", "surface-pulse-pane"} {
		if !strings.Contains(body, want) {
			t.Errorf("index.html missing %q (D3 P6A.1 nav rebuild)", want)
		}
	}
}

// TestDashboardThreeSurfaceNav_SecurityHeaders — verifies that the surface
// page handlers participate in the existing securityMiddleware chain (i.e.,
// the brief's "no new http.Server, attach to existing mux" invariant).
//
// We can't easily test middleware composition without spinning up the full
// server, but we can assert each handler emits a CSP-clean shell (no
// inline `<script>` tags) so the page works under the existing CSP.
func TestDashboardThreeSurfaceNav_NoInlineScripts(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	for _, h := range []http.HandlerFunc{
		handlePulsePage(db),
		handleBriefingPage(db),
		handleReflectionPage(db),
	} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rr := httptest.NewRecorder()
		h(rr, req)
		body := rr.Body.String()
		if strings.Contains(body, "<script") {
			t.Errorf("surface shell contains inline <script> — violates existing CSP. body:\n%s", body)
		}
	}
}
