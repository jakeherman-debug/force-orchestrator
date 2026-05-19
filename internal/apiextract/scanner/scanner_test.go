package scanner_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"force-orchestrator/internal/apiextract"
	"force-orchestrator/internal/apiextract/scanner"
	"force-orchestrator/internal/store"
)

// ── stub extractors ──────────────────────────────────────────────────────────

// stubProvider is a test-only ProviderExtractor that matches .txt files.
type stubProvider struct {
	name string
}

func (s *stubProvider) Kind() string          { return "test_route" }
func (s *stubProvider) ExtractorName() string { return s.name }
func (s *stubProvider) Extract(repoName, filePath string, content []byte) ([]store.CrossRepoAPI, error) {
	return []store.CrossRepoAPI{{
		APIKind:       "test_route",
		APIIdentifier: "GET /test",
		SourceLine:    1,
	}}, nil
}

// stubConsumer is a test-only ConsumerExtractor.
type stubConsumer struct {
	apiIdentifier string
}

func (s *stubConsumer) SupportedCallKinds() []string { return []string{"fetch"} }
func (s *stubConsumer) Extract(repoName, filePath string, content []byte) ([]store.CrossRepoAPIDependency, error) {
	return []store.CrossRepoAPIDependency{{
		CallKind:      "fetch",
		MatchConf:     1.0,
		APIIdentifier: s.apiIdentifier,
	}}, nil
}

// ── registry tests ───────────────────────────────────────────────────────────

func TestExtractorRegistry_RegisterAndList(t *testing.T) {
	reg := apiextract.NewExtractorRegistry()

	p1 := &stubProvider{name: "p1"}
	p2 := &stubProvider{name: "p2"}
	reg.RegisterProvider(p1)
	reg.RegisterProvider(p2)

	providers := reg.AllProviders()
	if len(providers) != 2 {
		t.Errorf("AllProviders: got %d, want 2", len(providers))
	}

	c1 := &stubConsumer{}
	reg.RegisterConsumer(c1)

	consumers := reg.AllConsumers()
	if len(consumers) != 1 {
		t.Errorf("AllConsumers: got %d, want 1", len(consumers))
	}
}

func TestExtractorRegistry_Empty(t *testing.T) {
	reg := apiextract.NewExtractorRegistry()
	if len(reg.AllProviders()) != 0 {
		t.Error("empty registry: AllProviders should return empty slice")
	}
	if len(reg.AllConsumers()) != 0 {
		t.Error("empty registry: AllConsumers should return empty slice")
	}
}

// ── scanner tests ────────────────────────────────────────────────────────────

// railsLikeProvider is a stub that matches .rb files (like the rails extractor
// would but without importing the concrete package).
type rbProvider struct{}

func (e *rbProvider) Kind() string          { return "http_route" }
func (e *rbProvider) ExtractorName() string { return "rails-routes" }
func (e *rbProvider) Extract(repoName, filePath string, content []byte) ([]store.CrossRepoAPI, error) {
	return []store.CrossRepoAPI{{
		APIKind:       "http_route",
		APIIdentifier: "GET /users/:id",
		SourceLine:    1,
	}}, nil
}

func TestScanner_ScanProviders_Basic(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	reg := apiextract.NewExtractorRegistry()
	reg.RegisterProvider(&rbProvider{})

	// Create a temp repo with a .rb file.
	repoDir := t.TempDir()
	confDir := filepath.Join(repoDir, "config")
	if err := os.MkdirAll(confDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(confDir, "routes.rb"), []byte("get '/users/:id'"), 0644); err != nil {
		t.Fatal(err)
	}

	sc := scanner.New(db, reg)
	n, err := sc.ScanProviders(context.Background(), "test-repo", repoDir)
	if err != nil {
		t.Fatalf("ScanProviders: %v", err)
	}
	if n != 1 {
		t.Errorf("ScanProviders: got %d upserted, want 1", n)
	}

	// Verify the row is in the DB and has a normalised api_identifier.
	apis, err := store.ListCrossRepoAPIs(db, "test-repo")
	if err != nil {
		t.Fatalf("ListCrossRepoAPIs: %v", err)
	}
	if len(apis) != 1 {
		t.Fatalf("ListCrossRepoAPIs: got %d rows, want 1", len(apis))
	}
	if apis[0].APIIdentifier != "GET /users/:id" {
		t.Errorf("APIIdentifier = %q, want %q", apis[0].APIIdentifier, "GET /users/:id")
	}
	// Idempotence: a second scan should not add a duplicate row.
	n2, err := sc.ScanProviders(context.Background(), "test-repo", repoDir)
	if err != nil {
		t.Fatalf("ScanProviders idempotent: %v", err)
	}
	if n2 != 1 {
		t.Errorf("ScanProviders idempotent: got %d upserted, want 1", n2)
	}
	apis2, _ := store.ListCrossRepoAPIs(db, "test-repo")
	if len(apis2) != 1 {
		t.Errorf("idempotent: expected still 1 row, got %d", len(apis2))
	}
}

