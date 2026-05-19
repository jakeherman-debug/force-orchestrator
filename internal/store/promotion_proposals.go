// Package store: PromotionProposals query helpers. D14 Phase 5.
//
// Provides ListPendingPromotionProposals and the status-update helpers
// used by the MigrationClassifyProposals agent. Every mutator returns
// error per CLAUDE.md § "No silent failures".
package store

import (
	"database/sql"
	"fmt"
)

// PromotionProposalRow is the in-memory shape returned by
// ListPendingPromotionProposals. Fields mirror the PromotionProposals
// table columns that are relevant to the D14 Phase 5 classifier.
type PromotionProposalRow struct {
	ID                   int
	ExperimentID         int
	Kind                 string
	RuleKey              string
	ProposedContent      string
	EvidenceSummaryJSON  string
	AuthoredBy           string
	AuthoredAt           string
	ClassificationStatus string // '' = unclassified
	SuggestedScope       string
}

// ListPendingPromotionProposals returns every PromotionProposals row that
// has not yet been ratified, rejected, or classified by the D14 migration
// agent. "Pending" is defined as:
//
//	ratified_at = ''  (operator has not ratified)
//	rejected_at = ''  (operator has not rejected)
//	classification_status = ''  (agent has not classified yet)
//
// Ordered by id ASC so batching is deterministic (oldest-first).
func ListPendingPromotionProposals(db *sql.DB) ([]PromotionProposalRow, error) {
	rows, err := db.Query(`
		SELECT id, experiment_id, IFNULL(kind,''), IFNULL(rule_key,''),
		       IFNULL(proposed_content,''), IFNULL(evidence_summary_json,'{}'),
		       IFNULL(authored_by,''), IFNULL(authored_at,''),
		       IFNULL(classification_status,''), IFNULL(suggested_scope,'')
		  FROM PromotionProposals
		 WHERE IFNULL(ratified_at,'') = ''
		   AND IFNULL(rejected_at,'') = ''
		   AND IFNULL(classification_status,'') = ''
		 ORDER BY id ASC`)
	if err != nil {
		return nil, fmt.Errorf("ListPendingPromotionProposals: %w", err)
	}
	defer rows.Close()
	var out []PromotionProposalRow
	for rows.Next() {
		var r PromotionProposalRow
		if scanErr := rows.Scan(
			&r.ID, &r.ExperimentID, &r.Kind, &r.RuleKey,
			&r.ProposedContent, &r.EvidenceSummaryJSON,
			&r.AuthoredBy, &r.AuthoredAt,
			&r.ClassificationStatus, &r.SuggestedScope,
		); scanErr != nil {
			return nil, fmt.Errorf("ListPendingPromotionProposals: scan: %w", scanErr)
		}
		out = append(out, r)
	}
	if rErr := rows.Err(); rErr != nil {
		return nil, fmt.Errorf("ListPendingPromotionProposals: rows.Err: %w", rErr)
	}
	return out, nil
}

// SetProposalClassification updates a single PromotionProposals row's
// classification_status and suggested_scope. Called by the
// MigrationClassifyProposals agent after each LLM classification.
//
// Valid classification_status values:
//   - "absorbed_as_knowledge"  — fact absorbed into SenateMemory
//   - "awaiting_scope_review"  — enforceable rule awaiting operator scope sign-off
func SetProposalClassification(db *sql.DB, proposalID int, classificationStatus, suggestedScope string) error {
	switch classificationStatus {
	case "absorbed_as_knowledge", "awaiting_scope_review":
	default:
		return fmt.Errorf("SetProposalClassification(id=%d): invalid status %q", proposalID, classificationStatus)
	}
	res, err := db.Exec(`
		UPDATE PromotionProposals
		   SET classification_status = ?,
		       suggested_scope       = ?
		 WHERE id = ?
		   AND IFNULL(classification_status,'') = ''`,
		classificationStatus, suggestedScope, proposalID)
	if err != nil {
		return fmt.Errorf("SetProposalClassification(id=%d): %w", proposalID, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Already classified or row gone — idempotent no-op.
		return nil
	}
	return nil
}
