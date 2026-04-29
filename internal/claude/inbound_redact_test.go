package claude

import (
	"strings"
	"testing"
)

// realRSAPEM is a real-shape PEM block (random key generated for the test
// fixture only — not in use anywhere). The shape is what matters: the
// BEGIN/END markers and the multiline base64 body. The redactor should
// collapse the entire block into a single marker.
const realRSAPEM = `-----BEGIN RSA PRIVATE KEY-----
MIIEpAIBAAKCAQEAvEZQUmW7yZl4wKgL6yX7y5g7g6vBpYE3EzQ4FgYx7l1jX5d2
NsM9C8aV7fK5p6cR8eY3uQQrJ6vS2K5cV4Xz6TUw5nGZh3w0J0qZv4P3o5TG7uY9
F6pK8Qa9YhP5o4xMv3X2J9c8eN8d4yRkZx2KqJj9k2W3l6dF8b5K1J3n0P7aL2Qo
-----END RSA PRIVATE KEY-----`

const realECPEM = `-----BEGIN EC PRIVATE KEY-----
MHcCAQEEIDdvZAjQ5z5n3Q6m+y5pV8u9tH3K9X7Y2P8mJ3o4kZ3hoAoGCCqGSM49
AwEHoUQDQgAEdQ7yU3v+5p2cT8nL6w4o7c0M3qF+4M0P6sH8z3Y5n7E2Y8u5b9C0
-----END EC PRIVATE KEY-----`

const gcpServiceAccountJSON = `{
  "type": "service_account",
  "project_id": "demo-project",
  "private_key_id": "abc123",
  "private_key": "-----BEGIN PRIVATE KEY-----\nMIIEvgIBADANBgkqhkiG9w0BAQEFAASCBKgwggSkAgEAAoIBAQC...\n-----END PRIVATE KEY-----\n",
  "client_email": "demo@demo-project.iam.gserviceaccount.com"
}`

func TestScrubInbound_PEMBlock(t *testing.T) {
	in := "Some context.\n" + realRSAPEM + "\nMore context."
	out, n := ScrubInbound(in)
	if n != 1 {
		t.Fatalf("expected redactionCount=1 for single PEM block, got %d", n)
	}
	if !strings.Contains(out, "[REDACTED PEM PRIVATE KEY]") {
		t.Fatalf("expected output to contain marker, got: %q", out)
	}
	if strings.Contains(out, "BEGIN RSA PRIVATE KEY") || strings.Contains(out, "MIIEpAIBAA") {
		t.Fatalf("PEM body leaked through redaction: %q", out)
	}
	if !strings.Contains(out, "Some context.") || !strings.Contains(out, "More context.") {
		t.Fatalf("surrounding context was discarded: %q", out)
	}
}

func TestScrubInbound_PEMBlock_ECVariant(t *testing.T) {
	out, n := ScrubInbound(realECPEM)
	if n < 1 {
		t.Fatalf("EC PEM block was not redacted (count=%d)", n)
	}
	if strings.Contains(out, "BEGIN EC PRIVATE KEY") {
		t.Fatalf("EC PEM markers leaked: %q", out)
	}
}

func TestScrubInbound_PEMBlock_OPENSSHVariant(t *testing.T) {
	in := "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAA...\n-----END OPENSSH PRIVATE KEY-----"
	out, n := ScrubInbound(in)
	if n < 1 {
		t.Fatalf("OPENSSH PEM block was not redacted (count=%d)", n)
	}
	if strings.Contains(out, "BEGIN OPENSSH PRIVATE KEY") {
		t.Fatalf("OPENSSH PEM markers leaked: %q", out)
	}
}

