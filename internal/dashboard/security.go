// Package dashboard — security middleware (Fix #2 / AUDIT-001, -002, -003, -053, -054, -064).
//
// The dashboard binds 127.0.0.1 only. Every response sets a restrictive
// Content-Security-Policy and cache-hardening headers. Every mutating
// method (POST / PUT / PATCH / DELETE) is gated by an Origin/Referer
// allow-list AND a 256 KB body size cap. This blocks the attack chain
// the audit describes: a drive-by page at evil.example doing
// `fetch('http://127.0.0.1:8080/api/control/estop',{method:'POST'})`.
//
// Why the allow-list is strict: the dashboard has no auth. It's a single-
// user local tool. Origin == scheme + host + port, no path, no query.
// Requests from the file:// scheme (protocol-less fetch from a saved html
// file) and about:blank produce `Origin: null` — we reject those too.
// A genuine same-origin page XHR/fetch always produces the correct Origin.
//
// Why Referer is a fallback: older Safari and some CLI tools omit Origin
// on same-origin non-cross-origin requests. We accept a Referer that
// matches our allow-list when Origin is absent.
//
// Why 256 KB: the largest legitimate payload is a task `payload` field,
// capped by the UI at a few KB. A 256 KB ceiling leaves headroom for
// long descriptions while slamming the door on the `curl -d @/dev/zero`
// DoS AUDIT-054 describes.

package dashboard

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// maxRequestBodyBytes caps every mutating request body.
// 256 KB = 262144 bytes — generous for any legitimate dashboard payload,
// small enough that a malicious origin can't bury the daemon under one POST.
const maxRequestBodyBytes int64 = 256 << 10

// loopbackBindAddr returns the bind address for the dashboard.
// Always loopback — see CLAUDE.md "Dashboard invariants".
// If an operator ever needs remote access, the correct path is an SSH
// tunnel (`ssh -L 8080:localhost:8080`), not changing this bind.
func loopbackBindAddr(port int) string {
	return fmt.Sprintf("127.0.0.1:%d", port)
}

// allowedOriginsForPort returns the set of origins that count as
// "same-origin" for a dashboard bound to loopback on `port`.
// Both 127.0.0.1 and localhost resolve to the loopback; a user might type
// either in the browser.
func allowedOriginsForPort(port int) map[string]struct{} {
	return map[string]struct{}{
		fmt.Sprintf("http://127.0.0.1:%d", port): {},
		fmt.Sprintf("http://localhost:%d", port): {},
	}
}

// originAllowed reports whether the given Origin header value is permitted
// for a mutating request to a dashboard bound on `port`. Empty origin is
// NOT automatically allowed — callers fall back to Referer checking.
func originAllowed(origin string, port int) bool {
	if origin == "" || origin == "null" {
		return false
	}
	_, ok := allowedOriginsForPort(port)[strings.TrimRight(origin, "/")]
	return ok
}

// refererAllowed reports whether the given Referer URL is scheme/host/port
// match for our loopback allow-list. Referer carries the full URL; we only
// care about the origin prefix.
func refererAllowed(referer string, port int) bool {
	if referer == "" {
		return false
	}
	u, err := url.Parse(referer)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return false
	}
	origin := u.Scheme + "://" + u.Host
	return originAllowed(origin, port)
}

// isMutatingMethod reports whether the HTTP method is state-changing.
// Covers the full set the dashboard uses today (POST + DELETE) plus PUT/PATCH
// as a defence in depth for any future handler that adopts them.
func isMutatingMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// setSecurityHeaders applies the security headers to every response.
// Content-Security-Policy closes AUDIT-002 by preventing inline/remote
// script execution even if an XSS sink is reintroduced in future.
// 'self' is enough because everything is bundled under static/.
// 'unsafe-inline' is included ONLY in style-src because a few existing
// markup nodes (e.g. the banner) use inline style attributes; if those
// get cleaned up later, this can be tightened to style-src 'self'.
func setSecurityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy",
		"default-src 'self'; "+
			"script-src 'self'; "+
			"style-src 'self' 'unsafe-inline'; "+
			"img-src 'self' data:; "+
			"connect-src 'self'; "+
			"base-uri 'self'; "+
			"form-action 'self'; "+
			"frame-ancestors 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
}

// writeBodyReadError translates a body-read error into the right HTTP status.
// Callers pass the err returned from json.NewDecoder(r.Body).Decode(...) or
// io.ReadAll(r.Body). If the error is an *http.MaxBytesError (the body
// overshot our 256 KB cap), return 413 Request Entity Too Large. Otherwise
// return 400 Bad Request. Returns true if it wrote a response, false if err
// was nil.
func writeBodyReadError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		http.Error(w,
			fmt.Sprintf(`{"error":"request body exceeds %d-byte limit"}`, maxRequestBodyBytes),
			http.StatusRequestEntityTooLarge)
		return true
	}
	http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
	return true
}

// securityMiddleware is the outer gate on every request.
// It:
//  1. stamps security response headers,
//  2. on mutating methods (POST/PUT/PATCH/DELETE), enforces the Origin
//     allow-list (Referer as fallback) and caps the body at 256 KB.
//
// Any request that fails the Origin check is refused with 403 BEFORE the
// handler executes — no DB read, no cost, no side effects. Any body that
// exceeds 256 KB is caught at Read time by http.MaxBytesReader and the
// handler will receive an error from its json.Decode / io.ReadAll call,
// which it then translates to 413 via maxBytesHandled below.
func securityMiddleware(port int, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w)

		if isMutatingMethod(r.Method) {
			origin := r.Header.Get("Origin")
			referer := r.Header.Get("Referer")
			if !originAllowed(origin, port) && !refererAllowed(referer, port) {
				// AUDIT-001: a malicious page at evil.example doing
				// fetch('http://127.0.0.1:PORT/api/...', {method:'POST'})
				// has Origin: http://evil.example — reject.
				http.Error(w,
					`{"error":"forbidden: mutating requests require same-origin Origin or Referer"}`,
					http.StatusForbidden)
				return
			}
			// AUDIT-054: cap the body before any handler reads from r.Body.
			r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		}

		next.ServeHTTP(w, r)
	})
}
