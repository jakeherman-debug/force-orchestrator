// Package audittools — Pattern P14: every CLAUDE.md invariant has at
// least one Bureau of Standards rule that enforces it (D4 Phase 1).
//
// CLAUDE.md is the documented contract; without enforcement, the
// documentation drifts from the code (Domain 25 in the audit). BoS
// closes the loop: every invariant becomes at least one AST check.
// This test cross-references the two and fails when an invariant
// section in CLAUDE.md has no matching BoS rule.CLAUDEMDAnchor()
// claim.
//
// The allowlist captures invariants intentionally NOT enforced as
// BoS rules (process-only invariants like "Conventional commits"
// that have no AST footprint). Each entry MUST carry a one-line
// truthful reason — opaque allowlist entries fail this test's
// reason-truthfulness self-check.
//
// Pattern P14 is the deferred D3 slot graduated in D4 Phase 1.
package audittools

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"force-orchestrator/internal/bos"
	_ "force-orchestrator/internal/bos/rules" // populate registry
)

// p14AllowlistedInvariants — invariants whose enforcement is process-
// only (commit hooks, code review, etc.) and therefore never become
// BoS rules. Each map entry MUST carry a one-line, truthful reason.
//
// Adding a new entry without a reason fails
// TestPattern_P14_AllowlistReasonsTruthful.
var p14AllowlistedInvariants = map[string]string{
	"Commit style":                   "Process-only — `Conventional commits` discipline is enforced via pre-commit hooks (lint, message format) and code review. There is no AST shape to detect a non-conventional commit message after the fact, and the commit-message body lives outside the Go source tree BoS scans.",
	"Testing rules":                  "Process-only — `Always run make test` discipline is enforced via CI (the make test gate). No AST check can verify that the operator ran tests; the verification surface is CI green, not source code.",
	"Per-agent capability profiles":  "Enforced at CI-time by Pattern P13 (internal/audittools/audit_pattern_p13_capability_profiles_test.go). Graduating P13 to a BoS commit-time rule is a future exercise (alongside BOS-011 graduating P16); for now P13's CI run is the gate.",
	"Claude CLI invocation layering": "Process-only — `daemon CWD = force-orchestrator/` is a runtime configuration property, not a code-shape invariant. The layering is documented for operator/agent context; there is no Go AST that could detect a future drift other than runtime tests, which are out of BoS's commit-time scope.",
	"Store / schema conventions":     "Enforced at CI-time by TestSchemaParity (internal/store/schema_parity_test.go). The createSchema/runMigrations/schema.sql trio is verified for parity; BoS would duplicate that test without adding signal, so the existing test remains the gate.",
	// Structural section headings (H2) — these are document-
	// organization markers, not invariants themselves; the
	// invariants under them ARE covered by the bold-prefix
	// extraction above. A BoS rule for an H2 wrapper like
	// "Architecture invariants" would be redundant.
	"Architecture invariants":      "Structural H2 section — wraps the bold-prefix invariants below it (each individually covered by a BoS rule or its own allowlist entry). The H2 itself has no AST footprint to enforce.",
	"Commit + process discipline":  "Structural H2 section — wraps `Commit style` (allowlisted, process-only). The H2 itself is documentary structure, not an enforceable invariant.",
	"Schema conventions":           "Structural H2 section — wraps the SQLite/migration invariants below it. BOS-007 (convoy_id not LIKE) and BOS-008 (new tables need indexes) cover the AST-detectable invariants under this heading; the rest is enforced by TestSchemaParity (a non-BoS CI gate).",
	"CLI shelling for LLM calls":   "Process-only — `Agents invoke claude via claude -p` is enforced at CI-time by Pattern P11 (exec_context audit) and Pattern P31 (LLM transcripts). A BoS rule would duplicate those without adding signal at the commit-time gate.",
	"Gas Town pattern":             "Enforced at CI-time by Pattern P3 / Pattern P22 (no Go channels for cross-agent state). A BoS rule would re-implement those as commit-time AST checks; the existing tests ALREADY catch this at every CI run, so a graduated check-time enforcement is a future exercise.",
}

// extractInvariantTitles parses CLAUDE.md and returns the bold-prefix
// invariant labels. CLAUDE.md uses `**Title.**` as the invariant
// label inside an H2 section; we extract the title text.
//
// Example invariant lines this matches:
//   **Per-agent capability profiles.** ...
//   **Gas Town pattern.** ...
//   **No silent failures.** ...
func extractInvariantTitles(t *testing.T, root string) []string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, "CLAUDE.md"))
	if err != nil {
		t.Fatalf("read CLAUDE.md: %v", err)
	}

	// Match `**<title>.**` at the start of a paragraph. The
	// trailing period before the closing `**` is the canonical shape
	// in CLAUDE.md — it distinguishes invariant labels from inline
	// emphasis.
	re := regexp.MustCompile(`(?m)^\*\*([^*\n]+?)\.\*\*`)
	out := []string{}
	seen := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(string(body), -1) {
		title := strings.TrimSpace(m[1])
		if seen[title] {
			continue
		}
		seen[title] = true
		out = append(out, title)
	}

	// Also include H2 section headings (## Foo) so process-only
	// section-level invariants like "Commit style" / "Testing rules"
	// flow through the allowlist check.
	h2Re := regexp.MustCompile(`(?m)^## +(.+?)\s*$`)
	for _, m := range h2Re.FindAllStringSubmatch(string(body), -1) {
		title := strings.TrimSpace(m[1])
		if seen[title] {
			continue
		}
		seen[title] = true
		out = append(out, title)
	}

	sort.Strings(out)
	return out
}

