package store

import (
	"strings"
	"testing"
)

// TestRedactSecrets_GitHubPATPrefix asserts that the five GitHub token
// prefixes (ghp_, gho_, ghu_, ghs_, ghr_) are each replaced with the
// literal [REDACTED] token. Each prefix is an independent regex — a
// regression on one mustn't mask regressions on the others.
func TestRedactSecrets_GitHubPATPrefix(t *testing.T) {
	cases := []struct {
		name, in, mustNotContain string
	}{
		{"ghp_", "gh auth failed with token ghp_abcdefghij1234567890 boom", "ghp_abcdefghij"},
		{"gho_", "oauth exchange: gho_AAAA1111BBBB2222CCCC — bad scope", "gho_AAAA1111"},
		{"ghu_", "user token ghu_UserTokenABCDEFGH12345 expired", "ghu_UserToken"},
		{"ghs_", "server token ghs_ServerABCDEF12345 refused", "ghs_ServerABCDEF"},
		{"ghr_", "refresh token ghr_RefreshTokenXYZ12345 invalid", "ghr_RefreshToken"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := RedactSecrets(tc.in)
			if strings.Contains(out, tc.mustNotContain) {
				t.Errorf("redaction failed: output still contains %q\n  in=%q\n  out=%q",
					tc.mustNotContain, tc.in, out)
			}
			if !strings.Contains(out, "[REDACTED]") {
				t.Errorf("expected [REDACTED] placeholder in output, got %q", out)
			}
		})
	}
}

// TestRedactSecrets_BearerToken asserts the Authorization-header form
// "Bearer <token>" is scrubbed while the "Bearer " keyword is preserved
// so the log line remains human-readable. The match must be
// case-insensitive (HTTP headers are).
func TestRedactSecrets_BearerToken(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{
			name: "header form",
			in:   "Authorization: Bearer abc123def456ghi789.signature",
			want: "Authorization: Bearer [REDACTED]",
		},
		{
			name: "lowercase keyword",
			in:   "curl -H 'authorization: bearer secrettoken12345'",
			want: "curl -H 'authorization: bearer [REDACTED]'",
		},
		{
			name: "embedded in error",
			in:   "unexpected response: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9 rejected",
			want: "unexpected response: Bearer [REDACTED] rejected",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := RedactSecrets(tc.in)
			if out != tc.want {
				t.Errorf("Bearer redaction wrong:\n  in=%q\n  got=%q\n  want=%q", tc.in, out, tc.want)
			}
		})
	}
}

// TestRedactSecrets_URLBasicAuth asserts that URL-embedded userinfo
// (https://user:pass@host/...) is scrubbed while the scheme and host
// remain visible. Operators reading the log still see WHICH host was
// hit, just not the credentials used.
func TestRedactSecrets_URLBasicAuth(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{
			name: "https basic auth",
			in:   "git clone https://user:password@github.com/acme/repo.git",
			want: "git clone https://[REDACTED]@github.com/acme/repo.git",
		},
		{
			name: "http basic auth",
			in:   "remote url is http://admin:hunter2@internal.corp/path",
			want: "remote url is http://[REDACTED]@internal.corp/path",
		},
		{
			name: "git protocol left alone",
			in:   "remote: git@github.com:owner/repo.git",
			want: "remote: git@github.com:owner/repo.git",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := RedactSecrets(tc.in)
			if out != tc.want {
				t.Errorf("URL basic-auth redaction wrong:\n  in=%q\n  got=%q\n  want=%q", tc.in, out, tc.want)
			}
		})
	}
}

// TestRedactSecrets_GitHubPAT_FineGrained asserts the fine-grained
// `github_pat_<opaque>` format is caught. The opaque segment is longer
// than the classic prefix tokens, so the regex needs a higher minimum.
func TestRedactSecrets_GitHubPAT_FineGrained(t *testing.T) {
	cases := []struct {
		name, in, mustNotContain string
	}{
		{
			name:            "v1 format",
			in:              "token=github_pat_11AAABBBB0ZZZ1YYY2XXXVVVWWW_8SECRETTOKENBODY123",
			mustNotContain:  "github_pat_11AAABBBB0",
		},
		{
			name:            "embedded in auth error",
			in:              "HTTP 401: Bad credentials for github_pat_11abcdefgh0123456789_secretbodypayloaddata",
			mustNotContain:  "github_pat_11abcdefgh",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := RedactSecrets(tc.in)
			if strings.Contains(out, tc.mustNotContain) {
				t.Errorf("fine-grained PAT not redacted:\n  in=%q\n  out=%q", tc.in, out)
			}
			if !strings.Contains(out, "[REDACTED]") {
				t.Errorf("missing [REDACTED] placeholder in %q", out)
			}
		})
	}
}

