// D3 P6A.13 — Cooldown scheduler for high-stakes auto-execute.
//
// When a high-stakes auto-execute decision lands (Council-approved
// auto-merge on a critical convoy, Medic auto-fix on a critical
// convoy, etc.), the action gets a 60-second cooldown banner. The
// operator can pause, skip, or let it execute.
package agents

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"force-orchestrator/internal/store"
)

const CooldownDuration = 60 * time.Second

// CooldownPause is the round-trip shape consumed by the dashboard.
type CooldownPause struct {
	ID                int64  `json:"id"`
	DecisionID        int64  `json:"decision_id"`
	DecisionKind      string `json:"decision_kind"`
	ScheduledActionAt string `json:"scheduled_action_at"`
	PausedAt          string `json:"paused_at"`
	PausedByEmail     string `json:"paused_by_email"`
	ResumedAt         string `json:"resumed_at"`
	CancelledAt       string `json:"cancelled_at"`
	ExecutedAt        string `json:"executed_at"`
}

// ScheduleCooldown inserts a CooldownPauses row 60 seconds in the
// future. Pattern P30 enforces every high-stakes auto-execute call
// site routes through this helper.
func ScheduleCooldown(ctx context.Context, db *sql.DB, decisionKind string, decisionID int64) (int64, error) {
	scheduled := time.Now().Add(CooldownDuration).UTC().Format("2006-01-02 15:04:05")
	res, err := db.ExecContext(ctx, `INSERT INTO CooldownPauses
		(decision_id, decision_kind, scheduled_action_at) VALUES (?, ?, ?)`,
		decisionID, decisionKind, scheduled)
	if err != nil {
		return 0, fmt.Errorf("schedule cooldown: %w", err)
	}
	return res.LastInsertId()
}

// PauseCooldown pauses an in-flight cooldown. Idempotent: re-pausing a
// paused row is a no-op.
func PauseCooldown(ctx context.Context, db *sql.DB, id int64, byEmail string) error {
	_, err := db.ExecContext(ctx, `UPDATE CooldownPauses
		SET paused_at = ?, paused_by_email = ?
		WHERE id = ? AND executed_at = '' AND cancelled_at = '' AND paused_at = ''`,
		store.NowSQLite(), byEmail, id)
	if err != nil {
		return fmt.Errorf("pause cooldown: %w", err)
	}
	return nil
}

// ResumeCooldown resumes a paused cooldown. Requires a rationale.
func ResumeCooldown(ctx context.Context, db *sql.DB, id int64, rationale string) error {
	if len(rationale) < 5 {
		return errors.New("resume cooldown: rationale required (>=5 chars)")
	}
	_, err := db.ExecContext(ctx, `UPDATE CooldownPauses
		SET resumed_at = ?, scheduled_action_at = ?
		WHERE id = ? AND paused_at != '' AND executed_at = '' AND cancelled_at = ''`,
		store.NowSQLite(), time.Now().Add(CooldownDuration).UTC().Format("2006-01-02 15:04:05"), id)
	if err != nil {
		return fmt.Errorf("resume cooldown: %w", err)
	}
	return nil
}

// CancelCooldown cancels a cooldown — the action will not execute.
func CancelCooldown(ctx context.Context, db *sql.DB, id int64) error {
	_, err := db.ExecContext(ctx, `UPDATE CooldownPauses
		SET cancelled_at = ? WHERE id = ? AND executed_at = ''`,
		store.NowSQLite(), id)
	if err != nil {
		return fmt.Errorf("cancel cooldown: %w", err)
	}
	return nil
}

// MarkCooldownExecuted is called by the dispatcher when the action
// fires (either after the cooldown elapses or after explicit skip).
func MarkCooldownExecuted(ctx context.Context, db *sql.DB, id int64) error {
	_, err := db.ExecContext(ctx, `UPDATE CooldownPauses
		SET executed_at = ? WHERE id = ?`, store.NowSQLite(), id)
	if err != nil {
		return fmt.Errorf("mark executed: %w", err)
	}
	return nil
}

// ListPendingCooldowns returns rows that are scheduled but not paused,
// cancelled, or executed.
func ListPendingCooldowns(ctx context.Context, db *sql.DB) ([]CooldownPause, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, decision_id, decision_kind, scheduled_action_at,
			IFNULL(paused_at, ''), IFNULL(paused_by_email, ''), IFNULL(resumed_at, ''),
			IFNULL(cancelled_at, ''), IFNULL(executed_at, '')
		FROM CooldownPauses
		WHERE executed_at = '' AND cancelled_at = ''
		ORDER BY scheduled_action_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("query pending: %w", err)
	}
	defer rows.Close()
	var out []CooldownPause
	for rows.Next() {
		var c CooldownPause
		if err := rows.Scan(&c.ID, &c.DecisionID, &c.DecisionKind, &c.ScheduledActionAt,
			&c.PausedAt, &c.PausedByEmail, &c.ResumedAt, &c.CancelledAt, &c.ExecutedAt); err != nil {
			return nil, fmt.Errorf("scan pending: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iter pending: %w", err)
	}
	return out, nil
}
