package store

import (
	"testing"
)

// requireEvent searches events for one matching type+newValue and returns it.
func requireEvent(t *testing.T, events []ConvoyEvent, eventType, newValue string) ConvoyEvent {
	t.Helper()
	for _, e := range events {
		if e.EventType == eventType && e.NewValue == newValue {
			return e
		}
	}
	t.Errorf("no event type=%q newValue=%q in %+v", eventType, newValue, events)
	return ConvoyEvent{}
}

func requireEventType(t *testing.T, events []ConvoyEvent, eventType string) ConvoyEvent {
	t.Helper()
	for _, e := range events {
		if e.EventType == eventType {
			return e
		}
	}
	t.Errorf("no event type=%q in %+v", eventType, events)
	return ConvoyEvent{}
}

func TestConvoyEvents_StatusChange(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := CreateConvoy(db, "[1] status test")

	// Initial status is "Active" (set by CreateConvoy). Transition to DraftPROpen.
	if err := SetConvoyStatus(db, cid, "DraftPROpen"); err != nil {
		t.Fatal(err)
	}
	events := ListConvoyEvents(db, cid)
	e := requireEvent(t, events, "status_change", "DraftPROpen")
	if e.OldValue != "Active" {
		t.Errorf("expected old_value=Active, got %q", e.OldValue)
	}

	// Second transition.
	if err := SetConvoyStatus(db, cid, "Shipped"); err != nil {
		t.Fatal(err)
	}
	events = ListConvoyEvents(db, cid)
	e2 := requireEvent(t, events, "status_change", "Shipped")
	if e2.OldValue != "DraftPROpen" {
		t.Errorf("expected old_value=DraftPROpen, got %q", e2.OldValue)
	}
}

