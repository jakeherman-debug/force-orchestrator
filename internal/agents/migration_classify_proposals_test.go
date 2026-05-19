// Package agents — D14 Phase 5 MigrationClassifyProposals tests.
//
// Coverage:
//   - TestMigrationClassify_KnowledgeAndRule: 3 proposals, 2 knowledge + 1 rule
//     (deterministic stub via LIVE_HAIKU_DISABLED=1 from TestMain).
//   - TestMigrationClassify_DryRun: --dry-run produces zero DB mutations.
//   - TestMigrationClassify_Batching: >20 proposals are batched correctly.
//   - TestMigrationClassify_Idempotent: running twice is a no-op.
//   - TestMigrationClassify_NoProposals: empty table returns (0, 0, nil).
package agents

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"force-orchestrator/internal/store"
)

// insertTestProposal inserts a minimal PromotionProposals row for testing.
// It uses the 'candidate' kind and authored_by='librarian' so
// ListPendingPromotionProposals picks it up (ratified_at='', rejected_at='',
// classification_status='').
func insertTestProposal(t *testing.T, db *sql.DB, ruleKey, content string) int {
	t.Helper()
	var id int
	err := db.QueryRow(`
		INSERT INTO PromotionProposals
			(experiment_id, kind, rule_key, proposed_content, evidence_summary_json, authored_by)
		VALUES (0, 'candidate', ?, ?, '{}', 'librarian')
		RETURNING id`, ruleKey, content).Scan(&id)
	if err != nil {
		t.Fatalf("insertTestProposal(%q): %v", ruleKey, err)
	}
	return id
}

// fetchProposalStatus returns (classification_status, suggested_scope) for a proposal.
func fetchProposalStatus(t *testing.T, db *sql.DB, id int) (classification, scope string) {
	t.Helper()
	err := db.QueryRow(
		`SELECT IFNULL(classification_status,''), IFNULL(suggested_scope,'')
		   FROM PromotionProposals WHERE id = ?`, id).Scan(&classification, &scope)
	if err != nil {
		t.Fatalf("fetchProposalStatus(id=%d): %v", id, err)
	}
	return
}

// classifyLogger is a no-op logger that captures lines for assertions.
type classifyLogger struct {
	lines []string
}

func (l *classifyLogger) Printf(format string, args ...any) {
	l.lines = append(l.lines, format)
}

// TestMigrationClassify_KnowledgeAndRule seeds 3 proposals and verifies that
// the deterministic stub (LIVE_HAIKU_DISABLED=1 set in TestMain) classifies
// them: proposals 0,1 → knowledge, proposal 2 → rule (see classifyBatchStub).
func TestMigrationClassify_KnowledgeAndRule(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id0 := insertTestProposal(t, db, "repo/convention-0", "Uses GraphQL not REST")
	id1 := insertTestProposal(t, db, "repo/convention-1", "All controllers inherit ApplicationController")
	id2 := insertTestProposal(t, db, "repo/rule-0", "All PRs must have tests")

	logger := &classifyLogger{}
	knowledgeAbsorbed, rulesFound, err := RunMigrationClassifyProposals(context.Background(), db, false, logger)
	if err != nil {
		t.Fatalf("RunMigrationClassifyProposals: %v", err)
	}

	// Stub pattern: i%3==2 → rule, else knowledge.
	if knowledgeAbsorbed != 2 {
		t.Errorf("knowledgeAbsorbed = %d, want 2", knowledgeAbsorbed)
	}
	if rulesFound != 1 {
		t.Errorf("rulesFound = %d, want 1", rulesFound)
	}

	// Verify id0 and id1 are absorbed_as_knowledge.
	for _, id := range []int{id0, id1} {
		cls, _ := fetchProposalStatus(t, db, id)
		if cls != "absorbed_as_knowledge" {
			t.Errorf("proposal %d: classification_status = %q, want absorbed_as_knowledge", id, cls)
		}
	}

	// Verify id2 is awaiting_scope_review.
	cls, scope := fetchProposalStatus(t, db, id2)
	if cls != "awaiting_scope_review" {
		t.Errorf("proposal %d: classification_status = %q, want awaiting_scope_review", id2, cls)
	}
	if scope == "" {
		t.Errorf("proposal %d: suggested_scope is empty, want non-empty", id2)
	}

	// SenateMemory rows must exist for the two absorbed proposals.
	memCount := 0
	rows, qErr := db.Query(`SELECT COUNT(*) FROM SenateMemory WHERE source = 'migration'`)
	if qErr != nil {
		t.Fatalf("SenateMemory count query: %v", qErr)
	}
	defer rows.Close()
	for rows.Next() {
		rows.Scan(&memCount)
	}
	if memCount < 2 {
		t.Errorf("SenateMemory rows with source='migration': got %d, want >= 2", memCount)
	}

	// A fleet-mail summary must have been sent.
	mails := store.ListMail(db, "operator")
	found := false
	for _, m := range mails {
		if m.Subject == "[D14 MIGRATION] Proposal classification complete" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected fleet-mail with subject '[D14 MIGRATION] Proposal classification complete'")
	}
}

