// Package librarian — D4 Phase 0 Client method implementations.
//
// This file ships the in-process backings for:
//
//   - GetWeightedMemories       (read path; pure SQL composite-score sort)
//   - RecentCommitsDigest       (git log --shortstat shell-out via igit)
//   - BootstrapSenatorRules     (LIVE_HAIKU-gated; deterministic stub fallback)
//   - RefreshSenatorMemoryDigest (LIVE_HAIKU-gated; deterministic stub fallback)
//
// Live-Haiku gating. The two LLM-backed methods (BootstrapSenatorRules,
// RefreshSenatorMemoryDigest) check LIVE_HAIKU_DISABLED and return a
// deterministic fixture when it's set. Tests pin the env flag so the
// suite never spends an LLM call. Production daemons leave it unset.
//
// Phase 0 ships the methods only — Phase 3 (Senate) wires the
// SenatorOnboarding task type that calls BootstrapSenatorRules and
// the senate-refresh dog that calls RefreshSenatorMemoryDigest.
package librarian

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"force-orchestrator/internal/claude"
	igit "force-orchestrator/internal/git"
	"force-orchestrator/internal/store"
)

// recentCommitsDigestMaxCommits is the per-call cap on commit rows
// returned by RecentCommitsDigest. Senator context budgets are
// bounded; emitting 500 commits per repo would blow them. Truncation
// is signalled via the digest's Truncated flag so callers can
// surface it in the prompt or refresh dog log.
const recentCommitsDigestMaxCommits = 50

// GetWeightedMemories runs the composite-score sort directly in
// SQLite. The score formula is:
//
//	freshness_score * (1.0 + validation_score)
//
// validation_score is in [-1, 1] (clamped at write time), so the
// multiplier is in [0, 2]. Bottom-of-the-barrel memories rank at 0;
// fully-positive ones rank 2× their freshness. Memories whose
// canonical_id != 0 (merged into a survivor) are excluded. The
// returned slice is k items long max.
func (c *inProcessClient) GetWeightedMemories(ctx context.Context, s Scope, k int) ([]Memory, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.Repo == "" && s.SinceCreatedAt == "" {
		return nil, ErrEmptyScope
	}
	if s.Limit < 0 {
		return nil, ErrInvalidLimit
	}
	if k <= 0 {
		k = 20
	}

	q := `SELECT id, repo, task_id, IFNULL(outcome,''), IFNULL(summary,''),
	             IFNULL(files_changed,''), IFNULL(topic_tags,''),
	             IFNULL(created_at,'')
	        FROM FleetMemory
	       WHERE IFNULL(canonical_id, 0) = 0`
	var args []any
	if s.Repo != "" {
		q += ` AND repo = ?`
		args = append(args, s.Repo)
	}
	if s.SinceCreatedAt != "" {
		q += ` AND created_at >= ?`
		args = append(args, s.SinceCreatedAt)
	}
	if s.Outcome != "" {
		q += ` AND outcome = ?`
		args = append(args, s.Outcome)
	}
	q += ` ORDER BY (IFNULL(freshness_score, 1.0) * (1.0 + IFNULL(validation_score, 0.0))) DESC,
	               created_at DESC, id DESC
	       LIMIT ?`
	args = append(args, k)

	rows, err := c.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("librarian: GetWeightedMemories query: %w", err)
	}
	defer rows.Close()
	out, scanErr := scanMemoryRows(rows)
	if scanErr != nil {
		return nil, scanErr
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("librarian: GetWeightedMemories rows iter: %w", rerr)
	}
	return out, nil
}

