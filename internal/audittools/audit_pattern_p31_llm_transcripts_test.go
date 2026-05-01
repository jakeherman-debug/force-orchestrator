// D3 P6B.1 — Pattern P31: every Claude CLI call site must flow through
// the LLMCallTranscripts capture wrapper.
//
// Walks production code (non-_test.go) under internal/agents/ and
// internal/claude/ for direct calls to:
//
//   - claude.AskClaudeCLI
//   - claude.AskClaudeCLIContext
//   - claude.RunCLI
//   - claude.RunCLIStreaming
//   - claude.RunCLIStreamingContext
//
// Every direct call site must either:
//   - be reached only from inside a CallWithTranscript* wrapper (i.e. the
//     wrapper file itself: internal/claude/transcript.go), OR
//   - appear in p31Allowlist with a one-line truthful rationale that
//     names the call site, the migration path, and why the wrapper isn't
//     in place yet (typically: "scheduled for the 6B follow-up commit
//     train" — same shape as P27's notification-budget backlog).
//
// Anti-cheat: the allowlist is a SET, not a slope — every entry is a
// debt the orchestrator owes. Forward-going code MUST route through the
// wrapper; new entries require an explicit reviewer-visible rationale.
package audittools

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// p31Allowlist names "<file>:<call_kind>" entries that don't yet route
// through the transcript wrapper. Each carries a one-line rationale.
//
// Migration backlog from D3 P6B.1: every existing direct call site is
// recorded here at landing time. Forward-going code MUST go through
// claude.CallWithTranscript* helpers. The 6B follow-up commit train
// (and selected D4 work) sweeps these to the wrapper.
var p31Allowlist = map[string]string{
	// claude.AskClaudeCLI is itself a thin wrapper around
	// AskClaudeCLIContext. Routing it through the transcript wrapper
	// would be circular — the wrapper calls AskClaudeCLIContext.
	"internal/claude/claude.go": "claude.AskClaudeCLI / AskClaudeCLIContext / RunCLI / RunCLIStreaming{,Context} live in the same translation unit as the wrapper; routing them through CallWithTranscript would be infinite recursion. The wrapper IS the layer that calls them.",
	// transcript.go itself calls the underlying helpers — that's the
	// whole point of the wrapper.
	"internal/claude/transcript.go": "this file IS the wrapper; its calls to AskClaudeCLIContext / RunCLIStreamingContext / RunCLI are the wrapper's downward edge into the real CLI helpers",

	// Pre-P6B.1 call sites — migration backlog (sweep target for the
	// 6B follow-up commit train). Each rationale names the agent +
	// the migration path. The migration is mechanical: replace the
	// direct call with claude.CallWithTranscript(ctx, descriptor,
	// systemPrompt, userPrompt, ...), where descriptor is built from
	// (agent name, task id, prompt version) at the call site.
	"internal/agents/captain.go":              "Captain ruling — pre-6B direct AskClaudeCLIContext at line ~439; migration: pass desc.Agent='captain' / desc.TaskID=task.ID / desc.PromptVersion=captainPromptVersion. Migration target: 6B follow-up train.",
	"internal/agents/medic.go":                "Medic decision — pre-6B direct AskClaudeCLI; migration: route via CallWithTranscript with desc.Agent='medic'. Migration target: 6B follow-up train.",
	"internal/agents/medic_ci.go":             "Medic CI decision — pre-6B direct AskClaudeCLI; migration as for medic.go. Migration target: 6B follow-up train.",
	"internal/agents/jedi_council.go":         "Council ruling — pre-6B direct AskClaudeCLI; migration: desc.Agent='council'. Migration target: 6B follow-up train.",
	"internal/agents/convoy_review.go":        "ConvoyReview synthesis — pre-6B direct AskClaudeCLI; migration: desc.Agent='convoy-review'. Migration target: 6B follow-up train.",
	"internal/agents/pr_review_triage.go":     "PR review triage — pre-6B direct AskClaudeCLI; migration: desc.Agent='pr-review-triage'. Migration target: 6B follow-up train.",
	"internal/agents/chancellor.go":           "Chancellor merge synth — pre-6B direct AskClaudeCLIContext; migration: desc.Agent='chancellor'. Migration target: 6B follow-up train.",
	"internal/agents/astromech.go":            "Astromech streaming — pre-6B direct RunCLIStreamingContext; migration: route via CallWithTranscriptStreaming with desc.Agent='astromech'. Migration target: 6B follow-up train.",
	"internal/agents/auditor.go":              "Auditor RunCLI — pre-6B direct RunCLI; migration: CallWithTranscriptOneShot with desc.Agent='auditor'. Migration target: 6B follow-up train.",
	"internal/agents/investigator.go":         "Investigator RunCLI — pre-6B direct RunCLI; migration as auditor. Migration target: 6B follow-up train.",
	"internal/agents/commander.go":            "Commander streaming — pre-6B direct RunCLIStreaming; migration: CallWithTranscriptStreaming with desc.Agent='commander'. Migration target: 6B follow-up train.",
	"internal/agents/diplomat.go":             "Diplomat — pre-6B direct AskClaudeCLI; migration: desc.Agent='diplomat'. Migration target: 6B follow-up train.",
	"internal/agents/librarian.go":            "Librarian — pre-6B direct AskClaudeCLI; migration: desc.Agent='librarian'. Migration target: 6B follow-up train.",
	"internal/agents/pilot.go":                "Pilot — pre-6B direct AskClaudeCLI inside ClassifyTaskType-style helper; migration: desc.Agent='pilot'. Migration target: 6B follow-up train.",
	"internal/agents/boot.go":                 "Boot — pre-6B direct AskClaudeCLI on daemon startup; transient, low-volume; migration optional but slated. Migration target: 6B follow-up train.",
	"internal/agents/memory_rerank.go":        "Memory re-rank — pre-6B direct AskClaudeCLI; migration: desc.Agent='memory-rerank'. Migration target: 6B follow-up train.",
	"internal/agents/adversarial_wiring.go":   "Adversarial wiring (3 sites) — pre-6B direct AskClaudeCLIContext; migration: desc.Agent='adversarial-{discover,critique,tournament}'. Migration target: 6B follow-up train.",
	"internal/agents/engineering_corps/metric_author.go":     "EC metric author — pre-6B direct AskClaudeCLIContext; migration: desc.Agent='ec-metric-author'. Migration target: 6B follow-up train.",
	"internal/agents/engineering_corps/experiment_author.go": "EC experiment author — pre-6B direct AskClaudeCLIContext; migration: desc.Agent='ec-experiment-author'. Migration target: 6B follow-up train.",
}

