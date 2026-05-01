package isb

import (
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"regexp"
	"strings"
)

// Bypass represents a parsed `// ISB-BYPASS: AUDIT-NNN reason` comment.
// One row per bypass per file, keyed by the line number of the
// FOLLOWING source line — the violating code the bypass excuses.
type Bypass struct {
	GuardLine int    // 1-indexed line of the violating code (one below the comment)
	AuditID   string // 'AUDIT-NNN'
	Reason    string // operator rationale (>= 10 chars)
}

// MalformedBypass describes a bypass comment that failed parse. The
// ISB reviewer treats these as hard rejects: a bypass without a reason
// cannot pass per docs/roadmap.md § D4 anti-cheat ("No bypass comment
// proliferation").
type MalformedBypass struct {
	Line   int
	Detail string
}

// bypassRe matches the bypass comment shape. Captures: (1) audit id,
// (2) reason text. AUDIT-<digits> required so a typo'd id (e.g.
// AUDIT-FOO) fails parse rather than slipping through.
var bypassRe = regexp.MustCompile(`^//\s*ISB-BYPASS:\s*(AUDIT-\d+)\s+(.+)$`)

// bypassPrefixRe is the loose-match form: catches any comment that
// starts with `// ISB-BYPASS` so we can flag malformed shapes
// (missing AUDIT-NNN, short reason, etc.) as MalformedBypass instead
// of silently skipping them.
var bypassPrefixRe = regexp.MustCompile(`^//\s*ISB-BYPASS\b`)

// ParseBypasses walks every comment in the file and returns:
//   - the list of valid Bypass entries (key: line of the violating
//     code immediately below the comment).
//   - the list of MalformedBypass entries (any comment whose prefix
//     matches `// ISB-BYPASS` but fails the strict regex).
//
// The fset argument is required so positions resolve to lines.
func ParseBypasses(fset *token.FileSet, file *ast.File) ([]Bypass, []MalformedBypass) {
	if fset == nil || file == nil {
		return nil, nil
	}
	var (
		valid     []Bypass
		malformed []MalformedBypass
	)
	for _, cg := range file.Comments {
		for _, c := range cg.List {
			text := strings.TrimSpace(c.Text)
			if !bypassPrefixRe.MatchString(text) {
				continue
			}
			line := fset.Position(c.Pos()).Line
			b, err := parseBypassLine(text)
			if err != nil {
				malformed = append(malformed, MalformedBypass{
					Line:   line,
					Detail: err.Error(),
				})
				continue
			}
			b.GuardLine = line + 1 // bypass guards the line BELOW it
			valid = append(valid, b)
		}
	}
	return valid, malformed
}

// parseBypassLine returns a Bypass parsed from one comment's text.
// The strict shape is `// ISB-BYPASS: AUDIT-NNN <reason text>` where
// reason is >= 10 chars after trim.
func parseBypassLine(text string) (Bypass, error) {
	m := bypassRe.FindStringSubmatch(text)
	if m == nil {
		// Loose match said this is a bypass; strict failed → describe
		// the most likely defect.
		switch {
		case !strings.Contains(text, "AUDIT-"):
			return Bypass{}, errors.New("ISB-BYPASS missing AUDIT-NNN reference")
		case !strings.Contains(text, ":"):
			return Bypass{}, errors.New("ISB-BYPASS missing colon after directive")
		default:
			return Bypass{}, fmt.Errorf("ISB-BYPASS malformed shape: %q", text)
		}
	}
	auditID := strings.TrimSpace(m[1])
	reason := strings.TrimSpace(m[2])
	if len([]rune(reason)) < 10 {
		return Bypass{}, fmt.Errorf("ISB-BYPASS reason %q is shorter than 10 chars", reason)
	}
	return Bypass{AuditID: auditID, Reason: reason}, nil
}

// MatchBypass returns the Bypass that guards the given line, or nil
// if none matches. A bypass on line N guards line N+1 — exact match.
func MatchBypass(bypasses []Bypass, line int) *Bypass {
	for i := range bypasses {
		if bypasses[i].GuardLine == line {
			return &bypasses[i]
		}
	}
	return nil
}