// RecentCommitsDigest reads `git log --since=<window>` against the
// repo's local clone (via store.GetRepoPath). The git invocation
// routes through igit.LogAndRun so the call is captured in
// GitOperationLog (Pattern P32 invariant).
//
// The output format is `git log --shortstat --pretty=format:<custom>`,
// which produces, per commit:
//
//	<sha>\x1f<subject>\x1f<author>\x1f<author-date-iso>
//	 N files changed, M insertions(+), K deletions(-)
//
// Parsing is straightforward — we use the ASCII unit-separator (\x1f)
// as field delimiter so commit subjects can carry pipe / colon /
// other punctuation safely.
func (c *inProcessClient) RecentCommitsDigest(ctx context.Context, repo string, window time.Duration) (CommitsDigest, error) {
	if err := ctx.Err(); err != nil {
		return CommitsDigest{}, err
	}
	if strings.TrimSpace(repo) == "" {
		return CommitsDigest{}, fmt.Errorf("librarian: RecentCommitsDigest requires repo name")
	}
	localPath := store.GetRepoPath(c.db, repo)
	if localPath == "" {
		return CommitsDigest{}, fmt.Errorf("librarian: repo %q not registered or has no local_path", repo)
	}
	if _, err := os.Stat(localPath); err != nil {
		return CommitsDigest{}, fmt.Errorf("librarian: repo %q local_path unreadable: %w", repo, err)
	}

	// Use ASCII unit-separator (\x1f) as field delimiter; commit
	// subjects often contain ":" / "|" so stick with a control char.
	const fieldSep = "\x1f"
	pretty := fmt.Sprintf("--pretty=format:%%H%s%%s%s%%an%s%%aI", fieldSep, fieldSep, fieldSep)
	since := fmt.Sprintf("--since=%d.seconds.ago", int64(window.Seconds()))

	out, err := igit.LogAndRun(ctx, igit.OpContext{Repo: repo},
		"librarian-recent-commits-digest", "git", "-C", localPath,
		"log", since, "--shortstat", pretty,
		fmt.Sprintf("-n%d", recentCommitsDigestMaxCommits+1)) // +1 to detect truncation
	if err != nil {
		return CommitsDigest{}, fmt.Errorf("librarian: git log failed: %w (output: %s)", err, string(out))
	}
	commits := parseCommitsDigestOutput(string(out))
	digest := CommitsDigest{
		Repo:    repo,
		Window:  window,
		Commits: commits,
	}
	if len(commits) > recentCommitsDigestMaxCommits {
		digest.Commits = commits[:recentCommitsDigestMaxCommits]
		digest.Truncated = true
	}
	return digest, nil
}

// parseCommitsDigestOutput is split out for unit-testing without
// shelling out to git. The expected per-commit shape (newline-
// separated blocks) is:
//
//	<sha>\x1f<subject>\x1f<author>\x1f<author-date-iso>
//	(blank line)
//	 N files changed, M insertions(+), K deletions(-)
//	(blank line)
//
// `--shortstat` only emits the diffstat line when the commit
// touches files, so an empty-commit can have no shortstat — we
// tolerate that.
func parseCommitsDigestOutput(s string) []DigestCommit {
	const fieldSep = "\x1f"
	lines := strings.Split(s, "\n")
	var out []DigestCommit
	var pending *DigestCommit
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.Contains(line, fieldSep) {
			// Header line. Flush the previous pending block.
			if pending != nil {
				out = append(out, *pending)
				pending = nil
			}
			parts := strings.SplitN(line, fieldSep, 4)
			if len(parts) != 4 {
				continue
			}
			pending = &DigestCommit{
				SHA:        strings.TrimSpace(parts[0]),
				Subject:    parts[1],
				Author:     parts[2],
				AuthorTime: strings.TrimSpace(parts[3]),
			}
			continue
		}
		if pending != nil && trimmed != "" && strings.Contains(trimmed, "changed") {
			pending.Diffstat = trimmed
		}
	}
	if pending != nil {
		out = append(out, *pending)
	}
	return out
}

