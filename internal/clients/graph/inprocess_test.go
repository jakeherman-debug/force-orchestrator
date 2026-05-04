package graph_test

import (
	"context"
	"database/sql"
	"reflect"
	"sort"
	"testing"

	"force-orchestrator/internal/clients/graph"
	"force-orchestrator/internal/store"
)

func seedSymbol(t *testing.T, db *sql.DB, repo, symbolPath, file string, line int) int64 {
	t.Helper()
	id, err := store.UpsertCrossRepoSymbol(db, store.CrossRepoSymbol{
		RepoName:      repo,
		SymbolPath:    symbolPath,
		SymbolKind:    "function",
		FilePath:      file,
		LineNumber:    line,
		SignatureHash: "h-" + symbolPath,
		IsPublic:      true,
	})
	if err != nil {
		t.Fatalf("UpsertCrossRepoSymbol(%s, %s): %v", repo, symbolPath, err)
	}
	return id
}

func seedDep(t *testing.T, db *sql.DB, providerID int64, consumerRepo, consumerFile string, consumerLine int) {
	t.Helper()
	if _, err := store.UpsertCrossRepoDependency(db, store.CrossRepoDependency{
		ConsumerRepoName: consumerRepo,
		ConsumerFile:     consumerFile,
		ConsumerLine:     consumerLine,
		ProviderSymbolID: providerID,
	}); err != nil {
		t.Fatalf("UpsertCrossRepoDependency(provider=%d, %s/%s:%d): %v",
			providerID, consumerRepo, consumerFile, consumerLine, err)
	}
}

// TestBlastRadiusForModifications_HappyPath seeds a small cross-repo graph
// (one provider symbol with three consumer sites across two repos) and
// asserts BlastRadiusForModifications returns the expected modified-symbols
// list, the alphabetically-sorted affected_consumer_repos set, and the
// per-symbol consumer file:line breadcrumbs.
func TestBlastRadiusForModifications_HappyPath(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	provID := seedSymbol(t, db, "force", "auth.Login", "auth/login.go", 42)
	seedDep(t, db, provID, "consumer-a", "handlers/login.go", 10)
	seedDep(t, db, provID, "consumer-a", "handlers/login.go", 25)
	seedDep(t, db, provID, "consumer-b", "middleware/auth.go", 7)

	c := graph.NewInProcess(db)
	br, err := c.BlastRadiusForModifications(context.Background(), []graph.SymbolModification{
		{Repo: "force", FilePath: "auth/login.go", SymbolPath: "auth.Login"},
	})
	if err != nil {
		t.Fatalf("BlastRadiusForModifications: %v", err)
	}

	if got, want := len(br.ModifiedSymbols), 1; got != want {
		t.Fatalf("ModifiedSymbols len: got %d want %d", got, want)
	}
	if got, want := br.ModifiedSymbols[0].Name, "auth.Login"; got != want {
		t.Errorf("ModifiedSymbols[0].Name: got %q want %q", got, want)
	}

	wantRepos := []string{"consumer-a", "consumer-b"}
	if !reflect.DeepEqual(br.AffectedConsumerRepos, wantRepos) {
		t.Errorf("AffectedConsumerRepos: got %v want %v (must be alphabetically sorted + de-duplicated)",
			br.AffectedConsumerRepos, wantRepos)
	}

	sites := br.ConsumersBySymbol["auth.Login"]
	if got, want := len(sites), 3; got != want {
		t.Fatalf("ConsumersBySymbol[auth.Login] len: got %d want %d", got, want)
	}
	// Sort for stable comparison; ordering inside the per-symbol list
	// follows the store's ORDER BY (consumer_repo_name, file, line).
	sort.SliceStable(sites, func(i, j int) bool {
		if sites[i].Repo != sites[j].Repo {
			return sites[i].Repo < sites[j].Repo
		}
		if sites[i].FilePath != sites[j].FilePath {
			return sites[i].FilePath < sites[j].FilePath
		}
		return sites[i].Line < sites[j].Line
	})
	want := []graph.ConsumerSite{
		{Repo: "consumer-a", FilePath: "handlers/login.go", Line: 10},
		{Repo: "consumer-a", FilePath: "handlers/login.go", Line: 25},
		{Repo: "consumer-b", FilePath: "middleware/auth.go", Line: 7},
	}
	if !reflect.DeepEqual(sites, want) {
		t.Errorf("ConsumersBySymbol[auth.Login]: got %+v want %+v", sites, want)
	}
}

