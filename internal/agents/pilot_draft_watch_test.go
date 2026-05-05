package agents

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"force-orchestrator/internal/clients/librarian"
	"force-orchestrator/internal/store"
)

// installDraftPRViewStub swaps draftPRViewFn and restores on cleanup.
func installDraftPRViewStub(t *testing.T, responses map[int]struct {
	State  string
	Merged bool
	Err    error
}) {
	t.Helper()
	prev := draftPRViewFn
	draftPRViewFn = func(cwd, repo string, number int) (string, bool, error) {
		if r, ok := responses[number]; ok {
			return r.State, r.Merged, r.Err
		}
		return "", false, fmt.Errorf("no stub for PR #%d", number)
	}
	t.Cleanup(func() { draftPRViewFn = prev })
}

// ── dogDraftPRWatch ─────────────────────────────────────────────────────────

func TestDogDraftPRWatch_NoOpWhenNoDraftPROpenConvoys(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	// No DraftPROpen convoys.
	if err := dogDraftPRWatch(context.Background(), db, librarian.NewInProcess(db), testLogger{}); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestDogDraftPRWatch_MergedPR_TransitionsToShipped(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "api", "/tmp/api", "")
	cid, _ := store.CreateConvoy(db, "[1] t")
	_ = store.SetConvoyStatus(db, cid, "DraftPROpen")
	_ = store.UpsertConvoyAskBranch(db, cid, "api", "force/ask-1-t", "sha")
	_ = store.SetConvoyAskBranchDraftPR(db, cid, "api", "u", 42, "Open")

	installDraftPRViewStub(t, map[int]struct {
		State  string
		Merged bool
		Err    error
	}{42: {State: "MERGED", Merged: true}})

	if err := dogDraftPRWatch(context.Background(), db, librarian.NewInProcess(db), testLogger{}); err != nil {
		t.Fatal(err)
	}
	conv := store.GetConvoy(db, cid)
	if conv.Status != "Shipped" {
		t.Errorf("convoy should be Shipped, got %q", conv.Status)
	}
	// CleanupAskBranch must be queued.
	var cleanupCount int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type = 'CleanupAskBranch' AND status = 'Pending'`).Scan(&cleanupCount)
	if cleanupCount != 1 {
		t.Errorf("expected 1 CleanupAskBranch queued, got %d", cleanupCount)
	}
	// WriteMemory task queued.
	var memCount int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type = 'WriteMemory'`).Scan(&memCount)
	if memCount != 1 {
		t.Errorf("expected 1 WriteMemory queued, got %d", memCount)
	}
	// ConvoyAskBranch state updated + shipped_at stamped.
	ab := store.GetConvoyAskBranch(db, cid, "api")
	if ab.DraftPRState != "Merged" {
		t.Errorf("draft PR state should be Merged, got %q", ab.DraftPRState)
	}
	if ab.ShippedAt == "" {
		t.Errorf("shipped_at must be stamped when PR merges")
	}
}