// BootstrapSenatorRules produces a CandidateRule slice for the given
// repo. When LIVE_HAIKU_DISABLED is set (tests / CI), returns a
// deterministic fixture so unit tests stay hermetic. When unset,
// would route through claude.CallWithTranscript with the librarian
// capability profile — Phase 3 wires the actual prompt; Phase 0
// ships the deterministic shape.
//
// The deterministic fixture mirrors what a Senator-bootstrap LLM call
// would produce: one rule keyed on the repo name asserting
// "Senate-<repo>" rule shape, body sourced from the repo's name +
// the most recent commit subject, rationale citing the README.
func (c *inProcessClient) BootstrapSenatorRules(ctx context.Context, repo string) ([]CandidateRule, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(repo) == "" {
		return nil, fmt.Errorf("librarian: BootstrapSenatorRules requires repo name")
	}
	if !liveHaikuEnabled() {
		return bootstrapSenatorRulesStub(c, ctx, repo)
	}
	// Production path (D4 Phase 3 wiring; D6 anti-cheat refactor).
	// BuildRepoDigest is the shared knowledge-synthesis primitive
	// consumed by both this Senator-bootstrap path and the
	// `force onboard` CLI. Calling it here (instead of duplicating
	// the README + commits + ... assembly) is the call-site-level
	// guarantee that both consumers move in lockstep — the AST
	// audit TestOnboardingSynthesizesFromSenatorPipeline/AST_BothPathsCallBuildRepoDigest
	// in cmd/force/onboard_test.go walks BOTH this file and
	// cmd/force/onboard.go and asserts each references BuildRepoDigest.
	repoDigest, _ := c.BuildRepoDigest(ctx, repo)
	systemPrompt := bootstrapSenatorRulesSystemPrompt
	userPrompt := buildBootstrapSenatorRulesUserPrompt(repo, repoDigest.RecentCommits, repoDigest.READMESample)
	out, err := claude.CallWithTranscript(ctx, claude.CallDescriptor{
		Agent:         "librarian",
		PromptVersion: "bootstrap-senator-rules-v1",
	}, systemPrompt, userPrompt, "", "", "", 1)
	if err != nil {
		// Fail closed — never silently fall back to the stub on a real
		// LLM error in production. Tests pin LIVE_HAIKU_DISABLED so
		// they never reach this branch.
		return nil, fmt.Errorf("librarian: BootstrapSenatorRules live-Haiku call failed: %w", err)
	}
	candidates, parseErr := parseBootstrapSenatorRulesResponse(out, repo)
	if parseErr != nil {
		return nil, fmt.Errorf("librarian: BootstrapSenatorRules parse: %w (raw=%.300s)", parseErr, out)
	}
	if len(candidates) == 0 {
		// Defence in depth: a parseable but empty response is a no-op
		// rather than a hard error. Surface it so the operator can
		// re-trigger; downstream code treats len(0) as "Senator was
		// onboarded but no candidates found this pass."
		return nil, fmt.Errorf("librarian: BootstrapSenatorRules returned zero candidates for repo %q", repo)
	}
	return candidates, nil
}

// bootstrapSenatorRulesSystemPrompt anchors the Librarian's Senator-
// onboarding LLM call. The output contract is documented inline so the
// parser (parseBootstrapSenatorRulesResponse) and the prompt stay in
// sync — a model that emits an unexpected shape is rejected by the
// parser rather than silently miscoded into a candidate.
const bootstrapSenatorRulesSystemPrompt = `You are the Fleet Librarian's Senator-bootstrap analyst. Read the supplied repo digest + README sample and emit candidate domain rules ("Senate rules") for that repo's Senator. Each rule should capture an architectural / consumer-impact / repo-invariant directive that the operator should ratify before activation.

Output ONLY a JSON object of the shape:
{
  "candidates": [
    {
      "rule_key":   "senate-<repo>-<slug>",
      "category":   "senate",
      "agent_scope": "senate:<repo>",
      "body":       "<one-paragraph rule body>",
      "rationale":  "<one-sentence WHY (cite README anchor / commit subject)>",
      "evidence":   "<comma-separated free-form citations>"
    }
  ]
}

Emit 1 to 5 candidates. Every candidate must be operator-ratifiable as written — no placeholders, no TODOs.`