func TestScanner_ScanProviders_EmptyRegistry(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	reg := apiextract.NewExtractorRegistry()
	sc := scanner.New(db, reg)

	repoDir := t.TempDir()
	n, err := sc.ScanProviders(context.Background(), "test-repo", repoDir)
	if err != nil {
		t.Fatalf("ScanProviders with empty registry: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 upserted with empty registry, got %d", n)
	}
}

func TestScanner_ScanConsumers_ResolvesInline(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	now := store.NowSQLite()

	// Seed a provider API.
	apiID, err := store.UpsertCrossRepoAPI(db, store.CrossRepoAPI{
		RepoName: "provider-svc", APIKind: "http_route",
		APIIdentifier: "GET /users/:id",
		SourceFile: "routes.rb", SourceLine: 1,
		Extractor: "rails-routes", SignatureHash: "h", LastScannedAt: now,
	})
	if err != nil {
		t.Fatalf("seed provider API: %v", err)
	}
	if apiID <= 0 {
		t.Fatalf("seed: expected positive apiID, got %d", apiID)
	}

	// Register a consumer extractor that emits a dep for "GET /users/:id".
	reg := apiextract.NewExtractorRegistry()
	reg.RegisterConsumer(&stubConsumer{apiIdentifier: "GET /users/:id"})

	// Create a temp file for the consumer extractor to match (.ts → jsclient/fetch).
	repoDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoDir, "api.ts"), []byte("fetch('/users/1')"), 0644); err != nil {
		t.Fatal(err)
	}

	sc := scanner.New(db, reg)
	n, err := sc.ScanConsumers(context.Background(), "consumer-svc", repoDir)
	if err != nil {
		t.Fatalf("ScanConsumers: %v", err)
	}
	// The dep resolves to apiID (provider found inline), so 1 row upserted.
	if n != 1 {
		t.Errorf("ScanConsumers: got %d upserted, want 1", n)
	}

	// Verify the dep row has the correct provider_api_id.
	deps, err := store.GetAPIBlastRadius(db, apiID)
	if err != nil {
		t.Fatalf("GetAPIBlastRadius: %v", err)
	}
	if len(deps) != 1 {
		t.Fatalf("expected 1 dependency, got %d", len(deps))
	}
	if deps[0].ConsumerRepo != "consumer-svc" {
		t.Errorf("consumer_repo = %q, want consumer-svc", deps[0].ConsumerRepo)
	}
}

func TestScanner_ScanConsumers_SkipsUnresolvable(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// No provider APIs in DB.
	reg := apiextract.NewExtractorRegistry()
	reg.RegisterConsumer(&stubConsumer{apiIdentifier: "/no/such/api"})

	repoDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoDir, "api.ts"), []byte("fetch('/no/such/api')"), 0644); err != nil {
		t.Fatal(err)
	}

	sc := scanner.New(db, reg)
	n, err := sc.ScanConsumers(context.Background(), "consumer-svc", repoDir)
	if err != nil {
		t.Fatalf("ScanConsumers: %v", err)
	}
	// Provider not found → dep not inserted.
	if n != 0 {
		t.Errorf("ScanConsumers: expected 0 upserted (unresolvable), got %d", n)
	}
}

