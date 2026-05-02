package supplydeferral

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	// Force-register the gemfile parser so manifests.Default().Detect("Gemfile")
	// finds it. Tests in this package don't import internal/isb/rules
	// (cycle), so we rely on the manifests sub-package's init() chain.
	_ "force-orchestrator/internal/isb/scanners/manifests/gemfile"

	"force-orchestrator/internal/store"
)

// ── Shared fixtures ──────────────────────────────────────────────────────

// initGemfileRepo sets up a temp git repo on branch `feature/x` with
// the supplied Gemfile content already committed at branch tip. Returns
// the repo path. Tests skip if `git` isn't available.
func initGemfileRepo(t *testing.T, gemfileContent string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH — skipping replay test")
	}

	dir := t.TempDir()

	gitRun := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test",
			"GIT_AUTHOR_EMAIL=test@test.com",
			"GIT_COMMITTER_NAME=Test",
			"GIT_COMMITTER_EMAIL=test@test.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}

	gitRun("init", "-b", "main")
	gitRun("config", "user.email", "test@test.com")
	gitRun("config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitRun("add", ".")
	gitRun("commit", "-m", "initial commit")

	gitRun("checkout", "-b", "feature/x")
	if err := os.WriteFile(filepath.Join(dir, "Gemfile"), []byte(gemfileContent), 0644); err != nil {
		t.Fatal(err)
	}
	gitRun("add", ".")
	gitRun("commit", "-m", "add Gemfile")

	return dir
}

// fakeRule is a stub ReplayableRule whose Run output is canned.
type fakeRule struct {
	id       string
	findings []ReplayFinding
	err      error
	called   int
	lastIn   ReplayInput
}

func (r *fakeRule) ID() string { return r.id }

func (r *fakeRule) Run(_ context.Context, _ *sql.DB, in ReplayInput) ([]ReplayFinding, error) {
	r.called++
	r.lastIn = in
	if r.err != nil {
		return nil, r.err
	}
	out := make([]ReplayFinding, len(r.findings))
	for i, f := range r.findings {
		// Canonicalise RuleID + Path so the replay's filter accepts.
		if f.RuleID == "" {
			f.RuleID = r.id
		}
		out[i] = f
	}
	return out, nil
}

// recordingLogger captures Printf calls for inspection.
type recordingLogger struct {
	lines []string
}

func (l *recordingLogger) Printf(format string, args ...any) {
	l.lines = append(l.lines, format)
	_ = args
}

// seedDeferral records one token_expired SecurityFinding row and
// returns its id.
func seedDeferral(t *testing.T, db *sql.DB, taskID int, ruleKey, branch, manifestPath string) int {
	t.Helper()
	id, err := RecordDeferral(db, taskID, DeferralPayload{
		RuleKey:      ruleKey,
		ManifestPath: manifestPath,
		Branch:       branch,
		CommitSHA:    "deadbeef",
	})
	if err != nil {
		t.Fatalf("RecordDeferral: %v", err)
	}
	if id == 0 {
		t.Fatal("RecordDeferral returned 0 (dedup) on first insert")
	}
	return id
}

// ── Tests ────────────────────────────────────────────────────────────────

