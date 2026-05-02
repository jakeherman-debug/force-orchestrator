package stagegate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// stubLabelFetcher is a tiny in-memory PRLabelFetcher for the
// release_label_present unit tests. Tests populate the labels map keyed
// by "<repo>#<number>"; an entry's `err` field surfaces a gh-side error
// to drive the gh-error → ErrPending path.
type stubLabelFetcher struct {
	labels map[string]stubLabelEntry
	calls  []stubLabelCall
}

type stubLabelEntry struct {
	labels []string
	err    error
}

type stubLabelCall struct {
	repo   string
	number int
}

func (s *stubLabelFetcher) PRLabels(_ string, repo string, number int) ([]string, error) {
	s.calls = append(s.calls, stubLabelCall{repo: repo, number: number})
	key := fmt.Sprintf("%s#%d", repo, number)
	e, ok := s.labels[key]
	if !ok {
		return []string{}, nil
	}
	return e.labels, e.err
}

func newStubLabelFetcher() *stubLabelFetcher {
	return &stubLabelFetcher{labels: map[string]stubLabelEntry{}}
}

// seedRepoForTest registers a repo in Repositories with an optional
// release_label_pattern. The schema's UPSERT preserves the pattern across
// re-inserts of the same name (per holocron.go), so we set it explicitly
// via SetRepositoryReleaseLabelPattern after AddRepo.
func seedRepoForTest(t *testing.T, db *sql.DB, name, pattern string) {
	t.Helper()
	store.AddRepo(db, name, "/tmp/"+name, "")
	if pattern != "" {
		if err := store.SetRepositoryReleaseLabelPattern(db, name, pattern); err != nil {
			t.Fatalf("SetRepositoryReleaseLabelPattern(%s, %q): %v", name, pattern, err)
		}
	}
}

// seedStagedConvoyWithMergedPR creates a single-stage convoy + one
// ConvoyAskBranch in the "Merged" state with the given draft_pr_number.
// Returns convoyID, stageID for the gate's StageContext.
func seedStagedConvoyWithMergedPR(t *testing.T, db *sql.DB, repo, askBranch string, prNumber int) (int, int) {
	t.Helper()
	specs := []store.StagedStageSpec{
		{StageNum: 1, Intent: "stage 1 — verify release label"},
	}
	convoyID, stageIDs, err := store.CreateStagedConvoy(db, "release-label-test-"+repo, store.StagingStrategyStrict, specs)
	if err != nil {
		t.Fatalf("CreateStagedConvoy: %v", err)
	}
	if err := store.UpsertConvoyAskBranch(db, convoyID, repo, askBranch, "deadbeef"); err != nil {
		t.Fatalf("UpsertConvoyAskBranch: %v", err)
	}
	// Pin the ask-branch row to stage 1 + populate draft_pr fields. The
	// Diplomat does this in production; for test isolation we set the
	// columns directly with raw SQL so we don't depend on Diplomat's wire-up.
	if _, err := db.Exec(`UPDATE ConvoyAskBranches
		SET stage_id = ?, draft_pr_number = ?, draft_pr_state = 'Merged'
		WHERE convoy_id = ? AND repo = ?`,
		stageIDs[0], prNumber, convoyID, repo); err != nil {
		t.Fatalf("update ask-branch to Merged: %v", err)
	}
	return convoyID, stageIDs[0]
}

func TestReleaseLabelPresent_Type(t *testing.T) {
	if (ReleaseLabelPresent{}).Type() != "release_label_present" {
		t.Errorf("Type() = %q, want release_label_present", ReleaseLabelPresent{}.Type())
	}
}

