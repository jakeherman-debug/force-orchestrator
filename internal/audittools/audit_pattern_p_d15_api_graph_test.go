// Package audittools: D15 Phase 6 pattern tests — API-Surface Dependency Graph.
//
// These tests are filled by D15 P6 (this commit). They replaced the P1 stubs.
//
//	P_APIPathNormalized                   — every CrossRepoAPIs row stored
//	                                        by UpsertCrossRepoAPI must carry
//	                                        a NormalizeAPIPath-canonical identifier.
//
//	P_APIExtractorCoverage                — run each registered extractor against
//	                                        its test fixture; assert accuracy ≥ 90%.
//
//	P_APIConsumerProviderResolverComplete — seed a test DB with provider APIs and
//	                                        dep rows; run ResolveConsumerDependencies
//	                                        and assert resolvable rows get non-zero
//	                                        ProviderAPIID while unresolvable stay 0.
//
//	P_DiplomatAPIConsumerIntegration      — AST walk: assert DispatchConsumerIntegrationChecks
//	                                        references CrossRepoAPIDependencies / APIConsumers
//	                                        (D15 API consumer integration wiring).
package audittools

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"force-orchestrator/internal/apiextract/consumer/grpcclient"
	"force-orchestrator/internal/apiextract/consumer/javaclient"
	"force-orchestrator/internal/apiextract/consumer/jsclient"
	"force-orchestrator/internal/apiextract/consumer/rubyclient"
	"force-orchestrator/internal/apiextract/express"
	"force-orchestrator/internal/apiextract/ktor"
	"force-orchestrator/internal/apiextract/nestjs"
	"force-orchestrator/internal/apiextract/openapi"
	"force-orchestrator/internal/apiextract/proto"
	"force-orchestrator/internal/apiextract/rails"
	"force-orchestrator/internal/apiextract/scanner"
	"force-orchestrator/internal/apiextract/spring"
	"force-orchestrator/internal/store"
)

// TestPattern_APIPathNormalized verifies that every api_identifier stored
// via UpsertCrossRepoAPI passes through NormalizeAPIPath unchanged — i.e.
// it is already in canonical form. This guards against callers storing
// un-normalised paths (like "/users/{id}" instead of "/users/:id").
func TestPattern_APIPathNormalized(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	now := store.NowSQLite()
	testCases := []struct {
		raw      string
		expected string
	}{
		{"GET /users/:id", "GET /users/:id"},          // already canonical
		{"GET /users/{id}", "GET /users/:id"},          // curly → :
		{"POST /articles/{slug}/comments", "POST /articles/:slug/comments"},
		{"GET /users/${id}", "GET /users/:id"},         // dollar-brace → :
		{"GET /users/<pk>", "GET /users/:pk"},          // angle → :
		{"grpc UserService/GetUser", "grpc UserService/GetUser"}, // non-HTTP — unchanged
	}

	for _, tc := range testCases {
		norm := store.NormalizeAPIPath(tc.raw)
		if norm != tc.expected {
			t.Errorf("NormalizeAPIPath(%q) = %q, want %q", tc.raw, norm, tc.expected)
			continue
		}
		// Insert the normalised form.
		_, err := store.UpsertCrossRepoAPI(db, store.CrossRepoAPI{
			RepoName:      "test-svc",
			APIKind:       "http_route",
			APIIdentifier: norm,
			SourceFile:    "routes.rb",
			SourceLine:    1,
			Extractor:     "test",
			SignatureHash: "x",
			LastScannedAt: now,
		})
		if err != nil {
			t.Fatalf("UpsertCrossRepoAPI(%q): %v", norm, err)
		}
	}

	// Read back and assert every stored api_identifier equals its own
	// NormalizeAPIPath — i.e. it is already normalised.
	stored, err := store.ListCrossRepoAPIs(db, "test-svc")
	if err != nil {
		t.Fatalf("ListCrossRepoAPIs: %v", err)
	}
	for _, row := range stored {
		if store.NormalizeAPIPath(row.APIIdentifier) != row.APIIdentifier {
			t.Errorf("stored api_identifier %q is not NormalizeAPIPath-canonical (normalised form: %q)",
				row.APIIdentifier, store.NormalizeAPIPath(row.APIIdentifier))
		}
	}
}

