package dashboard

// D3 Phase 3 — Engineering Corps ratification surface.
//
// Operators ratify (or reject) PromotionProposals rows. Two row
// shapes coexist on this surface:
//
//   - kind='candidate' / authored_by='librarian' — the Librarian → EC
//     handoff (paired-runs.md § Composition with Promotion Pipeline).
//     Ratifying flips it from "Librarian's hypothesis" to "operator-
//     blessed input the EC ExperimentAuthor may consume to mint an
//     Experiments row." Rejecting kills the hypothesis with an audit
//     trail.
//
//   - kind='promote' / authored_by='engineering-corps' — emitted by
//     experiments.MaybePromoteRule when a terminated experiment has a
//     declared winner. Ratifying flips the proposal to ratified and
//     (in a follow-up phase) writes a FleetRules row; the FleetRules
//     write itself is Phase 6's atomic DB+render+commit dance — we
//     ship the operator-routed gate now.
//
// Endpoints (registered in dashboard.go):
//
//   GET  /api/ec/proposals             — list pending (both kinds)
//   GET  /api/ec/proposals/:id         — single
//   POST /api/ec/proposals/:id/ratify  — operator ratifies
//   POST /api/ec/proposals/:id/reject  — operator rejects
//
// The Ratify and Reject handlers mirror experiments.Ratify exactly:
// require operator email (rejected as 400 if blank), AuditLog row in
// the same statement chain, conditional UPDATE that fails if the row
// is already in a terminal state. Reject additionally honours the
// concern #7 schema: rejection_rationale is mandatory (≥ 20 chars)
// when rejection_action is anything other than 'leave_as_is'.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"force-orchestrator/internal/store"
)

// ecProposalSummary is the shared shape returned by GET list and
// GET detail. AuthoredBy doubles as the origin column (paired-runs.md
// § Composition with Promotion Pipeline).
type ecProposalSummary struct {
	ID                 int    `json:"id"`
	Kind               string `json:"kind"`
	RuleKey            string `json:"rule_key"`
	ProposedContent    string `json:"proposed_content"`
	EvidenceJSON       string `json:"evidence_summary_json"`
	AuthoredBy         string `json:"authored_by"`
	AuthoredAt         string `json:"authored_at"`
	ExperimentID       int    `json:"experiment_id"`
	RatifiedAt         string `json:"ratified_at,omitempty"`
	RatifiedBy         string `json:"ratified_by,omitempty"`
	RejectedAt         string `json:"rejected_at,omitempty"`
	RejectedReason     string `json:"rejected_reason,omitempty"`
	RejectionAction    string `json:"rejection_action,omitempty"`
	RejectionRationale string `json:"rejection_rationale,omitempty"`
	TTLExpiresAt       string `json:"ttl_expires_at,omitempty"`
}

// validRejectionActions enumerates the concern #7 set
// (paired-runs.md § PromotionProposals revert handling).
var validRejectionActions = map[string]bool{
	"leave_as_is":     true,
	"clean_revert":    true,
	"cascade_revert":  true,
	"surgical_revert": true,
	"escalate":        true,
}

// minRejectionRationaleLen is the schema-encoded floor (concern #7)
// for any non-leave_as_is rejection.
const minRejectionRationaleLen = 20

