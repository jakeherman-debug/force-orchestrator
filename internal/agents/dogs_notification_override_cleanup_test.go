package agents

import (
	"io"
	"log"
	"testing"

	"force-orchestrator/internal/store"
)

// TestDogNotificationOverrideCleanup_HappyPath seeds three overrides and
// asserts the dog deletes only the row whose closure stamp is older than
// the 7-day retention window.
func TestDogNotificationOverrideCleanup_HappyPath(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Convoy 1: closed 8d ago — should be deleted.
	if _, err := db.Exec(`INSERT INTO ConvoyNotificationOverrides
		(convoy_id, mode, custom_json, set_at, set_by, reason, convoy_closed_at)
		VALUES (1, 'verbose', '{}', datetime('now', '-10 days'), 'op', '', datetime('now', '-8 days'))`); err != nil {
		t.Fatalf("seed convoy=1: %v", err)
	}
	// Convoy 2: closed 1d ago — should be preserved (within 7d retention).
	if _, err := db.Exec(`INSERT INTO ConvoyNotificationOverrides
		(convoy_id, mode, custom_json, set_at, set_by, reason, convoy_closed_at)
		VALUES (2, 'quiet', '{}', datetime('now', '-3 days'), 'op', '', datetime('now', '-1 day'))`); err != nil {
		t.Fatalf("seed convoy=2: %v", err)
	}
	// Convoy 3: still open (NULL closure stamp) — should be preserved.
	if _, err := db.Exec(`INSERT INTO ConvoyNotificationOverrides
		(convoy_id, mode, custom_json, set_at, set_by, reason)
		VALUES (3, 'verbose', '{}', datetime('now'), 'op', '')`); err != nil {
		t.Fatalf("seed convoy=3: %v", err)
	}

	logger := log.New(io.Discard, "", 0)
	if err := dogNotificationOverrideCleanup(db, logger); err != nil {
		t.Fatalf("dogNotificationOverrideCleanup: %v", err)
	}

	// Verify exactly the 8d-ago row was deleted; the 1d-ago and open rows survive.
	var count1, count2, count3 int
	db.QueryRow(`SELECT COUNT(*) FROM ConvoyNotificationOverrides WHERE convoy_id = 1`).Scan(&count1)
	db.QueryRow(`SELECT COUNT(*) FROM ConvoyNotificationOverrides WHERE convoy_id = 2`).Scan(&count2)
	db.QueryRow(`SELECT COUNT(*) FROM ConvoyNotificationOverrides WHERE convoy_id = 3`).Scan(&count3)
	if count1 != 0 {
		t.Errorf("expected convoy=1 (closed 8d ago) deleted, got count=%d", count1)
	}
	if count2 != 1 {
		t.Errorf("expected convoy=2 (closed 1d ago) preserved, got count=%d", count2)
	}
	if count3 != 1 {
		t.Errorf("expected convoy=3 (still open / NULL stamp) preserved, got count=%d", count3)
	}
}

// TestDogNotificationOverrideCleanup_RetentionBoundary asserts the
// inclusive boundary at exactly 7 days. A row stamped 6d23h59m ago
// stays; a row stamped 7d1m ago goes.
func TestDogNotificationOverrideCleanup_RetentionBoundary(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Just inside retention (6d 23h 59m ago) — preserved.
	if _, err := db.Exec(`INSERT INTO ConvoyNotificationOverrides
		(convoy_id, mode, custom_json, set_at, set_by, reason, convoy_closed_at)
		VALUES (10, 'verbose', '{}', datetime('now', '-10 days'), 'op', '',
		        datetime('now', '-6 days', '-23 hours', '-59 minutes'))`); err != nil {
		t.Fatalf("seed inside-boundary: %v", err)
	}
	// Just past retention (7d 1m ago) — deleted.
	if _, err := db.Exec(`INSERT INTO ConvoyNotificationOverrides
		(convoy_id, mode, custom_json, set_at, set_by, reason, convoy_closed_at)
		VALUES (11, 'quiet', '{}', datetime('now', '-10 days'), 'op', '',
		        datetime('now', '-7 days', '-1 minute'))`); err != nil {
		t.Fatalf("seed past-boundary: %v", err)
	}

	logger := log.New(io.Discard, "", 0)
	if err := dogNotificationOverrideCleanup(db, logger); err != nil {
		t.Fatalf("dogNotificationOverrideCleanup: %v", err)
	}

	var inside, past int
	db.QueryRow(`SELECT COUNT(*) FROM ConvoyNotificationOverrides WHERE convoy_id = 10`).Scan(&inside)
	db.QueryRow(`SELECT COUNT(*) FROM ConvoyNotificationOverrides WHERE convoy_id = 11`).Scan(&past)
	if inside != 1 {
		t.Errorf("expected 6d23h59m-ago row preserved, got count=%d", inside)
	}
	if past != 0 {
		t.Errorf("expected 7d1m-ago row deleted, got count=%d", past)
	}
}

// TestDogNotificationOverrideCleanup_NoOverridesNoOp confirms the dog
// returns nil when the table is empty (cold-start fleet, or a fleet
// where no convoy ever set an override).
func TestDogNotificationOverrideCleanup_NoOverridesNoOp(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	logger := log.New(io.Discard, "", 0)
	if err := dogNotificationOverrideCleanup(db, logger); err != nil {
		t.Fatalf("dogNotificationOverrideCleanup on empty table: %v", err)
	}

	// And running it again is the same no-op.
	if err := dogNotificationOverrideCleanup(db, logger); err != nil {
		t.Fatalf("dogNotificationOverrideCleanup second pass: %v", err)
	}
}