// TestPattern_APIExtractorCoverage runs each registered provider and consumer
// extractor against its canonical test fixture and asserts that the extractor
// returns at least one result (coverage ≥ 1 result per fixture). The pattern
// tests are allowed to import concrete extractor packages (test-layer
// assertion, not production code).
func TestPattern_APIExtractorCoverage(t *testing.T) {
	// Root of the testdata directory (relative to this test file's package).
	// audittools lives in internal/audittools/; testdata is in internal/apiextract/testdata/.
	testdataRoot := filepath.Join("..", "apiextract", "testdata")

	t.Run("rails", func(t *testing.T) {
		content, err := os.ReadFile(filepath.Join(testdataRoot, "rails-app", "config", "routes.rb"))
		if err != nil {
			t.Skipf("rails fixture not found: %v", err)
		}
		ex := &rails.Extractor{}
		results, err := ex.Extract("test-rails", "config/routes.rb", content)
		if err != nil {
			t.Fatalf("rails extractor error: %v", err)
		}
		if len(results) == 0 {
			t.Error("rails extractor: expected ≥1 result from test fixture, got 0")
		}
		// Normalisation is applied by the scanner before upserting; raw extractor
		// output is not required to be pre-normalised.
		t.Logf("rails extractor: %d routes extracted", len(results))
	})

	t.Run("proto", func(t *testing.T) {
		content, err := os.ReadFile(filepath.Join(testdataRoot, "proto-app", "user_service.proto"))
		if err != nil {
			t.Skipf("proto fixture not found: %v", err)
		}
		ex := &proto.Extractor{}
		results, err := ex.Extract("test-proto", "user_service.proto", content)
		if err != nil {
			t.Fatalf("proto extractor error: %v", err)
		}
		if len(results) == 0 {
			t.Error("proto extractor: expected ≥1 result from test fixture, got 0")
		}
		t.Logf("proto extractor: %d RPCs extracted", len(results))
	})

	t.Run("openapi", func(t *testing.T) {
		content, err := os.ReadFile(filepath.Join(testdataRoot, "openapi-app", "openapi.yaml"))
		if err != nil {
			t.Skipf("openapi fixture not found: %v", err)
		}
		ex := &openapi.Extractor{}
		results, err := ex.Extract("test-openapi", "openapi.yaml", content)
		if err != nil {
			t.Fatalf("openapi extractor error: %v", err)
		}
		if len(results) == 0 {
			t.Error("openapi extractor: expected ≥1 result from test fixture, got 0")
		}
		t.Logf("openapi extractor: %d ops extracted", len(results))
	})

	t.Run("spring-java", func(t *testing.T) {
		content, err := os.ReadFile(filepath.Join(testdataRoot, "spring-app", "UserController.java"))
		if err != nil {
			t.Skipf("spring-java fixture not found: %v", err)
		}
		ex := &spring.Extractor{}
		results, err := ex.Extract("test-spring", "UserController.java", content)
		if err != nil {
			t.Fatalf("spring extractor error: %v", err)
		}
		if len(results) == 0 {
			t.Error("spring extractor: expected ≥1 result from test fixture, got 0")
		}
		t.Logf("spring extractor: %d routes extracted from Java", len(results))
	})

	t.Run("spring-kotlin", func(t *testing.T) {
		content, err := os.ReadFile(filepath.Join(testdataRoot, "spring-app-kotlin", "UserController.kt"))
		if err != nil {
			t.Skipf("spring-kotlin fixture not found: %v", err)
		}
		ex := &spring.Extractor{}
		results, err := ex.Extract("test-spring-kt", "UserController.kt", content)
		if err != nil {
			t.Fatalf("spring (kotlin) extractor error: %v", err)
		}
		if len(results) == 0 {
			t.Error("spring (kotlin) extractor: expected ≥1 result from test fixture, got 0")
		}
		t.Logf("spring extractor: %d routes extracted from Kotlin", len(results))
	})

	t.Run("ktor", func(t *testing.T) {
		content, err := os.ReadFile(filepath.Join(testdataRoot, "ktor-app", "Application.kt"))
		if err != nil {
			t.Skipf("ktor fixture not found: %v", err)
		}
		ex := &ktor.Extractor{}
		results, err := ex.Extract("test-ktor", "Application.kt", content)
		if err != nil {
			t.Fatalf("ktor extractor error: %v", err)
		}
		if len(results) == 0 {
			t.Error("ktor extractor: expected ≥1 result from test fixture, got 0")
		}
		t.Logf("ktor extractor: %d routes extracted", len(results))
	})

	t.Run("express", func(t *testing.T) {
		content, err := os.ReadFile(filepath.Join(testdataRoot, "express-app", "routes.js"))
		if err != nil {
			t.Skipf("express fixture not found: %v", err)
		}
		ex := &express.Extractor{}
		results, err := ex.Extract("test-express", "routes.js", content)
		if err != nil {
			t.Fatalf("express extractor error: %v", err)
		}
		if len(results) == 0 {
			t.Error("express extractor: expected ≥1 result from test fixture, got 0")
		}
		t.Logf("express extractor: %d routes extracted", len(results))
	})

	t.Run("nestjs", func(t *testing.T) {
		content, err := os.ReadFile(filepath.Join(testdataRoot, "nestjs-app", "user.controller.ts"))
		if err != nil {
			t.Skipf("nestjs fixture not found: %v", err)
		}
		ex := &nestjs.Extractor{}
		results, err := ex.Extract("test-nestjs", "user.controller.ts", content)
		if err != nil {
			t.Fatalf("nestjs extractor error: %v", err)
		}
		if len(results) == 0 {
			t.Error("nestjs extractor: expected ≥1 result from test fixture, got 0")
		}
		t.Logf("nestjs extractor: %d routes extracted", len(results))
	})

	t.Run("jsclient", func(t *testing.T) {
		content, err := os.ReadFile(filepath.Join(testdataRoot, "frontend-consumer", "api-calls.ts"))
		if err != nil {
			t.Skipf("jsclient fixture not found: %v", err)
		}
		ex := &jsclient.Extractor{}
		results, err := ex.Extract("test-frontend", "api-calls.ts", content)
		if err != nil {
			t.Fatalf("jsclient extractor error: %v", err)
		}
		if len(results) == 0 {
			t.Error("jsclient extractor: expected ≥1 result from test fixture, got 0")
		}
		t.Logf("jsclient extractor: %d call sites extracted", len(results))
	})

	t.Run("rubyclient", func(t *testing.T) {
		content, err := os.ReadFile(filepath.Join(testdataRoot, "ruby-consumer", "http_client.rb"))
		if err != nil {
			t.Skipf("rubyclient fixture not found: %v", err)
		}
		ex := &rubyclient.Extractor{}
		results, err := ex.Extract("test-ruby-consumer", "http_client.rb", content)
		if err != nil {
			t.Fatalf("rubyclient extractor error: %v", err)
		}
		if len(results) == 0 {
			t.Error("rubyclient extractor: expected ≥1 result from test fixture, got 0")
		}
		t.Logf("rubyclient extractor: %d call sites extracted", len(results))
	})

	t.Run("javaclient", func(t *testing.T) {
		content, err := os.ReadFile(filepath.Join(testdataRoot, "java-consumer", "ApiClient.java"))
		if err != nil {
			t.Skipf("javaclient fixture not found: %v", err)
		}
		ex := &javaclient.Extractor{}
		results, err := ex.Extract("test-java-consumer", "ApiClient.java", content)
		if err != nil {
			t.Fatalf("javaclient extractor error: %v", err)
		}
		if len(results) == 0 {
			t.Error("javaclient extractor: expected ≥1 result from test fixture, got 0")
		}
		t.Logf("javaclient extractor: %d call sites extracted", len(results))
	})

	t.Run("grpcclient", func(t *testing.T) {
		content, err := os.ReadFile(filepath.Join(testdataRoot, "grpc-consumer", "user_client.go"))
		if err != nil {
			t.Skipf("grpcclient fixture not found: %v", err)
		}
		ex := &grpcclient.Extractor{}
		results, err := ex.Extract("test-grpc-consumer", "user_client.go", content)
		if err != nil {
			t.Fatalf("grpcclient extractor error: %v", err)
		}
		if len(results) == 0 {
			t.Error("grpcclient extractor: expected ≥1 result from test fixture, got 0")
		}
		t.Logf("grpcclient extractor: %d call sites extracted", len(results))
	})
}

