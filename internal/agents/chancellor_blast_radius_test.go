package agents

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"force-orchestrator/internal/clients/graph"
	"force-orchestrator/internal/store"
)

type captureLogger struct{ lines []string }

func (c *captureLogger) Printf(format string, args ...any) {
	c.lines = append(c.lines, fmt.Sprintf(format, args...))
}

func mustFeature(t *testing.T, db *sql.DB, payload string) int {
	t.Helper()
	var id int64
	res, err := db.Exec(`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, priority, created_at)
		VALUES (0, 'force', 'Feature', 'Locked', ?, 0, datetime('now'))`, payload)
	if err != nil {
		t.Fatalf("insert Feature: %v", err)
	}
	id, _ = res.LastInsertId()
	return int(id)
}

func mustConvoy(t *testing.T, db *sql.DB, name string) int {
	t.Helper()
	id, err := store.CreateConvoy(db, name)
	if err != nil {
		t.Fatalf("CreateConvoy: %v", err)
	}
	return id
}

// TestExtractSymbolModifications_PayloadScan asserts the deterministic
// payload-scan path: a payload that mentions a registered symbol_path
// yields a SymbolModification with that symbol's metadata.
func TestExtractSymbolModifications_PayloadScan(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	if _, err := store.UpsertCrossRepoSymbol(db, store.CrossRepoSymbol{
		RepoName:      "force",
		SymbolPath:    "auth.LoginHandler",
		SymbolKind:    "function",
		FilePath:      "auth/login.go",
		LineNumber:    42,
		SignatureHash: "h1",
		IsPublic:      true,
	}); err != nil {
		t.Fatalf("seed symbol: %v", err)
	}

	mods, err := ExtractSymbolModifications(db, store.TaskPlan{
		TempID: 1,
		Repo:   "force",
		Task:   "Update auth.LoginHandler to support OIDC.",
	})
	if err != nil {
		t.Fatalf("ExtractSymbolModifications: %v", err)
	}
	if got, want := len(mods), 1; got != want {
		t.Fatalf("len mods: got %d want %d", got, want)
	}
	if got, want := mods[0].SymbolPath, "auth.LoginHandler"; got != want {
		t.Errorf("SymbolPath: got %q want %q", got, want)
	}
	if got, want := mods[0].FilePath, "auth/login.go"; got != want {
		t.Errorf("FilePath: got %q want %q", got, want)
	}
}

// TestExtractSymbolModifications_NoMatch returns an empty slice (no error)
// when the payload doesn't mention any registered symbol — Chancellor
// post-process tolerates this and persists an empty BlastRadiusRecord.
func TestExtractSymbolModifications_NoMatch(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	mods, err := ExtractSymbolModifications(db, store.TaskPlan{
		TempID: 1,
		Repo:   "force",
		Task:   "Refactor a module that has no symbols.",
	})
	if err != nil {
		t.Fatalf("ExtractSymbolModifications: %v", err)
	}
	if len(mods) != 0 {
		t.Errorf("expected no mods on no-symbol payload, got %v", mods)
	}
}

