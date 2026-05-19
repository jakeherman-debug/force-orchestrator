// Package store: SenateReview — one row per (Feature, Senator) verdict.
// D4 Phase 3.
//
// The Senate router fans out one review per active Senator for each
// Feature whose plan reaches AwaitingSenateReview. Each Senator's
// verdict is persisted here so the Chancellor's downstream decision
// (auto-approve, amend, escalate) is fully auditable.
//
// Schema lives in schema.go (createSchema + runMigrations) and
// schema/schema.sql. Every mutator returns error per CLAUDE.md
// § "No silent failures".
package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// SenateReviewRow is the in-memory shape of one SenateReview row.
type SenateReviewRow struct {
	ID         int
	FeatureID  int
	Senator    string
	Position   string  // 'concur' | 'amend' | 'dissent'
	Concerns   string  // JSON array
	Amendments string  // JSON array
	Rationale  string
	Confidence float64 // [0, 1]
	CreatedAt  string
}

// InsertSenateReview persists one Senator verdict against a Feature.
// FeatureID + Senator + Position are required.
func InsertSenateReview(db *sql.DB, r SenateReviewRow) (int, error) {
	if r.FeatureID == 0 {
		return 0, errors.New("InsertSenateReview: FeatureID required")
	}
	if r.Senator == "" {
		return 0, errors.New("InsertSenateReview: Senator required")
	}
	switch r.Position {
	case "concur", "amend", "dissent":
	default:
		return 0, fmt.Errorf("InsertSenateReview: invalid position %q", r.Position)
	}
	if r.Concerns == "" {
		r.Concerns = "[]"
	}
	if r.Amendments == "" {
		r.Amendments = "[]"
	}
	res, err := db.Exec(`
		INSERT INTO SenateReview
			(feature_id, senator, position, concerns, amendments, rationale, confidence)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		r.FeatureID, r.Senator, r.Position, r.Concerns, r.Amendments, r.Rationale, r.Confidence)
	if err != nil {
		return 0, fmt.Errorf("InsertSenateReview(feature=%d, senator=%s): %w", r.FeatureID, r.Senator, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("InsertSenateReview(feature=%d, senator=%s): LastInsertId: %w", r.FeatureID, r.Senator, err)
	}
	return int(id), nil
}

// GetSenateReviewPassCount returns the current review_pass_count for a Feature
// (the number of Senate re-review passes triggered so far by material amendments).
// Returns 0 if the row does not exist or the column is NULL.
func GetSenateReviewPassCount(db *sql.DB, featureID int) (int, error) {
	var count int
	err := db.QueryRow(`
		SELECT IFNULL(review_pass_count, 0)
		  FROM BountyBoard
		 WHERE id = ?`, featureID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("GetSenateReviewPassCount(feature=%d): %w", featureID, err)
	}
	return count, nil
}

// IncrementSenateReviewPassCount atomically increments review_pass_count for a
// Feature row and returns the updated value. The increment is unconditional —
// callers must read the count first with GetSenateReviewPassCount to enforce
// the cap before calling this. Returns error on any DB write failure.
func IncrementSenateReviewPassCount(db *sql.DB, featureID int) (int, error) {
	_, err := db.Exec(`
		UPDATE BountyBoard
		   SET review_pass_count = IFNULL(review_pass_count, 0) + 1
		 WHERE id = ?`, featureID)
	if err != nil {
		return 0, fmt.Errorf("IncrementSenateReviewPassCount(feature=%d): %w", featureID, err)
	}
	// Re-read the updated value so the caller has the confirmed post-increment count.
	count, err := GetSenateReviewPassCount(db, featureID)
	if err != nil {
		return 0, fmt.Errorf("IncrementSenateReviewPassCount(feature=%d): re-read after increment: %w", featureID, err)
	}
	return count, nil
}

// ListSenateReviewsForFeature returns every Senator's verdict on the
// given Feature, ordered by senator name (stable, idempotent for
// dashboard rendering).
func ListSenateReviewsForFeature(db *sql.DB, featureID int) ([]SenateReviewRow, error) {
	rows, err := db.Query(`
		SELECT id, feature_id, senator, position,
		       IFNULL(concerns,'[]'), IFNULL(amendments,'[]'),
		       IFNULL(rationale,''), IFNULL(confidence, 0),
		       IFNULL(created_at,'')
		  FROM SenateReview
		 WHERE feature_id = ?
		 ORDER BY senator ASC, id ASC`, featureID)
	if err != nil {
		return nil, fmt.Errorf("ListSenateReviewsForFeature(%d): %w", featureID, err)
	}
	defer rows.Close()
	var out []SenateReviewRow
	for rows.Next() {
		var r SenateReviewRow
		if scanErr := rows.Scan(&r.ID, &r.FeatureID, &r.Senator, &r.Position,
			&r.Concerns, &r.Amendments, &r.Rationale, &r.Confidence,
			&r.CreatedAt); scanErr != nil {
			return nil, fmt.Errorf("ListSenateReviewsForFeature(%d): scan: %w", featureID, scanErr)
		}
		out = append(out, r)
	}
	if rErr := rows.Err(); rErr != nil {
		return nil, fmt.Errorf("ListSenateReviewsForFeature(%d): rows.Err: %w", featureID, rErr)
	}
	return out, nil
}