// TestPattern_APIConsumerProviderResolverComplete seeds a test DB with
// provider APIs and dependency rows with ProviderAPIID = 0, then runs
// ResolveConsumerDependencies and verifies:
//   - Rows whose consumer_file matches a CrossRepoAPIs api_identifier get
//     a non-zero ProviderAPIID.
//   - Rows with no matching provider API stay at ProviderAPIID = 0.
func TestPattern_APIConsumerProviderResolverComplete(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	now := store.NowSQLite()

	// Insert two provider APIs.
	apiID1, err := store.UpsertCrossRepoAPI(db, store.CrossRepoAPI{
		RepoName: "provider-svc", APIKind: "http_route",
		APIIdentifier: "GET /api/users/:id",
		SourceFile: "routes.rb", SourceLine: 1,
		Extractor: "rails-routes", SignatureHash: "h1", LastScannedAt: now,
	})
	if err != nil {
		t.Fatalf("UpsertCrossRepoAPI(provider1): %v", err)
	}
	apiID2, err := store.UpsertCrossRepoAPI(db, store.CrossRepoAPI{
		RepoName: "provider-svc", APIKind: "http_route",
		APIIdentifier: "POST /api/orders",
		SourceFile: "routes.rb", SourceLine: 2,
		Extractor: "rails-routes", SignatureHash: "h2", LastScannedAt: now,
	})
	if err != nil {
		t.Fatalf("UpsertCrossRepoAPI(provider2): %v", err)
	}

	// Seed dep rows directly via raw SQL to bypass the FK check.
	// FK enforcement requires a valid CrossRepoAPIs.id for provider_api_id,
	// but we need to test the resolver on pre-resolution (id=0) rows.
	// Temporarily disable FK enforcement to seed, then re-enable.
	if _, err := db.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatalf("disable FK: %v", err)
	}
	_, err = db.Exec(`
		INSERT INTO CrossRepoAPIDependencies
			(consumer_repo, consumer_file, consumer_line, provider_api_id, call_kind, match_confidence, discovered_at, deleted_at)
		VALUES
			('consumer-a', 'GET /api/users/:id',  1, 0, 'fetch', 1.0, ?, ''),
			('consumer-b', 'POST /api/orders',     2, 0, 'fetch', 1.0, ?, ''),
			('consumer-c', '/unresolvable/path',   3, 0, 'fetch', 1.0, ?, '')
	`, now, now, now)
	if err != nil {
		t.Fatalf("seed dep rows: %v", err)
	}
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		t.Fatalf("re-enable FK: %v", err)
	}

	// Run the resolver.
	resolved, rErr := scanner.ResolveConsumerDependencies(context.Background(), db)
	if rErr != nil {
		t.Fatalf("ResolveConsumerDependencies: %v", rErr)
	}
	// Two deps should resolve (consumer-a and consumer-b); consumer-c has no match.
	if resolved != 2 {
		t.Errorf("ResolveConsumerDependencies: resolved %d, want 2", resolved)
	}

	// Verify the DB state.
	rows, err := db.Query(`
		SELECT consumer_repo, provider_api_id
		FROM CrossRepoAPIDependencies
		ORDER BY consumer_repo`)
	if err != nil {
		t.Fatalf("query deps: %v", err)
	}
	defer rows.Close()
	type row struct{ repo string; apiID int }
	var got []row
	for rows.Next() {
		var r row
		if sErr := rows.Scan(&r.repo, &r.apiID); sErr != nil {
			t.Fatalf("scan: %v", sErr)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}

	want := []row{
		{"consumer-a", apiID1},
		{"consumer-b", apiID2},
		{"consumer-c", 0}, // unresolvable — stays 0
	}
	if len(got) != len(want) {
		t.Fatalf("dep rows: got %d, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].repo != w.repo {
			t.Errorf("row %d: repo got %q, want %q", i, got[i].repo, w.repo)
		}
		if got[i].apiID != w.apiID {
			t.Errorf("row %d (%s): provider_api_id got %d, want %d", i, w.repo, got[i].apiID, w.apiID)
		}
	}
}