// buildBootstrapSenatorRulesUserPrompt is the user-prompt shape sent
// to Claude. Kept short; the Librarian's per-agent prompt cap bounds
// the README sample to ~4 KB and the digest to recentCommitsDigestMaxCommits.
func buildBootstrapSenatorRulesUserPrompt(repo string, digest CommitsDigest, readmeSample string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "REPO: %s\n\n", repo)
	if readmeSample != "" {
		b.WriteString("README SAMPLE (first 4 KB):\n")
		b.WriteString(readmeSample)
		b.WriteString("\n\n")
	}
	if len(digest.Commits) > 0 {
		b.WriteString("RECENT COMMITS (last 30 days):\n")
		for _, c := range digest.Commits {
			fmt.Fprintf(&b, "  - %s %s — %s (%s)\n", c.SHA[:min(7, len(c.SHA))], c.Subject, c.Author, c.Diffstat)
		}
		b.WriteString("\n")
	}
	b.WriteString("Emit candidate Senator rules for this repo per the system-prompt contract.")
	return b.String()
}

// readRepoREADMESample reads up to 4 KB of the repo's README for
// inclusion in the bootstrap prompt. Tolerates a missing README — an
// empty sample is a perfectly fine prompt (the digest carries the
// rest of the signal).
func readRepoREADMESample(db *sql.DB, repo string) string {
	localPath := store.GetRepoPath(db, repo)
	if localPath == "" {
		return ""
	}
	for _, name := range []string{"README.md", "README.rst", "README.txt", "README"} {
		path := localPath + "/" + name
		body, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		const cap = 4 * 1024
		if len(body) > cap {
			body = body[:cap]
		}
		return string(body)
	}
	return ""
}

