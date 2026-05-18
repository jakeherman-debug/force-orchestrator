// Package librarian — Sweep-E (D12) GenerateRepoDescription
// implementation.
//
// The Sweep-D regex-only deriveRepoDescription in
// cmd/force/add_repo_smart_defaults.go missed 8-of-17 real-world
// READMEs (blockquoted headings, list-only paragraphs, link-only
// paragraphs, fenced code, empty before-heading content, etc.). This
// implementation teaches the librarian to generate the description
// via Claude (full README → one-sentence prose) with the existing
// regex scrape preserved as the deterministic
// LIVE_HAIKU_DISABLED / LLM-failure fallback.
//
// Live-Haiku gating mirrors BootstrapSenatorRules: when
// LIVE_HAIKU_DISABLED is set the implementation returns the
// deterministic stub directly (no LLM call). When unset the
// implementation calls Claude via CallWithTranscriptOneShot — a
// single-turn no-tool call wrapped in the standard transcript
// shadow — and falls back to the deterministic stub if the LLM
// returns an error or an empty / sentinel response.
//
// Anti-cheat: the deterministic stub IS the same path
// cmd/force/add_repo_smart_defaults.go uses when no librarian client
// is wired (test path). The shared scraper lives in
// internal/clients/librarian/readme.go so the LLM-disabled fallback
// and the LLM-unavailable test path produce identical output.
package librarian

import (
	"context"
	"fmt"
	"strings"
	"time"

	"force-orchestrator/internal/claude"
)

// generateRepoDescriptionReadmeCap bounds the README bytes fed to
// the LLM. 16 KB is large enough to capture the description-bearing
// preamble of every real-world README we've sampled and small
// enough that the prompt cost stays predictable.
const generateRepoDescriptionReadmeCap = 16 * 1024

// generateRepoDescriptionTimeout caps the LLM call so the
// `force add-repo` operator path never hangs. The regex fallback
// fires on timeout via the err != nil branch.
const generateRepoDescriptionTimeout = 60 * time.Second

// generateRepoDescriptionNoDescSentinel is the literal the LLM
// emits when the README has no usable description. The sanitizer
// recognises it and propagates an empty string to the caller — the
// add-repo path treats empty as "no description" and writes a
// blank Repositories.description.
const generateRepoDescriptionNoDescSentinel = "(no description)"

// generateRepoDescriptionSystemPrompt is the LLM's contract. Kept
// short + focused; the user prompt is just the README content. The
// constraints (single line, no markdown, no prefix, ≤200 chars,
// "(no description)" sentinel) align with the sanitizer in
// sanitizeGeneratedDescription.
const generateRepoDescriptionSystemPrompt = `You are the Fleet Librarian's repo-description analyst. Given a repo's README, produce a single-sentence description suitable for a registry listing.

Output a single line of plain prose. No markdown formatting. No leading "Description:" prefix. No quotes around the output. Maximum 200 characters. If the README is empty or has no description-worthy content, output exactly: "(no description)".

Focus on WHAT the repo does, not HOW to use it. Skip installation/build instructions, badge content, contribution links.`

// GenerateRepoDescription is the in-process Client implementation of
// the LLM-backed repo-description helper. See the package docstring
// above for the full flow.
func (c *inProcessClient) GenerateRepoDescription(ctx context.Context, repoPath string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if strings.TrimSpace(repoPath) == "" {
		return "", fmt.Errorf("librarian: GenerateRepoDescription requires repoPath")
	}

	// Read README (bounded). Absent README → both LLM and stub will
	// return empty; we still attempt the LLM path with an empty
	// README so the LLM gets a chance to emit "(no description)"
	// explicitly. In practice an empty README routes straight to
	// the stub via the readmeFound check below.
	readme, _, readmeFound := ReadReadmeBytes(repoPath, generateRepoDescriptionReadmeCap)

	// LIVE_HAIKU_DISABLED → deterministic stub immediately. Mirrors
	// BootstrapSenatorRules's gate.
	if !liveHaikuEnabled() {
		return ScrapeReadmeFirstParagraph(repoPath), nil
	}

	// No README at all → there's nothing to feed the LLM. Return
	// the stub's empty result rather than spending a Claude call on
	// a guaranteed-no-op prompt.
	if !readmeFound {
		return ScrapeReadmeFirstParagraph(repoPath), nil
	}

	// LLM path. CallWithTranscriptOneShot records the call in
	// LLMCallTranscripts; the transcript is good audit substrate
	// for "why did the librarian propose this description?".
	callCtx, cancel := context.WithTimeout(ctx, generateRepoDescriptionTimeout)
	defer cancel()

	// Assemble the full prompt: system instructions + README.
	// CallWithTranscriptOneShot has no system/user split (it wraps
	// RunCLI's single-prompt shape), so we concatenate with a
	// labelled boundary so the model can distinguish.
	prompt := generateRepoDescriptionSystemPrompt + "\n\nREADME:\n" + readme

	out, err := claude.CallWithTranscriptOneShot(callCtx, claude.CallDescriptor{
		Agent:         "librarian",
		PromptVersion: "generate-repo-description-v1",
	}, prompt, "", "", "", "", 1, generateRepoDescriptionTimeout)
	if err != nil {
		// LLM error → log a single line and fall back to the
		// deterministic stub. No silent failure: the transcript row
		// has the error excerpt, the operator sees the description
		// it would have gotten pre-Sweep-E, and the add-repo flow
		// completes.
		fmt.Printf("[librarian] GenerateRepoDescription LLM call failed for %s: %v — falling back to regex scrape\n", repoPath, err)
		return ScrapeReadmeFirstParagraph(repoPath), nil
	}

	cleaned := sanitizeGeneratedDescription(out)
	if cleaned == "" {
		// LLM emitted the sentinel or an empty / unusable response.
		// Try the regex scrape one more time; if THAT is also empty
		// the caller (add-repo) writes a blank description.
		return ScrapeReadmeFirstParagraph(repoPath), nil
	}
	return cleaned, nil
}