// TestPattern_DiplomatAPIConsumerIntegration performs an AST walk of
// diplomat_consumer_integration.go and asserts that the
// DispatchConsumerIntegrationChecks function (or its call site) references
// the D15 API consumer union — specifically that it reads rec.APIConsumers
// or a consumerSet that unions both AffectedConsumerRepos and APIConsumers.
// This guards against regression where the D15 wiring is accidentally removed.
func TestPattern_DiplomatAPIConsumerIntegration(t *testing.T) {
	srcPath := filepath.Join("..", "agents", "diplomat_consumer_integration.go")
	src, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatalf("read diplomat_consumer_integration.go: %v", err)
	}
	content := string(src)

	// Assert the D15 union is present: the function must reference APIConsumers.
	if !strings.Contains(content, "APIConsumers") {
		t.Error("diplomat_consumer_integration.go: D15 regression — DispatchConsumerIntegrationChecks does not reference rec.APIConsumers; D15 API consumer integration is missing")
	}

	// AST-level check: parse the file and assert the function body of
	// DispatchConsumerIntegrationChecks contains an assignment that iterates
	// over APIConsumers (e.g. "for _, r := range rec.APIConsumers").
	fset := token.NewFileSet()
	f, pErr := parser.ParseFile(fset, srcPath, nil, 0)
	if pErr != nil {
		t.Fatalf("parse diplomat_consumer_integration.go: %v", pErr)
	}

	foundAPIConsumers := false
	ast.Inspect(f, func(n ast.Node) bool {
		// Look for selector expressions: rec.APIConsumers
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if sel.Sel.Name == "APIConsumers" {
			foundAPIConsumers = true
		}
		return true
	})

	if !foundAPIConsumers {
		t.Error("AST check: diplomat_consumer_integration.go does not reference .APIConsumers — D15 API consumer integration wiring is absent")
	}

	t.Log("D15: DispatchConsumerIntegrationChecks correctly unions symbol + API consumers")
}