// handleECProposalsList serves GET /api/ec/proposals — every pending
// proposal, both kinds, ordered newest-first.
//
// Pending = ratified_at='' AND rejected_at=''. Operator-rejected rows
// stay in the table for audit trail but drop off this list (operator
// would query AuditLog for the historical reject reason).
//
// Optional ?kind= filters to a single shape ('candidate' | 'promote').
// Optional ?status= filters lifecycle: 'pending' (default) | 'ratified'
// | 'rejected' | 'all'.
func handleECProposalsList(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		kindFilter := r.URL.Query().Get("kind")
		statusFilter := r.URL.Query().Get("status")
		if statusFilter == "" {
			statusFilter = "pending"
		}

		query := `SELECT id, kind, IFNULL(rule_key,''), IFNULL(proposed_content,''),
		                 IFNULL(evidence_summary_json,'{}'),
		                 IFNULL(authored_by,''), IFNULL(authored_at,''),
		                 IFNULL(experiment_id, 0),
		                 IFNULL(ratified_at,''), IFNULL(ratified_by,''),
		                 IFNULL(rejected_at,''), IFNULL(rejected_reason,''),
		                 IFNULL(rejection_action,''), IFNULL(rejection_rationale,''),
		                 IFNULL(ttl_expires_at,'')
		            FROM PromotionProposals WHERE 1=1`
		args := []any{}
		switch statusFilter {
		case "pending":
			query += ` AND IFNULL(ratified_at,'') = '' AND IFNULL(rejected_at,'') = ''`
		case "ratified":
			query += ` AND IFNULL(ratified_at,'') != ''`
		case "rejected":
			query += ` AND IFNULL(rejected_at,'') != ''`
		case "all":
			// no filter
		default:
			http.Error(w, `{"error":"status must be pending|ratified|rejected|all"}`, http.StatusBadRequest)
			return
		}
		if kindFilter != "" {
			if kindFilter != "candidate" && kindFilter != "promote" {
				http.Error(w, `{"error":"kind must be candidate or promote"}`, http.StatusBadRequest)
				return
			}
			query += ` AND kind = ?`
			args = append(args, kindFilter)
		}
		query += ` ORDER BY id DESC`

		rows, err := db.QueryContext(r.Context(), query, args...)
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"query: %s"}`, err.Error()), http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		out := []ecProposalSummary{}
		for rows.Next() {
			var s ecProposalSummary
			if err := rows.Scan(&s.ID, &s.Kind, &s.RuleKey, &s.ProposedContent,
				&s.EvidenceJSON, &s.AuthoredBy, &s.AuthoredAt, &s.ExperimentID,
				&s.RatifiedAt, &s.RatifiedBy, &s.RejectedAt, &s.RejectedReason,
				&s.RejectionAction, &s.RejectionRationale, &s.TTLExpiresAt); err != nil {
				log.Printf("handleECProposalsList: scan: %v", err)
				continue
			}
			out = append(out, s)
		}
		if err := rows.Err(); err != nil {
			log.Printf("handleECProposalsList: rows: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"proposals":     out,
			"count":         len(out),
			"status_filter": statusFilter,
			"kind_filter":   kindFilter,
		})
	}
}

// handleECProposalDetail serves GET /api/ec/proposals/:id.
func handleECProposalDetail(db *sql.DB, id int) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		var s ecProposalSummary
		err := db.QueryRowContext(r.Context(), `
			SELECT id, kind, IFNULL(rule_key,''), IFNULL(proposed_content,''),
			       IFNULL(evidence_summary_json,'{}'),
			       IFNULL(authored_by,''), IFNULL(authored_at,''),
			       IFNULL(experiment_id, 0),
			       IFNULL(ratified_at,''), IFNULL(ratified_by,''),
			       IFNULL(rejected_at,''), IFNULL(rejected_reason,''),
			       IFNULL(rejection_action,''), IFNULL(rejection_rationale,''),
			       IFNULL(ttl_expires_at,'')
			  FROM PromotionProposals WHERE id = ?`, id).
			Scan(&s.ID, &s.Kind, &s.RuleKey, &s.ProposedContent,
				&s.EvidenceJSON, &s.AuthoredBy, &s.AuthoredAt, &s.ExperimentID,
				&s.RatifiedAt, &s.RatifiedBy, &s.RejectedAt, &s.RejectedReason,
				&s.RejectionAction, &s.RejectionRationale, &s.TTLExpiresAt)
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, `{"error":"proposal not found"}`, http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"load: %s"}`, err.Error()), http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(s)
	}
}

