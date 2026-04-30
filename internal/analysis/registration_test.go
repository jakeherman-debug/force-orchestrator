package analysis

import (
	"context"
	"database/sql"
	"testing"

	"force-orchestrator/internal/store"
)

// TestRegisterBayesianBetaBinomial_HappyPath — first call inserts a
// row; the row's version, config_hash, and description match the
// constants exposed by the package.
func TestRegisterBayesianBetaBinomial_HappyPath(t *testing.T) {
	db := openMemoryDB(t)
	ctx := context.Background()
	if err := RegisterBayesianBetaBinomial(ctx, db); err != nil {
		t.Fatalf("RegisterBayesianBetaBinomial: %v", err)
	}
	var version, hash, description string
	err := db.QueryRowContext(ctx, `
		SELECT version, config_hash, description
		FROM AnalysisFrameworks WHERE version = ?
	`, BayesianBetaBinomialVersion).Scan(&version, &hash, &description)
	if err != nil {
		t.Fatalf("read row: %v", err)
	}
	if version != BayesianBetaBinomialVersion {
		t.Errorf("version: got %q, want %q", version, BayesianBetaBinomialVersion)
	}
	if hash == "" {
		t.Errorf("config_hash should be non-empty")
	}
	if description == "" {
		t.Errorf("description should be non-empty")
	}
}

// TestRegisterBayesianBetaBinomial_Idempotent — calling Register
// twice produces exactly one row, and the second call is a no-op
// (no error, no duplicate).
func TestRegisterBayesianBetaBinomial_Idempotent(t *testing.T) {
	db := openMemoryDB(t)
	ctx := context.Background()
	if err := RegisterBayesianBetaBinomial(ctx, db); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if err := RegisterBayesianBetaBinomial(ctx, db); err != nil {
		t.Fatalf("second call: %v", err)
	}
	var count int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM AnalysisFrameworks WHERE version = ?`,
		BayesianBetaBinomialVersion,
	).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("row count after two registrations: got %d, want 1", count)
	}
}

// TestRegisterBayesianBetaBinomial_RejectsDifferentManifestSameVersion —
// pre-seed a row at the same version with a tampered config_hash.
// Re-registering must error rather than silently overwrite — the
// AnalysisFrameworks contract is that published versions are
// immutable.
func TestRegisterBayesianBetaBinomial_RejectsDifferentManifestSameVersion(t *testing.T) {
	db := openMemoryDB(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO AnalysisFrameworks
			(version, config_content, config_hash, published_by)
		VALUES (?, '{}', 'definitely-different-hash', 'tampered')
	`, BayesianBetaBinomialVersion); err != nil {
		t.Fatalf("seed tampered row: %v", err)
	}
	if err := RegisterBayesianBetaBinomial(ctx, db); err == nil {
		t.Errorf("expected immutability error, got nil")
	}
}

// TestRegisterBayesianBetaBinomial_NilDB — nil db is a programmer
// error, not a runtime panic.
func TestRegisterBayesianBetaBinomial_NilDB(t *testing.T) {
	if err := RegisterBayesianBetaBinomial(context.Background(), nil); err == nil {
		t.Errorf("expected error for nil db, got nil")
	}
}

func openMemoryDB(t *testing.T) *sql.DB {
	t.Helper()
	db := store.InitHolocronDSN(":memory:")
	if db == nil {
		t.Fatalf("InitHolocronDSN returned nil")
	}
	t.Cleanup(func() { db.Close() })
	return db
}
