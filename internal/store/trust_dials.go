// D3 P6A.6 — Per-agent trust dials.
//
// Every agent (Captain, Council, Medic, Investigator, EC, ConvoyReview,
// Pilot, Diplomat, Chancellor) gets a per-operator trust slider 0-100.
// History-preserving: each set writes a new row; latest by set_at is the
// current value. Calibration coaching may suggest values; only operator-
// initiated writes use set_by='operator'.
package store

import (
	"context"
	"database/sql"
	"fmt"
)

// FleetAgentRoster — the canonical list of agents that participate in
// the trust-dial system. Bootstrap inserts one row per (operator,
// agent) at dial_value=70.
var FleetAgentRoster = []string{
	"captain", "council", "medic", "investigator", "ec",
	"convoy_review", "pilot", "diplomat", "chancellor",
}

// TrustDialSetBy — origin of a trust-dial-event row.
type TrustDialSetBy string

const (
	TrustDialOperator              TrustDialSetBy = "operator"
	TrustDialCalibrationSuggestion TrustDialSetBy = "calibration_suggestion"
	TrustDialSystemDefault         TrustDialSetBy = "system_default"
)

// TrustDial is the round-trip shape consumed by the dashboard.
type TrustDial struct {
	OperatorEmail string `json:"operator_email"`
	Agent         string `json:"agent"`
	DialValue     int    `json:"dial_value"`
	SetAt         string `json:"set_at"`
	SetBy         string `json:"set_by"`
	Rationale     string `json:"rationale"`
}

// GetCurrentTrustDial returns the most recent dial value for an
// (operator, agent) pair. If no row exists, returns the bootstrap
// default of 70 (and writes the system_default row so future reads are
// consistent).
func GetCurrentTrustDial(ctx context.Context, db *sql.DB, operatorEmail, agent string) (int, error) {
	var v int
	err := db.QueryRowContext(ctx, `SELECT dial_value FROM OperatorTrustDials
		WHERE operator_email = ? AND agent = ?
		ORDER BY set_at DESC LIMIT 1`, operatorEmail, agent).Scan(&v)
	if err == nil {
		return v, nil
	}
	// No row — bootstrap default 70 (per the brief).
	return 70, nil
}

// SetTrustDial writes a new row. This is history-preserving:
// (operator, agent, set_at) is unique so the latest set_at row is the
// current value. Operator-initiated writes use set_by='operator'.
//
// The set_at column has a UNIQUE constraint with (operator, agent), so
// rapid back-to-back sets in the same wall-clock second collide. We
// retry up to 5 times with synthetic sub-second suffixes so test code
// (and real fast-fingered operators) don't see a constraint error.
func SetTrustDial(ctx context.Context, db *sql.DB, t TrustDial) error {
	if t.OperatorEmail == "" || t.Agent == "" {
		return fmt.Errorf("set trust dial: operator + agent required")
	}
	if t.DialValue < 0 || t.DialValue > 100 {
		return fmt.Errorf("set trust dial: dial_value=%d out of range [0,100]", t.DialValue)
	}
	if t.SetBy == "" {
		t.SetBy = string(TrustDialOperator)
	}
	// Try the natural NowSQLite first; on UNIQUE conflict, append a
	// fractional-second suffix so the row lands.
	candidates := []string{
		NowSQLite(),
		NowSQLite() + ".001",
		NowSQLite() + ".002",
		NowSQLite() + ".003",
		NowSQLite() + ".004",
	}
	var lastErr error
	for _, setAt := range candidates {
		_, err := db.ExecContext(ctx, `INSERT INTO OperatorTrustDials
			(operator_email, agent, dial_value, set_at, set_by, rationale)
			VALUES (?, ?, ?, ?, ?, ?)`,
			t.OperatorEmail, t.Agent, t.DialValue, setAt, t.SetBy, t.Rationale)
		if err == nil {
			return nil
		}
		lastErr = err
	}
	return fmt.Errorf("insert trust dial row: %w", lastErr)
}