// ecRatifyBody is the POST body for /api/ec/proposals/:id/ratify.
// Operator email comes EITHER from this body (preferred — explicit
// per-action attribution) OR from the X-Operator-Email header (a
// session-level convenience). Empty in both → 400.
type ecRatifyBody struct {
	OperatorEmail string `json:"operator_email"`
}

// ecRejectBody is the POST body for /api/ec/proposals/:id/reject.
// rejection_action is one of {leave_as_is, clean_revert, cascade_revert,
// surgical_revert, escalate}; default is leave_as_is. rejection_rationale
// is mandatory ≥ 20 chars when rejection_action != 'leave_as_is'
// (concern #7).
type ecRejectBody struct {
	OperatorEmail      string `json:"operator_email"`
	RejectedReason     string `json:"rejected_reason"`
	RejectionAction    string `json:"rejection_action"`
	RejectionRationale string `json:"rejection_rationale"`
}

// handleECProposalRatify is the operator-routed ratify gate.
// Mirrors experiments.Ratify exactly: requires operator email, writes
// AuditLog, conditional update fails if the row is already terminal.
func handleECProposalRatify(db *sql.DB, id int) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		var body ecRatifyBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			// AUDIT-054: 256 KB cap inherited via securityMiddleware.
			// Fall through if just empty body — header may carry email.
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				writeBodyReadError(w, err)
				return
			}
		}
		operator := strings.TrimSpace(body.OperatorEmail)
		if operator == "" {
			operator = strings.TrimSpace(r.Header.Get("X-Operator-Email"))
		}
		if operator == "" {
			http.Error(w,
				`{"error":"operator_email is required (operator-routed gate)"}`,
				http.StatusBadRequest)
			return
		}

		// Conditional update — only flip rows that are still pending.
		// Mirrors the CAS shape Fix #8d codifies.
		res, err := db.ExecContext(r.Context(), `
			UPDATE PromotionProposals
			   SET ratified_at = datetime('now'),
			       ratified_by = ?
			 WHERE id = ?
			   AND IFNULL(ratified_at, '') = ''
			   AND IFNULL(rejected_at, '') = ''
		`, operator, id)
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"ratify update: %s"}`, err.Error()),
				http.StatusInternalServerError)
			return
		}
		n, err := res.RowsAffected()
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"rows: %s"}`, err.Error()),
				http.StatusInternalServerError)
			return
		}
		if n == 0 {
			// Either id doesn't exist or it's already terminal.
			var exists int
			_ = db.QueryRowContext(r.Context(),
				`SELECT COUNT(*) FROM PromotionProposals WHERE id = ?`, id).Scan(&exists)
			if exists == 0 {
				http.Error(w, `{"error":"proposal not found"}`, http.StatusNotFound)
				return
			}
			http.Error(w,
				`{"error":"proposal is not in pending state — refusing to flip"}`,
				http.StatusConflict)
			return
		}

		// AuditLog. Mirrors experiments.Ratify shape exactly: actor=operator,
		// action='ec.ratify', task_id=proposal id (PromotionProposals.id is
		// the natural reference here — consistent with the experiments
		// surface that uses experiment_id as task_id).
		store.LogAudit(db, operator, "ec.ratify", id,
			fmt.Sprintf("Ratified PromotionProposal %d", id))

		_, _ = fmt.Fprintf(w, `{"ok":true,"id":%d,"ratified_by":%q}`, id, operator)
	}
}

