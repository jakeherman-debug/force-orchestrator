// Package store — D4 Phase 0 — hypothesis emission from quality-scored
// memories.
//
// EmitHypothesisCandidates walks FleetMemory rows whose quality
// signal exceeds configurable thresholds and produces a candidate
// PromotionProposal per row (one shot per memory; idempotent via
// hypothesis_emitted_at + source_memory_id stamping).
//
// The dog (librarian-hypothesis-emit) calls this helper directly. It
// is in the store package — not the Librarian Client — because the
// emission is pure database work (no LLM call); the Client surface
// only carries methods that need a service-layer dependency
// (RecentCommitsDigest, BootstrapSenatorRules, etc.).
package store

import (
	"context"
	"database/sql"
	"fmt"
)

// HypothesisRetrievalThreshold is the minimum retrieval_count below
// which a memory is NOT eligible to emit a hypothesis. 5 by default
// — a memory consulted at least 5 times has demonstrably been
// useful (operator-tunable via SystemConfig key, not yet wired).
var HypothesisRetrievalThreshold = 5

// HypothesisValidationThreshold is the minimum validation_score below
// which a memory is NOT eligible. 0.3 by default — corresponds to
// ~6 net positive validation signals (with the default 0.05 delta).
var HypothesisValidationThreshold = 0.3

// EmitHypothesisCandidates walks FleetMemory looking for high-signal
// memories that have not yet emitted a hypothesis, and inserts a
// candidate PromotionProposal per match. Returns the count of new
// candidates emitted.
//
// Idempotence: a memory whose hypothesis_emitted_at != '' is skipped
// (the column is stamped at emit time). A defensive secondary check
// looks at PromotionProposals.source_memory_id so a manual reset of
// hypothesis_emitted_at doesn't produce duplicates.
//
// Why store-side and not Client-side: this is a candidate-row INSERT.
// The Client.EmitCandidate path exists for the LLM-driven flow
// (Librarian-LLM authored a candidate from richer reasoning); this
// path is pure DB work driven by score thresholds. Both paths land
// in the same PromotionProposals table with kind='candidate' /
// authored_by='librarian'.
func EmitHypothesisCandidates(ctx context.Context, db *sql.DB) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	rows, err := db.QueryContext(ctx, `
		SELECT m.id, m.repo, IFNULL(m.summary, ''), IFNULL(m.topic_tags, ''),
		       IFNULL(m.retrieval_count, 0), IFNULL(m.validation_score, 0.0)
		  FROM FleetMemory m
		 WHERE IFNULL(m.canonical_id, 0) = 0
		   AND IFNULL(m.hypothesis_emitted_at, '') = ''
		   AND IFNULL(m.retrieval_count, 0) >= ?
		   AND IFNULL(m.validation_score, 0.0) >= ?
		   AND NOT EXISTS (
		       SELECT 1 FROM PromotionProposals p
		        WHERE IFNULL(p.source_memory_id, 0) = m.id
		   )
		 ORDER BY m.id`,
		HypothesisRetrievalThreshold, HypothesisValidationThreshold)
	if err != nil {
		return 0, fmt.Errorf("EmitHypothesisCandidates: query: %w", err)
	}
	type cand struct {
		id              int
		repo            string
		summary         string
		topicTags       string
		retrievalCount  int
		validationScore float64
	}
	var all []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.id, &c.repo, &c.summary, &c.topicTags,
			&c.retrievalCount, &c.validationScore); err != nil {
			rows.Close()
			return 0, fmt.Errorf("EmitHypothesisCandidates: scan: %w", err)
		}
		all = append(all, c)
	}
	if rerr := rows.Err(); rerr != nil {
		rows.Close()
		return 0, fmt.Errorf("EmitHypothesisCandidates: rows iter: %w", rerr)
	}
	rows.Close()

	emitted := 0
	for _, c := range all {
		// Insert candidate. Use a transaction so the stamp + insert
		// are atomic.
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return emitted, fmt.Errorf("EmitHypothesisCandidates: begin: %w", err)
		}
		// rule_key is a generated identifier so the candidate is
		// unique-keyed (the FleetRules unique key on rule_key,version
		// requires non-empty rule_key for promotions; for candidates
		// it's an audit-pivot only). Shape: librarian-hyp-<id>.
		ruleKey := fmt.Sprintf("librarian-hyp-%d", c.id)
		// proposed_content is the memory summary — the LLM-judge / EC
		// experiment will refine this. tags + repo are encoded in
		// evidence_summary_json so the hypothesis carries scope.
		evidence := fmt.Sprintf(
			`{"source":"librarian-quality","memory_id":%d,"repo":%q,"topic_tags":%q,"retrieval_count":%d,"validation_score":%.4f}`,
			c.id, c.repo, c.topicTags, c.retrievalCount, c.validationScore)

		var proposalID int
		err = tx.QueryRowContext(ctx, `
			INSERT INTO PromotionProposals
				(experiment_id, kind, rule_key, proposed_content,
				 evidence_summary_json, authored_by, authored_at, source_memory_id)
			VALUES (0, 'candidate', ?, ?, ?, 'librarian', datetime('now'), ?)
			RETURNING id`,
			ruleKey, c.summary, evidence, c.id).Scan(&proposalID)
		if err != nil {
			tx.Rollback() //nolint:errcheck
			return emitted, fmt.Errorf("EmitHypothesisCandidates: insert candidate for memory %d: %w", c.id, err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE FleetMemory SET hypothesis_emitted_at = datetime('now') WHERE id = ?`,
			c.id); err != nil {
			tx.Rollback() //nolint:errcheck
			return emitted, fmt.Errorf("EmitHypothesisCandidates: stamp memory %d: %w", c.id, err)
		}
		if err := tx.Commit(); err != nil {
			return emitted, fmt.Errorf("EmitHypothesisCandidates: commit memory %d: %w", c.id, err)
		}
		emitted++
	}
	return emitted, nil
}
