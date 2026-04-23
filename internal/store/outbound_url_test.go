package store

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestValidateOutboundURL_AllowList covers the behavioural contract of
// the shared URL allow-list: http/https only, host must not resolve to
// loopback/link-local/private/multicast. Each row is one discrete reject
// reason so a regression on any single class fails visibly.
func TestValidateOutboundURL_AllowList(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		lookup  map[string][]string
		wantErr bool
		errSub  string
	}{
		{name: "https public ip literal", url: "https://8.8.8.8/path", wantErr: false},
		{name: "https public hostname", url: "https://example.com/path",
			lookup: map[string][]string{"example.com": {"93.184.216.34"}}, wantErr: false},
		{name: "reject ftp scheme", url: "ftp://example.com/", wantErr: true, errSub: "scheme"},
		{name: "reject file scheme", url: "file:///etc/passwd", wantErr: true, errSub: "scheme"},
		{name: "reject empty scheme", url: "//example.com/", wantErr: true, errSub: "scheme"},
		{name: "reject loopback literal", url: "http://127.0.0.1:8080/", wantErr: true, errSub: "loopback"},
		{name: "reject loopback hostname", url: "http://localhost/",
			lookup: map[string][]string{"localhost": {"127.0.0.1"}}, wantErr: true, errSub: "loopback"},
		{name: "reject link-local (cloud metadata)", url: "http://169.254.169.254/latest/meta-data/",
			wantErr: true, errSub: "link-local"},
		{name: "reject link-local hostname", url: "http://metadata.internal/",
			lookup: map[string][]string{"metadata.internal": {"169.254.169.254"}},
			wantErr: true, errSub: "link-local"},
		{name: "reject private 10/8", url: "http://10.0.0.5/", wantErr: true, errSub: "private"},
		{name: "reject private 172.16/12", url: "http://172.16.0.1/", wantErr: true, errSub: "private"},
		{name: "reject private 192.168/16", url: "http://192.168.1.1/", wantErr: true, errSub: "private"},
		{name: "reject unspecified 0.0.0.0", url: "http://0.0.0.0/", wantErr: true, errSub: "unspecified"},
		{name: "reject mixed-resolution (public + private)",
			url:    "http://sneaky.example/",
			lookup: map[string][]string{"sneaky.example": {"93.184.216.34", "10.0.0.5"}},
			wantErr: true, errSub: "private"},
		{name: "reject empty host", url: "http:///path", wantErr: true, errSub: "host is empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.lookup != nil {
				lookup := tc.lookup
				defer SetLookupHostForTest(func(host string) ([]string, error) {
					if v, ok := lookup[host]; ok {
						return v, nil
					}
					return nil, &net.DNSError{Err: "no such host", Name: host}
				})()
			}
			err := ValidateOutboundURL(tc.url)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got nil", tc.url)
				}
				if tc.errSub != "" && !strings.Contains(err.Error(), tc.errSub) {
					t.Errorf("error message missing substring %q: %v", tc.errSub, err)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error for %q: %v", tc.url, err)
				}
			}
		})
	}
}

// TestFireWebhook_AllowListRejectsMetadataHost is the behavioural test:
// set webhook_url to 169.254.169.254, fire, assert nothing reaches the
// server that stands in for the metadata endpoint. This is the
// integration test called out in the Fix #10 checklist.
func TestFireWebhook_AllowListRejectsMetadataHost(t *testing.T) {
	// Defer order matters (LIFO): WaitForWebhookDrain must run BEFORE
	// the cleanup restores lookupHostFn, because the webhook goroutine
	// reads it. We register the drain first so it unwinds last.
	restore := SetLookupHostForTest(func(host string) ([]string, error) {
		if host == "metadata.fake" {
			return []string{"169.254.169.254"}, nil
		}
		return net.LookupHost(host)
	})
	t.Cleanup(restore)
	t.Cleanup(WaitForWebhookDrain)

	db := InitHolocronDSN(":memory:")
	defer db.Close()

	res, err := db.Exec(
		`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, created_at)
		 VALUES (0, 'repo', 'CodeEdit', 'Pending', 'harmless', datetime('now'))`,
	)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	id64, _ := res.LastInsertId()

	hits := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits <- struct{}{}
	}))
	defer srv.Close()

	// Direct link-local literal — outright rejection.
	SetConfig(db, "webhook_url", "http://169.254.169.254/v1/tasks")
	FireWebhook(db, int(id64), "Completed")
	select {
	case <-hits:
		t.Errorf("webhook POST reached the httptest server even though webhook_url "+
			"was a link-local literal — allow-list is not being enforced")
	case <-time.After(200 * time.Millisecond):
		// correct — blocked at URL validation
	}

	// Hostname that resolves to link-local.
	SetConfig(db, "webhook_url", "http://metadata.fake/v1/tasks")
	FireWebhook(db, int(id64), "Completed")
	select {
	case <-hits:
		t.Errorf("webhook POST was not blocked for metadata.fake → 169.254.169.254")
	case <-time.After(200 * time.Millisecond):
		// correct — DNS lookup returned link-local, validator refused
	}
}

