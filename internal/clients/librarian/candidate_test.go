package librarian_test

// Tests for D3 Phase 3 — Librarian → EC handoff (EmitCandidate +
// ListPendingCandidates). The convention (P2 closure note + paired-runs.md
// § Composition with Promotion Pipeline): kind='candidate' and
// authored_by='librarian' double as the origin column. Verified here by
// asserting the row is filtered correctly when authored_by='engineering-corps'
// rows coexist (kind='promote' from EC's MaybePromoteRule).

import (
	"context"
	"strings"
	"testing"

	"force-orchestrator/internal/clients/librarian"
)

func TestEmitCandidate_RoundTrip(t *testing.T) {
	db := newTestDB(t)
	c := librarian.NewInProcess(db)

	id, err := c.EmitCandidate(context.Background(), librarian.Candidate{
		HypothesisKey: "captain.haiku-for-classify",
		HypothesisRaw: "Captain should use Haiku tier for low-stakes spawn classification.",
		EvidenceJSON:  `{"source":"librarian","memory_ids":[1,2,3]}`,
	})
	if err != nil {
		t.Fatalf("EmitCandidate: %v", err)
	}
	if id <= 0 {
		t.Fatalf("expected positive proposal ID, got %d", id)
	}

	// Verify the row landed with the right shape.
	var kind, authoredBy, ruleKey, content, evidence, authoredAt, ratifiedAt, rejectedAt string
	if scanErr := db.QueryRow(`
		SELECT kind, authored_by, rule_key, proposed_content,
		       evidence_summary_json, IFNULL(authored_at,''),
		       IFNULL(ratified_at,''), IFNULL(rejected_at,'')
		  FROM PromotionProposals WHERE id = ?`, id).
		Scan(&kind, &authoredBy, &ruleKey, &content, &evidence, &authoredAt, &ratifiedAt, &rejectedAt); scanErr != nil {
		t.Fatalf("read back row: %v", scanErr)
	}
	if kind != "candidate" {
		t.Errorf("kind = %q, want candidate", kind)
	}
	if authoredBy != "librarian" {
		t.Errorf("authored_by = %q, want librarian (origin convention)", authoredBy)
	}
	if ruleKey != "captain.haiku-for-classify" {
		t.Errorf("rule_key = %q", ruleKey)
	}
	if !strings.Contains(content, "Haiku tier") {
		t.Errorf("proposed_content = %q (want HypothesisRaw round-tripped)", content)
	}
	if !strings.Contains(evidence, `"source":"librarian"`) {
		t.Errorf("evidence_summary_json = %q", evidence)
	}
	if authoredAt == "" {
		t.Errorf("authored_at should be populated by datetime('now')")
	}
	if ratifiedAt != "" || rejectedAt != "" {
		t.Errorf("new candidate should be pending — got ratified_at=%q rejected_at=%q", ratifiedAt, rejectedAt)
	}
}

func TestEmitCandidate_RejectsEmptyKey(t *testing.T) {
	db := newTestDB(t)
	c := librarian.NewInProcess(db)
	_, err := c.EmitCandidate(context.Background(), librarian.Candidate{
		HypothesisKey: "  ",
		HypothesisRaw: "non-empty body",
	})
	if err == nil {
		t.Fatal("expected error for blank HypothesisKey, got none")
	}
}

func TestEmitCandidate_RejectsEmptyRaw(t *testing.T) {
	db := newTestDB(t)
	c := librarian.NewInProcess(db)
	_, err := c.EmitCandidate(context.Background(), librarian.Candidate{
		HypothesisKey: "key",
		HypothesisRaw: "",
	})
	if err == nil {
		t.Fatal("expected error for blank HypothesisRaw, got none")
	}
}

func TestEmitCandidate_DefaultsBlankEvidenceToEmptyJSON(t *testing.T) {
	db := newTestDB(t)
	c := librarian.NewInProcess(db)
	id, err := c.EmitCandidate(context.Background(), librarian.Candidate{
		HypothesisKey: "k",
		HypothesisRaw: "r",
	})
	if err != nil {
		t.Fatalf("EmitCandidate: %v", err)
	}
	var evidence string
	if scanErr := db.QueryRow(`SELECT evidence_summary_json FROM PromotionProposals WHERE id = ?`, id).
		Scan(&evidence); scanErr != nil {
		t.Fatalf("scan: %v", scanErr)
	}
	if evidence != "{}" {
		t.Errorf("blank evidence should default to '{}', got %q", evidence)
	}
}

