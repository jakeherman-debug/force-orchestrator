package agents

// p5_shakedown_test.go — three end-to-end shakedown tests for D3 Phase
// 5's exit criteria, per docs/roadmap.md §D3 P5:
//
//   1. one tool-using-agent experiment runs in shadow mode to termination
//   2. one adversarial pair surfaces a real disagreement (operator
//      handles via the surfaced flow)
//   3. first golden-set evaluation cycle completes
//
// The shakedowns are fully self-contained: no real claude calls, no
// real gh / git network ops. Each exercises the package surface
// landed in tracks A / B / C end-to-end against an in-memory SQLite
// + a temp-dir git repo where applicable.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"force-orchestrator/internal/agents/adversarial"
	"force-orchestrator/internal/agents/golden_set"
	"force-orchestrator/internal/agents/shadow"
	"force-orchestrator/internal/gh"
	"force-orchestrator/internal/store"
)

// shakedownInitRepo initializes a tiny git repo with one commit on
// main. Returns the absolute repo path.
func shakedownInitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", abs}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s: %v", args, string(out), err)
		}
	}
	run("init", "-b", "main")
	if err := os.WriteFile(filepath.Join(abs, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	run("add", "README.md")
	run("commit", "-m", "init")
	return abs
}

// shakedownStubRunner is a deterministic gh.Runner that records each
// call's args and returns canned (stdout, stderr, err). Used to prove
// the shadow proxy intercepts every gh invocation without making real
// network calls.
type shakedownStubRunner struct {
	calls   [][]string
	stdouts map[string]string // keyed by args[0] for cheap routing
}

func (s *shakedownStubRunner) Run(cwd string, args []string, stdin []byte) ([]byte, []byte, error) {
	s.calls = append(s.calls, append([]string(nil), args...))
	if len(args) > 0 {
		if out, ok := s.stdouts[args[0]]; ok {
			return []byte(out), nil, nil
		}
	}
	return []byte("{}"), nil, nil
}

// TestShakedown_ShadowExperimentToTermination exercises a tool-using-
// agent experiment running in shadow mode end-to-end:
//
//   - SetupShadowWorktreeAt creates the .force-shadow-worktrees/...
//     directory + the gh recording file
//   - the agent's gh calls go through the recording proxy and are
//     captured to JSONL
//   - shadow-mode write attempts (gh pr create) are suppressed via
//     the IsShadowGhWrite classifier + AppendSuppressed
//   - shadow-mode pushes are rewritten to a local-only refspec via
//     SuppressPush; no real git push to origin
//   - CleanupShadowWorktreeAt tears down the worktree on termination;
//     the gh recording file is preserved for post-hoc scoring
//
// Termination criterion: the test asserts the recording file
// contains both a read pass-through and a write suppression entry,
// and that no real gh write was issued (delegate stub never saw the
// "pr create" args).
func TestShakedown_ShadowExperimentToTermination(t *testing.T) {
	repo := shakedownInitRepo(t)
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Seed a paired_shadow ExperimentRuns row.
	res, err := db.Exec(`INSERT INTO ExperimentRuns
		(experiment_id, treatment_id, natural_unit_kind, natural_unit_id, mode, agent_name)
		VALUES (1, 1, 'feature', 100, 'paired_shadow', 'astromech-shakedown')`)
	if err != nil {
		t.Fatalf("seed run: %v", err)
	}
	runID, _ := res.LastInsertId()

	// 1. Setup shadow worktree.
	sess, err := shadow.SetupShadowWorktreeAt(context.Background(), db, runID, shadow.SetupOptions{
		RepoPath:  repo,
		BaseRef:   "main",
		AgentName: "astromech-shakedown",
	})
	if err != nil {
		t.Fatalf("SetupShadowWorktreeAt: %v", err)
	}
	if !strings.Contains(sess.WorktreePath, shadow.ShadowWorktreeRoot) {
		t.Fatalf("shadow worktree path missing prefix: %q", sess.WorktreePath)
	}

	// 2. Wrap a stub gh.Runner with the recording proxy.
	stub := &shakedownStubRunner{
		stdouts: map[string]string{
			"pr": `{"number":42,"state":"OPEN"}`,
		},
	}
	rec, err := shadow.NewRecordingRunner(gh.Runner(stub), sess.GhRecordingPath)
	if err != nil {
		t.Fatalf("NewRecordingRunner: %v", err)
	}

	// 3. Simulate the agent issuing one read (gh pr view) and one
	// write (gh pr create). The read flows through; the write is
	// classified as a write and routed to AppendSuppressed.
	readArgs := []string{"pr", "view", "42", "--json", "number"}
	if shadow.IsShadowGhWrite(readArgs) {
		t.Fatalf("read classifier broken: %v reported as write", readArgs)
	}
	rec.Run(sess.WorktreePath, readArgs, nil)

	writeArgs := []string{"pr", "create", "--title", "shadow PR", "--body", "do not merge"}
	if !shadow.IsShadowGhWrite(writeArgs) {
		t.Fatalf("write classifier broken: %v reported as read", writeArgs)
	}
	// Write path: do NOT delegate to real gh; record as suppressed.
	rr, ok := rec.(interface {
		AppendSuppressed(string, []string, string, string)
	})
	if !ok {
		t.Fatalf("recorder does not expose AppendSuppressed")
	}
	rr.AppendSuppressed(sess.WorktreePath, writeArgs, `{"url":"<suppressed>"}`, "")

	// 4. Simulate a shadow-mode push attempt. ShouldSuppressPush
	// returns true; SuppressPush rewrites to a local-only branch.
	if !shadow.ShouldSuppressPush(sess) {
		t.Fatalf("ShouldSuppressPush must return true for shadow runs")
	}
	pushOut := shadow.SuppressPush(context.Background(), sess, "origin main")
	if !pushOut.Suppressed || !strings.HasPrefix(pushOut.RewrittenBranch, "shadow-exp-") {
		t.Fatalf("SuppressPush did not rewrite to shadow branch: %+v", pushOut)
	}

	// 5. Close the recorder. Read the recording file BEFORE cleanup
	// (cleanup nukes the worktree directory; production code that
	// wants to retain the recording should copy it out before
	// teardown — the shakedown reads it in-place).
	if err := rec.Close(); err != nil {
		t.Fatalf("rec.Close: %v", err)
	}
	recordings, err := shadow.ReadRecordings(sess.GhRecordingPath)
	if err != nil {
		t.Fatalf("ReadRecordings: %v", err)
	}
	if err := shadow.CleanupShadowWorktreeAt(context.Background(), repo, sess); err != nil {
		t.Fatalf("CleanupShadowWorktreeAt: %v", err)
	}

	// 6. Termination assertions.
	//    - delegate stub saw the read but NOT the write.
	if len(stub.calls) != 1 {
		t.Fatalf("delegate stub: expected 1 pass-through (the read); got %d calls: %+v", len(stub.calls), stub.calls)
	}
	if stub.calls[0][1] != "view" {
		t.Fatalf("delegate stub: expected 'pr view' pass-through; got %v", stub.calls[0])
	}
	//    - recording file has both entries.
	if len(recordings) != 2 {
		t.Fatalf("expected 2 recorded gh invocations; got %d: %+v", len(recordings), recordings)
	}
	hasRead, hasSuppressedWrite := false, false
	for _, r := range recordings {
		if r.Suppressed {
			hasSuppressedWrite = true
		} else {
			hasRead = true
		}
	}
	if !hasRead || !hasSuppressedWrite {
		t.Fatalf("recordings missing read or suppressed write: %+v", recordings)
	}
}

// TestShakedown_AdversarialPairSurfacesDisagreement seeds a Council
// decision, runs RunAdversarialPair with a critic that disagrees,
// and asserts:
//
//   - AdversarialPairings row created with agreement = 0
//   - prompt_version_primary and prompt_version_critic both populated
//     with distinct values (anti-cheat invariant)
//   - SurfaceDisagreementToOperator wrote a Fleet_Mail row to the
//     operator inbox
func TestShakedown_AdversarialPairSurfacesDisagreement(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Register a critic that always rejects (disagrees with primary's
	// approval).
	adversarial.RegisterCritic(adversarial.AgentCouncil,
		func(ctx context.Context, p adversarial.PrimaryDecision) (adversarial.CriticOutcome, error) {
			return adversarial.CriticOutcome{
				Outcome:       `{"approved":false,"feedback":"missed regression in concurrent path"}`,
				PromptVersion: "council-critic-v1",
			}, nil
		})

	pair, err := adversarial.RunAdversarialPair(context.Background(), db, adversarial.PrimaryDecision{
		DecisionID:    777,
		Agent:         adversarial.AgentCouncil,
		Outcome:       `{"approved":true,"feedback":""}`,
		Reasoning:     "diff is small and tests look adequate",
		PromptVersion: "council-v3",
	})
	if err != nil {
		t.Fatalf("RunAdversarialPair: %v", err)
	}
	if pair.Agreement {
		t.Fatalf("primary=approved + critic=rejected must be a disagreement")
	}
	if pair.PromptVersionPrimary == pair.PromptVersionCritic {
		t.Fatalf("anti-cheat broken: primary and critic prompt versions match")
	}

	// Surface the disagreement.
	if err := adversarial.SurfaceDisagreementToOperator(context.Background(), db, pair.ID); err != nil {
		t.Fatalf("SurfaceDisagreementToOperator: %v", err)
	}

	// Confirm AdversarialPairings row.
	var (
		decisionID    int64
		agentStr      string
		agreement     int
		surfacedAt    string
		ppPrimary     string
		ppCritic      string
	)
	err = db.QueryRow(`SELECT decision_id, agent, IFNULL(agreement,0), IFNULL(surfaced_at,''),
	                          IFNULL(prompt_version_primary,''), IFNULL(prompt_version_critic,'')
	                   FROM AdversarialPairings WHERE id=?`, pair.ID).Scan(
		&decisionID, &agentStr, &agreement, &surfacedAt, &ppPrimary, &ppCritic)
	if err != nil {
		t.Fatalf("query AdversarialPairings: %v", err)
	}
	if decisionID != 777 {
		t.Fatalf("decision_id mismatch: %d", decisionID)
	}
	if agentStr != "council" {
		t.Fatalf("agent mismatch: %q", agentStr)
	}
	if agreement != 0 {
		t.Fatalf("agreement must be 0 for disagreement; got %d", agreement)
	}
	if surfacedAt == "" {
		t.Fatalf("surfaced_at must be set after SurfaceDisagreementToOperator")
	}
	if ppPrimary != "council-v3" || ppCritic != "council-critic-v1" {
		t.Fatalf("prompt versions not persisted: primary=%q critic=%q", ppPrimary, ppCritic)
	}

	// Confirm Fleet_Mail row.
	var mailCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM Fleet_Mail WHERE from_agent='adversarial-pairing'`).Scan(&mailCount); err != nil {
		t.Fatalf("count Fleet_Mail: %v", err)
	}
	if mailCount != 1 {
		t.Fatalf("expected 1 operator-facing mail row; got %d", mailCount)
	}
}

// TestShakedown_FirstGoldenSetCycleCompletes auto-curates fixtures
// from synthetic clean-shipping convoys, runs RunEvaluationCycleWith
// for one agent, and asserts:
//
//   - GoldenSetEvaluations populated with accuracy scores
//   - ReportAccuracyTrend returns a baseline-week value (regression
//     0.0; first week has no prior to compare against)
func TestShakedown_FirstGoldenSetCycleCompletes(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Seed two clean-shipping tasks for "council".
	_, err := db.Exec(`INSERT INTO BountyBoard (id, type, status, payload, owner) VALUES
		(901, 'CodeEdit', 'Completed', '{"task":"add login"}', 'council-Yoda'),
		(902, 'CodeEdit', 'Completed', '{"task":"add tests"}', 'council-Yoda')`)
	if err != nil {
		t.Fatalf("seed bounties: %v", err)
	}
	_, err = db.Exec(`INSERT INTO TaskHistory (task_id, attempt, agent, session_id, claude_output, outcome) VALUES
		(901, 1, 'council-Yoda', 's1', 'reviewed', '{"approved":true,"feedback":""}'),
		(902, 1, 'council-Yoda', 's2', 'reviewed', '{"approved":true,"feedback":""}')`)
	if err != nil {
		t.Fatalf("seed history: %v", err)
	}

	// Auto-curate.
	n, err := golden_set.CurateFromCleanShipping(context.Background(), db, "council")
	if err != nil {
		t.Fatalf("CurateFromCleanShipping: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 fixtures auto-curated; got %d", n)
	}

	// Add one operator-curated negative example so the set isn't a
	// pure tautology stack (the "honesty-keeper" from the roadmap).
	if _, err := golden_set.AddManualFixture(context.Background(), db, "council",
		`{"task":"do regression-prone refactor"}`,
		`{"approved":false,"feedback":"insufficient tests"}`,
		"jake@example.com"); err != nil {
		t.Fatalf("AddManualFixture: %v", err)
	}

	// Run an evaluation cycle. The injected EvaluatorFn echoes
	// expected_output verbatim → all three score 1.0.
	echo := func(ctx context.Context, fx golden_set.Fixture) (string, error) {
		return fx.ExpectedOutput, nil
	}
	count, err := golden_set.RunEvaluationCycleWith(context.Background(), db, "council", "council-v3", echo, nil)
	if err != nil {
		t.Fatalf("RunEvaluationCycleWith: %v", err)
	}
	if count != 3 {
		t.Fatalf("expected 3 evaluations (2 auto + 1 manual); got %d", count)
	}

	// Confirm GoldenSetEvaluations row count + perfect scores.
	var rows int
	db.QueryRow(`SELECT COUNT(*) FROM GoldenSetEvaluations WHERE agent='council' AND prompt_version='council-v3'`).Scan(&rows)
	if rows != 3 {
		t.Fatalf("expected 3 GoldenSetEvaluations rows; got %d", rows)
	}

	// Report accuracy trend. Baseline week → 1 trend row (or 1 per
	// week the seed timestamps span). All scores 1.0 → MeanAccuracy
	// = 1.0; baseline → RegressionFromPriorWeek = 0.0.
	trends, err := golden_set.ReportAccuracyTrend(context.Background(), db, "council", "")
	if err != nil {
		t.Fatalf("ReportAccuracyTrend: %v", err)
	}
	if len(trends) == 0 {
		t.Fatalf("expected at least 1 baseline-week trend row; got %d", len(trends))
	}
	if trends[0].MeanAccuracy != 1.0 {
		t.Fatalf("baseline MeanAccuracy: want 1.0, got %v", trends[0].MeanAccuracy)
	}
	if trends[0].RegressionFromPriorWeek != 0.0 {
		t.Fatalf("baseline RegressionFromPriorWeek: want 0.0, got %v", trends[0].RegressionFromPriorWeek)
	}
}

// (Confirm the not-found gating for the shadow setup so the
// shakedown's negative space is also covered: a non-paired-shadow run
// returns the sentinel.)
func TestShakedown_Shadow_RealRunSentinel(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	res, err := db.Exec(`INSERT INTO ExperimentRuns
		(experiment_id, treatment_id, natural_unit_kind, natural_unit_id, mode, agent_name)
		VALUES (2, 2, 'feature', 200, 'paired_real', 'astromech-real')`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	runID, _ := res.LastInsertId()
	_, err = shadow.SetupShadowWorktreeAt(context.Background(), db, runID, shadow.SetupOptions{
		RepoPath: t.TempDir(),
		BaseRef:  "HEAD",
	})
	if !errors.Is(err, shadow.ErrShadowNotConfigured) {
		t.Fatalf("real-mode run: want ErrShadowNotConfigured, got %v", err)
	}
}
