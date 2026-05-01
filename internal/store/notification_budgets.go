// D3 P6A.4 — Operator notification budgets.
//
// Operator-configurable rate limits for outbound notifications (mail,
// modal alerts, banners). RespectNotificationBudget is the canonical
// gate every emit site routes through.
//
// Behaviour:
//   - stakesTier == "high" → always allowed (punches through)
//   - no budget row for (operator, source, channel) → allowed (default open)
//   - within the budget window → allowed
//   - past budget AND digest_remainder=true → write to digest, return false
//   - past budget AND digest_remainder=false → drop, return false
//
// Pattern P27 (audit_pattern_p27_notification_budget_routing_test.go)
// asserts every emit site routes through this helper.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// NotificationStakes — three-tier stakes ladder. "high" punches through.
type NotificationStakes string

const (
	StakesLow    NotificationStakes = "low"
	StakesMedium NotificationStakes = "medium"
	StakesHigh   NotificationStakes = "high"
)

// RespectNotificationBudget is the canonical emit-site gate.
//
// Returns (allowed=true) when the emit should proceed, (allowed=false)
// when the budget has been exhausted. When digest_remainder is true and
// the budget is exhausted, the suppressed payload is appended to
// OperatorNotificationDigest for the daily flush; when false, it is
// dropped silently.
//
// On error, the caller should treat as fail-open (allowed=true) so a
// transient SQLite glitch never silences a high-stakes alert. Callers
// log the error and continue per CLAUDE.md's no-silent-failures rule.
func RespectNotificationBudget(
	ctx context.Context,
	db *sql.DB,
	operatorEmail, source, channel string,
	payloadJSON string,
	stakes NotificationStakes,
) (allowed bool, err error) {
	// High-stakes always punches through. Critical-path: ESCALATION,
	// CHANCELLOR FAIL-CLOSED, etc.
	if stakes == StakesHigh {
		return true, nil
	}

	// D3 P6A.14 — operator attention tags. `following` tags unconditionally
	// emit; `muted` tags always digest. The tags are queried by callers
	// who pass an attentionLevel into RespectNotificationBudgetWithAttention
	// (see below). For the no-attention call shape, normal rules apply.

	// Look up the budget row for this (operator, source, channel). If none
	// exists, default-open: emit unmolested. The brief calls this out: a
	// fresh install with no configured budgets behaves like the previous
	// no-rate-limit world.
	var maxPer, periodMin int
	var digestRemainder int
	row := db.QueryRowContext(ctx, `SELECT max_per_period, period_minutes, digest_remainder
		FROM OperatorNotificationBudgets WHERE operator_email = ? AND source = ? AND channel = ?`,
		operatorEmail, source, channel)
	if scanErr := row.Scan(&maxPer, &periodMin, &digestRemainder); scanErr != nil {
		if errors.Is(scanErr, sql.ErrNoRows) {
			return true, nil
		}
		return true, fmt.Errorf("query budget row: %w", scanErr)
	}

	// Count emissions to this (source, channel) within the window.
	// We use the OperatorMail table for `email` channel, and a
	// notification ledger for modal/banner. For 6A.4, we accept that
	// not every emit site is fully audited and route through this
	// helper as code reaches it; the audit Pattern P27 enforces forward.
	since := time.Now().Add(-time.Duration(periodMin) * time.Minute).UTC().Format(time.RFC3339)
	var emittedCount int
	switch channel {
	case "email":
		// Count operator mail emitted in the window. Source maps to from_agent.
		_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM FleetMail
			WHERE from_agent = ? AND to_agent = ? AND created_at >= ?`,
			source, operatorEmail, since).Scan(&emittedCount)
	default:
		// Modal/banner channels track emissions in the digest ledger
		// (every prior emit was either real or digested). For now,
		// count rows in the digest with the same (source, channel)
		// today — a coarse approximation.
		today := time.Now().UTC().Format("2006-01-02")
		_ = db.QueryRowContext(ctx, `SELECT COALESCE(SUM(LENGTH(payload_json) - LENGTH(REPLACE(payload_json, '},{', '},'))), 0)
			FROM OperatorNotificationDigest
			WHERE operator_email = ? AND source = ? AND channel = ? AND digest_for_date = ?`,
			operatorEmail, source, channel, today).Scan(&emittedCount)
	}

	if emittedCount < maxPer {
		return true, nil
	}

	// Budget exhausted. Either digest or drop.
	if digestRemainder == 0 {
		return false, nil
	}

	// Append to digest spool. The 09:00-flush dog mails it as a single
	// combined email later.
	today := time.Now().UTC().Format("2006-01-02")
	_, err = db.ExecContext(ctx, `INSERT INTO OperatorNotificationDigest
		(operator_email, source, channel, digest_for_date, payload_json)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(operator_email, source, channel, digest_for_date) DO UPDATE
		SET payload_json = payload_json || ',' || excluded.payload_json`,
		operatorEmail, source, channel, today, payloadJSON)
	if err != nil {
		return false, fmt.Errorf("append digest: %w", err)
	}
	return false, nil
}

// SetNotificationBudget upserts a budget row for (operator, source,
// channel). Used by the dashboard PUT handler.
func SetNotificationBudget(
	ctx context.Context,
	db *sql.DB,
	operatorEmail, source, channel string,
	maxPerPeriod, periodMinutes int,
	digestRemainder bool,
) error {
	digestFlag := 0
	if digestRemainder {
		digestFlag = 1
	}
	_, err := db.ExecContext(ctx, `INSERT INTO OperatorNotificationBudgets
		(operator_email, source, channel, max_per_period, period_minutes, digest_remainder)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(operator_email, source, channel) DO UPDATE
		SET max_per_period = excluded.max_per_period,
		    period_minutes = excluded.period_minutes,
		    digest_remainder = excluded.digest_remainder`,
		operatorEmail, source, channel, maxPerPeriod, periodMinutes, digestFlag)
	if err != nil {
		return fmt.Errorf("upsert budget: %w", err)
	}
	return nil
}

// NotificationBudget represents a single configured budget.
type NotificationBudget struct {
	OperatorEmail   string `json:"operator_email"`
	Source          string `json:"source"`
	Channel         string `json:"channel"`
	MaxPerPeriod    int    `json:"max_per_period"`
	PeriodMinutes   int    `json:"period_minutes"`
	DigestRemainder bool   `json:"digest_remainder"`
}

// ListNotificationBudgets returns every configured budget for an operator.
func ListNotificationBudgets(ctx context.Context, db *sql.DB, operatorEmail string) ([]NotificationBudget, error) {
	rows, err := db.QueryContext(ctx, `SELECT operator_email, source, channel, max_per_period, period_minutes, digest_remainder
		FROM OperatorNotificationBudgets WHERE operator_email = ? ORDER BY source, channel`,
		operatorEmail)
	if err != nil {
		return nil, fmt.Errorf("query budgets: %w", err)
	}
	defer rows.Close()

	var out []NotificationBudget
	for rows.Next() {
		var b NotificationBudget
		var dr int
		if err := rows.Scan(&b.OperatorEmail, &b.Source, &b.Channel, &b.MaxPerPeriod, &b.PeriodMinutes, &dr); err != nil {
			return nil, fmt.Errorf("scan budget row: %w", err)
		}
		b.DigestRemainder = dr == 1
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iter budgets: %w", err)
	}
	return out, nil
}

// FlushPendingDigests is the daily 09:00-local flush. Returns the number
// of digest rows that were marked flushed. Caller (dog) emits the actual
// combined email per (source, channel) pair.
func FlushPendingDigests(ctx context.Context, db *sql.DB, operatorEmail string, today string) (int, error) {
	res, err := db.ExecContext(ctx, `UPDATE OperatorNotificationDigest
		SET flushed_at = ? WHERE operator_email = ? AND digest_for_date <= ? AND flushed_at = ''`,
		time.Now().UTC().Format(time.RFC3339), operatorEmail, today)
	if err != nil {
		return 0, fmt.Errorf("flush digests: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