func TestScrubInbound_GCPServiceAccountJSON(t *testing.T) {
	out, n := ScrubInbound(gcpServiceAccountJSON)
	if n < 1 {
		t.Fatalf("GCP private_key was not redacted (count=%d)", n)
	}
	if strings.Contains(out, "MIIEvgIBADANBgkqhkiG9w0BAQEFAA") {
		t.Fatalf("GCP key body leaked: %q", out)
	}
	if !strings.Contains(out, `"private_key":"[REDACTED]"`) {
		t.Fatalf("GCP redaction did not preserve JSON shape; got: %q", out)
	}
	// Surrounding fields should remain visible (debugging context for
	// the operator who will read this on the dashboard).
	if !strings.Contains(out, `"client_email"`) {
		t.Fatalf("surrounding JSON field stripped: %q", out)
	}
}

func TestScrubInbound_EnvAssignments(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantMark string // a token that MUST appear
		wantHide string // a token that MUST NOT appear after redaction
	}{
		{
			name:     "API_KEY",
			in:       "API_KEY=sk-livedeadbeef1234567890",
			wantMark: "API_KEY=[REDACTED]",
			wantHide: "sk-livedeadbeef",
		},
		{
			name:     "PREFIX_TOKEN",
			in:       "GITHUB_TOKEN=ghp_realtokenhere1234567890",
			wantMark: "GITHUB_TOKEN=[REDACTED]",
			wantHide: "realtokenhere",
		},
		{
			name:     "DATABASE_PASSWORD_PROD",
			in:       "DATABASE_PASSWORD_PROD=hunter2longpassword",
			wantMark: "DATABASE_PASSWORD_PROD=[REDACTED]",
			wantHide: "hunter2longpassword",
		},
		{
			name:     "MY_AUTH_HEADER",
			in:       "MY_AUTH_HEADER=Basic dXNlcjpwYXNz",
			wantMark: "MY_AUTH_HEADER=[REDACTED]",
			wantHide: "dXNlcjpwYXNz",
		},
		{
			name:     "PRIVATE_KEY_PATH (variable name)",
			in:       "PRIVATE_KEY=secretvaluexyz",
			wantMark: "PRIVATE_KEY=[REDACTED]",
			wantHide: "secretvaluexyz",
		},
		{
			name:     "AWS_SECRET_ACCESS_KEY",
			in:       "AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
			wantMark: "AWS_SECRET_ACCESS_KEY=[REDACTED]",
			wantHide: "wJalrXUtnFEMI",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, n := ScrubInbound(tc.in)
			if n < 1 {
				t.Fatalf("expected at least one redaction, got %d (out=%q)", n, out)
			}
			if !strings.Contains(out, tc.wantMark) {
				t.Fatalf("expected output to contain %q, got %q", tc.wantMark, out)
			}
			if strings.Contains(out, tc.wantHide) {
				t.Fatalf("secret %q leaked through redaction in %q", tc.wantHide, out)
			}
		})
	}
}