// TestMigrationClassify_DryRun verifies that --dry-run produces zero DB
// mutations: proposals remain unclassified, no SenateMemory rows added,
// no fleet-mail sent.
func TestMigrationClassify_DryRun(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id0 := insertTestProposal(t, db, "repo/dry-0", "Some knowledge")
	id1 := insertTestProposal(t, db, "repo/dry-1", "Some rule")
	id2 := insertTestProposal(t, db, "repo/dry-2", "Another rule")

	logger := &classifyLogger{}
	knowledgeAbsorbed, rulesFound, err := RunMigrationClassifyProposals(context.Background(), db, true, logger)
	if err != nil {
		t.Fatalf("RunMigrationClassifyProposals(dry-run): %v", err)
	}

	// Counts are still reported.
	if knowledgeAbsorbed+rulesFound != 3 {
		t.Errorf("dry-run total classified = %d, want 3", knowledgeAbsorbed+rulesFound)
	}

	// No DB mutations.
	for _, id := range []int{id0, id1, id2} {
		cls, _ := fetchProposalStatus(t, db, id)
		if cls != "" {
			t.Errorf("[dry-run] proposal %d: classification_status = %q, want '' (no mutation)", id, cls)
		}
	}

	// No SenateMemory rows.
	var memCount int
	db.QueryRow(`SELECT COUNT(*) FROM SenateMemory WHERE source = 'migration'`).Scan(&memCount)
	if memCount != 0 {
		t.Errorf("[dry-run] SenateMemory rows inserted = %d, want 0", memCount)
	}

	// No fleet-mail.
	mails := store.ListMail(db, "operator")
	for _, m := range mails {
		if m.Subject == "[D14 MIGRATION] Proposal classification complete" {
			t.Errorf("[dry-run] fleet-mail was sent, should be suppressed in dry-run mode")
		}
	}
}

// TestMigrationClassify_Batching verifies that 25 proposals (> batch size 20)
// are all classified. The stub processes any batch size correctly.
func TestMigrationClassify_Batching(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	const n = 25
	for i := 0; i < n; i++ {
		insertTestProposal(t, db, fmt.Sprintf("repo/key-%d", i), fmt.Sprintf("Proposal content %d", i))
	}

	logger := &classifyLogger{}
	knowledgeAbsorbed, rulesFound, err := RunMigrationClassifyProposals(context.Background(), db, false, logger)
	if err != nil {
		t.Fatalf("RunMigrationClassifyProposals(n=25): %v", err)
	}

	total := knowledgeAbsorbed + rulesFound
	if total != n {
		t.Errorf("total classified = %d, want %d", total, n)
	}

	// Verify no proposal is left unclassified.
	var unclassified int
	db.QueryRow(`SELECT COUNT(*) FROM PromotionProposals WHERE classification_status = ''`).Scan(&unclassified)
	if unclassified != 0 {
		t.Errorf("unclassified proposals remaining = %d, want 0", unclassified)
	}
}

// TestMigrationClassify_Idempotent verifies that running the classifier
// twice on the same DB is a no-op on the second run: no double-inserts
// into SenateMemory and no status changes.
func TestMigrationClassify_Idempotent(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	insertTestProposal(t, db, "repo/idem-0", "Idempotent test knowledge")
	insertTestProposal(t, db, "repo/idem-1", "Idempotent test rule candidate")
	insertTestProposal(t, db, "repo/idem-2", "Another knowledge")

	logger := &classifyLogger{}

	// First run.
	k1, r1, err := RunMigrationClassifyProposals(context.Background(), db, false, logger)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}

	// Capture SenateMemory count after first run.
	var memAfterFirst int
	db.QueryRow(`SELECT COUNT(*) FROM SenateMemory WHERE source = 'migration'`).Scan(&memAfterFirst)

	// Second run — should classify 0 proposals (all already done).
	k2, r2, err := RunMigrationClassifyProposals(context.Background(), db, false, logger)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if k2 != 0 || r2 != 0 {
		t.Errorf("second run classified (k=%d, r=%d), want (0, 0)", k2, r2)
	}
	_ = k1
	_ = r1

	// SenateMemory row count must not have grown.
	var memAfterSecond int
	db.QueryRow(`SELECT COUNT(*) FROM SenateMemory WHERE source = 'migration'`).Scan(&memAfterSecond)
	if memAfterSecond != memAfterFirst {
		t.Errorf("SenateMemory count changed from %d to %d after second run — no double-inserts expected",
			memAfterFirst, memAfterSecond)
	}
}

// TestMigrationClassify_NoProposals verifies the empty-table path returns
// (0, 0, nil) without error.
func TestMigrationClassify_NoProposals(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	logger := &classifyLogger{}
	k, r, err := RunMigrationClassifyProposals(context.Background(), db, false, logger)
	if err != nil {
		t.Fatalf("empty table: %v", err)
	}
	if k != 0 || r != 0 {
		t.Errorf("empty table: got (k=%d, r=%d), want (0, 0)", k, r)
	}
}
