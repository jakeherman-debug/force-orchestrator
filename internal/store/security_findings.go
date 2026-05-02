// Package store: SecurityFindings — shared persistence for Bureau of
// Standards (BoS, D4 Phase 1) and Imperial Security Bureau (ISB, D4 Phase 2).
//
// Schema lives in schema.go (createSchema + runMigrations) and
// schema/schema.sql per CLAUDE.md § "Store / schema conventions". This
// file is the operator-facing helper layer: insert / list / set
// disposition. Every mutator returns error per CLAUDE.md § "No silent
// failures".
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
)

// SecurityFinding is the in-memory shape of one row.
type SecurityFinding struct {
	ID            int
	TaskID        int
	Bureau        string // 'BoS' | 'ISB'
	RuleID        string // 'BOS-001' | 'ISB-002' | ...
	Severity      string // 'advise' | 'block'
	FilePath      string
	LineNumber    int
	Message       string
	CommitSHA     string
	Disposition   string // ''|'overridden'|'resolved'|'suppressed'|'closed'
	BypassAuditID string // AUDIT-NNN when Disposition='overridden'
	BypassReason  string // >= 10 chars when Disposition='overridden'
	CreatedAt     string
	ResolvedAt    string
}

// InsertSecurityFinding records one finding. Returns the new row id, or
// (0, error) on insert failure. Caller is responsible for setting
// Disposition='overridden' + BypassAuditID + BypassReason when a bypass
// comment was matched on the violating line.
func InsertSecurityFinding(db *sql.DB, f SecurityFinding) (int, error) {
	if f.RuleID == "" {
		return 0, errors.New("InsertSecurityFinding: RuleID is required")
	}
	if f.Bureau == "" {
		f.Bureau = "BoS"
	}
	if f.Severity == "" {
		f.Severity = "advise"
	}
	res, err := db.Exec(`
		INSERT INTO SecurityFindings (
			task_id, bureau, rule_id, severity, file_path, line_number,
			message, commit_sha, disposition, bypass_audit_id, bypass_reason
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		f.TaskID, f.Bureau, f.RuleID, f.Severity, f.FilePath, f.LineNumber,
		f.Message, f.CommitSHA, f.Disposition, f.BypassAuditID, f.BypassReason)
	if err != nil {
		return 0, fmt.Errorf("InsertSecurityFinding(%s): %w", f.RuleID, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("InsertSecurityFinding(%s): LastInsertId: %w", f.RuleID, err)
	}
	return int(id), nil
}

// ListSecurityFindings returns every finding for a given task. Used by
// the BoS reviewer to compute "any block-severity findings remain after
// bypass downgrades?" verdict. Empty (nil, nil) on no matches.
func ListSecurityFindings(db *sql.DB, taskID int) ([]SecurityFinding, error) {
	rows, err := db.Query(`
		SELECT id, task_id, bureau, rule_id, severity, file_path,
		       line_number, message, commit_sha,
		       IFNULL(disposition,''), IFNULL(bypass_audit_id,''),
		       IFNULL(bypass_reason,''),
		       IFNULL(created_at,''), IFNULL(resolved_at,'')
		FROM SecurityFindings
		WHERE task_id = ?
		ORDER BY id ASC`, taskID)
	if err != nil {
		return nil, fmt.Errorf("ListSecurityFindings(task=%d): %w", taskID, err)
	}
	defer rows.Close()
	var out []SecurityFinding
	for rows.Next() {
		var f SecurityFinding
		if scanErr := rows.Scan(&f.ID, &f.TaskID, &f.Bureau, &f.RuleID, &f.Severity,
			&f.FilePath, &f.LineNumber, &f.Message, &f.CommitSHA,
			&f.Disposition, &f.BypassAuditID, &f.BypassReason,
			&f.CreatedAt, &f.ResolvedAt); scanErr != nil {
			return nil, fmt.Errorf("ListSecurityFindings(task=%d): scan: %w", taskID, scanErr)
		}
		out = append(out, f)
	}
	if rErr := rows.Err(); rErr != nil {
		return nil, fmt.Errorf("ListSecurityFindings(task=%d): rows.Err: %w", taskID, rErr)
	}
	return out, nil
}

// SetDisposition transitions a finding to a new disposition. Used by:
//   - BoS reviewer when a // BOS-BYPASS comment downgrades a finding to
//     'overridden' (with bypassAuditID + bypassReason populated).
//   - Resolve sweep when the violating code is gone in a follow-up
//     commit ('resolved').
//   - Operator UI for explicit suppression ('suppressed').
//
// disposition='overridden' requires both bypassAuditID and a >= 10-char
// bypassReason. Other dispositions ignore those fields. Returns error
// if the row doesn't exist or the disposition rule is violated.
func SetDisposition(db *sql.DB, findingID int, disposition, bypassAuditID, bypassReason string) error {
	if disposition == "overridden" {
		if bypassAuditID == "" {
			return errors.New("SetDisposition(overridden): bypassAuditID required")
		}
		if len([]rune(bypassReason)) < 10 {
			return errors.New("SetDisposition(overridden): bypassReason must be >= 10 chars")
		}
	}
	res, err := db.Exec(`
		UPDATE SecurityFindings
		SET disposition = ?, bypass_audit_id = ?, bypass_reason = ?,
		    resolved_at = CASE WHEN ? IN ('resolved','closed','suppressed','overridden')
		                       THEN datetime('now') ELSE '' END
		WHERE id = ?`,
		disposition, bypassAuditID, bypassReason, disposition, findingID)
	if err != nil {
		return fmt.Errorf("SetDisposition(id=%d, %s): %w", findingID, disposition, err)
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return fmt.Errorf("SetDisposition(id=%d): no row updated", findingID)
	}
	return nil
}

// SecurityFindingsFilter narrows ListSecurityFindingsFiltered. Empty
// fields fall through (no filter); Limit defaults to 50, Offset to 0.
// D4 fix-loop-1 α1 — backs the dashboard Security Findings list view.
type SecurityFindingsFilter struct {
	Bureau      string // 'BoS' | 'ISB' | '' (no filter)
	Disposition string // 'open' | 'overridden' | 'escalated' | 'closed' | '' (no filter)
	RuleID      string // exact match; '' = no filter
	Since       string // datetime string; rows with created_at >= Since
	Limit       int
	Offset      int
}

// ListSecurityFindingsFiltered returns SecurityFindings rows matching
// the filter, ordered by id DESC (most-recent-first). Empty result is
// (nil, nil). The 'open' disposition filter is the special case:
// disposition='' OR disposition='escalated' (i.e. NOT a terminal state).
func ListSecurityFindingsFiltered(db *sql.DB, f SecurityFindingsFilter) ([]SecurityFinding, error) {
	if f.Limit <= 0 {
		f.Limit = 50
	}
	if f.Limit > 500 {
		f.Limit = 500
	}
	if f.Offset < 0 {
		f.Offset = 0
	}

	q := `SELECT id, task_id, bureau, rule_id, severity, file_path,
	             line_number, message, commit_sha,
	             IFNULL(disposition,''), IFNULL(bypass_audit_id,''),
	             IFNULL(bypass_reason,''),
	             IFNULL(created_at,''), IFNULL(resolved_at,'')
	      FROM SecurityFindings WHERE 1=1`
	args := []any{}
	if f.Bureau != "" {
		q += " AND bureau = ?"
		args = append(args, f.Bureau)
	}
	if f.RuleID != "" {
		q += " AND rule_id = ?"
		args = append(args, f.RuleID)
	}
	if f.Since != "" {
		q += " AND created_at >= ?"
		args = append(args, f.Since)
	}
	switch f.Disposition {
	case "":
		// no filter
	case "open":
		q += " AND (disposition = '' OR disposition = 'escalated')"
	default:
		q += " AND disposition = ?"
		args = append(args, f.Disposition)
	}
	q += " ORDER BY id DESC LIMIT ? OFFSET ?"
	args = append(args, f.Limit, f.Offset)

	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("ListSecurityFindingsFiltered: %w", err)
	}
	defer rows.Close()
	var out []SecurityFinding
	for rows.Next() {
		var ff SecurityFinding
		if scanErr := rows.Scan(&ff.ID, &ff.TaskID, &ff.Bureau, &ff.RuleID, &ff.Severity,
			&ff.FilePath, &ff.LineNumber, &ff.Message, &ff.CommitSHA,
			&ff.Disposition, &ff.BypassAuditID, &ff.BypassReason,
			&ff.CreatedAt, &ff.ResolvedAt); scanErr != nil {
			return nil, fmt.Errorf("ListSecurityFindingsFiltered: scan: %w", scanErr)
		}
		out = append(out, ff)
	}
	if rErr := rows.Err(); rErr != nil {
		return nil, fmt.Errorf("ListSecurityFindingsFiltered: rows.Err: %w", rErr)
	}
	return out, nil
}

// CountSecurityFindingsFiltered returns the total count for a filter
// (without limit/offset). Useful for paginated UIs that need a total.
func CountSecurityFindingsFiltered(db *sql.DB, f SecurityFindingsFilter) (int, error) {
	q := `SELECT COUNT(*) FROM SecurityFindings WHERE 1=1`
	args := []any{}
	if f.Bureau != "" {
		q += " AND bureau = ?"
		args = append(args, f.Bureau)
	}
	if f.RuleID != "" {
		q += " AND rule_id = ?"
		args = append(args, f.RuleID)
	}
	if f.Since != "" {
		q += " AND created_at >= ?"
		args = append(args, f.Since)
	}
	switch f.Disposition {
	case "":
	case "open":
		q += " AND (disposition = '' OR disposition = 'escalated')"
	default:
		q += " AND disposition = ?"
		args = append(args, f.Disposition)
	}
	var n int
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("CountSecurityFindingsFiltered: %w", err)
	}
	return n, nil
}

// HasBlockingFindings returns true iff any of the task's findings are
// (severity='block' AND disposition NOT IN ('overridden','resolved','suppressed','closed')).
// This is the BoS-approve gate: the reviewer rejects the task iff this
// returns true after bypass-comment processing.
func HasBlockingFindings(db *sql.DB, taskID int) (bool, error) {
	var n int
	err := db.QueryRow(`
		SELECT COUNT(*) FROM SecurityFindings
		WHERE task_id = ?
		  AND severity = 'block'
		  AND disposition NOT IN ('overridden','resolved','suppressed','closed')`,
		taskID).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("HasBlockingFindings(task=%d): %w", taskID, err)
	}
	return n > 0, nil
}

// rowsErrLog is the no-silent-failure helper. Logs rows.Err() through
// the standard logger if non-nil; lifted out of the main path so the
// helpers above stay readable.
func rowsErrLog(prefix string, err error) {
	if err != nil {
		log.Printf("%s: rows.Err: %v", prefix, err)
	}
}

// Compile-time guard that we still use the helper.
var _ = rowsErrLog