// TestReplay_BranchClean_FlipsToResolvedLate exercises the happy path:
// the rule no longer fires → original row flipped to resolved_late.
func TestReplay_BranchClean_FlipsToResolvedLate(t *testing.T) {
	repo := initGemfileRepo(t, "gem 'redis', '5.0.0'\n")
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id := seedDeferral(t, db, 42, "SUPPLY-001", "feature/x", "Gemfile")

	rule := &fakeRule{id: "SUPPLY-001"} // no findings → resolved_late
	rules := map[string]ReplayableRule{"SUPPLY-001": rule}
	resolver := func(int) (string, error) { return repo, nil }
	logger := &recordingLogger{}

	results, err := ReplayPendingDeferrals(context.Background(), db, resolver, rules, logger)
	if err != nil {
		t.Fatalf("ReplayPendingDeferrals: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Outcome != ReplayOutcomeResolvedLate {
		t.Errorf("outcome: want resolved_late, got %s", results[0].Outcome)
	}
	if results[0].OriginalFindingID != id {
		t.Errorf("OriginalFindingID: want %d, got %d", id, results[0].OriginalFindingID)
	}

	// Original row should now have disposition='resolved_late'.
	var disp string
	if err := db.QueryRow(`SELECT disposition FROM SecurityFindings WHERE id = ?`, id).Scan(&disp); err != nil {
		t.Fatalf("query disposition: %v", err)
	}
	if disp != DispositionResolvedLate {
		t.Errorf("disposition: want %s, got %q", DispositionResolvedLate, disp)
	}

	// Rule must have been called exactly once.
	if rule.called != 1 {
		t.Errorf("rule.called: want 1, got %d", rule.called)
	}

	// The rule must have received the parsed deps from the branch tip
	// — proves the helper really read the Gemfile (not just stub data).
	if got := len(rule.lastIn.ChangedManifests); got != 1 {
		t.Fatalf("ChangedManifests len: want 1, got %d", got)
	}
	cm := rule.lastIn.ChangedManifests[0]
	if cm.Path != "Gemfile" {
		t.Errorf("manifest path: want Gemfile, got %q", cm.Path)
	}
	if len(cm.DepsAdded) == 0 {
		t.Errorf("expected at least one parsed dep from the branch tip")
	} else {
		found := false
		for _, d := range cm.DepsAdded {
			if d.Name == "redis" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected redis in parsed deps, got %+v", cm.DepsAdded)
		}
	}
}

// TestReplay_BranchStillFlagged_InsertsNewBlock — rule fires again →
// new SecurityFindings block row inserted, original flipped to superseded.
func TestReplay_BranchStillFlagged_InsertsNewBlock(t *testing.T) {
	repo := initGemfileRepo(t, "gem 'evilpkg', '0.0.1'\n")
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id := seedDeferral(t, db, 43, "SUPPLY-001", "feature/x", "Gemfile")

	rule := &fakeRule{
		id: "SUPPLY-001",
		findings: []ReplayFinding{
			{RuleID: "SUPPLY-001", Severity: "advise", Path: "Gemfile", Message: "evilpkg@0.0.1 still flagged"},
		},
	}
	rules := map[string]ReplayableRule{"SUPPLY-001": rule}
	resolver := func(int) (string, error) { return repo, nil }
	logger := &recordingLogger{}

	results, err := ReplayPendingDeferrals(context.Background(), db, resolver, rules, logger)
	if err != nil {
		t.Fatalf("ReplayPendingDeferrals: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	r := results[0]
	if r.Outcome != ReplayOutcomeStillFlagged {
		t.Errorf("outcome: want still_flagged, got %s", r.Outcome)
	}
	if r.NewFindingID == 0 {
		t.Errorf("expected non-zero NewFindingID")
	}

	// Original row → superseded.
	var origDisp string
	if err := db.QueryRow(`SELECT disposition FROM SecurityFindings WHERE id = ?`, id).Scan(&origDisp); err != nil {
		t.Fatalf("query orig disposition: %v", err)
	}
	if origDisp != DispositionSuperseded {
		t.Errorf("orig disposition: want %s, got %q", DispositionSuperseded, origDisp)
	}

	// New row → severity='block', disposition='' (open).
	var newDisp, newSev, newMsg string
	if err := db.QueryRow(`SELECT IFNULL(disposition,''), severity, message FROM SecurityFindings WHERE id = ?`, r.NewFindingID).Scan(&newDisp, &newSev, &newMsg); err != nil {
		t.Fatalf("query new row: %v", err)
	}
	if newDisp != "" {
		t.Errorf("new disposition: want '', got %q", newDisp)
	}
	if newSev != "block" {
		t.Errorf("new severity: want block, got %q", newSev)
	}
	if !strings.Contains(newMsg, "evilpkg") {
		t.Errorf("new message should embed rule output, got %q", newMsg)
	}

	if r.Reason == "" {
		t.Errorf("Reason should embed primary finding message, got empty")
	}
}

// TestReplay_BranchMissing_FlagsBranchGone — branch was deleted
// → flip original to disposition='branch_gone', rule never invoked.
func TestReplay_BranchMissing_FlagsBranchGone(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}

	// Init a repo without the deferred branch — simulates a deleted/rebased branch.
	dir := t.TempDir()
	gitRun := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	gitRun("init", "-b", "main")
	gitRun("config", "user.email", "t@t")
	gitRun("config", "user.name", "T")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("x\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitRun("add", ".")
	gitRun("commit", "-m", "initial")

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id := seedDeferral(t, db, 44, "SUPPLY-001", "feature/gone", "Gemfile")

	rule := &fakeRule{id: "SUPPLY-001"}
	rules := map[string]ReplayableRule{"SUPPLY-001": rule}
	resolver := func(int) (string, error) { return dir, nil }

	results, err := ReplayPendingDeferrals(context.Background(), db, resolver, rules, nil)
	if err != nil {
		t.Fatalf("ReplayPendingDeferrals: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Outcome != ReplayOutcomeBranchMissing {
		t.Errorf("outcome: want branch_missing, got %s", results[0].Outcome)
	}

	// Rule must NOT have been invoked.
	if rule.called != 0 {
		t.Errorf("rule.called: want 0 (branch missing), got %d", rule.called)
	}

	// Original row → branch_gone.
	var disp string
	if err := db.QueryRow(`SELECT disposition FROM SecurityFindings WHERE id = ?`, id).Scan(&disp); err != nil {
		t.Fatalf("query disposition: %v", err)
	}
	if disp != DispositionBranchGone {
		t.Errorf("disposition: want %s, got %q", DispositionBranchGone, disp)
	}
}

// TestReplay_PartialFailure_ContinuesOnError — one branch errors, the
// others still process. We model the "errors" by registering a rule
// adapter for one branch's rule key but NOT another's: the missing
// adapter row should surface as a per-row error while the registered
// adapter's row replays normally.
func TestReplay_PartialFailure_ContinuesOnError(t *testing.T) {
	repo := initGemfileRepo(t, "gem 'redis', '5.0.0'\n")
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Branch A → SUPPLY-001 registered, will resolve clean.
	idA := seedDeferral(t, db, 1, "SUPPLY-001", "feature/x", "Gemfile")

	// Branch B → SUPPLY-002 NOT registered — should produce a per-row
	// error and skip the row (no flip).
	idB := seedDeferral(t, db, 2, "SUPPLY-002", "feature/x", "Gemfile")

	rule001 := &fakeRule{id: "SUPPLY-001"} // empty findings → clean
	rules := map[string]ReplayableRule{"SUPPLY-001": rule001}
	resolver := func(int) (string, error) { return repo, nil }

	results, err := ReplayPendingDeferrals(context.Background(), db, resolver, rules, nil)
	if err == nil {
		t.Fatalf("expected per-row error in errors.Join, got nil")
	}
	// idA should be processed (resolved_late); idB should NOT be processed.
	if len(results) != 1 {
		t.Fatalf("expected 1 result (idA only), got %d", len(results))
	}
	if results[0].OriginalFindingID != idA {
		t.Errorf("expected idA in results, got %d", results[0].OriginalFindingID)
	}
	if results[0].Outcome != ReplayOutcomeResolvedLate {
		t.Errorf("idA outcome: want resolved_late, got %s", results[0].Outcome)
	}

	// idB should still carry disposition='token_expired' (untouched).
	var dispB string
	if err := db.QueryRow(`SELECT disposition FROM SecurityFindings WHERE id = ?`, idB).Scan(&dispB); err != nil {
		t.Fatalf("query idB: %v", err)
	}
	if dispB != DeferralDisposition {
		t.Errorf("idB disposition: want %s (untouched), got %q", DeferralDisposition, dispB)
	}
}

// TestReplay_GroupedByBranch_OneRunPerBranch — multiple deferrals on
// the same (branch, rule) → the rule's Run is invoked once per row
// (per the implementation's per-row scoping, which lets a parser
// error on row A not sink row B). We just assert the per-row
// outcomes are correct, which exercises the branch-grouping code.
//
// (The grouping's purpose is I/O dedupe — the branch existence probe
// + repo-resolver call run once per branch group, not once per row.
// The rule call itself runs per-row.)
func TestReplay_GroupedByBranch_OneRunPerBranch(t *testing.T) {
	repo := initGemfileRepo(t, "gem 'redis', '5.0.0'\n")
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Two deferrals on the same branch, different rule keys → both
	// share the branch-existence probe + repo resolution.
	idA := seedDeferral(t, db, 10, "SUPPLY-001", "feature/x", "Gemfile")
	idB := seedDeferral(t, db, 10, "SUPPLY-003", "feature/x", "Gemfile")

	r001 := &fakeRule{id: "SUPPLY-001"}
	r003 := &fakeRule{id: "SUPPLY-003"}
	rules := map[string]ReplayableRule{
		"SUPPLY-001": r001,
		"SUPPLY-003": r003,
	}

	// Counting resolver: each call increments. With the branch grouping
	// we expect exactly ONE call (both rows share the same branch).
	resolverCalls := 0
	resolver := func(int) (string, error) {
		resolverCalls++
		return repo, nil
	}

	results, err := ReplayPendingDeferrals(context.Background(), db, resolver, rules, nil)
	if err != nil {
		t.Fatalf("ReplayPendingDeferrals: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results (1 per row), got %d", len(results))
	}
	if resolverCalls != 1 {
		t.Errorf("resolver called %d time(s); expected 1 (one per branch group)", resolverCalls)
	}
	if r001.called != 1 || r003.called != 1 {
		t.Errorf("rule call counts: 001=%d 003=%d (want 1 each)", r001.called, r003.called)
	}

	// Both rows should be resolved_late (empty rule findings).
	for _, id := range []int{idA, idB} {
		var disp string
		db.QueryRow(`SELECT disposition FROM SecurityFindings WHERE id = ?`, id).Scan(&disp)
		if disp != DispositionResolvedLate {
			t.Errorf("finding %d disposition: want %s, got %q", id, DispositionResolvedLate, disp)
		}
	}
}

// TestReplayForBranch_FiltersCorrectly — only the named branch's
// deferrals are processed.
func TestReplayForBranch_FiltersCorrectly(t *testing.T) {
	repo := initGemfileRepo(t, "gem 'redis', '5.0.0'\n")
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	idHit := seedDeferral(t, db, 20, "SUPPLY-001", "feature/x", "Gemfile")
	idMiss := seedDeferral(t, db, 21, "SUPPLY-001", "feature/other", "Gemfile")

	rule := &fakeRule{id: "SUPPLY-001"}
	rules := map[string]ReplayableRule{"SUPPLY-001": rule}
	resolver := func(int) (string, error) { return repo, nil }

	results, err := ReplayPendingDeferralsForBranch(context.Background(), db, "feature/x", resolver, rules, nil)
	if err != nil {
		t.Fatalf("ReplayPendingDeferralsForBranch: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result for feature/x, got %d", len(results))
	}
	if results[0].OriginalFindingID != idHit {
		t.Errorf("expected hit row %d, got %d", idHit, results[0].OriginalFindingID)
	}

	// idMiss should be untouched.
	var dispMiss string
	db.QueryRow(`SELECT disposition FROM SecurityFindings WHERE id = ?`, idMiss).Scan(&dispMiss)
	if dispMiss != DeferralDisposition {
		t.Errorf("idMiss disposition: want %s (untouched), got %q", DeferralDisposition, dispMiss)
	}

	// Empty-branch arg → error.
	if _, err := ReplayPendingDeferralsForBranch(context.Background(), db, "", resolver, rules, nil); err == nil {
		t.Errorf("expected error for empty branch, got nil")
	}
}

// TestReplay_NoDeferrals_IsNoOp — empty queue → no error, no results.
func TestReplay_NoDeferrals_IsNoOp(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	resolver := func(int) (string, error) { return "/nonexistent", nil }
	results, err := ReplayPendingDeferrals(context.Background(), db, resolver, nil, nil)
	if err != nil {
		t.Errorf("expected nil error on empty queue, got %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results, got %d", len(results))
	}
}

// TestReplay_ResolverError_PerBranchScoping — a resolver error for
// branch A should not prevent branch B from processing.
func TestReplay_ResolverError_PerBranchScoping(t *testing.T) {
	repo := initGemfileRepo(t, "gem 'redis', '5.0.0'\n")
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	seedDeferral(t, db, 30, "SUPPLY-001", "feature/dead", "Gemfile") // resolver errs
	idGood := seedDeferral(t, db, 31, "SUPPLY-001", "feature/x", "Gemfile")

	rule := &fakeRule{id: "SUPPLY-001"}
	rules := map[string]ReplayableRule{"SUPPLY-001": rule}
	resolver := func(taskID int) (string, error) {
		if taskID == 30 {
			return "", errors.New("repo lookup failed")
		}
		return repo, nil
	}

	results, err := ReplayPendingDeferrals(context.Background(), db, resolver, rules, nil)
	if err == nil {
		t.Errorf("expected per-row error, got nil")
	}
	// idGood should still process.
	found := false
	for _, r := range results {
		if r.OriginalFindingID == idGood && r.Outcome == ReplayOutcomeResolvedLate {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected feature/x row to still be resolved_late despite feature/dead resolver error; got results=%+v", results)
	}
}