// handleECProposalReject is the operator-routed reject gate. Concern #7
// schema: rejection_rationale is mandatory ≥ 20 chars when
// rejection_action != 'leave_as_is'.
func handleECProposalReject(db *sql.DB, id int) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		var body ecRejectBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				writeBodyReadError(w, err)
				return
			}
			http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
			return
		}
		operator := strings.TrimSpace(body.OperatorEmail)
		if operator == "" {
			operator = strings.TrimSpace(r.Header.Get("X-Operator-Email"))
		}
		if operator == "" {
			http.Error(w,
				`{"error":"operator_email is required (operator-routed gate)"}`,
				http.StatusBadRequest)
			return
		}
		action := strings.TrimSpace(body.RejectionAction)
		if action == "" {
			action = "leave_as_is"
		}
		if !validRejectionActions[action] {
			http.Error(w,
				`{"error":"rejection_action must be one of leave_as_is|clean_revert|cascade_revert|surgical_revert|escalate"}`,
				http.StatusBadRequest)
			return
		}
		rationale := strings.TrimSpace(body.RejectionRationale)
		if action != "leave_as_is" && len(rationale) < minRejectionRationaleLen {
			http.Error(w,
				fmt.Sprintf(`{"error":"rejection_rationale required (>=%d chars) when rejection_action != leave_as_is"}`,
					minRejectionRationaleLen),
				http.StatusBadRequest)
			return
		}
		reason := strings.TrimSpace(body.RejectedReason)
		if reason == "" {
			// Default the reason to the rationale if present, else action.
			if rationale != "" {
				reason = rationale
			} else {
				reason = action
			}
		}

		res, err := db.ExecContext(r.Context(), `
			UPDATE PromotionProposals
			   SET rejected_at = datetime('now'),
			       rejected_reason = ?,
			       rejection_action = ?,
			       rejection_rationale = ?
			 WHERE id = ?
			   AND IFNULL(ratified_at, '') = ''
			   AND IFNULL(rejected_at, '') = ''
		`, reason, action, rationale, id)
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"reject update: %s"}`, err.Error()),
				http.StatusInternalServerError)
			return
		}
		n, err := res.RowsAffected()
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"rows: %s"}`, err.Error()),
				http.StatusInternalServerError)
			return
		}
		if n == 0 {
			var exists int
			_ = db.QueryRowContext(r.Context(),
				`SELECT COUNT(*) FROM PromotionProposals WHERE id = ?`, id).Scan(&exists)
			if exists == 0 {
				http.Error(w, `{"error":"proposal not found"}`, http.StatusNotFound)
				return
			}
			http.Error(w,
				`{"error":"proposal is not in pending state — refusing to flip"}`,
				http.StatusConflict)
			return
		}
		store.LogAudit(db, operator, "ec.reject", id,
			fmt.Sprintf("Rejected PromotionProposal %d action=%s reason=%s", id, action, reason))
		_, _ = fmt.Fprintf(w, `{"ok":true,"id":%d,"rejected_by":%q,"action":%q}`,
			id, operator, action)
	}
}

// handleECProposalsSubroutes dispatches /api/ec/proposals/{id}[/action].
//
//	GET  /api/ec/proposals/{id}          → detail
//	POST /api/ec/proposals/{id}/ratify   → operator ratify
//	POST /api/ec/proposals/{id}/reject   → operator reject
func handleECProposalsSubroutes(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		// Path: /api/ec/proposals/{id}[/action]
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		// Expect: ["api", "ec", "proposals", "{id}"] or ["api", "ec", "proposals", "{id}", "{action}"]
		if len(parts) < 4 || parts[0] != "api" || parts[1] != "ec" || parts[2] != "proposals" {
			http.NotFound(w, r)
			return
		}
		id, err := strconv.Atoi(parts[3])
		if err != nil || id <= 0 {
			http.Error(w, `{"error":"invalid proposal id"}`, http.StatusBadRequest)
			return
		}
		// Detail: /api/ec/proposals/{id}
		if len(parts) == 4 {
			if r.Method != http.MethodGet {
				http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
				return
			}
			handleECProposalDetail(db, id)(w, r)
			return
		}
		// Action: /api/ec/proposals/{id}/{action}
		if len(parts) != 5 {
			http.NotFound(w, r)
			return
		}
		action := parts[4]
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		switch action {
		case "ratify":
			handleECProposalRatify(db, id)(w, r)
		case "reject":
			handleECProposalReject(db, id)(w, r)
		default:
			http.NotFound(w, r)
		}
	}
}
