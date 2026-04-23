package store

import (
	"regexp"
	"strings"
)

// RedactSecrets replaces well-known secret patterns in s with a fixed
// placeholder. It is the single chokepoint for scrubbing content before it
// leaves the orchestrator via webhooks, telemetry events, or wrapped gh
// error messages (the P9 audit pattern — AUDIT-016, AUDIT-055, AUDIT-056).
//
// Patterns covered:
//   - GitHub PAT prefixes: ghp_, gho_, ghu_, ghs_, ghr_
//   - Fine-grained PATs:   github_pat_<segment>_<segment>
//   - Bearer tokens in HTTP Authorization headers or log lines
//   - URL-embedded basic auth (https://user:password@host/...)
//
// The function is deliberately conservative: it prefers false positives
// (over-redacting a benign "ghp_demo" literal in documentation) to false
// negatives (a real token slipping into BountyBoard.error_log, which is
// rendered on the unauth dashboard). All replacements collapse to the
// literal "[REDACTED]" token so operators reading a log line know
// redaction occurred. Bearer- and URL-basic-auth replacements preserve the
// surrounding structure (keyword, scheme, host) so the line remains
// human-readable.
//
// RedactSecrets is allocation-free on inputs that contain no matches.
func RedactSecrets(s string) string {
	if s == "" {
		return s
	}
	// Fast path: if none of the anchor substrings appear, skip regex work.
	if !redactQuickCheck(s) {
		return s
	}
	out := s
	// Token prefixes (ghp_, gho_, ghu_, ghs_, ghr_, github_pat_) replace
	// the whole match with [REDACTED] — there is nothing salvageable.
	for _, re := range tokenPatterns {
		out = re.ReplaceAllString(out, "[REDACTED]")
	}
	// Bearer keyword: preserve "Bearer " prefix, redact the token body.
	out = bearerPattern.ReplaceAllString(out, "${1}[REDACTED]")
	// URL basic-auth: preserve scheme, redact userinfo@, keep host+path.
	out = urlBasicAuthPattern.ReplaceAllString(out, "${1}[REDACTED]@")
	return out
}

// redactQuickCheck is a cheap substring filter — any of these prefixes
// MUST be present for a real redaction to fire. Keeping the hot path
// allocation-free matters because every gh error and every telemetry
// event funnels through RedactSecrets.
func redactQuickCheck(s string) bool {
	// Lowercase Bearer check done separately because HTTP headers are
	// case-insensitive but the common rendering is "Bearer ".
	if strings.Contains(s, "ghp_") ||
		strings.Contains(s, "gho_") ||
		strings.Contains(s, "ghu_") ||
		strings.Contains(s, "ghs_") ||
		strings.Contains(s, "ghr_") ||
		strings.Contains(s, "github_pat_") ||
		strings.Contains(s, "://") { // URL-basic-auth candidate
		return true
	}
	// Bearer scan — case-insensitive without allocating.
	for i := 0; i+6 <= len(s); i++ {
		c0 := s[i]
		if c0 != 'B' && c0 != 'b' {
			continue
		}
		if strings.EqualFold(s[i:i+6], "bearer") {
			return true
		}
	}
	return false
}

// tokenPatterns fully replace the match — GitHub token prefixes have no
// recoverable structure. The opaque-id portion of a GitHub token is
// base62 (A-Za-z0-9) plus occasional underscore; [A-Za-z0-9_]+ is a safe
// superset that won't cross whitespace or punctuation boundaries.
var tokenPatterns = []*regexp.Regexp{
	regexp.MustCompile(`ghp_[A-Za-z0-9_]{10,}`),
	regexp.MustCompile(`gho_[A-Za-z0-9_]{10,}`),
	regexp.MustCompile(`ghu_[A-Za-z0-9_]{10,}`),
	regexp.MustCompile(`ghs_[A-Za-z0-9_]{10,}`),
	regexp.MustCompile(`ghr_[A-Za-z0-9_]{10,}`),
	// github_pat_<11+ chars>_<secret>. Fine-grained PAT format.
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`),
}

// bearerPattern preserves the "Bearer " keyword (case-insensitive) and
// redacts the token body only. Log lines stay human-readable.
var bearerPattern = regexp.MustCompile(`(?i)(Bearer\s+)[A-Za-z0-9._~+\-/=]{8,}`)

// urlBasicAuthPattern matches the userinfo section of a URL with embedded
// credentials. Preserves scheme (e.g., "https://") and host, redacts only
// the "user:password@" prefix so operators can still see which host was
// being hit.
var urlBasicAuthPattern = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://)[^\s/:@]+:[^\s/@]+@`)

// RedactSecretsBytes is a []byte-typed convenience wrapper so callers
// working with byte slices (like captured gh stderr) don't have to
// string-convert inline.
func RedactSecretsBytes(b []byte) []byte {
	if len(b) == 0 {
		return b
	}
	redacted := RedactSecrets(string(b))
	return []byte(redacted)
}