// TestBlastRadiusForModifications_MultiSymbolAggregation asserts the
// per-modification aggregation: two distinct provider symbols, each with
// its own consumer set, fold into one BlastRadius with both
// ConsumersBySymbol entries and the union of affected consumer repos.
func TestBlastRadiusForModifications_MultiSymbolAggregation(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	loginID := seedSymbol(t, db, "force", "auth.Login", "auth/login.go", 42)
	seedDep(t, db, loginID, "consumer-a", "handlers/login.go", 10)

	logoutID := seedSymbol(t, db, "force", "auth.Logout", "auth/logout.go", 60)
	seedDep(t, db, logoutID, "consumer-c", "handlers/logout.go", 5)

	c := graph.NewInProcess(db)
	br, err := c.BlastRadiusForModifications(context.Background(), []graph.SymbolModification{
		{Repo: "force", FilePath: "auth/login.go", SymbolPath: "auth.Login"},
		{Repo: "force", FilePath: "auth/logout.go", SymbolPath: "auth.Logout"},
	})
	if err != nil {
		t.Fatalf("BlastRadiusForModifications: %v", err)
	}

	wantRepos := []string{"consumer-a", "consumer-c"}
	if !reflect.DeepEqual(br.AffectedConsumerRepos, wantRepos) {
		t.Errorf("AffectedConsumerRepos: got %v want %v", br.AffectedConsumerRepos, wantRepos)
	}
	if _, ok := br.ConsumersBySymbol["auth.Login"]; !ok {
		t.Errorf("ConsumersBySymbol missing auth.Login key; got keys=%v", reflect.ValueOf(br.ConsumersBySymbol).MapKeys())
	}
	if _, ok := br.ConsumersBySymbol["auth.Logout"]; !ok {
		t.Errorf("ConsumersBySymbol missing auth.Logout key; got keys=%v", reflect.ValueOf(br.ConsumersBySymbol).MapKeys())
	}
}

// TestBlastRadiusForModifications_UnknownSymbolSkipped asserts that a
// modification whose (repo, symbol_path) isn't in the index returns
// successfully with empty consumer sets — blast-radius is "best known
// consumers", not "completeness guarantee", so unknowns are skipped
// rather than erroring.
func TestBlastRadiusForModifications_UnknownSymbolSkipped(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	br, err := graph.NewInProcess(db).BlastRadiusForModifications(context.Background(), []graph.SymbolModification{
		{Repo: "force", FilePath: "missing.go", SymbolPath: "missing.Symbol"},
	})
	if err != nil {
		t.Fatalf("BlastRadiusForModifications on unknown symbol: %v (expected no error, just empty)", err)
	}
	if len(br.AffectedConsumerRepos) != 0 {
		t.Errorf("AffectedConsumerRepos: got %v want empty", br.AffectedConsumerRepos)
	}
	if len(br.ModifiedSymbols) != 0 {
		t.Errorf("ModifiedSymbols: got %v want empty", br.ModifiedSymbols)
	}
}

// TestBlastRadiusForModifications_SoftDeletedDependencyExcluded asserts
// the reader-side honors the soft-delete contract: a tombstoned consumer
// edge does NOT show up in the blast radius. (Track 1's ListConsumersOfSymbol
// already filters by deleted_at='', this is the cross-layer assertion.)
func TestBlastRadiusForModifications_SoftDeletedDependencyExcluded(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	provID := seedSymbol(t, db, "force", "auth.Login", "auth/login.go", 42)
	// Insert two edges, then soft-delete one.
	if _, err := store.UpsertCrossRepoDependency(db, store.CrossRepoDependency{
		ConsumerRepoName: "consumer-a", ConsumerFile: "live.go", ConsumerLine: 1,
		ProviderSymbolID: provID,
	}); err != nil {
		t.Fatalf("seed live edge: %v", err)
	}
	delID, err := store.UpsertCrossRepoDependency(db, store.CrossRepoDependency{
		ConsumerRepoName: "consumer-b", ConsumerFile: "tomb.go", ConsumerLine: 1,
		ProviderSymbolID: provID,
	})
	if err != nil {
		t.Fatalf("seed soft-deleted edge: %v", err)
	}
	if err := store.SoftDeleteCrossRepoDependency(db, delID); err != nil {
		t.Fatalf("SoftDeleteCrossRepoDependency: %v", err)
	}

	br, err := graph.NewInProcess(db).BlastRadiusForModifications(context.Background(), []graph.SymbolModification{
		{Repo: "force", FilePath: "auth/login.go", SymbolPath: "auth.Login"},
	})
	if err != nil {
		t.Fatalf("BlastRadiusForModifications: %v", err)
	}
	if want := []string{"consumer-a"}; !reflect.DeepEqual(br.AffectedConsumerRepos, want) {
		t.Errorf("AffectedConsumerRepos: got %v want %v (consumer-b should be excluded; soft-deleted)",
			br.AffectedConsumerRepos, want)
	}
}

// TestBlastRadius_SingleSymbolCompat documents the legacy single-symbol
// API still works post-D8-T2 — it just composes BlastRadiusForModifications
// with one input and keeps the Direct[] field populated for callers that
// already depend on it.
func TestBlastRadius_SingleSymbolCompat(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	provID := seedSymbol(t, db, "force", "auth.Login", "auth/login.go", 42)
	seedDep(t, db, provID, "consumer-x", "x.go", 9)

	c := graph.NewInProcess(db)
	br, err := c.BlastRadius(context.Background(), graph.Symbol{
		Repo: "force", Path: "auth/login.go", Name: "auth.Login", Kind: "func", Line: 42,
	})
	if err != nil {
		t.Fatalf("BlastRadius: %v", err)
	}
	if got, want := br.Modified.Name, "auth.Login"; got != want {
		t.Errorf("Modified.Name: got %q want %q", got, want)
	}
	if got, want := len(br.Direct), 1; got != want {
		t.Errorf("Direct len: got %d want %d", got, want)
	}
	if want := []string{"consumer-x"}; !reflect.DeepEqual(br.AffectedConsumerRepos, want) {
		t.Errorf("AffectedConsumerRepos: got %v want %v", br.AffectedConsumerRepos, want)
	}
}