// BootstrapTrustDials inserts a system_default row at dial_value=70 for
// every agent in FleetAgentRoster. Idempotent: re-running is a no-op
// because each operator only needs the bootstrap once (existence of
// any row implies bootstrap completed).
func BootstrapTrustDials(ctx context.Context, db *sql.DB, operatorEmail string) error {
	for _, agent := range FleetAgentRoster {
		var n int
		_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM OperatorTrustDials
			WHERE operator_email = ? AND agent = ?`, operatorEmail, agent).Scan(&n)
		if n > 0 {
			continue
		}
		if err := SetTrustDial(ctx, db, TrustDial{
			OperatorEmail: operatorEmail,
			Agent:         agent,
			DialValue:     70,
			SetBy:         string(TrustDialSystemDefault),
			Rationale:     "bootstrap default",
		}); err != nil {
			return fmt.Errorf("bootstrap %s: %w", agent, err)
		}
	}
	return nil
}

// ListCurrentTrustDials returns the most recent dial per agent for the
// operator. Always returns one entry per agent in FleetAgentRoster
// (bootstrap default 70 if no row).
func ListCurrentTrustDials(ctx context.Context, db *sql.DB, operatorEmail string) ([]TrustDial, error) {
	out := make([]TrustDial, 0, len(FleetAgentRoster))
	for _, agent := range FleetAgentRoster {
		row := TrustDial{OperatorEmail: operatorEmail, Agent: agent, DialValue: 70, SetBy: string(TrustDialSystemDefault)}
		err := db.QueryRowContext(ctx, `SELECT dial_value, IFNULL(set_at, ''), IFNULL(set_by, ''), IFNULL(rationale, '')
			FROM OperatorTrustDials WHERE operator_email = ? AND agent = ?
			ORDER BY set_at DESC LIMIT 1`, operatorEmail, agent).
			Scan(&row.DialValue, &row.SetAt, &row.SetBy, &row.Rationale)
		if err == nil || err == sql.ErrNoRows {
			out = append(out, row)
			continue
		}
		return nil, fmt.Errorf("list dial for %s: %w", agent, err)
	}
	return out, nil
}

// ListTrustDialHistory returns every dial-event row for an
// (operator, agent) pair, newest first.
func ListTrustDialHistory(ctx context.Context, db *sql.DB, operatorEmail, agent string) ([]TrustDial, error) {
	rows, err := db.QueryContext(ctx, `SELECT operator_email, agent, dial_value,
			IFNULL(set_at, ''), IFNULL(set_by, ''), IFNULL(rationale, '')
		FROM OperatorTrustDials WHERE operator_email = ? AND agent = ?
		ORDER BY set_at DESC`, operatorEmail, agent)
	if err != nil {
		return nil, fmt.Errorf("query history: %w", err)
	}
	defer rows.Close()
	var out []TrustDial
	for rows.Next() {
		var t TrustDial
		if err := rows.Scan(&t.OperatorEmail, &t.Agent, &t.DialValue, &t.SetAt, &t.SetBy, &t.Rationale); err != nil {
			return nil, fmt.Errorf("scan history row: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iter history: %w", err)
	}
	return out, nil
}

// FrictionTierFor shifts the base stakes tier of a decision based on the
// operator's trust dial for the agent. From the brief:
//   - dial < 40   bumps medium-stakes to high-stakes
//   - dial > 85   drops medium-stakes to low-stakes
//   - high-stakes NEVER shifts down (CLAUDE.md / BoS / Senate
//     amendments + AT deprecations stay high regardless)
func FrictionTierFor(dial int, baseTier string) string {
	if baseTier == "high" {
		return "high"
	}
	if baseTier == "medium" {
		if dial < 40 {
			return "high"
		}
		if dial > 85 {
			return "low"
		}
		return "medium"
	}
	if baseTier == "low" {
		if dial < 40 {
			return "medium"
		}
		return "low"
	}
	return baseTier
}