// TestListPendingCandidates_FiltersToOriginLibrarian seeds three rows
// with different shapes — librarian-candidate (pending), librarian-candidate
// (rejected), and EC-promote — and asserts only the pending librarian
// candidate is returned. This is the load-bearing assertion that the
// authored_by-as-origin convention works.
func TestListPendingCandidates_FiltersToOriginLibrarian(t *testing.T) {
	db := newTestDB(t)
	c := librarian.NewInProcess(db)
	ctx := context.Background()

	// Seed: pending librarian candidate.
	pendingID, err := c.EmitCandidate(ctx, librarian.Candidate{
		HypothesisKey: "pending-key",
		HypothesisRaw: "pending hypothesis",
	})
	if err != nil {
		t.Fatalf("emit pending: %v", err)
	}

	// Seed: rejected librarian candidate.
	rejectedID, err := c.EmitCandidate(ctx, librarian.Candidate{
		HypothesisKey: "rejected-key",
		HypothesisRaw: "rejected hypothesis",
	})
	if err != nil {
		t.Fatalf("emit rejected: %v", err)
	}
	if _, err := db.Exec(
		`UPDATE PromotionProposals SET rejected_at = datetime('now'), rejected_reason = 'no good' WHERE id = ?`,
		rejectedID); err != nil {
		t.Fatalf("mark rejected: %v", err)
	}

	// Seed: ratified librarian candidate (should also be excluded).
	ratifiedID, err := c.EmitCandidate(ctx, librarian.Candidate{
		HypothesisKey: "ratified-key",
		HypothesisRaw: "ratified hypothesis",
	})
	if err != nil {
		t.Fatalf("emit ratified: %v", err)
	}
	if _, err := db.Exec(
		`UPDATE PromotionProposals SET ratified_at = datetime('now'), ratified_by = 'op@x.com' WHERE id = ?`,
		ratifiedID); err != nil {
		t.Fatalf("mark ratified: %v", err)
	}

	// Seed: EC-emitted promote (kind='promote', authored_by='engineering-corps').
	if _, err := db.Exec(`
		INSERT INTO PromotionProposals
			(experiment_id, kind, rule_key, proposed_content, evidence_summary_json,
			 authored_by, authored_at)
		VALUES (1, 'promote', 'ec.rule', 'EC content', '{}', 'engineering-corps', datetime('now'))
	`); err != nil {
		t.Fatalf("seed EC row: %v", err)
	}

	out, err := c.ListPendingCandidates(ctx)
	if err != nil {
		t.Fatalf("ListPendingCandidates: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected exactly 1 pending librarian candidate, got %d: %+v", len(out), out)
	}
	if out[0].ProposalID != pendingID {
		t.Errorf("got proposal %d, want %d (pending row)", out[0].ProposalID, pendingID)
	}
	if out[0].HypothesisKey != "pending-key" {
		t.Errorf("HypothesisKey = %q, want 'pending-key'", out[0].HypothesisKey)
	}
	if out[0].AuthoredAt == "" {
		t.Errorf("AuthoredAt should be populated")
	}
}

// TestListPendingCandidates_EmptyDBReturnsEmpty — fresh DB returns
// (nil, nil), not an error.
func TestListPendingCandidates_EmptyDBReturnsEmpty(t *testing.T) {
	db := newTestDB(t)
	c := librarian.NewInProcess(db)
	out, err := c.ListPendingCandidates(context.Background())
	if err != nil {
		t.Fatalf("ListPendingCandidates: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("empty DB returned %d rows, want 0", len(out))
	}
}

// TestListPendingCandidates_OrderingNewestFirst — multiple emit calls
// return rows ordered by id DESC.
func TestListPendingCandidates_OrderingNewestFirst(t *testing.T) {
	db := newTestDB(t)
	c := librarian.NewInProcess(db)
	ctx := context.Background()

	id1, _ := c.EmitCandidate(ctx, librarian.Candidate{HypothesisKey: "k1", HypothesisRaw: "r1"})
	id2, _ := c.EmitCandidate(ctx, librarian.Candidate{HypothesisKey: "k2", HypothesisRaw: "r2"})
	id3, _ := c.EmitCandidate(ctx, librarian.Candidate{HypothesisKey: "k3", HypothesisRaw: "r3"})

	out, err := c.ListPendingCandidates(ctx)
	if err != nil {
		t.Fatalf("ListPendingCandidates: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("got %d, want 3", len(out))
	}
	if out[0].ProposalID != id3 || out[1].ProposalID != id2 || out[2].ProposalID != id1 {
		t.Errorf("ordering wrong: got [%d, %d, %d], want [%d, %d, %d]",
			out[0].ProposalID, out[1].ProposalID, out[2].ProposalID, id3, id2, id1)
	}
}

// TestEmitCandidate_ContextCanceled and ListPendingCandidates_ContextCanceled
// — the ctx cancellation paths fire before the DB call.
func TestEmitCandidate_ContextCanceled(t *testing.T) {
	db := newTestDB(t)
	c := librarian.NewInProcess(db)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.EmitCandidate(ctx, librarian.Candidate{HypothesisKey: "k", HypothesisRaw: "r"}); err == nil {
		t.Error("expected ctx canceled error from EmitCandidate")
	}
	if _, err := c.ListPendingCandidates(ctx); err == nil {
		t.Error("expected ctx canceled error from ListPendingCandidates")
	}
}

// TestMockClient_EmitCandidate_Idempotent — the mock auto-increments
// the proposal ID and pushes onto Candidates so a follow-up
// ListPendingCandidates call returns the seeded rows.
func TestMockClient_EmitCandidate_Idempotent(t *testing.T) {
	m := librarian.NewMock()
	id1, err := m.EmitCandidate(context.Background(), librarian.Candidate{HypothesisKey: "k1", HypothesisRaw: "r1"})
	if err != nil {
		t.Fatalf("emit 1: %v", err)
	}
	id2, err := m.EmitCandidate(context.Background(), librarian.Candidate{HypothesisKey: "k2", HypothesisRaw: "r2"})
	if err != nil {
		t.Fatalf("emit 2: %v", err)
	}
	if id1 == id2 {
		t.Errorf("expected distinct proposal IDs, got %d twice", id1)
	}
	if len(m.EmitCalls) != 2 {
		t.Errorf("EmitCalls len = %d, want 2", len(m.EmitCalls))
	}
	out, err := m.ListPendingCandidates(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(out) != 2 {
		t.Errorf("ListPendingCandidates returned %d, want 2", len(out))
	}
	if m.ListPendingCalls != 1 {
		t.Errorf("ListPendingCalls = %d, want 1", m.ListPendingCalls)
	}
}
