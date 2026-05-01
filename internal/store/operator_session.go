// D3 P6A.5 — OperatorSessionState (resume-where-you-left-off).
//
// One row per operator. Stores the most recent surface, route, focused
// decision, and partial-form state JSON so the dashboard can rehydrate
// after an idle period or process restart.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// OperatorSession is the round-trip shape consumed by the dashboard.
type OperatorSession struct {
	OperatorEmail          string `json:"operator_email"`
	LastActiveAt           string `json:"last_active_at"`
	LastViewedSurface      string `json:"last_viewed_surface"`
	LastViewedRoute        string `json:"last_viewed_route"`
	LastFocusedDecisionID  int64  `json:"last_focused_decision_id"`
	PartialReviewStateJSON string `json:"partial_review_state_json"`
}

// MaxPartialReviewStateBytes — the brief specifies 32 KB. Reject larger
// payloads with 413 at the API layer.
const MaxPartialReviewStateBytes = 32 * 1024

// GetOperatorSession returns the most recent session row for the given
// operator. Returns sql.ErrNoRows when the operator has no row yet
// (treat as "fresh session"). All other errors are returned wrapped.
func GetOperatorSession(ctx context.Context, db *sql.DB, operatorEmail string) (OperatorSession, error) {
	var s OperatorSession
	err := db.QueryRowContext(ctx, `SELECT
			operator_email,
			IFNULL(last_active_at, ''),
			IFNULL(last_viewed_surface, ''),
			IFNULL(last_viewed_route, ''),
			IFNULL(last_focused_decision_id, 0),
			IFNULL(partial_review_state_json, '')
		FROM OperatorSessionState WHERE operator_email = ?`, operatorEmail).
		Scan(&s.OperatorEmail, &s.LastActiveAt, &s.LastViewedSurface,
			&s.LastViewedRoute, &s.LastFocusedDecisionID, &s.PartialReviewStateJSON)
	if err != nil {
		return OperatorSession{}, err
	}
	return s, nil
}

// SaveOperatorSession upserts the row. Bounds partial_review_state_json
// at MaxPartialReviewStateBytes — caller must reject larger payloads
// before calling. Returns ErrSessionPayloadTooLarge if the helper sees
// an oversized payload.
var ErrSessionPayloadTooLarge = errors.New("operator session: partial_review_state_json exceeds 32KB cap")

func SaveOperatorSession(ctx context.Context, db *sql.DB, s OperatorSession) error {
	if len(s.PartialReviewStateJSON) > MaxPartialReviewStateBytes {
		return ErrSessionPayloadTooLarge
	}
	if s.OperatorEmail == "" {
		return fmt.Errorf("save operator session: operator_email required")
	}
	_, err := db.ExecContext(ctx, `INSERT INTO OperatorSessionState
		(operator_email, last_active_at, last_viewed_surface, last_viewed_route,
		 last_focused_decision_id, partial_review_state_json)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(operator_email) DO UPDATE
		SET last_active_at = excluded.last_active_at,
		    last_viewed_surface = excluded.last_viewed_surface,
		    last_viewed_route = excluded.last_viewed_route,
		    last_focused_decision_id = excluded.last_focused_decision_id,
		    partial_review_state_json = excluded.partial_review_state_json`,
		s.OperatorEmail, NowSQLite(), s.LastViewedSurface, s.LastViewedRoute,
		s.LastFocusedDecisionID, s.PartialReviewStateJSON)
	if err != nil {
		return fmt.Errorf("upsert session: %w", err)
	}
	return nil
}

// ClearOperatorSession nulls out partial state and route — used by the
// "Start fresh" button.
func ClearOperatorSession(ctx context.Context, db *sql.DB, operatorEmail string) error {
	_, err := db.ExecContext(ctx, `UPDATE OperatorSessionState
		SET last_viewed_route = '',
		    last_focused_decision_id = 0,
		    partial_review_state_json = ''
		WHERE operator_email = ?`, operatorEmail)
	if err != nil {
		return fmt.Errorf("clear session: %w", err)
	}
	return nil
}

// IsSessionStale returns true when last_active_at is older than the
// supplied threshold. Used to decide whether to show the "Resume?"
// banner on dashboard load. The brief: 30 minutes idle.
const SessionStaleAfter = 30 * time.Minute

func IsSessionStale(s OperatorSession, now time.Time) bool {
	if s.LastActiveAt == "" {
		return false
	}
	t, err := ParseSQLiteTime(s.LastActiveAt)
	if err != nil {
		return false
	}
	return now.Sub(t) > SessionStaleAfter
}
