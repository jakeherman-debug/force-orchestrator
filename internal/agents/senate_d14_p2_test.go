// Package agents — D14 Phase 2 Senate tests.
//
// Covers:
//   - TestSenatorOnboarding_D14P2_TransitionsToActive: verifies that
//     runSenatorOnboardingTask with a stub LLM (LIVE_HAIKU_DISABLED)
//     transitions the chamber to 'active'.
//   - TestSenatorOnboarding_D14P2_NewTagCreated: tag suggestions with a
//     non-existent tag result in the tag being created in the Tags table first.
//   - TestSenatorOnboarding_D14P2_EmptyMemory: a senator with no existing
//     SenateMemory still completes (graceful empty outputs).
//   - TestDogSenateRefresh_D14P2_SkipsDuplicate: dogSenateRefresh skips
//     senators that already have a Pending/Locked SenatorRefresh task.
//   - TestDogSenateRefresh_D14P2_QueuesRefreshTasks: dogSenateRefresh
//     queues SenatorRefresh tasks for all active senators.
//   - TestSenatorRefreshTask_D14P2_RoundTrip: runSenatorRefreshTask
//     completes and bumps last_refreshed_at without changing the chamber
//     status (stays 'active').
package agents

import (
	"context"
	"database/sql"
	"testing"

	"force-orchestrator/internal/clients/librarian"
	"force-orchestrator/internal/store"
)

// seedActiveChamber inserts an active SenateChambers row for the given repo.
func seedActiveChamber(t *testing.T, db *sql.DB, repoID string) {
	t.Helper()
	if err := store.UpsertSenateChamber(db, store.SenateChamber{
		SenatorName: repoID,
		Scope:       "repo:" + repoID,
		Status:      "active",
	}); err != nil {
		t.Fatalf("seedActiveChamber(%s): %v", repoID, err)
	}
}

// TestSenatorOnboarding_D14P2_TransitionsToActive verifies that
// runSenatorOnboardingTask with stub LLM (LIVE_HAIKU_DISABLED=1)
// transitions the chamber to 'active'.
func TestSenatorOnboarding_D14P2_TransitionsToActive(t *testing.T) {
	// LIVE_HAIKU_DISABLED is already 1 from TestMain.
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	lib := librarian.NewInProcess(db)

	taskID, err := store.QueueSenatorOnboarding(db, "testrepo", "test")
	if err != nil {
		t.Fatalf("QueueSenatorOnboarding: %v", err)
	}
	bounty := loadBountyWithPayload(t, db, taskID)
	logger := &senateTestLogger{}
	runSenatorOnboardingTask(context.Background(), db, "Senate-d14-test", bounty, lib, logger)

	// Chamber must be 'active' after onboarding completes.
	chamber, err := store.GetSenateChamber(db, "testrepo")
	if err != nil {
		t.Fatalf("GetSenateChamber: %v", err)
	}
	if chamber == nil {
		t.Fatal("chamber missing after onboarding")
	}
	if chamber.Status != "active" {
		t.Errorf("chamber.Status = %q, want active", chamber.Status)
	}

	// Task itself must be Completed.
	tb, _ := store.GetBounty(db, taskID)
	if tb.Status != "Completed" {
		t.Errorf("onboarding task status = %q, want Completed", tb.Status)
	}

	// At least one SenateMemory row must exist (from deterministic stub).
	mem, err := store.ListSenateMemory(db, "testrepo", 50)
	if err != nil {
		t.Fatalf("ListSenateMemory: %v", err)
	}
	if len(mem) < 1 {
		t.Errorf("ListSenateMemory: got %d, want >= 1", len(mem))
	}
}

