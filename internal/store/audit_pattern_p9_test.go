package store

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestPattern_P9_SecretLeaksInOutboundChannels verifies the P9 audit pattern:
// outbound surfaces (webhook_url, FORCE_OTEL_LOGS_URL, gh stderr) are
// unvalidated exfil channels with no central redaction. All three sub-tests
// are EXPECTED TO FAIL under the current code (the findings are open defects);
// once a redaction helper + URL allow-list land, these assertions flip.
//
// Findings:
//   - AUDIT-016: webhook_url has no scheme/host allow-list + follows redirects
//   - AUDIT-055: gh stderr is wrapped into returned errors unredacted
//   - AUDIT-056: webhook payload includes raw task payload, unredacted
//   - AUDIT-017/-057 are documented but not asserted here (env-var + OOM shape)
func TestPattern_P9_SecretLeaksInOutboundChannels(t *testing.T) {

	// ── Sub-test A: AUDIT-016 — webhook follows redirects to link-local ────
	// Stand up a test server playing the role of "cloud metadata". Set
	// webhook_url to it directly → assert the POST lands (no allow-list).
	// Then stand up a redirector that 302s to the metadata server → assert
	// the default http.Client follows the redirect (no CheckRedirect policy).
	t.Run("A_WebhookFollowsRedirectToLinkLocal", func(t *testing.T) {
		db := InitHolocronDSN(":memory:")
		defer db.Close()

		res, err := db.Exec(
			`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, created_at)
			 VALUES (0, 'repo', 'CodeEdit', 'Pending', 'harmless payload', datetime('now'))`,
		)
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		id64, _ := res.LastInsertId()
		taskID := int(id64)

		// Stand-in for 169.254.169.254 — any httptest server will do; the
		// point is that nothing in sendWebhook says "this host is RFC1918 /
		// link-local / loopback; refuse to POST."
		metadataHits := make(chan struct{}, 2)
		metadata := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			metadataHits <- struct{}{}
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusOK)
		}))
		defer metadata.Close()

		// Direct POST: no allow-list means we reach the (pretend) metadata host.
		SetConfig(db, "webhook_url", metadata.URL)
		FireWebhook(db, taskID, "Completed")

		select {
		case <-metadataHits:
			// Current (broken) behavior: webhook reached link-local stand-in.
			t.Errorf("AUDIT-016: webhook POST reached %s with no scheme/host allow-list. "+
				"A malicious or misconfigured webhook_url can exfil task payloads to "+
				"http://169.254.169.254/... (cloud metadata). Fix: validate scheme/host "+
				"against an RFC1918/link-local/loopback blocklist before POSTing.",
				metadata.URL)
		case <-time.After(3 * time.Second):
			// Would be the desired post-fix state: allow-list blocked the POST.
			t.Log("webhook POST was blocked — allow-list appears to be in place")
		}

		// Redirector: 302 → metadata. Default http.Client follows up to 10
		// redirects with no CheckRedirect, so the final POST still lands.
		redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, metadata.URL, http.StatusFound)
		}))
		defer redirector.Close()

		SetConfig(db, "webhook_url", redirector.URL)
		FireWebhook(db, taskID, "Completed")

		select {
		case <-metadataHits:
			t.Errorf("AUDIT-016: webhook followed a 302 redirect from %s to %s. "+
				"FireWebhook uses a default http.Client with no CheckRedirect policy, "+
				"so an attacker who controls any allowed host can bounce us to "+
				"link-local. Fix: install a CheckRedirect that re-validates every hop.",
				redirector.URL, metadata.URL)
		case <-time.After(3 * time.Second):
			t.Log("302 redirect was not followed — CheckRedirect policy appears to be in place")
		}
	})

	// ── Sub-test B: AUDIT-056 — webhook payload is not redacted ────────────
	// The task payload goes out verbatim (truncated to 500 chars). A payload
	// containing what looks like a GitHub PAT is POSTed unchanged.
	t.Run("B_WebhookBodyLeaksTokens", func(t *testing.T) {
		db := InitHolocronDSN(":memory:")
		defer db.Close()

		const fakeToken = "ghp_testFakeTokenABC123"
		payload := "please update the helm chart; token=" + fakeToken + " thanks"

		res, err := db.Exec(
			`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, created_at)
			 VALUES (0, 'repo', 'CodeEdit', 'Pending', ?, datetime('now'))`,
			payload,
		)
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		id64, _ := res.LastInsertId()

		received := make(chan []byte, 1)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			received <- body
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		SetConfig(db, "webhook_url", srv.URL)
		FireWebhook(db, int(id64), "Completed")

		select {
		case body := <-received:
			if strings.Contains(string(body), fakeToken) {
				t.Errorf("AUDIT-056: webhook POST body contained the raw fake PAT %q. "+
					"FireWebhook ships the first 500 chars of BountyBoard.payload with no "+
					"redaction pass. Operator-pasted tokens, Claude stdout echoing secrets, "+
					"and PR-review-comment bodies all exfil. Fix: route payload through "+
					"a central store.RedactSecrets helper (ghp_/gho_/ghu_/ghs_/github_pat_/"+
					"Bearer/url-basic-auth) before marshaling. Body=%q", fakeToken, string(body))
			} else {
				t.Log("webhook body redacted the fake PAT — redaction helper appears to be in place")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for webhook POST")
		}
	})

	// ── Sub-test C: AUDIT-055 — gh stderr wrapped unredacted ───────────────
	// Static source check: gh.go wraps stderr straight into the returned
	// error via `fmt.Errorf("...: %w: %s", err, strings.TrimSpace(string(stderr)))`.
	// Assert there is no redaction regex applied to stderr (ghp_, gho_, etc.)
	// anywhere in the file before that wrap happens.
	t.Run("C_GhStderrNotRedacted", func(t *testing.T) {
		// Locate gh.go relative to this test file.
		_, thisFile, _, ok := runtime.Caller(0)
		if !ok {
			t.Fatal("runtime.Caller failed")
		}
		repoRoot := filepath.Dir(filepath.Dir(filepath.Dir(thisFile))) // .../internal/store -> repo root
		ghPath := filepath.Join(repoRoot, "internal", "gh", "gh.go")

		srcBytes, err := os.ReadFile(ghPath)
		if err != nil {
			t.Fatalf("read %s: %v", ghPath, err)
		}
		src := string(srcBytes)

		// Confirm the file actually wraps stderr into returned errors — this
		// is the exfil vector we care about. If the file stops doing this,
		// the audit lens shifts and the test should be revisited.
		if !strings.Contains(src, "string(stderr)") {
			t.Fatalf("AUDIT-055 sanity: gh.go no longer contains `string(stderr)` "+
				"in any error wrap — the file shape changed; update this test. Path=%s", ghPath)
		}

		// Any of these patterns applied to stderr would count as a fix:
		//   - a regex that matches token prefixes (ghp_, gho_, ghu_, ghs_, github_pat_, Bearer)
		//   - a call to a redaction helper (RedactSecrets, redactSecrets, redact())
		tokenRegexCandidates := []*regexp.Regexp{
			regexp.MustCompile(`ghp_|gho_|ghu_|ghs_|github_pat_`),
			regexp.MustCompile(`(?i)Bearer\s`),
		}
		redactorNameCandidates := []string{
			"RedactSecrets", "redactSecrets", "redactStderr", "scrubStderr", "scrub(",
		}

		redactionFound := false
		for _, re := range tokenRegexCandidates {
			if re.MatchString(src) {
				redactionFound = true
				break
			}
		}
		if !redactionFound {
			for _, name := range redactorNameCandidates {
				if strings.Contains(src, name) {
					redactionFound = true
					break
				}
			}
		}

		// Count the number of fmt.Errorf wraps that interpolate stderr — this
		// quantifies the blast radius for the failure message.
		wrapCount := strings.Count(src, "string(stderr)")

		if !redactionFound {
			t.Errorf("AUDIT-055: internal/gh/gh.go wraps `stderr` into %d returned errors via "+
				"`fmt.Errorf(\"...: %%w: %%s\", err, strings.TrimSpace(string(stderr)))` with no "+
				"redaction regex or helper applied before the wrap. `gh` auth-failure stderr can "+
				"contain ghp_/gho_/ghu_/ghs_/github_pat_ token prefixes and URL-embedded basic "+
				"auth; those errors land in BountyBoard.error_log, Escalations.message, and "+
				"Fleet_Mail.body — all visible via the unauth dashboard. "+
				"Fix: redact stderr inside ExecRunner.Run (or via a central store.RedactSecrets) "+
				"before returning. Path=%s", wrapCount, ghPath)
		} else {
			t.Log("a redaction regex / helper was found in gh.go — AUDIT-055 appears addressed")
		}
	})

	// Keep atomic imported for potential future sub-test expansion; silences unused.
	var _ atomic.Int32
}
