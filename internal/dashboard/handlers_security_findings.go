// D4 fix-loop-1 α1 — Security Findings dashboard view.
//
// Endpoints (registered in dashboard.go):
//
//   GET  /api/security-findings                — list with filters
//   POST /api/security-findings/:id/resolve    — operator-only: change disposition
//
// Filters: ?bureau=BoS|ISB|all (default all), ?disposition=open|overridden|
// escalated|resolved|closed|all (default all), ?rule_id=BOS-001,
// ?since=<RFC3339|datetime>, ?limit=50&offset=0.
//
// The list view is the BoS/ISB visibility surface — operators inspect
// open findings and decide whether to escalate, override (with bypass
// comment), resolve, or close. The resolve endpoint mirrors the existing
// store.SetDisposition helper (which is the same code path the BoS/ISB
// reviewers use when a // BOS-BYPASS or // ISB-BYPASS comment matches).
//
// Pattern P25: the resolve endpoint is operator-routed; CLI parity
// allowlist entry sits in audit_pattern_p25_cli_parity_test.go.

package dashboard

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"force-orchestrator/internal/store"
)

// handleSecurityFindings serves GET /api/security-findings.
func handleSecurityFindings(db *sql.DB) http.HandlerFunc {
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
		// Validate bureau if provided — fail closed on garbage input so the
		// SPA doesn't accidentally hide rows by typoing a filter.
		switch bureau {
		case "", "BoS", "ISB":
		default:
			http.Error(w, `{"error":"bureau must be BoS, ISB, or all"}`, http.StatusBadRequest)
			return
		}
		disposition := q.Get("disposition")
		if strings.EqualFold(disposition, "all") {
			disposition = ""
		}
		switch disposition {
		case "", "open", "overridden", "escalated", "resolved", "suppressed", "closed":
		default:
			http.Error(w, `{"error":"disposition invalid"}`, http.StatusBadRequest)
			return
		}
		ruleID := strings.TrimSpace(q.Get("rule_id"))
		since := strings.TrimSpace(q.Get("since"))
		limit := atoiDefault(q.Get("limit"), 50)
		offset := atoiDefault(q.Get("offset"), 0)

		filter := store.SecurityFindingsFilter{
			Bureau:      bureau,
			Disposition: disposition,
			RuleID:      ruleID,
			Since:       since,
			Limit:       limit,
			Offset:      offset,
		}
		rows, err := store.ListSecurityFindingsFiltered(db, filter)
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusInternalServerError)
			return
		}
		total, err := store.CountSecurityFindingsFiltered(db, filter)
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusInternalServerError)
			return
		}
		if rows == nil {
			rows = []store.SecurityFinding{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"findings": rows,
			"count":    len(rows),
			"total":    total,
			"limit":    limit,
			"offset":   offset,
		})
	}
}

// handleSecurityFindingsSubroutes handles the per-id POST verbs.
//
//	POST /api/security-findings/<id>/resolve
func handleSecurityFindingsSubroutes(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		path := strings.TrimPrefix(r.URL.Path, "/api/security-findings/")
		parts := strings.Split(path, "/")
		if len(parts) == 0 || parts[0] == "" {
			http.Error(w, `{"error":"finding id required"}`, http.StatusBadRequest)
			return
		}
		idStr := parts[0]
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil || id <= 0 {
			http.Error(w, `{"error":"invalid finding id"}`, http.StatusBadRequest)
			return
		}
		verb := ""
		if len(parts) > 1 {
			verb = parts[1]
		}
		switch {
		case verb == "resolve" && r.Method == http.MethodPost:
			handleSecurityFindingResolve(w, r, db, int(id))
		default:
			http.Error(w, `{"error":"unknown verb or method"}`, http.StatusNotFound)
		}
	}
}

type securityFindingResolveRequest struct {
	Disposition   string `json:"disposition"` // 'resolved' | 'closed' | 'suppressed' | 'overridden'
	OperatorEmail string `json:"operator_email"`
	BypassAuditID string `json:"bypass_audit_id"` // required iff disposition='overridden'
	BypassReason  string `json:"bypass_reason"`   // >= 10 chars iff disposition='overridden'
}

func handleSecurityFindingResolve(w http.ResponseWriter, r *http.Request, db *sql.DB, id int) {
	var req securityFindingResolveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid JSON body"}`, http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.OperatorEmail) == "" {
		http.Error(w, `{"error":"operator_email required"}`, http.StatusBadRequest)
		return
	}
	switch req.Disposition {
	case "resolved", "closed", "suppressed", "overridden":
	default:
		http.Error(w, `{"error":"disposition must be resolved|closed|suppressed|overridden"}`, http.StatusBadRequest)
		return
	}
	if err := store.SetDisposition(db, id, req.Disposition, req.BypassAuditID, req.BypassReason); err != nil {
		// 'no row updated' → 404; bypass-rule violation → 400.
		if strings.Contains(err.Error(), "no row updated") {
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusNotFound)
			return
		}
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusBadRequest)
		return
	}
	store.LogAudit(db, req.OperatorEmail, "security-finding-resolve", id,
		fmt.Sprintf("disposition=%s bypass_audit=%s reason=%s",
			req.Disposition, req.BypassAuditID, truncateForAudit(req.BypassReason, 200)))
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}