// TestSenatorOnboarding_D14P2_NewTagCreated verifies that if the LLM
// output contains a tag that doesn't exist in the Tags table, the tag
// is created before the TagSuggestion is inserted.
func TestSenatorOnboarding_D14P2_NewTagCreated(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Directly call persistSenatorTagSuggestions with a tag that doesn't exist.
	tags := []senatorTagSuggestion{
		{Tag: "brand-new-tag", Rationale: "This is a new tag created during onboarding."},
	}
	logger := &senateTestLogger{}
	count := persistSenatorTagSuggestions(db, 42, "myrepo", tags, logger)

	if count != 1 {
		t.Errorf("persistSenatorTagSuggestions: returned %d, want 1", count)
	}

	// Tag must now exist in the Tags table.
	tag, err := store.GetTag(db, "brand-new-tag")
	if err != nil {
		t.Fatalf("GetTag(brand-new-tag): %v", err)
	}
	if tag.Name != "brand-new-tag" {
		t.Errorf("tag.Name = %q, want brand-new-tag", tag.Name)
	}
	if tag.Description == "" {
		t.Error("tag.Description empty, want rationale as description")
	}

	// TagSuggestion must exist.
	suggestions, err := store.ListTagSuggestions(db, "pending")
	if err != nil {
		t.Fatalf("ListTagSuggestions: %v", err)
	}
	if len(suggestions) != 1 {
		t.Fatalf("ListTagSuggestions: got %d, want 1", len(suggestions))
	}
	s := suggestions[0]
	if s.Tag != "brand-new-tag" {
		t.Errorf("suggestion.Tag = %q, want brand-new-tag", s.Tag)
	}
	if s.RepoName != "myrepo" {
		t.Errorf("suggestion.RepoName = %q, want myrepo", s.RepoName)
	}
	if s.SuggestedBy != "librarian:senate-onboarding" {
		t.Errorf("suggestion.SuggestedBy = %q, want librarian:senate-onboarding", s.SuggestedBy)
	}
}

// TestSenatorOnboarding_D14P2_ExistingTagNotDuplicated verifies that if the
// tag already exists in the Tags table, CreateTag is NOT called again (no
// duplicate key error) and the TagSuggestion is still inserted.
func TestSenatorOnboarding_D14P2_ExistingTagNotDuplicated(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Pre-create the tag.
	if err := store.CreateTag(db, "existing-tag", "pre-existing description", "operator"); err != nil {
		t.Fatalf("CreateTag: %v", err)
	}

	tags := []senatorTagSuggestion{
		{Tag: "existing-tag", Rationale: "Senator thinks this repo belongs here."},
	}
	logger := &senateTestLogger{}
	count := persistSenatorTagSuggestions(db, 42, "myrepo", tags, logger)
	if count != 1 {
		t.Errorf("persistSenatorTagSuggestions: returned %d, want 1", count)
	}

	// Original tag description should be unchanged.
	tag, _ := store.GetTag(db, "existing-tag")
	if tag.Description != "pre-existing description" {
		t.Errorf("tag.Description = %q, want pre-existing description", tag.Description)
	}
}

// TestSenatorOnboarding_D14P2_EmptyMemory verifies that a senator with no
// existing SenateMemory still completes onboarding gracefully.
func TestSenatorOnboarding_D14P2_EmptyMemory(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	lib := librarian.NewInProcess(db)

	// With LIVE_HAIKU_DISABLED=1 (from TestMain), the stub LLM returns a
	// minimal knowledge_digest. We verify the task completes and the
	// chamber transitions to 'active' even though there's no pre-existing
	// SenateMemory.
	taskID, err := store.QueueSenatorOnboarding(db, "empty-mem-repo", "test")
	if err != nil {
		t.Fatalf("QueueSenatorOnboarding: %v", err)
	}
	bounty := loadBountyWithPayload(t, db, taskID)
	logger := &senateTestLogger{}
	runSenatorOnboardingTask(context.Background(), db, "Senate-d14-test", bounty, lib, logger)

	// Task completed.
	tb, _ := store.GetBounty(db, taskID)
	if tb.Status != "Completed" {
		t.Errorf("task status = %q, want Completed", tb.Status)
	}

	// Chamber is active.
	chamber, _ := store.GetSenateChamber(db, "empty-mem-repo")
	if chamber == nil || chamber.Status != "active" {
		t.Errorf("chamber = %+v, want status=active", chamber)
	}

	// At least one memory row (from the stub).
	mem, _ := store.ListSenateMemory(db, "empty-mem-repo", 50)
	if len(mem) < 1 {
		t.Errorf("ListSenateMemory: got %d, want >= 1", len(mem))
	}
}

