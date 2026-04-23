package store

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type webhookPayload struct {
	ID         int    `json:"id"`
	Type       string `json:"type"`
	Status     string `json:"status"`
	TargetRepo string `json:"target_repo"`
	Payload    string `json:"payload"`
}

// FireWebhook posts a task status notification to the configured webhook_url.
// It is a no-op when webhook_url is not set. The HTTP call runs in a goroutine
// with a 5-second timeout so it never blocks the caller.
//
// Outbound-channel hardening (Fix #10):
//   - Destination URL validated via ValidateOutboundURL before each POST
//     (rejects RFC1918, link-local, loopback, cloud-metadata hosts, and
//     non-http(s) schemes). A malicious or misconfigured webhook_url
//     cannot exfil task payloads to 169.254.169.254 or an attacker-
//     controlled loopback service.
//   - http.Client.CheckRedirect re-validates the target host on every
//     30x hop — defeats the "set webhook_url to an allowed host that
//     302-redirects to internal metadata" SSRF variant.
//   - Task payload fed through RedactSecrets so ghp_/gho_/Bearer/
//     url-basic-auth tokens pasted into task bodies (Claude stdout, PR
//     review comments, operator prompts) don't leave the orchestrator.
func FireWebhook(db *sql.DB, id int, status string) {
	rawURL := GetConfig(db, "webhook_url", "")
	if rawURL == "" {
		return
	}
	if err := ValidateOutboundURL(rawURL); err != nil {
		// Silent drop — webhooks are best-effort and we don't want to
		// fail the enclosing task for a misconfiguration. The config
		// validator at write-time is the primary gate; this is defense
		// in depth in case a DB edit bypassed it.
		return
	}

	var taskType, targetRepo, payload string
	err := db.QueryRow(
		`SELECT type, target_repo, payload FROM BountyBoard WHERE id = ?`, id,
	).Scan(&taskType, &targetRepo, &payload)
	if err != nil {
		return
	}

	// Redact BEFORE truncation so a token that straddles the 500-char
	// cut-off is still scrubbed.
	payload = RedactSecrets(payload)
	const maxPayload = 500
	if len(payload) > maxPayload {
		payload = payload[:maxPayload] + "…"
	}

	body, err := json.Marshal(webhookPayload{
		ID:         id,
		Type:       taskType,
		Status:     status,
		TargetRepo: targetRepo,
		Payload:    payload,
	})
	if err != nil {
		return
	}

	webhookInFlight.Add(1)
	go func() {
		defer webhookInFlight.Done()
		client := &http.Client{
			Timeout: 5 * time.Second,
			// Revalidate every redirect hop. An attacker who controls any
			// allowed destination could otherwise 302 us to an internal
			// address after DNS resolution. Each hop's target is
			// re-checked against the same outbound allow-list.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return errors.New("webhook: too many redirects")
				}
				if err := ValidateOutboundURL(req.URL.String()); err != nil {
					return fmt.Errorf("webhook: redirect blocked: %w", err)
				}
				return nil
			},
		}
		client.Post(rawURL, "application/json", bytes.NewReader(body)) //nolint:errcheck
	}()
}

// webhookInFlight tracks fire-and-forget webhook goroutines launched
// by FireWebhook. Tests that swap lookupHostFn or allowLoopbackForTests
// MUST call WaitForWebhookDrain() before returning — otherwise the
// test's deferred restore races the goroutine's read.
var webhookInFlight sync.WaitGroup

// WaitForWebhookDrain blocks until every in-flight webhook POST has
// returned. Never used in production (FireWebhook is fire-and-forget);
// tests call this in their teardown to avoid racing the async POST.
func WaitForWebhookDrain() { webhookInFlight.Wait() }

// ── Outbound URL allow-list ──────────────────────────────────────────────────
//
// Shared between FireWebhook and the OTLP telemetry exporter. Centralising
// the rules means a fix here protects every outbound channel (AUDIT-016,
// AUDIT-017, AUDIT-056, AUDIT-057 live in the same P9 pattern).

// allowLoopbackForTests is set to true by tests that need to POST to
// httptest.NewServer() (which binds to 127.0.0.1). Never true in
// production: setting it is gated behind SetAllowLoopbackForTest, which
// only the test helper files call. Protected by outboundGlobalsMu
// because FireWebhook reads it from an async goroutine (via
// ValidateOutboundURL → checkIP) — a test cleanup that reset the var
// would otherwise race the reader under `-race`.
var (
	allowLoopbackForTests = false
	outboundGlobalsMu     sync.RWMutex
)

// SetAllowLoopbackForTest is an escape hatch so existing webhook_test.go
// tests that hit httptest.NewServer on 127.0.0.1 can keep running. The
// name deliberately ends in ForTest to make it obvious in a grep that a
// production code path calling this is a bug.
//
// Always call as `defer SetAllowLoopbackForTest(true)()` to restore the
// prior value.
func SetAllowLoopbackForTest(v bool) func() {
	outboundGlobalsMu.Lock()
	prev := allowLoopbackForTests
	allowLoopbackForTests = v
	outboundGlobalsMu.Unlock()
	return func() {
		outboundGlobalsMu.Lock()
		allowLoopbackForTests = prev
		outboundGlobalsMu.Unlock()
	}
}