// TestRedactSecrets_EmptyAndBenign exercises the hot path. Inputs that
// carry no secret must pass through unchanged — the fast-path substring
// check is what keeps RedactSecrets cheap enough to call on every
// outbound log line.
func TestRedactSecrets_EmptyAndBenign(t *testing.T) {
	cases := []string{
		"",
		"ordinary log line with no secrets",
		"git commit: fix typo in README",
		"The Captain approved task #42",
		"repo url: git@github.com:acme/api.git",
	}
	for _, in := range cases {
		if out := RedactSecrets(in); out != in {
			t.Errorf("benign input mutated:\n  in=%q\n  out=%q", in, out)
		}
	}
}

// TestRedactSecretsBytes mirrors the []byte convenience wrapper — same
// behaviour, no string conversion on the caller's side.
func TestRedactSecretsBytes(t *testing.T) {
	in := []byte("error: ghp_abcdefghij1234567890 rejected")
	out := RedactSecretsBytes(in)
	if strings.Contains(string(out), "ghp_abcdef") {
		t.Errorf("[]byte wrapper failed to redact: %q", out)
	}
	if strings.Contains(string(out), "[REDACTED]") == false {
		t.Errorf("expected [REDACTED] in %q", out)
	}
	// Empty bytes return empty — no nil panic.
	if got := RedactSecretsBytes(nil); len(got) != 0 {
		t.Errorf("nil input should yield empty output, got %q", got)
	}
}

// FuzzRedactSecrets exercises the helper with arbitrary inputs. The
// invariant under fuzz is simple: RedactSecrets must never panic, must
// never return output LONGER than input by more than a constant
// multiplier (the replacement "[REDACTED]" is bounded), and must never
// emit any of the known sensitive prefixes unredacted when they were
// present in the input with enough characters to match the regex.
//
// Run with:
//
//	go test -tags sqlite_fts5 -fuzz=FuzzRedactSecrets -fuzztime=10s \
//	    ./internal/store
func FuzzRedactSecrets(f *testing.F) {
	// Seed corpus — hit every regex branch plus a benign case.
	for _, s := range []string{
		"",
		"plain log line",
		"ghp_aaaaaaaaaaaaaa",
		"gho_bbbbbbbbbbbbbb",
		"ghu_cccccccccccccc",
		"ghs_dddddddddddddd",
		"ghr_eeeeeeeeeeeeee",
		"github_pat_11ABC_aaaaabbbbbcccccddddd",
		"Authorization: Bearer token123.signature",
		"url https://user:pass@host.com/path",
		"mixed ghp_xxxxxxxxxxxx and Bearer yyyyyyyyyy in one line",
		"lots of : // :: @ mixed punctuation",
	} {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, s string) {
		out := RedactSecrets(s)
		// Bounded growth: each match replaces with at most "[REDACTED]"
		// plus some preserved context. A safe upper bound: output can't
		// be more than 2x the input + 64 bytes of literal placeholders.
		if len(out) > 2*len(s)+1024 {
			t.Fatalf("output grew unreasonably: in=%d out=%d", len(s), len(out))
		}
		// The output must not contain any of the raw token prefixes
		// WITH enough trailing chars to match the regex. If the input
		// contained "ghp_" followed by 10+ alphanumerics, the output
		// must contain [REDACTED] instead.
		for _, prefix := range []string{"ghp_", "gho_", "ghu_", "ghs_", "ghr_"} {
			if idx := strings.Index(s, prefix); idx >= 0 {
				// Look at what followed the prefix in the INPUT.
				tail := s[idx+len(prefix):]
				alnumLen := 0
				for alnumLen < len(tail) && isTokenChar(tail[alnumLen]) {
					alnumLen++
				}
				if alnumLen >= 10 {
					// Input had a matchable token; output must not.
					if strings.Contains(out, prefix+tail[:alnumLen]) {
						t.Fatalf("token %s%s survived redaction\n  in=%q\n  out=%q",
							prefix, tail[:alnumLen], s, out)
					}
				}
			}
		}
	})
}

// isTokenChar matches the [A-Za-z0-9_] class used by tokenPatterns.
// Duplicated here rather than exported from redact.go — the fuzz test
// is the only caller.
func isTokenChar(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '_'
}
