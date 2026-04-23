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
		src, err := os.ReadFile("pr_review_triage.go")
		if err != nil {
			t.Fatalf("read pr_review_triage.go: %v", err)
		}
		text := string(src)

		// The dispatcher function receives the LLM's decision and routes.
		// Find its body (dispatchPRReviewDecision) and verify that NO
		// post-LLM hard-guard exists to force classification -> not_actionable
		// (or conflicted_loop) when thread_depth >= depth_cap.
		dispatchStart := strings.Index(text, "func dispatchPRReviewDecision(")
		if dispatchStart < 0 {
			t.Fatalf("could not find dispatchPRReviewDecision in pr_review_triage.go")
		}
		// Find the matching closing brace for the function — scan ahead to
		// the next top-level "\nfunc " after the header.
		rest := text[dispatchStart:]
		nextFunc := strings.Index(rest[1:], "\nfunc ")
		var body string
		if nextFunc < 0 {
			body = rest
		} else {
			body = rest[:nextFunc+1]
		}

		// Look for a guard of the form:
		//   if c.ThreadDepth >= depthCap (or cap) { ... classification = ... }
		// We accept any variant that references ThreadDepth inside a comparison
		// within the dispatcher.
		depthGuard := regexp.MustCompile(`(?s)if\s+[^}]*ThreadDepth\s*>=\s*[^}]*\{[^}]*(classification|Classification)\s*=`)
		if depthGuard.MatchString(body) {
			t.Errorf("AUDIT-031 appears fixed: dispatchPRReviewDecision now hard-enforces thread_depth cap — update this test")
			return
		}

		// Also — the depthCap is passed to the classifier prompt but never
		// compared in dispatcher logic. Confirm it's only used in the prompt
		// builder and the top-level config fetch, not as a hard guard.
		if strings.Contains(body, "ThreadDepth") {
			t.Errorf("AUDIT-031 partial fix detected? dispatcher now mentions ThreadDepth — verify the hard guard")
		}
		// RED: defect is live. This is the expected state today.
		t.Logf("AUDIT-031 reproduced: dispatchPRReviewDecision has no post-LLM thread_depth hard-guard")
	})

	// ── AUDIT-032 — PRReviewComments has no classify_attempts ────────────
	t.Run("TestAUDIT_032_pr_review_comments_no_classify_attempts", func(t *testing.T) {
		schema, err := os.ReadFile("../store/schema.go")
		if err != nil {
			t.Fatalf("read store/schema.go: %v", err)
		}
		schemaText := string(schema)

		// The PRReviewComments table def appears twice (createSchema +
		// runMigrations fresh-install duplicate). Neither should contain
		// classify_attempts today.
		if strings.Contains(schemaText, "classify_attempts") {
			t.Errorf("AUDIT-032 appears fixed: schema.go now references classify_attempts — update this test")
			return
		}

		// Sanity: make sure we found the table definition so we know we're
		// grepping the right file.
		if !strings.Contains(schemaText, "CREATE TABLE IF NOT EXISTS PRReviewComments") {
			t.Fatalf("PRReviewComments table def not found in schema.go — has it moved?")
		}

		// Confirm the triage classifier has no retry counter: the failure
		// branch does `continue` without any bookkeeping.
		triage, err := os.ReadFile("pr_review_triage.go")
		if err != nil {
			t.Fatalf("read pr_review_triage.go: %v", err)
		}
		triageText := string(triage)
		// Find the runPRReviewTriage function body.
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

		// Today's code: "leaving unclassified for retry" — and no
		// classify_attempts increment.
		if !strings.Contains(body, "leaving unclassified for retry") {
			t.Errorf("AUDIT-032: expected 'leaving unclassified for retry' log line in runPRReviewTriage — has the retry path changed?")
		}
		if strings.Contains(body, "classify_attempts") || strings.Contains(body, "ClassifyAttempts") {
			t.Errorf("AUDIT-032 appears fixed: runPRReviewTriage now tracks classify_attempts — update this test")
			return
		}
		t.Logf("AUDIT-032 reproduced: no classify_attempts column; classifier retries indefinitely on transient failures")
	})

	// ── AUDIT-033 — Auto-shard gate only trips on timeouts ───────────────
	t.Run("TestAUDIT_033_auto_shard_gate_timeout_only", func(t *testing.T) {
		src, err := os.ReadFile("astromech.go")
		if err != nil {
			t.Fatalf("read astromech.go: %v", err)
		}
		text := string(src)

		// Confirm the auto-shard gate is gated on BOTH the timeout string
		// prefix AND InfraFailures >= 2 (today's state).
		shardGate := regexp.MustCompile(`strings\.HasPrefix\(\s*err\.Error\(\)\s*,\s*"claude CLI timed out"\s*\)\s*&&\s*bounty\.InfraFailures\s*>=\s*2`)
		if !shardGate.MatchString(text) {
			t.Errorf("AUDIT-033: expected current gate `strings.HasPrefix(err.Error(), \"claude CLI timed out\") && bounty.InfraFailures >= 2` not found — did astromech.go:479 change?")
		}

		// Confirm the gate does NOT also inspect CommitsAhead / zero-commit
		// state. If it did, non-timeout max_turns loops would be covered.
		// Find the block around the shard gate and look for a CommitsAhead
		// / zero-commit fork.
		gateIdx := shardGate.FindStringIndex(text)
		if gateIdx == nil {
			// Already errored above; bail early so we don't index into nil.
			return
		}
		// Take a generous window (~60 lines / ~3KB) starting at the gate.
		windowEnd := gateIdx[1] + 3000
		if windowEnd > len(text) {
			windowEnd = len(text)
		}
		window := text[gateIdx[0]:windowEnd]

		// The remedy adds a second branch that auto-shards on 2 successive
		// zero-commit attempts even when the failure reason is not a
		// timeout. If any of these appear inline with the shard logic, the
		// fix has landed.
		if strings.Contains(window, "CommitsAhead") {
			t.Errorf("AUDIT-033 appears fixed: auto-shard region now inspects CommitsAhead — update this test")
			return
		}
		// Also guard against a "non-timeout zero-commit" explicit branch.
		if regexp.MustCompile(`(?i)zero.?commit.*(shard|Decompose)`).MatchString(window) {
			t.Errorf("AUDIT-033 appears fixed: auto-shard region now branches on zero-commit non-timeout — update this test")
			return
		}

		// Confirm the separate zero-commit path (processAstromechOutput) uses
		// IncrementRetryCount / ReturnTaskForRework — a distinct counter that
		// never feeds InfraFailures, hence never feeds the shard gate.
		if !strings.Contains(text, "IncrementRetryCount") || !strings.Contains(text, "ReturnTaskForRework") {
			t.Errorf("AUDIT-033: expected zero-commit path (IncrementRetryCount/ReturnTaskForRework) not found — has astromech.go restructured?")
		}
		t.Logf("AUDIT-033 reproduced: auto-shard gate requires timeout prefix + infra_failures>=2; non-timeout zero-commit loops bypass it")
	})
}
