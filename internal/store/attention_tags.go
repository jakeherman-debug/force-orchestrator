// D3 P6A.14 — Operator attention tags.
//
// Operator marks any convoy / feature / agent / FleetRule as following
// (high attention — events ping with banner notifications) or muted
// (events route to digest only). Default is normal.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// AttentionLevel — three-state ladder.
type AttentionLevel string

const (
	AttentionFollowing AttentionLevel = "following"
	AttentionNormal    AttentionLevel = "normal"
	AttentionMuted     AttentionLevel = "muted"
)

func (a AttentionLevel) Valid() bool {
	switch a {
	case AttentionFollowing, AttentionNormal, AttentionMuted:
		return true
	}
	return false
}

// AttentionTag is the round-trip shape consumed by the dashboard.
type AttentionTag struct {
	OperatorEmail  string `json:"operator_email"`
	TargetKind     string `json:"target_kind"`
	TargetID       string `json:"target_id"`
	AttentionLevel string `json:"attention_level"`
	SetAt          string `json:"set_at"`
	Rationale      string `json:"rationale"`
}

// SetAttentionTag upserts the tag. Muted level requires a rationale
// (the brief: rationale required when 'muted').
func SetAttentionTag(ctx context.Context, db *sql.DB, t AttentionTag) error {
	level := AttentionLevel(t.AttentionLevel)
	if !level.Valid() {
		return fmt.Errorf("invalid attention level: %q", t.AttentionLevel)
	}
	if level == AttentionMuted && len(t.Rationale) < 5 {
		return errors.New("muted level requires rationale (>=5 chars)")
	}
	if t.OperatorEmail == "" || t.TargetKind == "" || t.TargetID == "" {
		return errors.New("operator + target kind + target id required")
	}
	_, err := db.ExecContext(ctx, `INSERT INTO OperatorAttentionTags
		(operator_email, target_kind, target_id, attention_level, set_at, rationale)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(operator_email, target_kind, target_id) DO UPDATE
		SET attention_level = excluded.attention_level,
		    set_at = excluded.set_at,
		    rationale = excluded.rationale`,
		t.OperatorEmail, t.TargetKind, t.TargetID, t.AttentionLevel,
		NowSQLite(), t.Rationale)
	if err != nil {
		return fmt.Errorf("upsert attention tag: %w", err)
	}
	return nil
}

// GetAttentionTag returns the tag for a (operator, kind, id) triple.
// If none exists, returns ('normal', no rationale).
func GetAttentionTag(ctx context.Context, db *sql.DB, operatorEmail, targetKind, targetID string) (AttentionTag, error) {
	var t AttentionTag
	err := db.QueryRowContext(ctx, `SELECT operator_email, target_kind, target_id,
			attention_level, IFNULL(set_at, ''), IFNULL(rationale, '')
		FROM OperatorAttentionTags
		WHERE operator_email = ? AND target_kind = ? AND target_id = ?`,
		operatorEmail, targetKind, targetID).
		Scan(&t.OperatorEmail, &t.TargetKind, &t.TargetID, &t.AttentionLevel, &t.SetAt, &t.Rationale)
	if errors.Is(err, sql.ErrNoRows) {
		return AttentionTag{
			OperatorEmail:  operatorEmail,
			TargetKind:     targetKind,
			TargetID:       targetID,
			AttentionLevel: string(AttentionNormal),
		}, nil
	}
	if err != nil {
		return AttentionTag{}, fmt.Errorf("get attention tag: %w", err)
	}
	return t, nil
}

// ListAttentionTags returns every tag for an operator.
func ListAttentionTags(ctx context.Context, db *sql.DB, operatorEmail string) ([]AttentionTag, error) {
	rows, err := db.QueryContext(ctx, `SELECT operator_email, target_kind, target_id,
			attention_level, IFNULL(set_at, ''), IFNULL(rationale, '')
		FROM OperatorAttentionTags WHERE operator_email = ?
		ORDER BY target_kind, target_id`, operatorEmail)
	if err != nil {
		return nil, fmt.Errorf("query tags: %w", err)
	}
	defer rows.Close()
	var out []AttentionTag
	for rows.Next() {
		var t AttentionTag
		if err := rows.Scan(&t.OperatorEmail, &t.TargetKind, &t.TargetID, &t.AttentionLevel, &t.SetAt, &t.Rationale); err != nil {
			return nil, fmt.Errorf("scan tag: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iter tags: %w", err)
	}
	return out, nil
}