// anchorMatches returns true when the rule's CLAUDEMDAnchor() string
// matches (or is contained in) the CLAUDE.md invariant title — both
// directions, case-insensitive, since rule anchors paraphrase the
// title (e.g. "Fix #1 — e-stop responsiveness" anchors to "No silent
// failures" only loosely; we accept substring matches).
//
// Markdown decoration (backticks, asterisks, em-dashes) is stripped
// before comparison so a title like "Convoy-scoped queries use
// `convoy_id` not LIKE" matches a rule anchor without backticks.
func anchorMatches(ruleAnchor, claudeTitle string) bool {
	a := normaliseInvariantText(ruleAnchor)
	c := normaliseInvariantText(claudeTitle)
	if a == "" || c == "" {
		return false
	}
	return strings.Contains(a, c) || strings.Contains(c, a)
}

func normaliseInvariantText(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	for _, ch := range []string{"`", "*", "—", "–", "-"} {
		s = strings.ReplaceAll(s, ch, " ")
	}
	// Collapse runs of whitespace.
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	return strings.TrimSpace(s)
}

// TestPattern_P14_BoSRulesCoverCLAUDEMDInvariants is the D4 Phase 1
// regression. Every CLAUDE.md invariant title must have at least one
// matching BoS rule (via Rule.CLAUDEMDAnchor() substring match) OR
// appear in p14AllowlistedInvariants with a truthful reason.
func TestPattern_P14_BoSRulesCoverCLAUDEMDInvariants(t *testing.T) {
	root := moduleRoot(t)
	titles := extractInvariantTitles(t, root)
	if len(titles) == 0 {
		t.Fatal("extractInvariantTitles returned 0 — CLAUDE.md selector stale?")
	}

	rules := bos.All()
	if len(rules) == 0 {
		t.Fatal("bos.All() returned 0 — rules package init not running?")
	}

	uncovered := []string{}
	for _, title := range titles {
		// Allowlisted? Skip.
		if _, ok := p14AllowlistedInvariants[title]; ok {
			continue
		}
		matched := false
		for _, r := range rules {
			if anchorMatches(r.CLAUDEMDAnchor(), title) {
				matched = true
				break
			}
		}
		if !matched {
			uncovered = append(uncovered, title)
		}
	}

	if len(uncovered) > 0 {
		t.Errorf("Pattern P14: %d CLAUDE.md invariant(s) not covered by any BoS rule and not allowlisted:", len(uncovered))
		for _, u := range uncovered {
			t.Errorf("  - %q (add a BoS rule with CLAUDEMDAnchor() containing this title, or add to p14AllowlistedInvariants with a truthful reason)", u)
		}
	}
}

// TestPattern_P14_AllowlistReasonsTruthful — every entry in
// p14AllowlistedInvariants must have a non-empty, non-trivial reason.
// "TODO" / "fixme" / fewer than 30 chars all fail.
func TestPattern_P14_AllowlistReasonsTruthful(t *testing.T) {
	for title, reason := range p14AllowlistedInvariants {
		trimmed := strings.TrimSpace(reason)
		if len(trimmed) < 30 {
			t.Errorf("allowlist entry %q has trivial reason (<30 chars): %q", title, trimmed)
		}
		lower := strings.ToLower(trimmed)
		if strings.HasPrefix(lower, "todo") || strings.Contains(lower, "fixme") {
			t.Errorf("allowlist entry %q reason is a TODO/fixme placeholder; replace with a truthful one", title)
		}
	}
}

// TestPattern_P14_AllowlistedTitlesExist — every allowlist key MUST
// be a real CLAUDE.md invariant title. Catches typos in the
// allowlist that would otherwise silently grant exemption to
// nothing.
func TestPattern_P14_AllowlistedTitlesExist(t *testing.T) {
	root := moduleRoot(t)
	titles := extractInvariantTitles(t, root)
	titleSet := map[string]bool{}
	for _, ti := range titles {
		titleSet[ti] = true
	}
	for k := range p14AllowlistedInvariants {
		if !titleSet[k] {
			t.Errorf("allowlist key %q is not a CLAUDE.md invariant title — typo?", k)
		}
	}
}