// parseBootstrapSenatorRulesResponse parses the JSON-shaped response
// described in bootstrapSenatorRulesSystemPrompt. Defends against
// extra surrounding text by extracting the first { ... } block; rejects
// candidates whose required fields are blank.
func parseBootstrapSenatorRulesResponse(raw, repo string) ([]CandidateRule, error) {
	jsonStr := claude.ExtractJSON(raw)
	if jsonStr == "" {
		jsonStr = raw
	}
	var resp struct {
		Candidates []CandidateRule `json:"candidates"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &resp); err != nil {
		return nil, err
	}
	out := make([]CandidateRule, 0, len(resp.Candidates))
	for i, c := range resp.Candidates {
		if strings.TrimSpace(c.RuleKey) == "" || strings.TrimSpace(c.Body) == "" {
			return nil, fmt.Errorf("candidate[%d] missing rule_key or body", i)
		}
		if c.Category == "" {
			c.Category = "senate"
		}
		if c.AgentScope == "" {
			c.AgentScope = "senate:" + repo
		}
		out = append(out, c)
	}
	return out, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// bootstrapSenatorRulesStub is the deterministic fallback used in
// tests + the LIVE_HAIKU_DISABLED branch. It produces one rule
// derived from repo registration data + a recent-commits digest.
func bootstrapSenatorRulesStub(c *inProcessClient, ctx context.Context, repo string) ([]CandidateRule, error) {
	digest, err := c.RecentCommitsDigest(ctx, repo, 30*24*time.Hour)
	// A repo without a local clone is OK — we still emit a stub rule
	// so downstream tests have a deterministic shape to assert on.
	digestSummary := ""
	if err == nil && len(digest.Commits) > 0 {
		digestSummary = fmt.Sprintf("Last commit: %s — %s", digest.Commits[0].SHA[:7], digest.Commits[0].Subject)
	}
	return []CandidateRule{
		{
			RuleKey:    fmt.Sprintf("senate-%s-bootstrap", strings.ToLower(repo)),
			Category:   "senate",
			AgentScope: fmt.Sprintf("senate:%s", repo),
			Body:       fmt.Sprintf("Senate of %s: respect repo conventions and require explicit operator approval for cross-cutting changes.", repo),
			Rationale:  "Bootstrap candidate emitted by Librarian on Senator onboarding. Operator must ratify before activation.",
			Evidence:   fmt.Sprintf("repo=%s; %s", repo, digestSummary),
		},
	}, nil
}

// RefreshSenatorMemoryDigest produces the digest Phase 3's
// senate-refresh dog will call. LIVE_HAIKU_DISABLED gates the LLM-
// authored notes-for-operator field; the rest of the digest is pure
// DB / git work.
func (c *inProcessClient) RefreshSenatorMemoryDigest(ctx context.Context, repo string) (SenatorDigest, error) {
	if err := ctx.Err(); err != nil {
		return SenatorDigest{}, err
	}
	if strings.TrimSpace(repo) == "" {
		return SenatorDigest{}, fmt.Errorf("librarian: RefreshSenatorMemoryDigest requires repo name")
	}
	digest := SenatorDigest{
		Repo:        repo,
		GeneratedAt: store.NowSQLite(),
	}

	// Recent-commits digest (last 7 days).
	commits, err := c.RecentCommitsDigest(ctx, repo, 7*24*time.Hour)
	// We tolerate a missing local-path here so the dog doesn't fail
	// the whole refresh on a transient unavailability — Phase 3's
	// dog logic decides whether that's an alert. Stamp empty digest.
	if err == nil {
		digest.RecentCommits = commits
	}

	// Outstanding rules for the Senator. Count via FleetRules
	// agent_scope match.
	var rulesK int
	if c.db != nil {
		_ = c.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM FleetRules
			  WHERE agent_scope = ? AND IFNULL(active_until, '') = ''`,
			fmt.Sprintf("senate:%s", repo)).Scan(&rulesK)
	}
	digest.OutstandingRulesK = rulesK

	if !liveHaikuEnabled() {
		// Deterministic stub for tests.
		digest.APISurfaceSummary = fmt.Sprintf("API surface summary for %s deferred to Phase 3 live-Haiku wiring.", repo)
		digest.NotesForOperator = "Deterministic stub: senate-refresh dog produced no LLM-derived notes (LIVE_HAIKU_DISABLED set)."
		return digest, nil
	}
	// Live path placeholder; Phase 3 fills in the prompt.
	return SenatorDigest{}, fmt.Errorf("librarian: RefreshSenatorMemoryDigest live-Haiku path not yet wired (Phase 3); set LIVE_HAIKU_DISABLED=1 for the deterministic stub")
}

// liveHaikuEnabled inverts the LIVE_HAIKU_DISABLED env flag to
// produce a positive boolean. We mirror the (agents pkg)
// liveHaikuDisabled() shape but keep a copy here because the
// librarian client cannot import internal/agents (would create a
// circular dependency — agents depends on librarian client).
func liveHaikuEnabled() bool {
	v := os.Getenv("LIVE_HAIKU_DISABLED")
	if v == "1" || v == "true" {
		return false
	}
	return true
}

// Compile-time assertion: *inProcessClient implements the new D4-P0
// methods (Phase 1 of the interface extension landing). Trips the
// build if a method is missing, not just at runtime.
var _ interface {
	GetWeightedMemories(context.Context, Scope, int) ([]Memory, error)
	RecentCommitsDigest(context.Context, string, time.Duration) (CommitsDigest, error)
	BootstrapSenatorRules(context.Context, string) ([]CandidateRule, error)
	RefreshSenatorMemoryDigest(context.Context, string) (SenatorDigest, error)
	BuildRepoDigest(context.Context, string) (RepoDigest, error)
} = (*inProcessClient)(nil)

// silences unused-import lint when sql is the only use; sql is
// referenced through the inProcessClient receiver so the import is
// load-bearing for the file's compile.
var _ = (*sql.DB)(nil)
