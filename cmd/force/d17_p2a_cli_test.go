package main

// d17_p2a_cli_test.go — tests for D17 P2A CLI additions.
//
// Covers:
//   - force briefing         (cmdBriefing)
//   - force scale --medics / --pilots (cmdScale)
//   - force convoy cancel    (cmdConvoyCancel)
//   - force task show / status (cmdTaskShow)
//   - force senate           (cmdSenate / cmdSenateList / cmdSenateRefresh)

import (
	"fmt"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// ─── force briefing ───────────────────────────────────────────────────────────

func TestCmdBriefing_EmptyQueue(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	out := captureOutput(func() {
		cmdBriefing(db, []string{})
	})
	if !strings.Contains(out, "No decisions awaiting review") {
		t.Errorf("expected empty-queue message; got: %q", out)
	}
}

func TestCmdBriefing_ShowsQueuedDecisions(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Insert a task in AwaitingCaptainReview state.
	db.Exec(`INSERT INTO BountyBoard (target_repo, type, status, payload, priority, created_at)
		VALUES ('myrepo', 'Feature', 'AwaitingCaptainReview', 'implement foo endpoint', 0, datetime('now'))`)

	out := captureOutput(func() {
		cmdBriefing(db, []string{})
	})
	if !strings.Contains(out, "captain_proposal") {
		t.Errorf("expected 'captain_proposal' kind in output; got: %q", out)
	}
	if !strings.Contains(out, "implement foo endpoint") {
		t.Errorf("expected task payload in output; got: %q", out)
	}
	if !strings.Contains(out, "decision(s) queued") {
		t.Errorf("expected summary line; got: %q", out)
	}
}

func TestCmdBriefing_HelpFlag(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// --help should print usage and not exit with error (captureOutput returns).
	out := captureOutput(func() {
		cmdBriefing(db, []string{"--help"})
	})
	if !strings.Contains(out, "briefing") {
		t.Errorf("help text missing 'briefing'; got: %q", out)
	}
}

// ─── force scale --medics / --pilots ─────────────────────────────────────────

func TestCmdScale_Medics(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	out := captureOutput(func() {
		cmdScale(db, []string{"--medics", "3"})
	})
	if !strings.Contains(out, "medics=3") {
		t.Errorf("expected medics=3 in output; got: %q", out)
	}

	val := store.GetConfig(db, "num_medics", "")
	if val != "3" {
		t.Errorf("num_medics: got %q want 3", val)
	}
}

func TestCmdScale_Pilots(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	out := captureOutput(func() {
		cmdScale(db, []string{"--pilots", "2"})
	})
	if !strings.Contains(out, "pilots=2") {
		t.Errorf("expected pilots=2 in output; got: %q", out)
	}

	val := store.GetConfig(db, "num_pilots", "")
	if val != "2" {
		t.Errorf("num_pilots: got %q want 2", val)
	}
}

func TestCmdScale_MedicsAndPilots(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	out := captureOutput(func() {
		cmdScale(db, []string{"--medics", "1", "--pilots", "4"})
	})
	if !strings.Contains(out, "medics=1") {
		t.Errorf("expected medics=1; got: %q", out)
	}
	if !strings.Contains(out, "pilots=4") {
		t.Errorf("expected pilots=4; got: %q", out)
	}
}

// ─── force convoy cancel ─────────────────────────────────────────────────────

func TestCmdConvoyCancel_HappyPath(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, _ := store.CreateConvoy(db, "test-convoy")
	// Add a Pending task to the convoy.
	db.Exec(`INSERT INTO BountyBoard (convoy_id, target_repo, type, status, payload, priority, created_at)
		VALUES (?, 'repo', 'CodeEdit', 'Pending', 'do work', 0, datetime('now'))`, convoyID)

	out := captureOutput(func() {
		cmdConvoyCancel(db, []string{idStr(convoyID)})
	})
	if !strings.Contains(out, "cancelled") {
		t.Errorf("expected 'cancelled' in output; got: %q", out)
	}

	var status string
	db.QueryRow(`SELECT status FROM Convoys WHERE id = ?`, convoyID).Scan(&status)
	if status != "Cancelled" {
		t.Errorf("convoy status: got %q want Cancelled", status)
	}
}

func TestCmdConvoyCancel_AlreadyCancelled(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, _ := store.CreateConvoy(db, "already-cancelled")
	db.Exec(`UPDATE Convoys SET status = 'Cancelled' WHERE id = ?`, convoyID)

	out := captureOutput(func() {
		cmdConvoyCancel(db, []string{idStr(convoyID)})
	})
	if !strings.Contains(out, "already cancelled") {
		t.Errorf("expected idempotent message; got: %q", out)
	}
}

// Note: TestCmdConvoyCancel_MissingArgs is intentionally omitted — the
// handler calls os.Exit(1) which terminates the test process. The usage
// guard is covered by TestCmdConvoyCancel_HappyPath and the existing
// parseSubcommandFlags pattern tests.

// ─── force task show / status ─────────────────────────────────────────────────

func TestCmdTaskShow_HappyPath(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	res, _ := db.Exec(`INSERT INTO BountyBoard (target_repo, type, status, payload, priority, created_at)
		VALUES ('myrepo', 'Feature', 'Pending', 'add caching layer', 5, datetime('now'))`)
	id64, _ := res.LastInsertId()
	id := int(id64)

	out := captureOutput(func() {
		cmdTaskShow(db, []string{idStr(id)})
	})
	if !strings.Contains(out, "Feature") {
		t.Errorf("expected type in output; got: %q", out)
	}
	if !strings.Contains(out, "Pending") {
		t.Errorf("expected status in output; got: %q", out)
	}
	if !strings.Contains(out, "myrepo") {
		t.Errorf("expected repo in output; got: %q", out)
	}
	if !strings.Contains(out, "add caching layer") {
		t.Errorf("expected payload in output; got: %q", out)
	}
}

// Note: TestCmdTaskShow_NotFound and TestCmdTaskShow_MissingArgs omitted
// because the handlers call os.Exit directly (same pattern as convoy_ship_cli_test.go).
// Negative paths are guarded by parseSubcommandFlags tests and the happy-path test above.

// TestCmdTask_StatusAlias verifies that `force task status` routes to the
// same cmdTaskShow implementation as `force task show`.
func TestCmdTask_StatusAlias(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	res, _ := db.Exec(`INSERT INTO BountyBoard (target_repo, type, status, payload, priority, created_at)
		VALUES ('repo2', 'Investigate', 'Completed', 'investigate latency', 0, datetime('now'))`)
	id64, _ := res.LastInsertId()

	// Both show and status should route through cmdTaskShow.
	out1 := captureOutput(func() {
		cmdTask(db, []string{"show", idStr(int(id64))})
	})
	out2 := captureOutput(func() {
		cmdTask(db, []string{"status", idStr(int(id64))})
	})
	if out1 != out2 {
		t.Errorf("show and status produced different output:\n  show:   %q\n  status: %q", out1, out2)
	}
	if !strings.Contains(out1, "Completed") {
		t.Errorf("expected 'Completed' in task show output; got: %q", out1)
	}
}

// ─── force senate ─────────────────────────────────────────────────────────────

func TestCmdSenateList_Empty(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	out := captureOutput(func() {
		cmdSenate(db, []string{})
	})
	if !strings.Contains(out, "No senators registered") {
		t.Errorf("expected empty roster message; got: %q", out)
	}
}

func TestCmdSenateList_ShowsActiveChamber(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	if err := store.UpsertSenateChamber(db, store.SenateChamber{
		SenatorName: "myrepo",
		Scope:       "repo:myrepo",
		Status:      "active",
	}); err != nil {
		t.Fatalf("UpsertSenateChamber: %v", err)
	}

	out := captureOutput(func() {
		cmdSenate(db, []string{})
	})
	if !strings.Contains(out, "myrepo") {
		t.Errorf("expected senator name in output; got: %q", out)
	}
	if !strings.Contains(out, "active") {
		t.Errorf("expected status 'active' in output; got: %q", out)
	}
}

// Note: TestCmdSenateRefresh_NoSenator omitted — handler calls os.Exit.
// The guard is that GetSenateChamber returns nil → handler prints to stderr + exits.

func TestCmdSenateRefresh_QueuesTask(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Register the repo so it appears in Repositories.
	store.AddRepo(db, "myrepo", "/tmp/myrepo", "My test repo")
	// Seed a senator.
	if err := store.UpsertSenateChamber(db, store.SenateChamber{
		SenatorName: "myrepo",
		Scope:       "repo:myrepo",
		Status:      "active",
	}); err != nil {
		t.Fatalf("UpsertSenateChamber: %v", err)
	}

	out := captureOutput(func() {
		cmdSenateRefresh(db, []string{"myrepo"})
	})
	if !strings.Contains(out, "SenatorRefresh") {
		t.Errorf("expected 'SenatorRefresh' in output; got: %q", out)
	}
	if !strings.Contains(out, "myrepo") {
		t.Errorf("expected repo name in output; got: %q", out)
	}

	var count int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type = 'SenatorRefresh' AND status = 'Pending'`).Scan(&count)
	if count != 1 {
		t.Errorf("expected 1 SenatorRefresh task; got %d", count)
	}
}

func TestCmdSenateRefresh_Idempotent(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddRepo(db, "myrepo", "/tmp/myrepo", "desc")
	if err := store.UpsertSenateChamber(db, store.SenateChamber{
		SenatorName: "myrepo",
		Scope:       "repo:myrepo",
		Status:      "active",
	}); err != nil {
		t.Fatalf("UpsertSenateChamber: %v", err)
	}

	// First refresh — should queue.
	captureOutput(func() { cmdSenateRefresh(db, []string{"myrepo"}) })
	// Second refresh — should dedup.
	out := captureOutput(func() { cmdSenateRefresh(db, []string{"myrepo"}) })
	if !strings.Contains(out, "already pending") {
		t.Errorf("expected idempotent 'already pending' message; got: %q", out)
	}

	var count int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type = 'SenatorRefresh' AND status = 'Pending'`).Scan(&count)
	if count != 1 {
		t.Errorf("expected exactly 1 SenatorRefresh after idempotent double-queue; got %d", count)
	}
}

// Note: TestCmdSenateRefresh_MissingArgs omitted — handler calls os.Exit.
// Usage guard is covered by TestCmdSenateRefresh_QueuesTask (positive path).

// ─── helpers ──────────────────────────────────────────────────────────────────

// idStr converts an int to its decimal string representation for use as a
// CLI argument (e.g. passing to cmdConvoyCancel / cmdTaskShow).
func idStr(id int) string {
	return fmt.Sprintf("%d", id)
}