// TestReleaseLabelPresent_AllPRsLabeled_Passes — single repo, single merged
// PR carrying a label that matches the configured pattern.
func TestReleaseLabelPresent_AllPRsLabeled_Passes(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	seedRepoForTest(t, db, "api", `^released-prod$`)
	convoyID, stageID := seedStagedConvoyWithMergedPR(t, db, "api", "force/ask-1-test", 42)

	stub := newStubLabelFetcher()
	stub.labels["api#42"] = stubLabelEntry{labels: []string{"released-prod"}}
	g := NewReleaseLabelPresent(stub)

	passed, reason, err := g.Evaluate(context.Background(), db, StageContext{
		ConvoyID: convoyID, StageID: stageID, StageNum: 1,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !passed {
		t.Errorf("expected passed=true, reason=%q", reason)
	}
}

// TestReleaseLabelPresent_OnePRMissingLabel_Pending — two repos, one carries
// the label, the other doesn't → ErrPending (release rollout still in flight).
func TestReleaseLabelPresent_OnePRMissingLabel_Pending(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	seedRepoForTest(t, db, "api", `^released-prod$`)
	seedRepoForTest(t, db, "frontend", `^released-prod$`)
	specs := []store.StagedStageSpec{{StageNum: 1, Intent: "two-repo stage"}}
	convoyID, stageIDs, err := store.CreateStagedConvoy(db, "two-repo-stage", store.StagingStrategyStrict, specs)
	if err != nil {
		t.Fatalf("CreateStagedConvoy: %v", err)
	}
	for _, repo := range []string{"api", "frontend"} {
		if err := store.UpsertConvoyAskBranch(db, convoyID, repo, "force/ask-1-"+repo, "deadbeef"); err != nil {
			t.Fatalf("UpsertConvoyAskBranch(%s): %v", repo, err)
		}
	}
	if _, err := db.Exec(`UPDATE ConvoyAskBranches
		SET stage_id = ?, draft_pr_state = 'Merged',
		    draft_pr_number = CASE repo WHEN 'api' THEN 100 WHEN 'frontend' THEN 200 END
		WHERE convoy_id = ?`, stageIDs[0], convoyID); err != nil {
		t.Fatalf("update ask-branches to Merged: %v", err)
	}

	stub := newStubLabelFetcher()
	stub.labels["api#100"] = stubLabelEntry{labels: []string{"released-prod"}}
	stub.labels["frontend#200"] = stubLabelEntry{labels: []string{"deploy-pending"}}
	g := NewReleaseLabelPresent(stub)

	passed, reason, err := g.Evaluate(context.Background(), db, StageContext{
		ConvoyID: convoyID, StageID: stageIDs[0], StageNum: 1,
	})
	if !errors.Is(err, ErrPending) {
		t.Fatalf("expected ErrPending, got %v (reason=%q)", err, reason)
	}
	if passed {
		t.Error("expected passed=false")
	}
	if !strings.Contains(reason, "frontend#200") {
		t.Errorf("expected reason to mention frontend#200 as the lagging PR, got %q", reason)
	}
}

// TestReleaseLabelPresent_RepoMissingPattern_Errors — runtime defense for
// the planner-time pattern check. Returns a structural error (NOT ErrPending)
// because the operator must fix the configuration; retrying won't help.
func TestReleaseLabelPresent_RepoMissingPattern_Errors(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	seedRepoForTest(t, db, "api", "") // pattern intentionally empty
	convoyID, stageID := seedStagedConvoyWithMergedPR(t, db, "api", "force/ask-1", 1)

	stub := newStubLabelFetcher()
	g := NewReleaseLabelPresent(stub)

	_, _, err := g.Evaluate(context.Background(), db, StageContext{
		ConvoyID: convoyID, StageID: stageID, StageNum: 1,
	})
	if err == nil {
		t.Fatal("expected error on missing pattern, got nil")
	}
	if errors.Is(err, ErrPending) {
		t.Errorf("missing pattern should be a structural error, not ErrPending; got %v", err)
	}
	if !strings.Contains(err.Error(), "release_label_pattern") {
		t.Errorf("expected error to mention release_label_pattern, got %v", err)
	}
}

// TestReleaseLabelPresent_InvalidPatternRegex_Errors — a misconfigured regex
// surfaces as a structural error with the offending repo named.
func TestReleaseLabelPresent_InvalidPatternRegex_Errors(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	seedRepoForTest(t, db, "api", `[unterminated`)
	convoyID, stageID := seedStagedConvoyWithMergedPR(t, db, "api", "force/ask-1", 1)

	stub := newStubLabelFetcher()
	g := NewReleaseLabelPresent(stub)
	_, _, err := g.Evaluate(context.Background(), db, StageContext{
		ConvoyID: convoyID, StageID: stageID, StageNum: 1,
	})
	if err == nil {
		t.Fatal("expected error on invalid regex, got nil")
	}
	if errors.Is(err, ErrPending) {
		t.Errorf("invalid regex should be structural; got ErrPending")
	}
	if !strings.Contains(err.Error(), "invalid pattern") || !strings.Contains(err.Error(), "api") {
		t.Errorf("expected error to mention 'invalid pattern' and the repo name, got %v", err)
	}
}

// TestReleaseLabelPresent_PRNotMerged_Pending — defensive guard for the
// case where the gate is somehow evaluated before all PRs have been
// merged. Stays pending so the dog re-checks once the merge completes.
func TestReleaseLabelPresent_PRNotMerged_Pending(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	seedRepoForTest(t, db, "api", `^released-prod$`)
	specs := []store.StagedStageSpec{{StageNum: 1, Intent: "stage with not-yet-merged PR"}}
	convoyID, stageIDs, err := store.CreateStagedConvoy(db, "premerge", store.StagingStrategyStrict, specs)
	if err != nil {
		t.Fatalf("CreateStagedConvoy: %v", err)
	}
	if err := store.UpsertConvoyAskBranch(db, convoyID, "api", "force/ask-1", "deadbeef"); err != nil {
		t.Fatalf("UpsertConvoyAskBranch: %v", err)
	}
	// Leave draft_pr_state = '' (not yet merged) but pin to the stage.
	if _, err := db.Exec(`UPDATE ConvoyAskBranches SET stage_id = ?, draft_pr_number = 1, draft_pr_state = 'Open'
		WHERE convoy_id = ?`, stageIDs[0], convoyID); err != nil {
		t.Fatalf("update ask-branch: %v", err)
	}

	stub := newStubLabelFetcher()
	g := NewReleaseLabelPresent(stub)
	_, reason, err := g.Evaluate(context.Background(), db, StageContext{
		ConvoyID: convoyID, StageID: stageIDs[0], StageNum: 1,
	})
	if !errors.Is(err, ErrPending) {
		t.Fatalf("expected ErrPending while PR unmerged, got %v (reason=%q)", err, reason)
	}
	if !strings.Contains(reason, "not yet merged") {
		t.Errorf("expected reason to mention 'not yet merged', got %q", reason)
	}
}

// TestReleaseLabelPresent_GHError_Pending — gh failures are transient.
// The gate returns ErrPending so the dog retries; an indefinitely-broken
// gh is the dog's gate-timeout problem, not this gate's.
func TestReleaseLabelPresent_GHError_Pending(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	seedRepoForTest(t, db, "api", `^released-prod$`)
	convoyID, stageID := seedStagedConvoyWithMergedPR(t, db, "api", "force/ask-1", 5)

	stub := newStubLabelFetcher()
	stub.labels["api#5"] = stubLabelEntry{err: errors.New("gh: API rate limit exceeded")}
	g := NewReleaseLabelPresent(stub)

	_, reason, err := g.Evaluate(context.Background(), db, StageContext{
		ConvoyID: convoyID, StageID: stageID, StageNum: 1,
	})
	if !errors.Is(err, ErrPending) {
		t.Fatalf("expected ErrPending on gh error, got %v", err)
	}
	if !strings.Contains(reason, "gh error") || !strings.Contains(reason, "rate limit") {
		t.Errorf("expected reason to mention gh error + the underlying message, got %q", reason)
	}
}

// TestReleaseLabelPresent_MultipleReposEachWithOwnPattern_Passes — the
// per-repo pattern story: monorepo "release/v" pattern, library "v\d+\.\d+".
// Each PR's label is matched against ITS repo's pattern, not a shared one.
func TestReleaseLabelPresent_MultipleReposEachWithOwnPattern_Passes(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	seedRepoForTest(t, db, "monolith", `^release/v\d+\.\d+$`)
	seedRepoForTest(t, db, "library", `^v\d+\.\d+\.\d+$`)
	specs := []store.StagedStageSpec{{StageNum: 1, Intent: "two-repo same-stage rollout"}}
	convoyID, stageIDs, err := store.CreateStagedConvoy(db, "multi-repo", store.StagingStrategyStrict, specs)
	if err != nil {
		t.Fatalf("CreateStagedConvoy: %v", err)
	}
	for _, repo := range []string{"monolith", "library"} {
		if err := store.UpsertConvoyAskBranch(db, convoyID, repo, "force/ask-1-"+repo, "deadbeef"); err != nil {
			t.Fatalf("UpsertConvoyAskBranch(%s): %v", repo, err)
		}
	}
	if _, err := db.Exec(`UPDATE ConvoyAskBranches
		SET stage_id = ?, draft_pr_state = 'Merged',
		    draft_pr_number = CASE repo WHEN 'monolith' THEN 11 WHEN 'library' THEN 22 END
		WHERE convoy_id = ?`, stageIDs[0], convoyID); err != nil {
		t.Fatalf("update ask-branches: %v", err)
	}

	stub := newStubLabelFetcher()
	stub.labels["monolith#11"] = stubLabelEntry{labels: []string{"release/v1.2", "needs-qa"}}
	stub.labels["library#22"] = stubLabelEntry{labels: []string{"v0.5.3"}}
	g := NewReleaseLabelPresent(stub)

	passed, reason, err := g.Evaluate(context.Background(), db, StageContext{
		ConvoyID: convoyID, StageID: stageIDs[0], StageNum: 1,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !passed {
		t.Errorf("expected passed=true, reason=%q", reason)
	}
	// Verify the gh stub saw exactly one call per repo.
	if len(stub.calls) != 2 {
		t.Errorf("expected 2 gh calls, got %d (%v)", len(stub.calls), stub.calls)
	}
}

// TestReleaseLabelPresent_NewReleaseLabelPresent_NilPanics — wiring guard:
// constructing without a fetcher panics so the daemon fails fast on a
// misconfigured registry.
func TestReleaseLabelPresent_NewReleaseLabelPresent_NilPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on nil fetcher")
		}
	}()
	NewReleaseLabelPresent(nil)
}