// TestPostProcessBlastRadius_HappyPath_FullCycle is the headline
// Chancellor smoke: synthetic decomposed plan → post-process →
// blast_radius_json populated + auto-included tasks inserted with
// proper parent_id + convoy_id, both visible in BountyBoard.
func TestPostProcessBlastRadius_HappyPath_FullCycle(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Seed cross-repo graph: provider symbol in `force` with two consumer
	// sites in `consumer-a` + one in `consumer-b`.
	provID, err := store.UpsertCrossRepoSymbol(db, store.CrossRepoSymbol{
		RepoName: "force", SymbolPath: "auth.LoginHandler", SymbolKind: "function",
		FilePath: "auth/login.go", LineNumber: 42, SignatureHash: "h1", IsPublic: true,
	})
	if err != nil {
		t.Fatalf("seed symbol: %v", err)
	}
	for _, d := range []store.CrossRepoDependency{
		{ConsumerRepoName: "consumer-a", ConsumerFile: "handlers/login.go", ConsumerLine: 10, ProviderSymbolID: provID},
		{ConsumerRepoName: "consumer-a", ConsumerFile: "handlers/login.go", ConsumerLine: 25, ProviderSymbolID: provID},
		{ConsumerRepoName: "consumer-b", ConsumerFile: "middleware/auth.go", ConsumerLine: 7, ProviderSymbolID: provID},
	} {
		if _, err := store.UpsertCrossRepoDependency(db, d); err != nil {
			t.Fatalf("seed dep: %v", err)
		}
	}

	// Synthetic Feature + convoy + plan.
	featureID := mustFeature(t, db, "Update auth.LoginHandler signature.")
	convoyID := mustConvoy(t, db, "[t] update login handler")
	tasks := []store.TaskPlan{{
		TempID: 1, Repo: "force",
		Task: "Update auth.LoginHandler to accept the new credential blob.",
	}}
	idMapping := map[int]int{1: 100} // dummy real ID; only used for log lines

	gc := graph.NewInProcess(db)
	logger := &captureLogger{}
	rec, err := PostProcessBlastRadius(context.Background(), db, gc, featureID, convoyID, tasks, idMapping, logger)
	if err != nil {
		t.Fatalf("PostProcessBlastRadius: %v", err)
	}

	// Affected consumer repos should be alphabetically sorted + de-duplicated.
	if want := []string{"consumer-a", "consumer-b"}; !reflect.DeepEqual(rec.AffectedConsumerRepos, want) {
		t.Errorf("AffectedConsumerRepos: got %v want %v", rec.AffectedConsumerRepos, want)
	}
	// Two auto-included tasks (one per consumer repo).
	if got, want := len(rec.AutoIncludedTasks), 2; got != want {
		t.Fatalf("AutoIncludedTasks len: got %d want %d", got, want)
	}
	// Each auto-included task should be a CodeEdit row in BountyBoard with
	// parent_id=featureID, convoy_id=convoyID, and a [BLAST_RADIUS_UPDATE]
	// payload.
	for _, taskID := range rec.AutoIncludedTasks {
		var (
			parentID int
			cID      int
			repo     string
			payload  string
		)
		if err := db.QueryRow(`SELECT parent_id, convoy_id, target_repo, payload FROM BountyBoard WHERE id = ?`, taskID).
			Scan(&parentID, &cID, &repo, &payload); err != nil {
			t.Fatalf("scan auto-included task #%d: %v", taskID, err)
		}
		if parentID != featureID {
			t.Errorf("auto-included task #%d: parent_id got %d want %d", taskID, parentID, featureID)
		}
		if cID != convoyID {
			t.Errorf("auto-included task #%d: convoy_id got %d want %d", taskID, cID, convoyID)
		}
		if !strings.HasPrefix(payload, "[BLAST_RADIUS_UPDATE]") {
			t.Errorf("auto-included task #%d: payload missing [BLAST_RADIUS_UPDATE] prefix; got %q", taskID, payload)
		}
		if !strings.Contains(payload, "auth.LoginHandler") {
			t.Errorf("auto-included task #%d: payload missing modified symbol; got %q", taskID, payload)
		}
		if repo != "consumer-a" && repo != "consumer-b" {
			t.Errorf("auto-included task #%d: target_repo unexpected: %q", taskID, repo)
		}
	}

	// Blast radius json should be persisted on the Feature.
	persisted, err := store.GetFeatureBlastRadius(db, featureID)
	if err != nil {
		t.Fatalf("GetFeatureBlastRadius: %v", err)
	}
	if !reflect.DeepEqual(persisted.AffectedConsumerRepos, rec.AffectedConsumerRepos) {
		t.Errorf("persisted AffectedConsumerRepos: got %v want %v",
			persisted.AffectedConsumerRepos, rec.AffectedConsumerRepos)
	}
	if !reflect.DeepEqual(persisted.AutoIncludedTasks, rec.AutoIncludedTasks) {
		t.Errorf("persisted AutoIncludedTasks: got %v want %v",
			persisted.AutoIncludedTasks, rec.AutoIncludedTasks)
	}
}