// TestDogSenateRefresh_D14P2_SkipsDuplicate verifies that dogSenateRefresh
// does not queue a second SenatorRefresh task when one is already Pending
// for the same senator.
func TestDogSenateRefresh_D14P2_SkipsDuplicate(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	lib := librarian.NewInProcess(db)

	// Seed an active chamber.
	seedActiveChamber(t, db, "dupe-repo")

	// First dog run: should queue one task.
	logger := &senateTestLogger{}
	if err := dogSenateRefresh(context.Background(), db, lib, logger); err != nil {
		t.Fatalf("dogSenateRefresh (first): %v", err)
	}
	var count1 int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type = 'SenatorRefresh' AND status = 'Pending'`).Scan(&count1)
	if count1 != 1 {
		t.Fatalf("after first run: SenatorRefresh tasks = %d, want 1", count1)
	}

	// Second dog run: the existing Pending task should prevent a new insert.
	if err := dogSenateRefresh(context.Background(), db, lib, logger); err != nil {
		t.Fatalf("dogSenateRefresh (second): %v", err)
	}
	var count2 int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type = 'SenatorRefresh' AND target_repo = 'dupe-repo'`).Scan(&count2)
	if count2 != 1 {
		t.Errorf("after second run: SenatorRefresh tasks for dupe-repo = %d, want 1 (dedup)", count2)
	}
}