func TestConvoyEvents_StatusChangeTx(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := CreateConvoy(db, "[2] tx status test")

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := SetConvoyStatusTx(tx, cid, "Abandoned"); err != nil {
		tx.Rollback() //nolint:errcheck
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	events := ListConvoyEvents(db, cid)
	e := requireEvent(t, events, "status_change", "Abandoned")
	if e.OldValue != "Active" {
		t.Errorf("expected old_value=Active, got %q", e.OldValue)
	}
}

func TestConvoyEvents_StatusChangeTx_RollbackDropsEvent(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := CreateConvoy(db, "[3] rollback test")

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := SetConvoyStatusTx(tx, cid, "Shipped"); err != nil {
		tx.Rollback() //nolint:errcheck
		t.Fatal(err)
	}
	tx.Rollback() //nolint:errcheck

	events := ListConvoyEvents(db, cid)
	if len(events) != 0 {
		t.Errorf("rolled-back tx must not emit events; got %+v", events)
	}
}

func TestConvoyEvents_AutoRecover(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := CreateConvoy(db, "[4] auto-recover test")
	// Force convoy to Failed.
	if err := SetConvoyStatus(db, cid, "Failed"); err != nil {
		t.Fatal(err)
	}
	// No failed tasks → auto-recover should fire.
	AutoRecoverConvoy(db, cid, nil)

	c := GetConvoy(db, cid)
	if c.Status != "Active" {
		t.Fatalf("expected Active after auto-recover, got %q", c.Status)
	}
	events := ListConvoyEvents(db, cid)
	// Should have: Active→Failed, then Failed→Active.
	e := requireEvent(t, events, "status_change", "Active")
	if e.OldValue != "Failed" {
		t.Errorf("auto-recover event: expected old_value=Failed, got %q", e.OldValue)
	}
}

func TestConvoyEvents_AskBranchCreated(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := CreateConvoy(db, "[5] ask-branch test")
	if err := UpsertConvoyAskBranch(db, cid, "api", "force/ask-5-test", "sha0"); err != nil {
		t.Fatal(err)
	}

	events := ListConvoyEvents(db, cid)
	e := requireEventType(t, events, "ask_branch_created")
	if e.NewValue != "force/ask-5-test" {
		t.Errorf("expected new_value=force/ask-5-test, got %q", e.NewValue)
	}
	if e.Detail != "api" {
		t.Errorf("expected detail=api, got %q", e.Detail)
	}

	// Re-upsert same branch (base SHA update) must NOT emit another event.
	if err := UpsertConvoyAskBranch(db, cid, "api", "force/ask-5-test", "sha1"); err != nil {
		t.Fatal(err)
	}
	events2 := ListConvoyEvents(db, cid)
	count := 0
	for _, ev := range events2 {
		if ev.EventType == "ask_branch_created" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 ask_branch_created event after re-upsert, got %d", count)
	}
}

func TestConvoyEvents_DraftPROpened(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := CreateConvoy(db, "[6] draft PR test")
	if err := UpsertConvoyAskBranch(db, cid, "api", "force/ask-6-test", "sha0"); err != nil {
		t.Fatal(err)
	}
	if err := SetConvoyAskBranchDraftPR(db, cid, "api", "https://github.com/acme/api/pull/42", 42, "Open"); err != nil {
		t.Fatal(err)
	}

	events := ListConvoyEvents(db, cid)
	e := requireEventType(t, events, "draft_pr_opened")
	if e.NewValue != "https://github.com/acme/api/pull/42" {
		t.Errorf("expected PR URL in new_value, got %q", e.NewValue)
	}
	if e.Detail != "api" {
		t.Errorf("expected detail=api, got %q", e.Detail)
	}
}

func TestConvoyEvents_SubPRMerged(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	AddRepo(db, "api", "/tmp/api", "")
	cid, _ := CreateConvoy(db, "[7] sub-PR merged test")
	taskID, err := AddConvoyTask(db, 0, "api", "fix foo", cid, 0, "Pending")
	if err != nil {
		t.Fatal(err)
	}
	prID, err := CreateAskBranchPR(db, taskID, cid, "api", "https://github.com/acme/api/pull/7", 7)
	if err != nil {
		t.Fatal(err)
	}
	if err := MarkAskBranchPRMerged(db, prID); err != nil {
		t.Fatal(err)
	}

	events := ListConvoyEvents(db, cid)
	e := requireEventType(t, events, "sub_pr_merged")
	if e.NewValue != "7" {
		t.Errorf("expected new_value=7 (pr_number), got %q", e.NewValue)
	}
	if e.Detail != "api" {
		t.Errorf("expected detail=api, got %q", e.Detail)
	}

	// Exactly one sub_pr_merged event — no duplicates.
	count := 0
	for _, ev := range events {
		if ev.EventType == "sub_pr_merged" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 sub_pr_merged event, got %d", count)
	}
}

func TestConvoyEvents_SubPRMergedTx(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	AddRepo(db, "monolith", "/tmp/monolith", "")
	cid, _ := CreateConvoy(db, "[8] sub-PR merged tx test")
	taskID, err := AddConvoyTask(db, 0, "monolith", "fix bar", cid, 0, "Pending")
	if err != nil {
		t.Fatal(err)
	}
	prID, err := CreateAskBranchPR(db, taskID, cid, "monolith", "https://github.com/acme/monolith/pull/99", 99)
	if err != nil {
		t.Fatal(err)
	}

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := MarkAskBranchPRMergedTx(tx, prID); err != nil {
		tx.Rollback() //nolint:errcheck
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	events := ListConvoyEvents(db, cid)
	e := requireEventType(t, events, "sub_pr_merged")
	if e.NewValue != "99" {
		t.Errorf("expected new_value=99, got %q", e.NewValue)
	}
}

func TestConvoyEvents_Shipped(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := CreateConvoy(db, "[9] shipped test")
	if err := UpdateConvoyDraftPRState(db, cid, "Merged"); err != nil {
		t.Fatal(err)
	}

	events := ListConvoyEvents(db, cid)
	requireEventType(t, events, "shipped")
}

func TestConvoyEvents_ShippedNotEmittedOnClose(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := CreateConvoy(db, "[10] close test")
	if err := UpdateConvoyDraftPRState(db, cid, "Closed"); err != nil {
		t.Fatal(err)
	}

	events := ListConvoyEvents(db, cid)
	for _, e := range events {
		if e.EventType == "shipped" {
			t.Errorf("shipped event must not fire for state=Closed")
		}
	}
}

func TestListConvoyEvents_EmptyForUnknownConvoy(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	events := ListConvoyEvents(db, 9999)
	if events != nil {
		t.Errorf("expected nil for unknown convoy, got %v", events)
	}
}