// p31CallPattern detects call expressions to any of the canonical
// entry-point CLI helpers. The match is intentionally lexical (not AST)
// because the surface is small + stable, and a regex makes the
// allowlist semantics auditable from the test failure message alone.
var p31CallPattern = regexp.MustCompile(
	`claude\.(?:AskClaudeCLI(?:Context)?|RunCLI(?:Streaming(?:Context)?)?)\b`,
)

func TestPattern_P31_AllLLMCallsCaptured(t *testing.T) {
	root := repoRootP31(t)

	type hit struct {
		path string
		line int
		text string
	}
	var hits []hit

	walkDirs := []string{"internal/agents", "internal/claude"}
	for _, dir := range walkDirs {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}
			if strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, rErr := readFileP31(path)
			if rErr != nil {
				return nil
			}
			content := string(b)
			for ln, line := range strings.Split(content, "\n") {
				// Skip comments — we only care about real call expressions.
				trimmed := strings.TrimSpace(line)
				if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "*") {
					continue
				}
				if p31CallPattern.MatchString(line) {
					rel, _ := filepath.Rel(root, path)
					hits = append(hits, hit{path: rel, line: ln + 1, text: trimmed})
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}

	// For each hit, demand either: (a) the file is in the allowlist,
	// OR (b) the file is the wrapper itself (transcript.go). All other
	// hits are violations — they must move to the wrapper or take an
	// allowlist entry with rationale.
	violations := map[string][]hit{}
	for _, h := range hits {
		if _, ok := p31Allowlist[h.path]; ok {
			continue
		}
		violations[h.path] = append(violations[h.path], h)
	}

	if len(violations) > 0 {
		var msg strings.Builder
		msg.WriteString("Pattern P31 violation: direct claude.* CLI calls outside the LLMCallTranscripts wrapper:\n\n")
		paths := make([]string, 0, len(violations))
		for p := range violations {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		for _, p := range paths {
			for _, h := range violations[p] {
				msg.WriteString("  ")
				msg.WriteString(p)
				msg.WriteString(":")
				msg.WriteString(itoaP31(h.line))
				msg.WriteString(" ")
				msg.WriteString(h.text)
				msg.WriteString("\n")
			}
		}
		msg.WriteString("\nFix: route the call through claude.CallWithTranscript(ctx, claude.CallDescriptor{...}, ...)\n")
		msg.WriteString("OR: add an allowlist entry to p31Allowlist with a one-line truthful rationale.\n")
		t.Error(msg.String())
	}

	// Also assert the allowlist itself is non-empty AND every entry
	// carries a rationale — defence in depth so a future commit can't
	// silently neutralise the pattern by emptying the rationale.
	for path, rationale := range p31Allowlist {
		if strings.TrimSpace(rationale) == "" {
			t.Errorf("Pattern P31 allowlist: %q has empty rationale", path)
		}
	}
}

func repoRootP31(t *testing.T) string {
	t.Helper()
	wd, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	for d := wd; d != "/" && d != ""; d = filepath.Dir(d) {
		if _, err := readFileP31(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
	}
	t.Fatalf("repo root not found from %s", wd)
	return ""
}

// readFileP31 is a tiny helper kept local so the test doesn't depend on
// the broader audittools test infrastructure.
func readFileP31(path string) ([]byte, error) {
	return os.ReadFile(path)
}

func itoaP31(n int) string {
	// Tiny inline itoa to avoid an import cycle / strconv dance.
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	buf := make([]byte, 0, 12)
	for n > 0 {
		buf = append(buf, byte('0'+n%10))
		n /= 10
	}
	if neg {
		buf = append(buf, '-')
	}
	// reverse
	for i, j := 0, len(buf)-1; i < j; i, j = i+1, j-1 {
		buf[i], buf[j] = buf[j], buf[i]
	}
	return string(buf)
}
