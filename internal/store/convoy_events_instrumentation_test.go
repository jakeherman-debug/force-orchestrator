package store

import (
	"database/sql"
	"fmt"
	"testing"
)

// requireEvent asserts that at least one event exists with wantType, wantOld,
// and wantNew. Scans all events so multiple events of the same type are handled
// correctly — each call looks for a specific (type, old, new) triple.
func requireEvent(t *testing.T, db *sql.DB, convoyID int, wantType, wantOld, wantNew string) {
	t.Helper()
	events, err := ListConvoyEvents(db, int64(convoyID))
	if err != nil {
		t.Fatalf("ListConvoyEvents: %v", err)
	}
	for _, e := range events {
		if e.EventType == wantType && e.OldValue == wantOld && e.NewValue == wantNew {
			return
		}
	}
	t.Errorf("no event {type=%q old=%q new=%q} found; all events: %+v", wantType, wantOld, wantNew, events)
}

func TestSetConvoyStatus_EmitsStatusChangeEvent(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := CreateConvoy(db, "[1] status-test")

	if err := SetConvoyStatus(db, cid, "AwaitingDraftPR"); err != nil {
		t.Fatal(err)
	}
	requireEvent(t, db, cid, "status_change", "Active", "AwaitingDraftPR")

	if err := SetConvoyStatus(db, cid, "DraftPROpen"); err != nil {
		t.Fatal(err)
	}
	requireEvent(t, db, cid, "status_change", "AwaitingDraftPR", "DraftPROpen")
}

func TestSetConvoyStatusTx_EmitsStatusChangeEvent(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := CreateConvoy(db, "[1] status-tx-test")

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := SetConvoyStatusTx(tx, cid, "Abandoned"); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	requireEvent(t, db, cid, "status_change", "Active", "Abandoned")
}

func TestSetConvoyStatusTx_RollbackDropsEvent(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := CreateConvoy(db, "[1] rollback-test")

	tx, _ := db.Begin()
	_ = SetConvoyStatusTx(tx, cid, "Abandoned")
	tx.Rollback()

	events, _ := ListConvoyEvents(db, int64(cid))
	if len(events) != 0 {
		t.Errorf("rolled-back tx must not leave events; got %d", len(events))
	}
}

func TestAutoRecoverConvoy_EmitsStatusChangeEvent(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := CreateConvoy(db, "[1] recover-test")
	db.Exec(`UPDATE Convoys SET status = 'Failed' WHERE id = ?`, cid)

	AutoRecoverConvoy(db, cid, nil)

	requireEvent(t, db, cid, "status_change", "Failed", "Active")
}

func TestSetConvoyAskBranch_EmitsAskBranchCreatedEvent(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := CreateConvoy(db, "[1] ask-branch-legacy")

	if err := SetConvoyAskBranch(db, cid, "force/ask-1-legacy", "sha123"); err != nil {
		t.Fatal(err)
	}
	requireEvent(t, db, cid, "ask_branch_created", "", "force/ask-1-legacy")
}

func TestUpsertConvoyAskBranch_EmitsAskBranchCreatedOnlyOnCreate(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := CreateConvoy(db, "[1] ask-branch-upsert")

	// First upsert → creation → event emitted.
	if err := UpsertConvoyAskBranch(db, cid, "api", "force/ask-1-upsert", "sha0"); err != nil {
		t.Fatal(err)
	}
	events, _ := ListConvoyEvents(db, int64(cid))
	var created []ConvoyEvent
	for _, e := range events {
		if e.EventType == "ask_branch_created" {
			created = append(created, e)
		}
	}
	if len(created) != 1 {
		t.Fatalf("expected 1 ask_branch_created event after first upsert, got %d", len(created))
	}
	if created[0].NewValue != "force/ask-1-upsert" {
		t.Errorf("new_value: got %q want %q", created[0].NewValue, "force/ask-1-upsert")
	}

	// Second upsert (rebase, same branch, different SHA) → no new creation event.
	if err := UpsertConvoyAskBranch(db, cid, "api", "force/ask-1-upsert", "sha1"); err != nil {
		t.Fatal(err)
	}
	events, _ = ListConvoyEvents(db, int64(cid))
	var allCreated []ConvoyEvent
	for _, e := range events {
		if e.EventType == "ask_branch_created" {
			allCreated = append(allCreated, e)
		}
	}
	if len(allCreated) != 1 {
		t.Errorf("rebase upsert must not emit another ask_branch_created; got %d", len(allCreated))
	}
}

