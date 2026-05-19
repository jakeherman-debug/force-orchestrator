package store

import (
	"testing"
)

// TestUpsertCrossRepoAPI verifies insert and upsert/update behaviour for
// CrossRepoAPIs rows.
func TestUpsertCrossRepoAPI(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	api := CrossRepoAPI{
		RepoName:      "my-service",
		APIKind:       "http_route",
		APIIdentifier: "GET /api/v1/users/:id",
		SourceFile:    "config/routes.rb",
		SourceLine:    42,
		Extractor:     "rails-routes",
		SignatureHash: "abc123",
		LastScannedAt: NowSQLite(),
	}

	// Insert — should return a positive id.
	id, err := UpsertCrossRepoAPI(db, api)
	if err != nil {
		t.Fatalf("UpsertCrossRepoAPI (insert): %v", err)
	}
	if id <= 0 {
		t.Errorf("UpsertCrossRepoAPI: id = %d, want > 0", id)
	}

	// Idempotent re-insert — same id returned.
	id2, err := UpsertCrossRepoAPI(db, api)
	if err != nil {
		t.Fatalf("UpsertCrossRepoAPI (idempotent): %v", err)
	}
	if id2 != id {
		t.Errorf("UpsertCrossRepoAPI idempotent: got id %d, want %d", id2, id)
	}

	// Update — change signature_hash and source_line.
	api.SignatureHash = "def456"
	api.SourceLine = 99
	id3, err := UpsertCrossRepoAPI(db, api)
	if err != nil {
		t.Fatalf("UpsertCrossRepoAPI (update): %v", err)
	}
	if id3 != id {
		t.Errorf("UpsertCrossRepoAPI update: id changed from %d to %d", id, id3)
	}

	// Verify updated fields were persisted.
	apis, err := ListCrossRepoAPIs(db, "my-service")
	if err != nil {
		t.Fatalf("ListCrossRepoAPIs: %v", err)
	}
	if len(apis) != 1 {
		t.Fatalf("ListCrossRepoAPIs: got %d rows, want 1", len(apis))
	}
	if apis[0].SignatureHash != "def456" {
		t.Errorf("SignatureHash = %q, want def456", apis[0].SignatureHash)
	}
	if apis[0].SourceLine != 99 {
		t.Errorf("SourceLine = %d, want 99", apis[0].SourceLine)
	}
}

