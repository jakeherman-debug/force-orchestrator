package isb

// SUPPLY-BYPASS parser.
//
// Manifests are written in many languages; comment syntax differs:
//   - Go (// or /* */)              → covered by existing ISB-BYPASS parser
//   - Ruby (#)                       → SUPPLY-BYPASS in Gemfile, .gemspec
//   - Python (#)                     → SUPPLY-BYPASS in requirements.txt, pyproject.toml
//   - JavaScript/TypeScript (// or /* */) → SUPPLY-BYPASS in package.json (NOTE: JSON
//     doesn't support comments natively, but yarn.lock + npm-shrinkwrap variants accept
//     // comments at line head)
//   - Java/Kotlin XML (<!-- ... -->) → SUPPLY-BYPASS in pom.xml
//   - Java/Kotlin Groovy (// or /* */) → SUPPLY-BYPASS in build.gradle, build.gradle.kts
//
// We accept SUPPLY-BYPASS in any of those comment styles via a single regex that
// matches the bypass token (and the AUDIT-NNN + reason after) regardless of
// comment-prefix syntax. The token MUST be SUPPLY-BYPASS (not ISB-BYPASS — those
// are different rule families).
//
// Anti-cheat (per docs/roadmap.md § D5): bypass requires AUDIT-NNN + reason
// >= 10 chars; malformed markers produce NO match (silent skip — they simply
// don't suppress the finding). Per CLAUDE.md "no silent failures": failure to
// match is not an error path, it's the correct outcome of an invalid marker
// (the underlying finding will still surface).

import (
	"regexp"
	"strings"
)

// SupplyBypassMarker is one parsed SUPPLY-BYPASS comment from a manifest file.
type SupplyBypassMarker struct {
	LineNumber int    // 1-indexed line of the comment in the manifest source
	AuditID    string // e.g., "AUDIT-1234"
	Reason     string // operator rationale (>= 10 chars)
	RuleKey    string // optional: SUPPLY-001, SUPPLY-002, etc. — empty == applies to all SUPPLY rules
}

// supplyBypassRE matches a SUPPLY-BYPASS marker regardless of which comment
// prefix the host language uses.
//
// (?m)         — multi-line mode (^/$ match line boundaries)
// ^\s*         — leading whitespace
// (?://|/\*|#|<!--|--) — any supported comment prefix
//   //   Go / JS / TS / Groovy / Kotlin
//   /*   Go / JS / TS block comment open
//   #    Ruby / Python / TOML / YAML
//   <!-- XML / pom.xml
//   --   SQL line comment (cheap inclusion; not load-bearing for D5 but
//        keeps us honest if a manifest-adjacent file enters scope later)
// \s*          — whitespace between prefix and token
// SUPPLY-BYPASS — literal token
// :?\s* — optional colon (terminator after the directive) + optional whitespace.
//        Three accepted shapes: "SUPPLY-BYPASS: SUPPLY-001 ...",
//        "SUPPLY-BYPASS:SUPPLY-004 ...", and "SUPPLY-BYPASS AUDIT-NNN ...".
// (?:(SUPPLY-\d{3})\s+)? — optional rule-key + whitespace before AUDIT-N
// (AUDIT-\d+)  — required audit ID
// \s+(.+?)     — whitespace + reason (non-greedy)
// (?:\s*\*/|\s*-->)?\s*$ — trailing comment closer (stripped) or end-of-line
//
// Compiled once at package init. The regex is tolerant of trailing comment
// closers (`*/`, `-->`) which are stripped from the captured reason.
var supplyBypassRE = regexp.MustCompile(
	`(?m)^\s*(?://|/\*|#|<!--|--)\s*SUPPLY-BYPASS:?\s*(?:(SUPPLY-\d{3})\s+)?(AUDIT-\d+)\s+(.+?)(?:\s*\*/|\s*-->)?\s*$`,
)

// ParseSupplyBypasses scans the raw bytes of a manifest file and returns
// every SUPPLY-BYPASS marker found. Comment-prefix-agnostic: matches the
// token regardless of //, #, <!--, etc.
//
// Format: <comment-prefix> SUPPLY-BYPASS[:<RULE-KEY>] <AUDIT-NNNN> <reason>
//
// Examples (all valid):
//
//	// SUPPLY-BYPASS: SUPPLY-001 AUDIT-1234 vendored fork pending upstream merge
//	# SUPPLY-BYPASS AUDIT-1234 internal package, registry check N/A
//	<!-- SUPPLY-BYPASS:SUPPLY-004 AUDIT-5555 license matrix to be updated -->
//
// Reason MUST be >= 10 chars (anti-cheat: no bypass-by-default).
// AUDIT-NNNN MUST match /^AUDIT-\d+$/.
// RULE-KEY (if present) MUST match /^SUPPLY-\d{3}$/.
//
// Returns nil for nil/empty content. Markers that fail the reason length
// check (<10 chars after trim) are silently skipped — they do not suppress
// findings, which is the correct anti-cheat outcome.
func ParseSupplyBypasses(content []byte) []SupplyBypassMarker {
	if len(content) == 0 {
		return nil
	}

	// We need line numbers, and Go's regexp doesn't expose match-byte
	// offsets in a way that's cheap to translate to lines for multi-line
	// scans. Walk the file line-by-line so each match carries its 1-indexed
	// line number naturally. The regex uses (?m) so per-line ^/$ are still
	// correct against single lines.
	var out []SupplyBypassMarker
	lines := strings.SplitAfter(string(content), "\n")
	for i, line := range lines {
		// Strip the trailing newline so the regex's $ anchor doesn't have
		// to compete with it; SplitAfter keeps the separator on each line.
		trimmed := strings.TrimRight(line, "\r\n")
		m := supplyBypassRE.FindStringSubmatch(trimmed)
		if m == nil {
			continue
		}
		// m[1] = rule key (optional, may be empty)
		// m[2] = AUDIT-NNNN (required)
		// m[3] = reason (required)
		ruleKey := m[1]
		auditID := m[2]
		reason := strings.TrimSpace(m[3])

		// Anti-cheat: reason must be >= 10 chars.
		if len([]rune(reason)) < 10 {
			continue
		}

		out = append(out, SupplyBypassMarker{
			LineNumber: i + 1,
			AuditID:    auditID,
			Reason:     reason,
			RuleKey:    ruleKey,
		})
	}
	return out
}

// MatchSupplyBypass returns the first marker that applies to the given rule
// ID, or nil if none. A marker with empty RuleKey applies to all SUPPLY rules
// (operator-wide override); a marker with a specific RuleKey only suppresses
// findings for that exact rule (so a SUPPLY-001 bypass does NOT silence a
// SUPPLY-002 finding — anti-cheat: bypasses must be targeted by default).
func MatchSupplyBypass(markers []SupplyBypassMarker, ruleID string) *SupplyBypassMarker {
	for i := range markers {
		m := &markers[i]
		if m.RuleKey == "" || m.RuleKey == ruleID {
			return m
		}
	}
	return nil
}
