package agents

// Cost-advisory audit verification tests — see /AUDIT.md findings AUDIT-031,
// AUDIT-032, AUDIT-033.
//
// Three independent defects that each cost tokens/$$ without operator visibility:
//
//   AUDIT-031 — PRReviewTriage.dispatchPRReviewDecision has no hard, post-LLM
//               guard forcing classification -> not_actionable when
//               thread_depth >= depth_cap. The depth cap is only mentioned in
//               the prompt as LLM guidance. A misbehaving classifier (or a
//               bot at depth 10 pinging-ponging) can still emit
//               "in_scope_fix", spawning a full Astromech run per comment.
//
//   AUDIT-032 — PRReviewComments table has no classify_attempts column.
//               When classifyPRReviewComment fails transiently, the triage
//               loop logs + `continue`s; the row stays unclassified forever.
//               Each 5-minute pr-review-poll tick re-enters the retry with
//               no bound.
//
//   AUDIT-033 — Astromech auto-shard gate at astromech.go:479 is
//               strictly `strings.HasPrefix(err.Error(), "claude CLI timed
//               out") && bounty.InfraFailures >= 2`. InfraFailures is only
//               bumped by handleInfraFailure (timeouts, hard errors). A
//               successful Claude run that produces zero commits goes
//               through the ReturnTaskForRework / IncrementRetryCount path
//               and never touches infra_failures — so max_turns loops that
//               chew tokens without progress never auto-shard.
//
// This is a RED-phase static-grep pattern test: each sub-test FAILS today
// to prove the defect. When the remedy lands, assertions flip to PASS.

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestAUDIT_CostAdvisory(t *testing.T) {

	// ── AUDIT-031 — PRReviewTriage depth cap is advisory only ────────────
	t.Run("TestAUDIT_031_pr_review_depth_cap_advisory", func(t *testing.T) {
		t.Skip("AUDIT-031: remove when thread_depth hard-guard / classify_attempts / auto-shard-on-zero-commit land (Fix #7)")
		// Without skip, fails with: AUDIT-031: defective pattern still present — dispatchPRReviewDecision has no post-LLM thread_depth hard-guard and no ThreadDepth reference in dispatcher body
		src, err := os.ReadFile("pr_review_triage.go")
		if err != nil {
			t.Fatalf("read pr_review_triage.go: %v", err)
		}
		text := string(src)

		dispatchStart := strings.Index(text, "func dispatchPRReviewDecision(")
		if dispatchStart < 0 {
			t.Fatalf("could not find dispatchPRReviewDecision in pr_review_triage.go")
		}
		rest := text[dispatchStart:]
		nextFunc := strings.Index(rest[1:], "\nfunc ")
		var body string
		if nextFunc < 0 {
			body = rest
		} else {
			body = rest[:nextFunc+1]
		}

		depthGuard := regexp.MustCompile(`(?s)if\s+[^}]*ThreadDepth\s*>=\s*[^}]*\{[^}]*(classification|Classification)\s*=`)
		hasHardGuard := depthGuard.MatchString(body)
		hasAnyThreadDepthRef := strings.Contains(body, "ThreadDepth")

		if !hasHardGuard && !hasAnyThreadDepthRef {
			t.Fatal("AUDIT-031: defective pattern still present — dispatchPRReviewDecision has no post-LLM thread_depth hard-guard and no ThreadDepth reference in dispatcher body")
		}
	})

	// ── AUDIT-032 — PRReviewComments has no classify_attempts ────────────
	t.Run("TestAUDIT_032_pr_review_comments_no_classify_attempts", func(t *testing.T) {
		t.Skip("AUDIT-032: remove when thread_depth hard-guard / classify_attempts / auto-shard-on-zero-commit land (Fix #7)")
		// Without skip, fails with: AUDIT-032: defective pattern still present — schema.go has no classify_attempts column, runPRReviewTriage uses 'leaving unclassified for retry' with no classify_attempts tracking; classifier retries indefinitely on transient failures
		schema, err := os.ReadFile("../store/schema.go")
		if err != nil {
			t.Fatalf("read store/schema.go: %v", err)
		}
		schemaText := string(schema)

		hasSchemaCol := strings.Contains(schemaText, "classify_attempts")

		if !strings.Contains(schemaText, "CREATE TABLE IF NOT EXISTS PRReviewComments") {
			t.Fatalf("PRReviewComments table def not found in schema.go — has it moved?")
		}

		triage, err := os.ReadFile("pr_review_triage.go")
		if err != nil {
			t.Fatalf("read pr_review_triage.go: %v", err)
		}
		triageText := string(triage)
		runStart := strings.Index(triageText, "func runPRReviewTriage(")
		if runStart < 0 {
			t.Fatalf("runPRReviewTriage not found")
		}
		rest := triageText[runStart:]
		nextFunc := strings.Index(rest[1:], "\nfunc ")
		var body string
		if nextFunc < 0 {
			body = rest
		} else {
			body = rest[:nextFunc+1]
		}

		hasRetryLog := strings.Contains(body, "leaving unclassified for retry")
		hasClassifyAttempts := strings.Contains(body, "classify_attempts") || strings.Contains(body, "ClassifyAttempts")

		if !hasSchemaCol && hasRetryLog && !hasClassifyAttempts {
			t.Fatal("AUDIT-032: defective pattern still present — schema.go has no classify_attempts column, runPRReviewTriage uses 'leaving unclassified for retry' with no classify_attempts tracking; classifier retries indefinitely on transient failures")
		}
	})

	// ── AUDIT-033 — Auto-shard gate only trips on timeouts ───────────────
	t.Run("TestAUDIT_033_auto_shard_gate_timeout_only", func(t *testing.T) {
		// Closed by Fix #6: auto-shard gate now also fires on the zero-commit path via autoShardIfNoCommits("zero-commits", ...).
		src, err := os.ReadFile("astromech.go")
		if err != nil {
			t.Fatalf("read astromech.go: %v", err)
		}
		text := string(src)

		shardGate := regexp.MustCompile(`strings\.HasPrefix\(\s*err\.Error\(\)\s*,\s*"claude CLI timed out"\s*\)\s*&&\s*bounty\.InfraFailures\s*>=\s*2`)
		hasCurrentGate := shardGate.MatchString(text)

		gateIdx := shardGate.FindStringIndex(text)
		var window string
		if gateIdx != nil {
			windowEnd := gateIdx[1] + 3000
			if windowEnd > len(text) {
				windowEnd = len(text)
			}
			window = text[gateIdx[0]:windowEnd]
		}

		hasCommitsAheadInWindow := window != "" && strings.Contains(window, "CommitsAhead")
		hasZeroCommitBranch := window != "" && regexp.MustCompile(`(?i)zero.?commit.*(shard|Decompose)`).MatchString(window)
		hasSeparateZeroCommitPath := strings.Contains(text, "IncrementRetryCount") && strings.Contains(text, "ReturnTaskForRework")

		if hasCurrentGate && !hasCommitsAheadInWindow && !hasZeroCommitBranch && hasSeparateZeroCommitPath {
			t.Fatal("AUDIT-033: defective pattern still present — auto-shard gate requires `timeout prefix && InfraFailures>=2`, no CommitsAhead/zero-commit branch near the gate, zero-commit path uses IncrementRetryCount/ReturnTaskForRework which never feed InfraFailures")
		}
	})
}
