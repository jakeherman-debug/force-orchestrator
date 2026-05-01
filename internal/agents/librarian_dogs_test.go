package agents

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"testing"

	"force-orchestrator/internal/clients/librarian"
	"force-orchestrator/internal/store"
)

// librarianDogTestLogger satisfies the dog-handler logger interface.
type librarianDogTestLogger struct{ t *testing.T }

func (l *librarianDogTestLogger) Printf(f string, args ...any) {
	l.t.Helper()
	l.t.Logf(f, args...)
}

// TestDogLibrarianDedup_RoundTrip seeds two near-identical memories,
// runs the dog, and asserts the merge happened.
func TestDogLibrarianDedup_RoundTrip(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.StoreFleetMemory(db, "repoA", 1, "success",
		"Authentication middleware uses JWT bearer tokens; rotate signing keys quarterly.",
		"auth.go,middleware.go", "auth")
	store.StoreFleetMemory(db, "repoA", 2, "success",
		"Authentication middleware uses JWT bearer tokens. Rotate signing keys quarterly.",
		"auth.go,middleware.go", "auth")

	if err := dogLibrarianDedup(context.Background(), db, log.Default()); err != nil {
		t.Fatalf("dogLibrarianDedup: %v", err)
	}
	var canonID int
	db.QueryRow(`SELECT canonical_id FROM FleetMemory WHERE id = 2`).Scan(&canonID)
	if canonID != 1 {
		t.Errorf("expected canonical_id=1 on dup row, got %d", canonID)
	}
}

// TestDogLibrarianQualityRecompute decays a backdated row's freshness.
func TestDogLibrarianQualityRecompute(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.StoreFleetMemory(db, "repoA", 1, "success", "Memory.", "x.go", "x")
	if _, err := db.Exec(`UPDATE FleetMemory SET created_at = datetime('now', '-90 days') WHERE id = 1`); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	if err := dogLibrarianQualityRecompute(context.Background(), db, log.Default()); err != nil {
		t.Fatalf("dog: %v", err)
	}
	var s float64
	db.QueryRow(`SELECT freshness_score FROM FleetMemory WHERE id = 1`).Scan(&s)
	if s > 0.2 {
		t.Errorf("expected freshness < 0.2 after 90d age, got %.4f", s)
	}
}

// TestDogLibrarianConflictWatch fires the dog with a seeded
// contradiction pair and asserts the ticket lands.
func TestDogLibrarianConflictWatch(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.StoreFleetMemory(db, "repoA", 1, "success",
		"Mailer always retries transient errors twice.",
		"mailer.go", "")
	store.StoreFleetMemory(db, "repoA", 2, "success",
		"Mailer never retries transient errors automatically.",
		"mailer.go", "")
	if err := dogLibrarianConflictWatch(context.Background(), db, log.Default()); err != nil {
		t.Fatalf("dog: %v", err)
	}
	tickets, err := store.ListOpenConflictTickets(context.Background(), db, 10)
	if err != nil {
		t.Fatalf("ListOpenConflictTickets: %v", err)
	}
	if len(tickets) != 1 {
		t.Errorf("expected 1 ticket, got %d", len(tickets))
	}
}