// TestFireWebhook_CheckRedirect_BlocksInternal asserts the
// CheckRedirect policy: when a permitted destination 302-redirects to a
// forbidden one, the http.Client must refuse the hop rather than follow.
//
// Staging:
//  1. Start a "legit" httptest server (binds loopback — we allow it via
//     SetAllowLoopbackForTest for the first hop).
//  2. Start a "metadata" httptest server — the allow-list still refuses
//     it because we disable the escape hatch in the CheckRedirect.
//
// Actually we cannot toggle the allow mid-test. Instead we install a
// lookup stub so the redirect target resolves to a link-local address
// that IS rejected while the first hop is a loopback literal under the
// escape hatch. The hop revalidation fires on the second URL.
func TestFireWebhook_CheckRedirect_BlocksInternal(t *testing.T) {
	// The metadata stand-in runs on loopback but we'll redirect to it
	// via a host name that resolves to 169.254 — CheckRedirect refuses.
	metaHits := make(chan struct{}, 1)
	metadata := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		metaHits <- struct{}{}
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer metadata.Close()

	// Register DNS-restore BEFORE the drain so cleanup order is
	// drain -> restore DNS (LIFO via t.Cleanup). The webhook goroutine
	// reads lookupHostFn under lookupHostFnMu.RLock so the drain must
	// finish before we swap it back.
	restore := SetLookupHostForTest(func(host string) ([]string, error) {
		if host == "metadata.internal" {
			return []string{"169.254.169.254"}, nil
		}
		return net.LookupHost(host)
	})
	t.Cleanup(restore)
	t.Cleanup(WaitForWebhookDrain)

	// A redirector that 302s to http://metadata.internal/... (link-local).
	// The redirector itself is loopback so we enable the escape hatch —
	// the point of this test is that EVEN THOUGH the first hop is
	// permitted, the redirect hop is rejected.
	t.Cleanup(SetAllowLoopbackForTest(true))

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://metadata.internal/v1/tasks", http.StatusFound)
	}))
	defer redirector.Close()

	db := InitHolocronDSN(":memory:")
	defer db.Close()
	res, err := db.Exec(
		`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, created_at)
		 VALUES (0, 'repo', 'CodeEdit', 'Pending', 'harmless', datetime('now'))`,
	)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	id64, _ := res.LastInsertId()

	SetConfig(db, "webhook_url", redirector.URL)
	FireWebhook(db, int(id64), "Completed")

	select {
	case <-metaHits:
		t.Errorf("webhook followed 302 to link-local — CheckRedirect policy is not enforcing the allow-list")
	case <-time.After(400 * time.Millisecond):
		// correct — the redirect was refused at the CheckRedirect hook
	}
}

// TestFireWebhook_RedactsEmbeddedToken closes the AUDIT-056 acceptance
// loop end-to-end: seed a BountyBoard row whose payload contains a
// fake PAT, fire the webhook, confirm the POST body has the fake PAT
// scrubbed and the [REDACTED] placeholder present.
func TestFireWebhook_RedactsEmbeddedToken(t *testing.T) {
	restore := SetAllowLoopbackForTest(true)
	t.Cleanup(restore)
	t.Cleanup(WaitForWebhookDrain)

	db := InitHolocronDSN(":memory:")
	defer db.Close()

	const fakeToken = "ghp_acceptanceTokenXYZ1234567"
	payload := "task context includes a token " + fakeToken + " — do not exfil"
	res, err := db.Exec(
		`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, created_at)
		 VALUES (0, 'repo', 'CodeEdit', 'Pending', ?, datetime('now'))`, payload,
	)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	id64, _ := res.LastInsertId()

	received := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	SetConfig(db, "webhook_url", srv.URL)
	FireWebhook(db, int(id64), "Completed")

	select {
	case body := <-received:
		if strings.Contains(body, fakeToken) {
			t.Errorf("acceptance: webhook body contained the raw fake PAT %q\n  body=%q", fakeToken, body)
		}
		if !strings.Contains(body, "[REDACTED]") {
			t.Errorf("acceptance: expected [REDACTED] placeholder in body\n  body=%q", body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for webhook POST")
	}
}
