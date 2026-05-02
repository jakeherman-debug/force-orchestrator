// D4 fix-loop-1 α3 — Override-audit dashboard view.
//
// Endpoint:
//
//   GET /api/override-audit
//
// Returns every SecurityFindings row with disposition='overridden' along
// with the bypass-comment metadata (audit_id, reason, file:line, commit
// sha, overridden_at) that justified the downgrade. Filterable by:
//
//   ?bureau=BoS|ISB|all       (default all)
//   ?rule_id=BOS-001          (exact match)
//   ?audit_id=AUDIT-001       (exact match)
//   ?since=<datetime>         (rows with resolved_at >= since)
//   ?limit=50&offset=0
//
// Pure read; no operator action. The list is the audit-trail view of
// every // BOS-BYPASS / // ISB-BYPASS comment that landed in production.

package dashboard

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"force-orchestrator/internal/store"
)

func handleOverrideAudit(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		q := r.URL.Query()
		bureau := q.Get("bureau")
		if strings.EqualFold(bureau, "all") {
			bureau = ""
		}
		switch bureau {
		case "", "BoS", "ISB":
		default:
			http.Error(w, `{"error":"bureau must be BoS, ISB, or all"}`, http.StatusBadRequest)
			return
		}
		ruleID := strings.TrimSpace(q.Get("rule_id"))
		auditID := strings.TrimSpace(q.Get("audit_id"))
		since := strings.TrimSpace(q.Get("since"))
		limit := atoiDefault(q.Get("limit"), 50)
		offset := atoiDefault(q.Get("offset"), 0)
		if limit > 500 {
			limit = 500
		}
		if limit <= 0 {
			limit = 50
		}
		if offset < 0 {
			offset = 0
		}

		query := `
			SELECT id, task_id, bureau, rule_id, severity, file_path,
			       line_number, message, commit_sha,
			       IFNULL(disposition,''), IFNULL(bypass_audit_id,''),
			       IFNULL(bypass_reason,''),
			       IFNULL(created_at,''), IFNULL(resolved_at,'')
			  FROM SecurityFindings
			 WHERE disposition = 'overridden'`
		args := []any{}
		if bureau != "" {
			query += " AND bureau = ?"
			args = append(args, bureau)
		}
		if ruleID != "" {
			query += " AND rule_id = ?"
			args = append(args, ruleID)
		}
		if auditID != "" {
			query += " AND bypass_audit_id = ?"
			args = append(args, auditID)
		}
		if since != "" {
			query += " AND resolved_at >= ?"
			args = append(args, since)
		}
		query += " ORDER BY id DESC LIMIT ? OFFSET ?"
		args = append(args, limit, offset)

		rows, err := db.Query(query, args...)
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		out := []store.SecurityFinding{}
		for rows.Next() {
			var f store.SecurityFinding
			if scanErr := rows.Scan(&f.ID, &f.TaskID, &f.Bureau, &f.RuleID, &f.Severity,
				&f.FilePath, &f.LineNumber, &f.Message, &f.CommitSHA,
				&f.Disposition, &f.BypassAuditID, &f.BypassReason,
				&f.CreatedAt, &f.ResolvedAt); scanErr != nil {
				http.Error(w, fmt.Sprintf(`{"error":"%s"}`, scanErr.Error()), http.StatusInternalServerError)
				return
			}
			out = append(out, f)
		}
		if rErr := rows.Err(); rErr != nil {
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, rErr.Error()), http.StatusInternalServerError)
			return
		}

		// Total (no limit/offset) for paginated UIs.
		countQ := `SELECT COUNT(*) FROM SecurityFindings WHERE disposition = 'overridden'`
		countArgs := []any{}
		if bureau != "" {
			countQ += " AND bureau = ?"
			countArgs = append(countArgs, bureau)
		}
		if ruleID != "" {
			countQ += " AND rule_id = ?"
			countArgs = append(countArgs, ruleID)
		}
		if auditID != "" {
			countQ += " AND bypass_audit_id = ?"
			countArgs = append(countArgs, auditID)
		}
		if since != "" {
			countQ += " AND resolved_at >= ?"
			countArgs = append(countArgs, since)
		}
		var total int
		if err := db.QueryRow(countQ, countArgs...).Scan(&total); err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusInternalServerError)
			return
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"overrides": out,
			"count":     len(out),
			"total":     total,
			"limit":     limit,
			"offset":    offset,
		})
	}
}