// sanitizeGeneratedDescription strips the common LLM-output noise
// (token-usage annotations, "Description:" prefixes, leading bullet
// markers, surrounding quotes) and rune-truncates to
// ReadmeDescriptionMaxLen with a trailing "…" on overflow. Returns
// "" when the cleaned output is the "(no description)" sentinel OR
// is empty after sanitisation.
//
// Exported indirectly via tests in inprocess_d12_test.go; kept
// lowercase here so external packages can't grow a dependency on
// the exact sanitisation shape.
func sanitizeGeneratedDescription(raw string) string {
	// 1. Strip CLI runner usage annotation (mirrors the
	//    stripUsageAnnotation shape used by SummarizeForContextOverflow).
	if idx := strings.LastIndex(raw, "\n[claude_usage:"); idx >= 0 {
		raw = raw[:idx]
	}
	if idx := strings.LastIndex(raw, "\n[claude_model:"); idx >= 0 {
		raw = raw[:idx]
	}

	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}

	// 2. Collapse to a single line. The contract is "single line of
	//    plain prose" but well-meaning models still emit two-line
	//    answers; we fold them into one.
	s = CollapseWhitespace(s)
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}

	// 3-5. Strip leading bullet markers, surrounding quotes, and
	//      "Description:" / "Summary:" prefixes. Iterate until the
	//      output is stable so a compound input like
	//      `- "Description: Hello."` peels back to "Hello." in
	//      whatever order the markers stack.
	for i := 0; i < 4; i++ {
		before := s
		s = strings.TrimSpace(stripLeadingBullet(s))
		s = strings.TrimSpace(stripMatchedQuotes(s))
		for _, prefix := range []string{"description:", "summary:", "repo description:"} {
			if len(s) > len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
				s = strings.TrimSpace(s[len(prefix):])
			}
		}
		if s == before {
			break
		}
	}

	// 6. Sentinel check — the LLM's "no description" signal collapses
	//    to an empty string for callers.
	if strings.EqualFold(s, generateRepoDescriptionNoDescSentinel) {
		return ""
	}

	if s == "" {
		return ""
	}

	// 7. Rune-aware truncate to ReadmeDescriptionMaxLen with "…".
	runes := []rune(s)
	if len(runes) > ReadmeDescriptionMaxLen {
		runes = runes[:ReadmeDescriptionMaxLen]
		for len(runes) > 0 && (runes[len(runes)-1] == ' ' || runes[len(runes)-1] == '\t') {
			runes = runes[:len(runes)-1]
		}
		return string(runes) + "…"
	}
	return string(runes)
}

// stripLeadingBullet strips a single bullet / numbered-list marker
// from the start of s. Recognised forms: "- ", "* ", "• ", "1. ",
// "1) ". Returns s unchanged when no marker is present.
func stripLeadingBullet(s string) string {
	if s == "" {
		return s
	}
	if strings.HasPrefix(s, "- ") || strings.HasPrefix(s, "* ") || strings.HasPrefix(s, "• ") {
		return s[2:]
	}
	// "1. " / "1) " / "10. " / "10) " — single or multi-digit prefix
	// followed by . or ) followed by space.
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i > 0 && i+1 < len(s) && (s[i] == '.' || s[i] == ')') && s[i+1] == ' ' {
		return s[i+2:]
	}
	return s
}

// stripMatchedQuotes removes one matched pair of leading + trailing
// quote characters (", ', `, ", '). Asymmetric quotes pass through
// unchanged.
func stripMatchedQuotes(s string) string {
	if len(s) < 2 {
		return s
	}
	pairs := [][2]rune{
		{'"', '"'},
		{'\'', '\''},
		{'`', '`'},
		{'“', '”'}, // “ ”
		{'‘', '’'}, // ‘ ’
	}
	runes := []rune(s)
	for _, p := range pairs {
		if runes[0] == p[0] && runes[len(runes)-1] == p[1] {
			return string(runes[1 : len(runes)-1])
		}
	}
	return s
}