// loopbackAllowed reports whether the loopback escape hatch is active.
// Cheap RLock-read; safe to call from the webhook goroutine.
func loopbackAllowed() bool {
	outboundGlobalsMu.RLock()
	v := allowLoopbackForTests
	outboundGlobalsMu.RUnlock()
	return v
}

// ValidateOutboundURL parses rawURL and rejects it if:
//   - the scheme is neither http nor https
//   - the host is empty
//   - the host literally resolves to loopback, link-local, private, or
//     multicast address (covers 127.0.0.1, ::1, 169.254.169.254 aka the
//     AWS/GCP metadata endpoint, 10/8, 172.16/12, 192.168/16, fc00::/7)
//
// When the host is a DNS name (not a literal IP), every A/AAAA record is
// resolved and each one checked against the same rules. A DNS name that
// resolves to a mix of public and private addresses is rejected — we
// never want the first-hop routing decision to depend on order.
//
// Returns nil if the URL passes every check. Use at config-write time and
// again before every outbound POST (defense in depth — the stored value
// may have been edited directly in holocron.db).
func ValidateOutboundURL(rawURL string) error {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return fmt.Errorf("parse url: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("url scheme %q not allowed (require http or https)", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return errors.New("url host is empty")
	}
	// If host parses as a literal IP, check it directly.
	if ip := net.ParseIP(host); ip != nil {
		return checkIP(ip, host)
	}
	// Otherwise resolve — every resolved address must be public.
	ips, lookupErr := currentLookupHostFn()(host)
	if lookupErr != nil {
		return fmt.Errorf("resolve host %s: %w", host, lookupErr)
	}
	if len(ips) == 0 {
		return fmt.Errorf("resolve host %s: no addresses", host)
	}
	for _, addr := range ips {
		ip := net.ParseIP(addr)
		if ip == nil {
			return fmt.Errorf("resolve host %s: unparseable address %q", host, addr)
		}
		if err := checkIP(ip, host); err != nil {
			return err
		}
	}
	return nil
}

// lookupHostFn is indirected so tests can pin resolutions without
// depending on the test host's DNS. Protected by lookupHostFnMu
// because FireWebhook reads it from an async goroutine — setting the
// var from a test cleanup while the goroutine is still resolving
// otherwise races the reader. Tests call SetLookupHostForTest to
// swap it under the mutex.
var (
	lookupHostFn   = net.LookupHost
	lookupHostFnMu sync.RWMutex
)

// currentLookupHostFn returns the currently-installed resolver, holding
// the read lock only long enough to snapshot the pointer. The returned
// closure is safe to call without the lock held.
func currentLookupHostFn() func(string) ([]string, error) {
	lookupHostFnMu.RLock()
	fn := lookupHostFn
	lookupHostFnMu.RUnlock()
	return fn
}

// SetLookupHostForTest swaps the DNS resolver under the lock so tests
// can pin resolutions without racing the async webhook goroutine.
// Always call as `defer SetLookupHostForTest(customFn)()` to restore.
func SetLookupHostForTest(fn func(string) ([]string, error)) func() {
	lookupHostFnMu.Lock()
	prev := lookupHostFn
	lookupHostFn = fn
	lookupHostFnMu.Unlock()
	return func() {
		lookupHostFnMu.Lock()
		lookupHostFn = prev
		lookupHostFnMu.Unlock()
	}
}

// checkIP rejects loopback, link-local (incl. metadata), private, multicast,
// and unspecified addresses. The caller passes the original host string
// only for the error message; the decision is based on ip.
//
// Link-local is ALWAYS rejected — the cloud metadata endpoint lives at
// 169.254.169.254 and exists in exactly this block. The loopback test
// allows an opt-in via SetAllowLoopbackForTest so httptest.NewServer-backed
// tests can still exercise the redaction + payload-build paths.
func checkIP(ip net.IP, host string) error {
	switch {
	case ip.IsLoopback():
		if loopbackAllowed() {
			return nil
		}
		return fmt.Errorf("outbound to loopback address %s (via %q) refused", ip, host)
	case ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast():
		return fmt.Errorf("outbound to link-local address %s (via %q) refused — cloud metadata endpoints live here", ip, host)
	case ip.IsPrivate():
		return fmt.Errorf("outbound to private RFC1918 address %s (via %q) refused", ip, host)
	case ip.IsMulticast():
		return fmt.Errorf("outbound to multicast address %s (via %q) refused", ip, host)
	case ip.IsUnspecified():
		return fmt.Errorf("outbound to unspecified address %s (via %q) refused", ip, host)
	}
	return nil
}