func TestScanner_SkipsVendorDir(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	reg := apiextract.NewExtractorRegistry()
	reg.RegisterProvider(&rbProvider{})

	repoDir := t.TempDir()
	vendorDir := filepath.Join(repoDir, "vendor", "config")
	if err := os.MkdirAll(vendorDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Place a .rb file inside vendor/ — it should be skipped.
	if err := os.WriteFile(filepath.Join(vendorDir, "routes.rb"), []byte("get '/v'"), 0644); err != nil {
		t.Fatal(err)
	}

	sc := scanner.New(db, reg)
	n, err := sc.ScanProviders(context.Background(), "test-repo", repoDir)
	if err != nil {
		t.Fatalf("ScanProviders: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 (vendor dir skipped), got %d", n)
	}
}

// TestResolveConsumerDependencies tests the DB-only path-matcher.
func TestResolveConsumerDependencies_Basic(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	now := store.NowSQLite()

	apiID, err := store.UpsertCrossRepoAPI(db, store.CrossRepoAPI{
		RepoName: "svc", APIKind: "http_route",
		APIIdentifier: "GET /items/:id",
		SourceFile: "r.rb", SourceLine: 1,
		Extractor: "rails-routes", SignatureHash: "h", LastScannedAt: now,
	})
	if err != nil {
		t.Fatalf("seed provider: %v", err)
	}

	// Temporarily disable FK to seed the dep row with provider_api_id=0.
	if _, err := db.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatalf("disable FK: %v", err)
	}
	_, err = db.Exec(`
		INSERT INTO CrossRepoAPIDependencies
			(consumer_repo, consumer_file, consumer_line, provider_api_id, call_kind, match_confidence, discovered_at, deleted_at)
		VALUES ('consumer', 'GET /items/:id', 5, 0, 'fetch', 1.0, ?, '')`, now)
	if err != nil {
		t.Fatalf("seed dep: %v", err)
	}
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		t.Fatalf("re-enable FK: %v", err)
	}

	resolved, rErr := scanner.ResolveConsumerDependencies(context.Background(), db)
	if rErr != nil {
		t.Fatalf("ResolveConsumerDependencies: %v", rErr)
	}
	if resolved != 1 {
		t.Errorf("resolved %d, want 1", resolved)
	}

	// Verify ProviderAPIID is set.
	var gotAPIID int
	db.QueryRow(`SELECT provider_api_id FROM CrossRepoAPIDependencies WHERE consumer_repo='consumer'`).Scan(&gotAPIID)
	if gotAPIID != apiID {
		t.Errorf("provider_api_id = %d, want %d", gotAPIID, apiID)
	}
}

func TestResolveConsumerDependencies_Idempotent(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	now := store.NowSQLite()
	_, err := store.UpsertCrossRepoAPI(db, store.CrossRepoAPI{
		RepoName: "svc", APIKind: "http_route",
		APIIdentifier: "GET /items/:id",
		SourceFile: "r.rb", SourceLine: 1,
		Extractor: "rails-routes", SignatureHash: "h", LastScannedAt: now,
	})
	if err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	// Disable FK for seeding.
	db.Exec(`PRAGMA foreign_keys = OFF`) //nolint:errcheck
	_, err = db.Exec(`
		INSERT INTO CrossRepoAPIDependencies
			(consumer_repo, consumer_file, consumer_line, provider_api_id, call_kind, match_confidence, discovered_at, deleted_at)
		VALUES ('consumer', 'GET /items/:id', 5, 0, 'fetch', 1.0, ?, '')`, now)
	if err != nil {
		t.Fatalf("seed dep: %v", err)
	}
	db.Exec(`PRAGMA foreign_keys = ON`) //nolint:errcheck

	// Run twice — second run finds no unresolved rows, returns 0.
	scanner.ResolveConsumerDependencies(context.Background(), db) //nolint:errcheck
	n, rErr := scanner.ResolveConsumerDependencies(context.Background(), db)
	if rErr != nil {
		t.Fatalf("second ResolveConsumerDependencies: %v", rErr)
	}
	if n != 0 {
		t.Errorf("second run resolved %d, want 0 (already resolved)", n)
	}
}
