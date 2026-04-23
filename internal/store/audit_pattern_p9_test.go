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
	// Fix #10 has landed: RedactSecrets + webhook allow-list + CheckRedirect
	// are all wired. Sub-tests below assert the post-fix behaviour directly.
	// Closed by: Fix #10.

	// ── Sub-test A: AUDIT-016 — webhook follows redirects to link-local ────
	// Post-fix assertion (Fix #10): FireWebhook refuses to POST to loopback
	// or link-local hosts. The httptest server here stands in for
	// 169.254.169.254; the allow-list refuses it AND refuses a 302-redirect
	// hop to the same host via CheckRedirect. We do NOT enable the
	// SetAllowLoopbackForTest escape hatch — the whole point of this
	// sub-test is that the production allow-list is effective.
	t.Run("A_WebhookFollowsRedirectToLinkLocal", func(t *testing.T) {
		// Closed by: Fix #10.
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

		// Stand-in for 169.254.169.254 — the test server binds to 127.0.0.1
		// which is loopback; ValidateOutboundURL refuses both loopback and
		// link-local for the same reason (cloud-metadata / daemon-local
		// exfil vectors). Either rejection class counts as "the fix works."
		metadataHits := make(chan struct{}, 2)
		metadata := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			metadataHits <- struct{}{}
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusOK)
		}))
		defer metadata.Close()

		// Direct POST: allow-list must block it before the POST goroutine
		// even reaches the HTTP client.
		SetConfig(db, "webhook_url", metadata.URL)
		FireWebhook(db, taskID, "Completed")

		select {
		case <-metadataHits:
			t.Errorf("AUDIT-016 regression: webhook POST reached %s. "+
				"ValidateOutboundURL should reject loopback/link-local.",
				metadata.URL)
		case <-time.After(300 * time.Millisecond):
			// Desired: allow-list blocked the POST before it happened.
		}

		// Redirector: 302 → metadata. CheckRedirect must revalidate the
		// target on each hop. Even if the first URL were allowed (e.g.,
		// a legitimate public host bouncing to an attacker-controlled
		// redirect), the second hop back to 127.0.0.1 is refused.
		redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, metadata.URL, http.StatusFound)
		}))
		defer redirector.Close()

		SetConfig(db, "webhook_url", redirector.URL)
		FireWebhook(db, taskID, "Completed")

		select {
		case <-metadataHits:
			t.Errorf("AUDIT-016 regression: webhook followed a 302 from %s to %s. "+
				"CheckRedirect must re-validate each hop against the allow-list.",
				redirector.URL, metadata.URL)
		case <-time.After(300 * time.Millisecond):
			// Desired: allow-list blocked the redirector URL itself
			// (loopback), or CheckRedirect caught the second hop.
		}
	})

	// ── Sub-test B: AUDIT-056 — webhook payload is redacted ────────────────
	// Post-fix assertion (Fix #10): FireWebhook feeds the task payload
	// through store.RedactSecrets before marshaling, so a fake PAT embedded
	// in the payload never leaves the daemon verbatim. Uses the
	// SetAllowLoopbackForTest escape hatch so we can actually hit the
	// httptest server and inspect the body.
	t.Run("B_WebhookBodyLeaksTokens", func(t *testing.T) {
		// Closed by: Fix #10.
		restore := SetAllowLoopbackForTest(true)
		t.Cleanup(restore)
		t.Cleanup(WaitForWebhookDrain)
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
				t.Errorf("AUDIT-056 regression: webhook body contained the raw fake PAT %q. "+
					"Body=%q", fakeToken, string(body))
			}
			if !strings.Contains(string(body), "[REDACTED]") {
				t.Errorf("AUDIT-056 regression: redaction placeholder missing — "+
					"RedactSecrets may have changed replacement token. Body=%q", string(body))
			}
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for webhook POST")
		}
	})

	// ── Sub-test C: AUDIT-055 — gh stderr wrapped unredacted ───────────────
	// Post-fix assertion (Fix #10): gh.go wraps stderr through
	// redactGHError (which calls store.RedactSecrets) or the literal
	// helper name is otherwise referenced in the file.
	t.Run("C_GhStderrNotRedacted", func(t *testing.T) {
		// Closed by: Fix #10.
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
			t.Errorf("AUDIT-055 regression: internal/gh/gh.go wraps `stderr` into %d "+
				"returned errors with no redaction regex or helper visible. Fix #10 "+
				"introduced redactGHError / store.RedactSecrets — if either name was "+
				"renamed, update this static check to track the new helper. Path=%s",
				wrapCount, ghPath)
		}
	})

	// Keep atomic imported for potential future sub-test expansion; silences unused.
	var _ atomic.Int32
}
