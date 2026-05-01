package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// mustOpenDedupTestDB returns a fresh in-memory DB for the dedup /
// quality / hypothesis / conflict-detection tests in this directory.
// All tables come from the live createSchema + runMigrations so the
// tests exercise real columns.
func mustOpenDedupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db := InitHolocronDSN(":memory:")
	if db == nil {
		t.Fatalf("InitHolocronDSN: nil db")
	}
	return db
}

// TestDedupAndMerge_HappyPath seeds two near-identical FleetMemory
// rows in the same repo and asserts that DedupAndMerge folds the
// newer one into the older one (canonical_id stamped, retrieval and
// validation rolled up, audit row written).
func TestDedupAndMerge_HappyPath(t *testing.T) {
	db := mustOpenDedupTestDB(t)
	defer db.Close()

	StoreFleetMemory(db, "repoA", 1, "success",
		"Authentication middleware uses JWT bearer tokens; rotate signing keys quarterly.",
		"auth.go,middleware.go", "auth, jwt")
	StoreFleetMemory(db, "repoA", 2, "success",
		"Authentication middleware uses JWT bearer tokens. Rotate signing keys quarterly.",
		"auth.go,middleware.go", "auth, bearer")

	// Bump retrieval and validation on the duplicate so we can verify
	// roll-up math.
	if _, err := db.Exec(`UPDATE FleetMemory SET retrieval_count = 4, validation_score = 0.4 WHERE id = 2`); err != nil {
		t.Fatalf("seed retrieval/validation: %v", err)
	}
	if _, err := db.Exec(`UPDATE FleetMemory SET retrieval_count = 2, validation_score = 0.2 WHERE id = 1`); err != nil {
		t.Fatalf("seed canonical retrieval: %v", err)
	}

	merged, err := DedupAndMerge(context.Background(), db)
	if err != nil {
		t.Fatalf("DedupAndMerge: %v", err)
	}
	if merged != 1 {
		t.Fatalf("expected 1 merge, got %d", merged)
	}

	// Verify the duplicate row has canonical_id = 1.
	var canonID int
	if err := db.QueryRow(`SELECT canonical_id FROM FleetMemory WHERE id = 2`).Scan(&canonID); err != nil {
		t.Fatalf("read canonical_id: %v", err)
	}
	if canonID != 1 {
		t.Errorf("expected canonical_id=1 on duplicate row, got %d", canonID)
	}

	// Roll-up: canonical's retrieval_count = 2 + 4 = 6, validation_score
	// = (0.2 + 0.4) / 2 = 0.3, tags include both 'jwt' and 'bearer'.
	var rc int
	var vs float64
	var tags string
	if err := db.QueryRow(`SELECT retrieval_count, validation_score, topic_tags FROM FleetMemory WHERE id = 1`).
		Scan(&rc, &vs, &tags); err != nil {
		t.Fatalf("read canonical: %v", err)
	}
	if rc != 6 {
		t.Errorf("expected canonical retrieval_count=6, got %d", rc)
	}
	if vs < 0.29 || vs > 0.31 {
		t.Errorf("expected canonical validation_score~0.30, got %.4f", vs)
	}
	if !strings.Contains(tags, "jwt") || !strings.Contains(tags, "bearer") {
		t.Errorf("expected tags to include jwt+bearer, got %q", tags)
	}

	// Audit row landed.
	var auditCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM AuditLog WHERE action = 'librarian-dedup-merge' AND task_id = 2`).
		Scan(&auditCount); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if auditCount != 1 {
		t.Errorf("expected 1 audit row, got %d", auditCount)
	}
}

// TestDedupAndMerge_Idempotent ensures a second invocation against an
// already-merged DB is a no-op.
func TestDedupAndMerge_Idempotent(t *testing.T) {
	db := mustOpenDedupTestDB(t)
	defer db.Close()

	StoreFleetMemory(db, "repoA", 1, "success",
		"Force orchestrator daemon shutdown cancels context before drain loop sweep.",
		"cmd/force/daemon.go", "shutdown, context")
	StoreFleetMemory(db, "repoA", 2, "success",
		"Force orchestrator daemon shutdown cancels context before drain loop sweep!",
		"cmd/force/daemon.go", "shutdown, context")

	first, err := DedupAndMerge(context.Background(), db)
	if err != nil {
		t.Fatalf("first DedupAndMerge: %v", err)
	}
	if first != 1 {
		t.Fatalf("expected 1 merge on first run, got %d", first)
	}
	second, err := DedupAndMerge(context.Background(), db)
	if err != nil {
		t.Fatalf("second DedupAndMerge: %v", err)
	}
	if second != 0 {
		t.Errorf("expected 0 merges on second run (idempotence), got %d", second)
	}
}

// TestDedupAndMerge_DistinctRowsNotMerged guarantees rows that share a
// topic but carry distinct lessons stay separate.
func TestDedupAndMerge_DistinctRowsNotMerged(t *testing.T) {
	db := mustOpenDedupTestDB(t)
	defer db.Close()

	StoreFleetMemory(db, "repoA", 1, "success",
		"Authentication middleware validates JWT signatures using ES256 elliptic-curve keys.",
		"auth.go", "auth")
	StoreFleetMemory(db, "repoA", 2, "success",
		"Authentication middleware caches user permissions in Redis for 5 minutes per session.",
		"auth.go", "auth")

	merged, err := DedupAndMerge(context.Background(), db)
	if err != nil {
		t.Fatalf("DedupAndMerge: %v", err)
	}
	if merged != 0 {
		t.Errorf("expected 0 merges (distinct lessons), got %d", merged)
	}
}

// TestDedupAndMerge_CrossRepoNotMerged proves a memory in repo A and a
// near-identical one in repo B are NOT merged — repos are separately
// scoped curatorial spaces.
func TestDedupAndMerge_CrossRepoNotMerged(t *testing.T) {
	db := mustOpenDedupTestDB(t)
	defer db.Close()

	StoreFleetMemory(db, "repoA", 1, "success",
		"Database connection pooling caps at 25 simultaneous connections per process.",
		"db.go", "db, pooling")
	StoreFleetMemory(db, "repoB", 2, "success",
		"Database connection pooling caps at 25 simultaneous connections per process.",
		"db.go", "db, pooling")

	merged, err := DedupAndMerge(context.Background(), db)
	if err != nil {
		t.Fatalf("DedupAndMerge: %v", err)
	}
	if merged != 0 {
		t.Errorf("expected 0 merges (cross-repo), got %d", merged)
	}
}

// TestShinglesOf sanity-checks the trigram extractor against fixture
// strings that exercise lowercase / punctuation / whitespace.
func TestShinglesOf(t *testing.T) {
	got := shinglesOf("The quick brown fox jumps")
	want := []string{"the quick brown", "quick brown fox", "brown fox jumps"}
	for _, w := range want {
		if _, ok := got[w]; !ok {
			t.Errorf("missing trigram %q in %v", w, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("expected %d trigrams, got %d", len(want), len(got))
	}
}

func TestJaccardSimilarity(t *testing.T) {
	a := map[string]struct{}{"x": {}, "y": {}, "z": {}}
	b := map[string]struct{}{"y": {}, "z": {}, "w": {}}
	got := jaccardSimilarity(a, b)
	want := 2.0 / 4.0 // |∩|=2 (y,z), |∪|=4 (x,y,z,w)
	if got != want {
		t.Errorf("expected %f, got %f", want, got)
	}
}