func TestDogDraftPRWatch_ClosedUnmerged_TransitionsToAbandoned(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "api", "/tmp/api", "")
	cid, _ := store.CreateConvoy(db, "[1] t")
	_ = store.SetConvoyStatus(db, cid, "DraftPROpen")
	_ = store.UpsertConvoyAskBranch(db, cid, "api", "force/ask-1-t", "sha")
	_ = store.SetConvoyAskBranchDraftPR(db, cid, "api", "u", 42, "Open")

	installDraftPRViewStub(t, map[int]struct {
		State  string
		Merged bool
		Err    error
	}{42: {State: "CLOSED", Merged: false}})

	if err := dogDraftPRWatch(context.Background(), db, librarian.NewInProcess(db), testLogger{}); err != nil {
		t.Fatal(err)
	}
	conv := store.GetConvoy(db, cid)
	if conv.Status != "Abandoned" {
		t.Errorf("convoy should be Abandoned, got %q", conv.Status)
	}
	// The ConvoyAskBranch row's state must also persist — otherwise a
	// subsequent draft-pr-watch tick sees the PR as Open and loops.
	ab := store.GetConvoyAskBranch(db, cid, "api")
	if ab.DraftPRState != "Closed" {
		t.Errorf("draft PR state must be persisted as Closed on the row, got %q", ab.DraftPRState)
	}
	var cleanupCount int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type = 'CleanupAskBranch' AND status = 'Pending'`).Scan(&cleanupCount)
	if cleanupCount != 1 {
		t.Errorf("expected cleanup queued on abandon, got %d", cleanupCount)
	}
}

func TestDogDraftPRWatch_StillOpenNoTransition(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "api", "/tmp/api", "")
	cid, _ := store.CreateConvoy(db, "[1] t")
	_ = store.SetConvoyStatus(db, cid, "DraftPROpen")
	_ = store.UpsertConvoyAskBranch(db, cid, "api", "force/ask-1-t", "sha")
	_ = store.SetConvoyAskBranchDraftPR(db, cid, "api", "u", 42, "Open")

	installDraftPRViewStub(t, map[int]struct {
		State  string
		Merged bool
		Err    error
	}{42: {State: "OPEN", Merged: false}})

	_ = dogDraftPRWatch(context.Background(), db, librarian.NewInProcess(db), testLogger{})
	conv := store.GetConvoy(db, cid)
	if conv.Status != "DraftPROpen" {
		t.Errorf("convoy should stay DraftPROpen, got %q", conv.Status)
	}
}

func TestDogDraftPRWatch_MultiRepoPartialMergeStaysDraft(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "api", "/tmp/api", "")
	store.AddRepo(db, "monolith", "/tmp/monolith", "")
	cid, _ := store.CreateConvoy(db, "[1] multi")
	_ = store.SetConvoyStatus(db, cid, "DraftPROpen")
	_ = store.UpsertConvoyAskBranch(db, cid, "api", "force/ask-1-multi", "sha-a")
	_ = store.UpsertConvoyAskBranch(db, cid, "monolith", "force/ask-1-multi", "sha-b")
	_ = store.SetConvoyAskBranchDraftPR(db, cid, "api", "u1", 1, "Open")
	_ = store.SetConvoyAskBranchDraftPR(db, cid, "monolith", "u2", 2, "Open")

	installDraftPRViewStub(t, map[int]struct {
		State  string
		Merged bool
		Err    error
	}{
		1: {State: "MERGED", Merged: true},
		2: {State: "OPEN", Merged: false},
	})

	_ = dogDraftPRWatch(context.Background(), db, librarian.NewInProcess(db), testLogger{})
	conv := store.GetConvoy(db, cid)
	if conv.Status != "DraftPROpen" {
		t.Errorf("partial merge should stay DraftPROpen, got %q", conv.Status)
	}
}

func TestDogDraftPRWatch_ClosedWithOpenStaysOpen(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "api", "/tmp/api", "")
	store.AddRepo(db, "monolith", "/tmp/monolith", "")
	cid, _ := store.CreateConvoy(db, "[1] mix")
	_ = store.SetConvoyStatus(db, cid, "DraftPROpen")
	_ = store.UpsertConvoyAskBranch(db, cid, "api", "force/ask-1-mix", "sha-a")
	_ = store.UpsertConvoyAskBranch(db, cid, "monolith", "force/ask-1-mix", "sha-b")
	_ = store.SetConvoyAskBranchDraftPR(db, cid, "api", "u1", 1, "Open")
	_ = store.SetConvoyAskBranchDraftPR(db, cid, "monolith", "u2", 2, "Open")

	installDraftPRViewStub(t, map[int]struct {
		State  string
		Merged bool
		Err    error
	}{
		1: {State: "CLOSED", Merged: false},
		2: {State: "OPEN", Merged: false},
	})

	_ = dogDraftPRWatch(context.Background(), db, librarian.NewInProcess(db), testLogger{})
	conv := store.GetConvoy(db, cid)
	if conv.Status == "Abandoned" {
		t.Errorf("convoy with one closed but one still open should stay DraftPROpen, got Abandoned")
	}
}

func TestDogDraftPRWatch_TransientViewErrorDoesNotTransition(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "api", "/tmp/api", "")
	cid, _ := store.CreateConvoy(db, "[1] t")
	_ = store.SetConvoyStatus(db, cid, "DraftPROpen")
	_ = store.UpsertConvoyAskBranch(db, cid, "api", "force/ask-1-t", "sha")
	_ = store.SetConvoyAskBranchDraftPR(db, cid, "api", "u", 42, "Open")

	installDraftPRViewStub(t, map[int]struct {
		State  string
		Merged bool
		Err    error
	}{42: {Err: fmt.Errorf("transient network")}})

	_ = dogDraftPRWatch(context.Background(), db, librarian.NewInProcess(db), testLogger{})
	conv := store.GetConvoy(db, cid)
	if conv.Status != "DraftPROpen" {
		t.Errorf("view errors should not advance state, got %q", conv.Status)
	}
}

// ── dogShipItNag ─────────────────────────────────────────────────────────────

func TestDogShipItNag_NoNagBeforeThreshold(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	cid, _ := store.CreateConvoy(db, "[1] young")
	_ = store.SetConvoyStatus(db, cid, "DraftPROpen")
	_ = store.UpsertConvoyAskBranch(db, cid, "api", "force/ask-1-young", "sha")
	_ = store.SetConvoyAskBranchDraftPR(db, cid, "api", "u", 42, "Open")

	_ = dogShipItNag(db, testLogger{})

	var mailCount int
	db.QueryRow(`SELECT COUNT(*) FROM Fleet_Mail WHERE subject LIKE 'SHIP IT%' OR subject LIKE '%SHIP IT%'`).Scan(&mailCount)
	if mailCount != 0 {
		t.Errorf("no nag should fire for a young convoy, got %d", mailCount)
	}
}

func TestDogShipItNag_Sends24hNagAndDeduplicates(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	cid, _ := store.CreateConvoy(db, "[1] aged")
	_ = store.SetConvoyStatus(db, cid, "DraftPROpen")
	_ = store.UpsertConvoyAskBranch(db, cid, "api", "force/ask-1-aged", "sha")
	_ = store.SetConvoyAskBranchDraftPR(db, cid, "api", "u", 42, "Open")

	// Backdate the ConvoyAskBranch created_at by 25h so the 24h threshold fires.
	oldTime := time.Now().Add(-25 * time.Hour).UTC().Format("2006-01-02 15:04:05")
	db.Exec(`UPDATE ConvoyAskBranches SET created_at = ? WHERE convoy_id = ?`, oldTime, cid)

	_ = dogShipItNag(db, testLogger{})
	var mail1 int
	db.QueryRow(`SELECT COUNT(*) FROM Fleet_Mail WHERE subject LIKE '%SHIP IT%'`).Scan(&mail1)
	if mail1 != 1 {
		t.Errorf("expected 1 nag at 24h, got %d", mail1)
	}

	// Second run should not dupe.
	_ = dogShipItNag(db, testLogger{})
	var mail2 int
	db.QueryRow(`SELECT COUNT(*) FROM Fleet_Mail WHERE subject LIKE '%SHIP IT%'`).Scan(&mail2)
	if mail2 != 1 {
		t.Errorf("second tick should not duplicate nag, got %d", mail2)
	}
}

func TestDogShipItNag_EscalatesThroughThresholds(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	cid, _ := store.CreateConvoy(db, "[1] very-aged")
	_ = store.SetConvoyStatus(db, cid, "DraftPROpen")
	_ = store.UpsertConvoyAskBranch(db, cid, "api", "force/ask-1-very-aged", "sha")
	_ = store.SetConvoyAskBranchDraftPR(db, cid, "api", "u", 42, "Open")

	// 25h old → 24h threshold.
	oldTime := time.Now().Add(-25 * time.Hour).UTC().Format("2006-01-02 15:04:05")
	db.Exec(`UPDATE ConvoyAskBranches SET created_at = ? WHERE convoy_id = ?`, oldTime, cid)
	_ = dogShipItNag(db, testLogger{})

	// Jump to 73h → 72h threshold. A new mail should fire.
	oldTime = time.Now().Add(-73 * time.Hour).UTC().Format("2006-01-02 15:04:05")
	db.Exec(`UPDATE ConvoyAskBranches SET created_at = ? WHERE convoy_id = ?`, oldTime, cid)
	_ = dogShipItNag(db, testLogger{})

	var mailCount int
	db.QueryRow(`SELECT COUNT(*) FROM Fleet_Mail WHERE subject LIKE '%SHIP IT%'`).Scan(&mailCount)
	if mailCount != 2 {
		t.Errorf("should have fired both 24h and 72h nags, got %d", mailCount)
	}
}

// ── transition helpers ─────────────────────────────────────────────────────

func TestTransitionConvoyToShipped_QueuesCleanupAndMemory(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	cid, _ := store.CreateConvoy(db, "[1] done")
	_ = store.UpsertConvoyAskBranch(db, cid, "api", "force/ask-1-done", "sha")

	transitionConvoyToShipped(context.Background(), db, cid, "[1] done", librarian.NewInProcess(db), testLogger{})

	conv := store.GetConvoy(db, cid)
	if conv.Status != "Shipped" {
		t.Errorf("status: %q", conv.Status)
	}
	var cleanup, memory int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type = 'CleanupAskBranch'`).Scan(&cleanup)
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type = 'WriteMemory'`).Scan(&memory)
	if cleanup != 1 {
		t.Errorf("expected 1 cleanup, got %d", cleanup)
	}
	if memory != 1 {
		t.Errorf("expected 1 memory, got %d", memory)
	}
	// Memory payload should mention "convoy-shipped".
	var mp string
	db.QueryRow(`SELECT payload FROM BountyBoard WHERE type = 'WriteMemory'`).Scan(&mp)
	if !strings.Contains(mp, "convoy-shipped") {
		t.Errorf("memory payload missing outcome tag: %q", mp)
	}
}

func TestTransitionConvoyToAbandoned_QueuesCleanupAndFailureMemory(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	cid, _ := store.CreateConvoy(db, "[1] dropped")
	_ = store.UpsertConvoyAskBranch(db, cid, "api", "force/ask-1-dropped", "sha")

	transitionConvoyToAbandoned(context.Background(), db, cid, "[1] dropped", librarian.NewInProcess(db), testLogger{})

	conv := store.GetConvoy(db, cid)
	if conv.Status != "Abandoned" {
		t.Errorf("status: %q", conv.Status)
	}
	var mp string
	db.QueryRow(`SELECT payload FROM BountyBoard WHERE type = 'WriteMemory'`).Scan(&mp)
	if !strings.Contains(mp, "convoy-abandoned") {
		t.Errorf("memory payload missing abandoned tag: %q", mp)
	}
}

// D11 Phase 2 (sub-task C). The terminal-transition path must stamp
// convoy_closed_at on the ConvoyNotificationOverrides row (if any) so
// the daily notification-override-cleanup dog can purge it after 7d.
// We seed an override row, run transitionConvoyToShipped, and assert
// the closure stamp is non-empty.
func TestTransitionConvoyToShipped_StampsOverrideClosedAt(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	cid, _ := store.CreateConvoy(db, "[1] done")
	_ = store.UpsertConvoyAskBranch(db, cid, "api", "force/ask-1-done", "sha")
	if _, err := db.Exec(`INSERT INTO ConvoyNotificationOverrides
		(convoy_id, mode, custom_json, set_at, set_by, reason)
		VALUES (?, 'verbose', '{}', datetime('now'), 'op', 'test')`, cid); err != nil {
		t.Fatalf("seed override: %v", err)
	}

	transitionConvoyToShipped(context.Background(), db, cid, "[1] done", librarian.NewInProcess(db), testLogger{})

	var stamp string
	db.QueryRow(
		`SELECT IFNULL(convoy_closed_at, '') FROM ConvoyNotificationOverrides WHERE convoy_id = ?`,
		cid,
	).Scan(&stamp)
	if stamp == "" {
		t.Errorf("expected convoy_closed_at stamped on Shipped transition, got empty")
	}
}

// Same for the Abandoned transition path.
func TestTransitionConvoyToAbandoned_StampsOverrideClosedAt(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	cid, _ := store.CreateConvoy(db, "[1] dropped")
	_ = store.UpsertConvoyAskBranch(db, cid, "api", "force/ask-1-dropped", "sha")
	if _, err := db.Exec(`INSERT INTO ConvoyNotificationOverrides
		(convoy_id, mode, custom_json, set_at, set_by, reason)
		VALUES (?, 'quiet', '{}', datetime('now'), 'op', 'test')`, cid); err != nil {
		t.Fatalf("seed override: %v", err)
	}

	transitionConvoyToAbandoned(context.Background(), db, cid, "[1] dropped", librarian.NewInProcess(db), testLogger{})

	var stamp string
	db.QueryRow(
		`SELECT IFNULL(convoy_closed_at, '') FROM ConvoyNotificationOverrides WHERE convoy_id = ?`,
		cid,
	).Scan(&stamp)
	if stamp == "" {
		t.Errorf("expected convoy_closed_at stamped on Abandoned transition, got empty")
	}
}

// Convoys without an override row must not block the terminal
// transition. The hook's UPDATE silently affects 0 rows in that case.
func TestTransitionConvoyToShipped_NoOverrideRowSilentNoOp(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	cid, _ := store.CreateConvoy(db, "[1] no-override")
	_ = store.UpsertConvoyAskBranch(db, cid, "api", "force/ask-1-no", "sha")

	transitionConvoyToShipped(context.Background(), db, cid, "[1] no-override", librarian.NewInProcess(db), testLogger{})

	conv := store.GetConvoy(db, cid)
	if conv.Status != "Shipped" {
		t.Errorf("expected Shipped, got %q", conv.Status)
	}
}
