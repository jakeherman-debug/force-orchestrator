package scanners

import "regexp"

// Per the all-Go decision in D4 Phase 2 scope: rules that would have
// used semgrep get deterministic regex fallbacks. This file is the
// central registry of those patterns so a reviewer can audit the
// fingerprint set in one place.
//
// Each Pattern carries the rule_id it serves, a compiled regex, and a
// human-readable description. The regex matches against raw source
// text (line-oriented, MultiLine flag where useful) — the rule body
// in internal/isb/rules/ wraps the match to produce a Finding with
// the correct Line resolution.

// Pattern is a single regex-backed deterministic check.
type Pattern struct {
	RuleID  string
	Re      *regexp.Regexp
	Message string
}

// HardcodedSecretLikePatterns is the regex fallback for rule-classes
// that gitleaks already covers but where we want a complementary
// signal (basic-auth in URLs, ghp_/gho_ prefixes inline). gitleaks is
// the primary; these are belt-and-suspenders.
var HardcodedSecretLikePatterns = []Pattern{
	{
		RuleID:  "ISB-001",
		Re:      regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}\b`),
		Message: "GitHub token-like literal (ghp_/gho_/...) — never commit credentials",
	},
	{
		RuleID:  "ISB-001",
		Re:      regexp.MustCompile(`https?://[A-Za-z0-9._%+-]+:[^@\s/]+@`),
		Message: "Basic-auth credentials embedded in URL — never commit",
	},
}

// SQLConcatPatterns is the regex fallback for rules that would have
// used semgrep's "SQL string concatenation" check. The AST rule
// (ISB-003) is the primary detection; this list captures lexical
// shapes the AST rule's structural walk can't see (e.g., concatenated
// across line breaks via `+\n` continuation).
var SQLConcatPatterns = []Pattern{
	{
		RuleID:  "ISB-003",
		Re:      regexp.MustCompile(`(?i)"\s*(?:SELECT|INSERT|UPDATE|DELETE)\b[^"]*"\s*\+\s*[A-Za-z_]`),
		Message: "concatenated SQL fragment — use parameterized query (?, $1, :name) instead",
	},
}

// FilePermPatterns is the regex fallback for ISB-006 in cases where
// the AST rule (mode > 0700) misses os.WriteFile-style call sites.
var FilePermPatterns = []Pattern{
	{
		RuleID:  "ISB-006",
		Re:      regexp.MustCompile(`os\.(?:WriteFile|MkdirAll|OpenFile|Create)\([^)]*0[7-9]\d\d`),
		Message: "file mode > 0700 in os file op — restrict permissions on sensitive paths",
	},
}