// TestListCrossRepoAPIs verifies that ListCrossRepoAPIs returns only rows for
// the requested repo and in the expected order.
func TestListCrossRepoAPIs(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	now := NowSQLite()

	// Insert two APIs for "svc-a" and one for "svc-b".
	for _, a := range []CrossRepoAPI{
		{RepoName: "svc-a", APIKind: "http_route", APIIdentifier: "GET /a", LastScannedAt: now},
		{RepoName: "svc-a", APIKind: "grpc_rpc", APIIdentifier: "svc.A/GetItem", LastScannedAt: now},
		{RepoName: "svc-b", APIKind: "http_route", APIIdentifier: "GET /b", LastScannedAt: now},
	} {
		if _, err := UpsertCrossRepoAPI(db, a); err != nil {
			t.Fatalf("UpsertCrossRepoAPI(%q): %v", a.APIIdentifier, err)
		}
	}

	rows, err := ListCrossRepoAPIs(db, "svc-a")
	if err != nil {
		t.Fatalf("ListCrossRepoAPIs: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("ListCrossRepoAPIs(svc-a): got %d rows, want 2", len(rows))
	}
	for _, r := range rows {
		if r.RepoName != "svc-a" {
			t.Errorf("ListCrossRepoAPIs: got row for wrong repo %q", r.RepoName)
		}
	}

	// svc-b should not appear.
	bRows, err := ListCrossRepoAPIs(db, "svc-b")
	if err != nil {
		t.Fatalf("ListCrossRepoAPIs(svc-b): %v", err)
	}
	if len(bRows) != 1 {
		t.Errorf("ListCrossRepoAPIs(svc-b): got %d rows, want 1", len(bRows))
	}
}

// TestGetAPIBlastRadius verifies that GetAPIBlastRadius returns only active
// (non-soft-deleted) dependency rows for the given provider API id.
func TestGetAPIBlastRadius(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	now := NowSQLite()

	// Insert a provider API.
	providerID, err := UpsertCrossRepoAPI(db, CrossRepoAPI{
		RepoName:      "payment-svc",
		APIKind:       "http_route",
		APIIdentifier: "POST /api/v1/charges",
		LastScannedAt: now,
	})
	if err != nil {
		t.Fatalf("UpsertCrossRepoAPI (provider): %v", err)
	}

	// Insert three dependency rows: two active, one will be soft-deleted.
	deps := []CrossRepoAPIDependency{
		{ConsumerRepo: "checkout-svc", ConsumerFile: "app/services/payment.rb", ConsumerLine: 10, ProviderAPIID: providerID, CallKind: "httparty", MatchConf: 1.0, DiscoveredAt: now},
		{ConsumerRepo: "billing-svc", ConsumerFile: "src/billing.go", ConsumerLine: 55, ProviderAPIID: providerID, CallKind: "faraday", MatchConf: 0.9, DiscoveredAt: now},
		{ConsumerRepo: "legacy-svc", ConsumerFile: "lib/old.rb", ConsumerLine: 7, ProviderAPIID: providerID, CallKind: "httparty", MatchConf: 0.8, DiscoveredAt: now},
	}
	for i := range deps {
		if err := UpsertCrossRepoAPIDependency(db, deps[i]); err != nil {
			t.Fatalf("UpsertCrossRepoAPIDependency[%d]: %v", i, err)
		}
	}

	// Soft-delete the legacy-svc row.
	// Fetch its id first.
	var legacyID int
	err = db.QueryRow(`SELECT id FROM CrossRepoAPIDependencies WHERE consumer_repo = 'legacy-svc'`).Scan(&legacyID)
	if err != nil {
		t.Fatalf("get legacy-svc dep id: %v", err)
	}
	if err := SoftDeleteCrossRepoAPIDependency(db, legacyID); err != nil {
		t.Fatalf("SoftDeleteCrossRepoAPIDependency: %v", err)
	}

	// GetAPIBlastRadius should return 2 active rows (not the deleted one).
	radius, err := GetAPIBlastRadius(db, providerID)
	if err != nil {
		t.Fatalf("GetAPIBlastRadius: %v", err)
	}
	if len(radius) != 2 {
		t.Errorf("GetAPIBlastRadius: got %d rows, want 2", len(radius))
		for _, r := range radius {
			t.Logf("  consumer_repo=%q deleted_at=%q", r.ConsumerRepo, r.DeletedAt)
		}
	}

	// Verify no deleted row is returned.
	for _, r := range radius {
		if r.ConsumerRepo == "legacy-svc" {
			t.Errorf("GetAPIBlastRadius: returned soft-deleted row for legacy-svc")
		}
		if r.DeletedAt != "" {
			t.Errorf("GetAPIBlastRadius: returned row with deleted_at=%q", r.DeletedAt)
		}
	}
}

// TestUpsertCrossRepoAPIDependency_Idempotent verifies that upserting the same
// dependency twice is a no-op (INSERT OR IGNORE semantics).
func TestUpsertCrossRepoAPIDependency_Idempotent(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	now := NowSQLite()

	providerID, err := UpsertCrossRepoAPI(db, CrossRepoAPI{
		RepoName:      "api-svc",
		APIKind:       "http_route",
		APIIdentifier: "GET /api/items",
		LastScannedAt: now,
	})
	if err != nil {
		t.Fatalf("UpsertCrossRepoAPI: %v", err)
	}

	dep := CrossRepoAPIDependency{
		ConsumerRepo:  "frontend",
		ConsumerFile:  "src/api.ts",
		ConsumerLine:  20,
		ProviderAPIID: providerID,
		CallKind:      "fetch",
		MatchConf:     1.0,
		DiscoveredAt:  now,
	}

	if err := UpsertCrossRepoAPIDependency(db, dep); err != nil {
		t.Fatalf("UpsertCrossRepoAPIDependency (1st): %v", err)
	}
	// Second call must not error.
	if err := UpsertCrossRepoAPIDependency(db, dep); err != nil {
		t.Fatalf("UpsertCrossRepoAPIDependency (2nd, idempotent): %v", err)
	}

	radius, err := GetAPIBlastRadius(db, providerID)
	if err != nil {
		t.Fatalf("GetAPIBlastRadius: %v", err)
	}
	if len(radius) != 1 {
		t.Errorf("GetAPIBlastRadius after double-insert: got %d, want 1", len(radius))
	}
}

// TestNormalizeAPIPath_StoreIntegration verifies that a normalized path
// round-trips through UpsertCrossRepoAPI and ListCrossRepoAPIs unchanged.
func TestNormalizeAPIPath_StoreIntegration(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	raw := "GET /api/v1/users/{id}/posts/{postId}/"
	normalized := NormalizeAPIPath(raw)
	wantNorm := "GET /api/v1/users/:id/posts/:postId"

	if normalized != wantNorm {
		t.Fatalf("NormalizeAPIPath(%q) = %q, want %q", raw, normalized, wantNorm)
	}

	_, err := UpsertCrossRepoAPI(db, CrossRepoAPI{
		RepoName:      "test-svc",
		APIKind:       "http_route",
		APIIdentifier: normalized,
		LastScannedAt: NowSQLite(),
	})
	if err != nil {
		t.Fatalf("UpsertCrossRepoAPI: %v", err)
	}

	apis, err := ListCrossRepoAPIs(db, "test-svc")
	if err != nil {
		t.Fatalf("ListCrossRepoAPIs: %v", err)
	}
	if len(apis) != 1 {
		t.Fatalf("ListCrossRepoAPIs: got %d, want 1", len(apis))
	}
	if apis[0].APIIdentifier != wantNorm {
		t.Errorf("stored identifier = %q, want %q", apis[0].APIIdentifier, wantNorm)
	}
}
