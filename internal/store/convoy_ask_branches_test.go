package store

import (
	"testing"
)

func TestUpsertConvoyAskBranch_CreatesAndUpdatesBaseSHA(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := CreateConvoy(db, "[1] test")
	if err := UpsertConvoyAskBranch(db, cid, "api", "force/ask-1-test", "sha0"); err != nil {
		t.Fatal(err)
	}
	got := GetConvoyAskBranch(db, cid, "api")
	if got == nil {
		t.Fatal("row missing after insert")
	}
	if got.AskBranch != "force/ask-1-test" || got.AskBranchBaseSHA != "sha0" {
		t.Errorf("fields wrong: %+v", got)
	}

	// Re-upsert with same branch but different SHA — allowed (rebase updates base).
	if err := UpsertConvoyAskBranch(db, cid, "api", "force/ask-1-test", "sha1"); err != nil {
		t.Fatal(err)
	}
	got = GetConvoyAskBranch(db, cid, "api")
	if got.AskBranchBaseSHA != "sha1" {
		t.Errorf("base SHA not updated: %+v", got)
	}
}

func TestUpsertConvoyAskBranch_RefusesBranchNameChange(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	cid, _ := CreateConvoy(db, "[1] test")
	_ = UpsertConvoyAskBranch(db, cid, "api", "force/ask-1-test", "sha0")
	if err := UpsertConvoyAskBranch(db, cid, "api", "force/ask-1-different", "sha0"); err == nil {
		t.Errorf("expected error when trying to overwrite with different branch name")
	}
}

func TestUpsertConvoyAskBranch_Validates(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	cid, _ := CreateConvoy(db, "[1] test")
	cases := []struct {
		convoyID  int
		repo      string
		branch    string
		sha       string
	}{
		{0, "api", "b", "s"},
		{cid, "", "b", "s"},
		{cid, "api", "", "s"},
		{cid, "api", "b", ""},
	}
	for _, c := range cases {
		if err := UpsertConvoyAskBranch(db, c.convoyID, c.repo, c.branch, c.sha); err == nil {
			t.Errorf("expected error for case %+v", c)
		}
	}
}

func TestListConvoyAskBranches_MultiRepo(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := CreateConvoy(db, "[1] multi")
	_ = UpsertConvoyAskBranch(db, cid, "api", "force/ask-1-multi", "sha-api")
	_ = UpsertConvoyAskBranch(db, cid, "monolith", "force/ask-1-multi", "sha-mono")

	rows := ListConvoyAskBranches(db, cid)
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
	// Sorted by repo ASC.
	if rows[0].Repo != "api" || rows[1].Repo != "monolith" {
		t.Errorf("unexpected order: %+v", rows)
	}
}

func TestUpdateConvoyAskBranchBase_StampsLastRebasedAt(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	cid, _ := CreateConvoy(db, "[1] rebase-test")
	_ = UpsertConvoyAskBranch(db, cid, "api", "force/ask-1-rebase-test", "sha-old")

	if err := UpdateConvoyAskBranchBase(db, cid, "api", "sha-new"); err != nil {
		t.Fatal(err)
	}
	got := GetConvoyAskBranch(db, cid, "api")
	if got.AskBranchBaseSHA != "sha-new" {
		t.Errorf("base SHA not updated: %q", got.AskBranchBaseSHA)
	}
	if got.LastRebasedAt == "" {
		t.Errorf("last_rebased_at should be stamped")
	}

	if err := UpdateConvoyAskBranchBase(db, cid, "api", ""); err == nil {
		t.Errorf("empty SHA must be rejected")
	}
}

func TestDraftPRFields(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	cid, _ := CreateConvoy(db, "[1] test")
	_ = UpsertConvoyAskBranch(db, cid, "api", "force/ask-1-test", "sha")

	if err := SetConvoyAskBranchDraftPR(db, cid, "api", "https://gh/pull/9", 9, "Open"); err != nil {
		t.Fatal(err)
	}
	got := GetConvoyAskBranch(db, cid, "api")
	if got.DraftPRURL != "https://gh/pull/9" || got.DraftPRNumber != 9 || got.DraftPRState != "Open" {
		t.Errorf("PR fields not stored: %+v", got)
	}

	// Transition to Merged — shipped_at must stamp.
	if err := UpdateConvoyAskBranchDraftState(db, cid, "api", "Merged"); err != nil {
		t.Fatal(err)
	}
	got = GetConvoyAskBranch(db, cid, "api")
	if got.DraftPRState != "Merged" || got.ShippedAt == "" {
		t.Errorf("Merged transition didn't stamp shipped_at: %+v", got)
	}
}

func TestDeleteConvoyAskBranch(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	cid, _ := CreateConvoy(db, "[1] t")
	_ = UpsertConvoyAskBranch(db, cid, "api", "b", "s")
	if err := DeleteConvoyAskBranch(db, cid, "api"); err != nil {
		t.Fatal(err)
	}
	if GetConvoyAskBranch(db, cid, "api") != nil {
		t.Errorf("row should be gone after delete")
	}
	// Delete is idempotent.
	if err := DeleteConvoyAskBranch(db, cid, "api"); err != nil {
		t.Errorf("second delete should be no-op: %v", err)
	}
}

func TestConvoyReposTouched(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := CreateConvoy(db, "[1] t")
	// Two tasks on api, one on monolith, one Cancelled — deduplicated and filtered.
	_, _ = AddConvoyTask(db, 0, "api", "t1", cid, 0, "Pending")
	_, _ = AddConvoyTask(db, 0, "api", "t2", cid, 0, "Pending")
	_, _ = AddConvoyTask(db, 0, "monolith", "t3", cid, 0, "Pending")
	cancelID, _ := AddConvoyTask(db, 0, "cron", "t4", cid, 0, "Pending")
	CancelTask(db, cancelID, "operator")
	// Also a task with empty target_repo — should be ignored.
	db.Exec(`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, convoy_id)
		VALUES (0, '', 'CodeEdit', 'Pending', 'no-repo', ?)`, cid)

	repos := ConvoyReposTouched(db, cid)
	if len(repos) != 2 || repos[0] != "api" || repos[1] != "monolith" {
		t.Errorf("expected [api, monolith], got %v", repos)
	}
}