// TestPostProcessBlastRadius_NoMatchingSymbols asserts the post-process
// is a safe no-op when the plan touches no indexed symbols: empty
// blast_radius_json (with empty arrays, not '{}'), zero auto-included
// tasks. This is the "Feature touches a repo the dog hasn't indexed
// yet" case — common during D8 rollout.
func TestPostProcessBlastRadius_NoMatchingSymbols(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	featureID := mustFeature(t, db, "Refactor unrelated module.")
	convoyID := mustConvoy(t, db, "[t] no symbols")
	tasks := []store.TaskPlan{{TempID: 1, Repo: "force", Task: "Refactor README only."}}

	gc := graph.NewInProcess(db)
	rec, err := PostProcessBlastRadius(context.Background(), db, gc, featureID, convoyID, tasks, map[int]int{1: 100}, nil)
	if err != nil {
		t.Fatalf("PostProcessBlastRadius: %v", err)
	}
	if len(rec.AffectedConsumerRepos) != 0 {
		t.Errorf("AffectedConsumerRepos: got %v want empty", rec.AffectedConsumerRepos)
	}
	if len(rec.AutoIncludedTasks) != 0 {
		t.Errorf("AutoIncludedTasks: got %v want empty", rec.AutoIncludedTasks)
	}

	// No auto-included CodeEdit rows in the convoy.
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE convoy_id = ? AND type = 'CodeEdit'`, convoyID).Scan(&n); err != nil {
		t.Fatalf("count auto-included: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 auto-included tasks for no-symbol plan, got %d", n)
	}
}

// TestPostProcessBlastRadius_SkipsModifyingRepo asserts that a consumer
// site within the SAME repo as the modification is excluded — Chancellor
// shouldn't auto-include a self-update task for the modifying repo (the
// modifying task is already in the convoy via the original plan).
func TestPostProcessBlastRadius_SkipsModifyingRepo(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	provID, err := store.UpsertCrossRepoSymbol(db, store.CrossRepoSymbol{
		RepoName: "force", SymbolPath: "auth.LoginHandler", SymbolKind: "function",
		FilePath: "auth/login.go", LineNumber: 42, SignatureHash: "h1", IsPublic: true,
	})
	if err != nil {
		t.Fatalf("seed symbol: %v", err)
	}
	// Consumer site INSIDE the modifying repo `force` — must be skipped.
	if _, err := store.UpsertCrossRepoDependency(db, store.CrossRepoDependency{
		ConsumerRepoName: "force", ConsumerFile: "internal/api/api.go", ConsumerLine: 9,
		ProviderSymbolID: provID,
	}); err != nil {
		t.Fatalf("seed self-dep: %v", err)
	}
	// Consumer site in a different repo — should be auto-included.
	if _, err := store.UpsertCrossRepoDependency(db, store.CrossRepoDependency{
		ConsumerRepoName: "consumer-x", ConsumerFile: "x.go", ConsumerLine: 1,
		ProviderSymbolID: provID,
	}); err != nil {
		t.Fatalf("seed external dep: %v", err)
	}

	featureID := mustFeature(t, db, "self-skip")
	convoyID := mustConvoy(t, db, "[t] self-skip")
	tasks := []store.TaskPlan{{TempID: 1, Repo: "force", Task: "Modify auth.LoginHandler."}}

	gc := graph.NewInProcess(db)
	rec, err := PostProcessBlastRadius(context.Background(), db, gc, featureID, convoyID, tasks, map[int]int{1: 100}, nil)
	if err != nil {
		t.Fatalf("PostProcessBlastRadius: %v", err)
	}
	// Auto-included tasks should be exactly one (consumer-x); the in-repo
	// site is excluded.
	if got, want := len(rec.AutoIncludedTasks), 1; got != want {
		t.Errorf("AutoIncludedTasks len: got %d want %d", got, want)
	}
	for _, taskID := range rec.AutoIncludedTasks {
		var repo string
		if err := db.QueryRow(`SELECT target_repo FROM BountyBoard WHERE id = ?`, taskID).Scan(&repo); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if repo == "force" {
			t.Errorf("auto-included task #%d targets the modifying repo `force` — should have been skipped", taskID)
		}
	}
}

// TestQueueBlastRadiusSenateConsultations_FiresPerActiveSenator asserts
// the per-affected-consumer-Senator dispatch: only repos with an active
// SenateChambers row get a SenateReview task queued.
func TestQueueBlastRadiusSenateConsultations_FiresPerActiveSenator(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Seed two chambers: consumer-a active, consumer-b onboarding.
	if err := store.UpsertSenateChamber(db, store.SenateChamber{
		SenatorName: "consumer-a", Scope: "repo:consumer-a", SenateMDPath: "SENATE.md",
		Status: "active",
	}); err != nil {
		t.Fatalf("seed chamber A: %v", err)
	}
	if err := store.UpsertSenateChamber(db, store.SenateChamber{
		SenatorName: "consumer-b", Scope: "repo:consumer-b", SenateMDPath: "SENATE.md",
		Status: "onboarding",
	}); err != nil {
		t.Fatalf("seed chamber B: %v", err)
	}

	featureID := mustFeature(t, db, "blast-radius senate")
	if err := QueueBlastRadiusSenateConsultations(db, featureID,
		[]string{"consumer-a", "consumer-b", "consumer-c"}, nil); err != nil {
		t.Fatalf("QueueBlastRadiusSenateConsultations: %v", err)
	}

	// Exactly one SenateReview task — consumer-a only.
	rows, err := db.Query(`SELECT target_repo, payload FROM BountyBoard
		WHERE type = 'SenateReview' AND parent_id = ?`, featureID)
	if err != nil {
		t.Fatalf("query SenateReview tasks: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var repo, payload string
		if err := rows.Scan(&repo, &payload); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, repo)
		if !strings.Contains(payload, "consumer-a") {
			t.Errorf("payload missing repo: %q", payload)
		}
	}
	if want := []string{"consumer-a"}; !reflect.DeepEqual(got, want) {
		t.Errorf("queued SenateReview target_repos: got %v want %v (only active senators should fire)", got, want)
	}
}