func TestScrubInbound_AWSAccessKey(t *testing.T) {
	in := "Found access key AKIAIOSFODNN7EXAMPLE in config."
	out, n := ScrubInbound(in)
	if n < 1 {
		t.Fatalf("AWS access key not redacted (count=%d)", n)
	}
	if strings.Contains(out, "AKIAIOSFODNN7EXAMPLE") {
		t.Fatalf("AWS access key leaked: %q", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Fatalf("expected [REDACTED] marker, got: %q", out)
	}
}

func TestScrubInbound_GHPAT(t *testing.T) {
	// store.RedactSecrets covers ghp_; verify the boundary still fires
	// when invoked through ScrubInbound.
	in := "Authorization: token ghp_realtokendeadbeefdeadbeefdeadbeef"
	out, n := ScrubInbound(in)
	if n < 1 {
		t.Fatalf("GH PAT not redacted (count=%d)", n)
	}
	if strings.Contains(out, "ghp_realtokendeadbeefdeadbeefdeadbeef") {
		t.Fatalf("GH PAT leaked: %q", out)
	}
}

func TestScrubInbound_NegativeProse(t *testing.T) {
	// Documentation prose mentioning sensitive keywords must NOT be
	// redacted. Anti-cheat: the env-shape pattern requires LHS=value, not
	// a free mention of the word.
	cases := []string{
		"The API_KEY environment variable should be set to your token.",
		"Use AUTHORIZATION header for token-based access.",
		"Reset the user's PASSWORD via the recovery flow.",
		"# How to configure: set TOKEN, then run.",
	}
	for _, in := range cases {
		t.Run(in[:20], func(t *testing.T) {
			out, n := ScrubInbound(in)
			if n != 0 {
				t.Fatalf("prose was redacted (count=%d): in=%q out=%q", n, in, out)
			}
			if out != in {
				t.Fatalf("prose was modified: in=%q out=%q", in, out)
			}
		})
	}
}

func TestScrubInbound_Empty(t *testing.T) {
	out, n := ScrubInbound("")
	if out != "" || n != 0 {
		t.Fatalf("ScrubInbound(\"\") = (%q, %d), want (\"\", 0)", out, n)
	}
}

func TestScrubInbound_NoSecrets(t *testing.T) {
	in := "This is a regular prompt with no secrets at all. Just some prose.\nAnd another line."
	out, n := ScrubInbound(in)
	if n != 0 || out != in {
		t.Fatalf("plain prose was modified: count=%d, out=%q", n, out)
	}
}

func TestScrubInbound_StringIdempotent(t *testing.T) {
	// The string output of ScrubInbound is idempotent — running twice
	// produces the same string as running once. Count is NOT idempotent
	// (some patterns re-match their own [REDACTED] replacement) — that's
	// fine because count drives an alert threshold, not a correctness
	// assertion.
	cases := []string{
		realRSAPEM,
		"API_KEY=secretval\nDATABASE_URL=postgres://user:pass@host/db",
		gcpServiceAccountJSON,
		"AKIAIOSFODNN7EXAMPLE plus ghp_realtokendeadbeefdeadbeefdeadbeef",
	}
	for _, in := range cases {
		out1, _ := ScrubInbound(in)
		out2, _ := ScrubInbound(out1)
		if out1 != out2 {
			t.Fatalf("not string-idempotent:\n  first  = %q\n  second = %q", out1, out2)
		}
	}
}

func TestScrubInbound_MultipleSecretsInOnePrompt(t *testing.T) {
	in := "First: API_KEY=sk-livevalue123456789\nSecond: " + realRSAPEM + "\nThird: AKIAIOSFODNN7EXAMPLE"
	out, n := ScrubInbound(in)
	if n < 3 {
		t.Fatalf("expected count >= 3 for three distinct secrets, got %d", n)
	}
	if strings.Contains(out, "sk-livevalue") {
		t.Fatalf(".env value leaked: %q", out)
	}
	if strings.Contains(out, "BEGIN RSA PRIVATE KEY") {
		t.Fatalf("PEM leaked: %q", out)
	}
	if strings.Contains(out, "AKIAIOSFODNN7EXAMPLE") {
		t.Fatalf("AWS key leaked: %q", out)
	}
}

func TestScrubInbound_URLBasicAuth(t *testing.T) {
	// store.RedactSecrets covers URL-embedded basic-auth; verify the
	// inbound boundary preserves that coverage.
	in := "Cloning from https://user:hunter2@github.com/foo/bar.git ..."
	out, n := ScrubInbound(in)
	if n < 1 {
		t.Fatalf("URL basic-auth not redacted (count=%d)", n)
	}
	if strings.Contains(out, "user:hunter2@") {
		t.Fatalf("basic-auth leaked: %q", out)
	}
}

func TestScrubInbound_RedactionCountAlignment(t *testing.T) {
	// Three discrete patterns; count should reflect each redaction.
	in := "ghp_realtokendeadbeef1234567890\nAPI_KEY=secretvalue123\nAKIAIOSFODNN7EXAMPLE"
	_, n := ScrubInbound(in)
	if n < 3 {
		t.Fatalf("expected count >= 3, got %d", n)
	}
}