// TestDogSenateRefresh_D14P2_QueuesRefreshTasks verifies that dogSenateRefresh
// queues one SenatorRefresh task per active senator.
func TestDogSenateRefresh_D14P2_QueuesRefreshTasks(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	lib := librarian.NewInProcess(db)

	for _, name := range []string{"repo-alpha", "repo-beta", "repo-gamma"} {
		seedActiveChamber(t, db, name)
	}

	logger := &senateTestLogger{}
	if err := dogSenateRefresh(context.Background(), db, lib, logger); err != nil {
		t.Fatalf("dogSenateRefresh: %v", err)
	}

	var count int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type = 'SenatorRefresh' AND status = 'Pending'`).Scan(&count)
	if count != 3 {
		t.Errorf("SenatorRefresh tasks queued = %d, want 3", count)
	}
}

// TestSenatorRefreshTask_D14P2_RoundTrip verifies that runSenatorRefreshTask
// completes, bumps last_refreshed_at, and leaves the chamber in 'active'.
func TestSenatorRefreshTask_D14P2_RoundTrip(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	lib := librarian.NewInProcess(db)

	seedActiveChamber(t, db, "refresh-repo")

	// Queue a SenatorRefresh task.
	taskID, alreadyExists, err := store.QueueSenatorRefresh(db, "refresh-repo", "test")
	if err != nil {
		t.Fatalf("QueueSenatorRefresh: %v", err)
	}
	if alreadyExists {
		t.Fatal("QueueSenatorRefresh: unexpected alreadyExists=true")
	}

	bounty := loadBountyWithPayload(t, db, taskID)
	logger := &senateTestLogger{}
	runSenatorRefreshTask(context.Background(), db, "Senate-d14-test", bounty, lib, logger)

	// Task completed.
	tb, _ := store.GetBounty(db, taskID)
	if tb.Status != "Completed" {
		t.Errorf("SenatorRefresh task status = %q, want Completed", tb.Status)
	}

	// Chamber still active (refresh must not change status).
	chamber, err := store.GetSenateChamber(db, "refresh-repo")
	if err != nil {
		t.Fatalf("GetSenateChamber: %v", err)
	}
	if chamber == nil || chamber.Status != "active" {
		t.Errorf("chamber = %+v, want status=active", chamber)
	}

	// last_refreshed_at should be bumped.
	if chamber.LastRefreshedAt == "" {
		t.Error("chamber.LastRefreshedAt empty after refresh, want non-empty")
	}

	// Memory rows should have been inserted.
	mem, _ := store.ListSenateMemory(db, "refresh-repo", 50)
	if len(mem) < 1 {
		t.Errorf("ListSenateMemory after refresh: got %d, want >= 1", len(mem))
	}
}

// TestSenatorRefreshTask_D14P2_InactiveChamber verifies that
// runSenatorRefreshTask completes without error when the chamber is
// no longer active (e.g. retired between queue and claim).
func TestSenatorRefreshTask_D14P2_InactiveChamber(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	lib := librarian.NewInProcess(db)

	// Seed as active, then retire.
	seedActiveChamber(t, db, "retiring-repo")
	if err := store.SetSenateChamberStatus(db, "retiring-repo", "retired"); err != nil {
		t.Fatalf("SetSenateChamberStatus: %v", err)
	}

	taskID, _, err := store.QueueSenatorRefresh(db, "retiring-repo", "test")
	if err != nil {
		t.Fatalf("QueueSenatorRefresh: %v", err)
	}

	bounty := loadBountyWithPayload(t, db, taskID)
	logger := &senateTestLogger{}
	runSenatorRefreshTask(context.Background(), db, "Senate-d14-test", bounty, lib, logger)

	// Task must complete (not fail).
	tb, _ := store.GetBounty(db, taskID)
	if tb.Status != "Completed" {
		t.Errorf("SenatorRefresh task status = %q, want Completed", tb.Status)
	}

	// No memory rows should have been inserted for the retired senator.
	mem, _ := store.ListSenateMemory(db, "retiring-repo", 50)
	if len(mem) != 0 {
		t.Errorf("ListSenateMemory for retired senator: got %d rows, want 0", len(mem))
	}
}

// TestPersistSenatorKnowledgeDigest verifies the weight clamping and topic
// construction done by persistSenatorKnowledgeDigest.
func TestPersistSenatorKnowledgeDigest(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	facts := []senatorKnowledgeFact{
		{Fact: "fact one", Weight: 5, Category: "architecture"},
		{Fact: "fact two", Weight: 0, Category: ""},    // weight clamped to 1
		{Fact: "fact three", Weight: 9, Category: "risks"}, // weight clamped to 5
		{Fact: "", Weight: 3, Category: "patterns"},   // empty fact — skipped
	}
	logger := &senateTestLogger{}
	count := persistSenatorKnowledgeDigest(db, 1, "testrepo", facts, logger)
	if count != 3 {
		t.Errorf("count = %d, want 3", count)
	}

	mem, _ := store.ListSenateMemory(db, "testrepo", 50)
	if len(mem) != 3 {
		t.Fatalf("memory rows = %d, want 3", len(mem))
	}

	// Verify weight clamping.
	weightMap := map[string]float64{}
	for _, m := range mem {
		weightMap[m.Summary] = m.Weight
	}
	if weightMap["fact two"] != 1.0 {
		t.Errorf("fact two weight = %v, want 1.0 (clamped from 0)", weightMap["fact two"])
	}
	if weightMap["fact three"] != 5.0 {
		t.Errorf("fact three weight = %v, want 5.0 (clamped from 9)", weightMap["fact three"])
	}
}

// TestQueueSenatorRefresh_Dedup verifies QueueSenatorRefresh dedup logic
// directly (unit-level, without running the dog).
func TestQueueSenatorRefresh_Dedup(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// First call: should insert.
	id1, exists1, err1 := store.QueueSenatorRefresh(db, "dedup-repo", "test")
	if err1 != nil {
		t.Fatalf("first QueueSenatorRefresh: %v", err1)
	}
	if exists1 {
		t.Error("first QueueSenatorRefresh: alreadyExists=true, want false")
	}
	if id1 == 0 {
		t.Error("first QueueSenatorRefresh: id=0, want >0")
	}

	// Second call while first is still Pending: should dedup.
	id2, exists2, err2 := store.QueueSenatorRefresh(db, "dedup-repo", "test")
	if err2 != nil {
		t.Fatalf("second QueueSenatorRefresh: %v", err2)
	}
	if !exists2 {
		t.Error("second QueueSenatorRefresh: alreadyExists=false, want true")
	}
	if id2 != 0 {
		t.Errorf("second QueueSenatorRefresh: id=%d, want 0 (dedup)", id2)
	}

	// Only one row in the DB.
	var count int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type = 'SenatorRefresh' AND target_repo = 'dedup-repo'`).Scan(&count)
	if count != 1 {
		t.Errorf("SenatorRefresh rows = %d, want 1", count)
	}
}