// TestDogLibrarianHypothesisEmit emits a candidate from a high-signal
// memory.
func TestDogLibrarianHypothesisEmit(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.StoreFleetMemory(db, "repoA", 1, "success", "Memory.", "x.go", "x")
	if _, err := db.Exec(`UPDATE FleetMemory SET retrieval_count = 10, validation_score = 0.5 WHERE id = 1`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := dogLibrarianHypothesisEmit(context.Background(), db, log.Default()); err != nil {
		t.Fatalf("dog: %v", err)
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM PromotionProposals WHERE kind='candidate' AND source_memory_id = 1`).Scan(&n)
	if n != 1 {
		t.Errorf("expected 1 candidate, got %d", n)
	}
}

// TestDogClaudeMDDriftWatch_EmitsForUnrepresentedInvariant places a
// fixture CLAUDE.md in a temp dir, points the dog at it (via Chdir),
// and asserts a candidate PromotionProposal is emitted via the
// Librarian Client.
func TestDogClaudeMDDriftWatch_EmitsForUnrepresentedInvariant(t *testing.T) {
	tmp := t.TempDir()
	cmd := filepath.Join(tmp, "CLAUDE.md")
	body := `# Test CLAUDE.md fixture

## Drift-worthy invariant

This invariant says that every Foo MUST validate Bar before issuing the request.

## Already-represented invariant

This one MUST never appear.
`
	if err := os.WriteFile(cmd, []byte(body), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	// Chdir so findClaudeMDPath finds our fixture.
	old, _ := os.Getwd()
	defer os.Chdir(old) //nolint:errcheck
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Seed an existing FleetRules row that matches the second section's
	// title so it gets skipped by sectionAlreadyRepresented.
	if _, err := db.Exec(`INSERT INTO FleetRules
		(rule_key, category, agent_scope, render_to, enforced_by, content,
		 content_hash, version, active_from, created_by)
		VALUES ('already-represented-invariant', 'general', 'all', 'docs', 'trust-only',
		        'X', 'h', 1, datetime('now'), 'test')`); err != nil {
		t.Fatalf("seed FleetRules: %v", err)
	}

	mock := librarian.NewMock()
	logger := &librarianDogTestLogger{t: t}
	if err := dogClaudeMDDriftWatch(context.Background(), db, mock, logger); err != nil {
		t.Fatalf("dog: %v", err)
	}
	if len(mock.EmitCalls) < 1 {
		t.Fatalf("expected ≥1 EmitCandidate call, got %d", len(mock.EmitCalls))
	}
	// The first section ("Drift-worthy invariant") should have triggered.
	found := false
	for _, c := range mock.EmitCalls {
		if c.HypothesisKey == "claude-md-drift-drift-worthy-invariant" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected drift candidate for unrepresented invariant; got calls: %+v", mock.EmitCalls)
	}
}

// TestDogClaudeMDDriftWatch_NoCLAUDEMD is a no-op when CLAUDE.md is
// not findable — exits without erroring.
func TestDogClaudeMDDriftWatch_NoCLAUDEMD(t *testing.T) {
	tmp := t.TempDir()
	old, _ := os.Getwd()
	defer os.Chdir(old) //nolint:errcheck
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	mock := librarian.NewMock()
	logger := &librarianDogTestLogger{t: t}
	if err := dogClaudeMDDriftWatch(context.Background(), db, mock, logger); err != nil {
		t.Fatalf("dog should not error on missing CLAUDE.md: %v", err)
	}
	if len(mock.EmitCalls) != 0 {
		t.Errorf("expected 0 emit calls, got %d", len(mock.EmitCalls))
	}
}

// TestDogClaudeMDDriftWatch_Idempotent runs the dog twice — second
// run should not emit duplicate candidates because the existing
// pending candidate is detected.
func TestDogClaudeMDDriftWatch_Idempotent(t *testing.T) {
	tmp := t.TempDir()
	cmd := filepath.Join(tmp, "CLAUDE.md")
	body := `# Test CLAUDE.md fixture

## Single drift section

This invariant MUST be enforced everywhere.
`
	if err := os.WriteFile(cmd, []byte(body), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	old, _ := os.Getwd()
	defer os.Chdir(old) //nolint:errcheck
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Use the in-process Client so writes hit PromotionProposals,
	// allowing the second-run idempotence query to find the row.
	c := librarian.NewInProcess(db)
	logger := &librarianDogTestLogger{t: t}

	if err := dogClaudeMDDriftWatch(context.Background(), db, c, logger); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := dogClaudeMDDriftWatch(context.Background(), db, c, logger); err != nil {
		t.Fatalf("second: %v", err)
	}

	var count int
	db.QueryRow(`SELECT COUNT(*) FROM PromotionProposals WHERE rule_key = 'claude-md-drift-single-drift-section'`).
		Scan(&count)
	if count != 1 {
		t.Errorf("expected 1 candidate after two runs, got %d", count)
	}
}