func TestSetConvoyDraftPR_EmitsDraftPROpenedEvent(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := CreateConvoy(db, "[1] draft-pr-legacy")

	if err := SetConvoyDraftPR(db, cid, "https://github.com/acme/api/pull/42", 42, "Open"); err != nil {
		t.Fatal(err)
	}
	requireEvent(t, db, cid, "draft_pr_opened", "", "https://github.com/acme/api/pull/42")
}

func TestSetConvoyAskBranchDraftPR_EmitsDraftPROpenedEvent(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := CreateConvoy(db, "[1] draft-pr-per-repo")
	_ = UpsertConvoyAskBranch(db, cid, "api", "force/ask-1-per-repo", "sha0")

	if err := SetConvoyAskBranchDraftPR(db, cid, "api", "https://github.com/acme/api/pull/7", 7, "Open"); err != nil {
		t.Fatal(err)
	}
	requireEvent(t, db, cid, "draft_pr_opened", "", "https://github.com/acme/api/pull/7")
}

func TestUpdateConvoyDraftPRState_Merged_EmitsShippedEvent(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := CreateConvoy(db, "[1] shipped-test")
	_ = SetConvoyDraftPR(db, cid, "https://github.com/acme/api/pull/5", 5, "Open")

	if err := UpdateConvoyDraftPRState(db, cid, "Merged"); err != nil {
		t.Fatal(err)
	}
	requireEvent(t, db, cid, "shipped", "", "")
}

func TestUpdateConvoyDraftPRState_Closed_NoShippedEvent(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := CreateConvoy(db, "[1] closed-test")
	_ = SetConvoyDraftPR(db, cid, "https://github.com/acme/api/pull/6", 6, "Open")

	if err := UpdateConvoyDraftPRState(db, cid, "Closed"); err != nil {
		t.Fatal(err)
	}
	events, _ := ListConvoyEvents(db, int64(cid))
	for _, e := range events {
		if e.EventType == "shipped" {
			t.Errorf("Closed transition must not emit a shipped event")
		}
	}
}

func TestMarkAskBranchPRMerged_EmitsSubPRMergedEvent(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	taskID, convoyID, repo := setupConvoyAndRepo(t, db)
	id, _ := CreateAskBranchPR(db, taskID, convoyID, repo, "https://github.com/acme/api/pull/99", 99)

	if err := MarkAskBranchPRMerged(db, id); err != nil {
		t.Fatal(err)
	}

	events, err := ListConvoyEvents(db, int64(convoyID))
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range events {
		if e.EventType == "sub_pr_merged" {
			found = true
			if e.NewValue != fmt.Sprintf("%d", 99) {
				t.Errorf("new_value: got %q want %q", e.NewValue, "99")
			}
			if e.Detail != repo {
				t.Errorf("detail: got %q want %q", e.Detail, repo)
			}
		}
	}
	if !found {
		t.Errorf("no sub_pr_merged event found")
	}
}

func TestMarkAskBranchPRMergedTx_EmitsSubPRMergedEvent(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	taskID, convoyID, repo := setupConvoyAndRepo(t, db)
	id, _ := CreateAskBranchPR(db, taskID, convoyID, repo, "https://github.com/acme/api/pull/100", 100)

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := MarkAskBranchPRMergedTx(tx, id); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	events, _ := ListConvoyEvents(db, int64(convoyID))
	var found bool
	for _, e := range events {
		if e.EventType == "sub_pr_merged" {
			found = true
			if e.NewValue != "100" {
				t.Errorf("new_value: got %q want %q", e.NewValue, "100")
			}
		}
	}
	if !found {
		t.Errorf("no sub_pr_merged event found after Tx merge")
	}
}
