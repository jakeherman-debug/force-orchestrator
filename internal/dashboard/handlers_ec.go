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
	"context"
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
//
// D3 fix-loop-1 (slice δ) adds two extension actions for exit
// criterion 14b end-to-end coverage:
//   - defer_revert: operator agrees a revert is needed but wants to
//     batch it with the next deploy window. Sets BountyBoard.deferred_revert
//     on the underlying landed task so the operator dashboard surfaces
//     the queue.
//   - refile:       the proposal's underlying need is real but the rule
//     content was wrong — open a fresh ProposedFeatures row pointing
//     back at this proposal as evidence, leaving the rule rejected.
var validRejectionActions = map[string]bool{
	"leave_as_is":     true,
	"clean_revert":    true,
	"cascade_revert":  true,
	"surgical_revert": true,
	"escalate":        true,
	"defer_revert":    true,
	"refile":          true,
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
				`{"error":"rejection_action must be one of leave_as_is|clean_revert|cascade_revert|surgical_revert|escalate|defer_revert|refile"}`,
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

		// D3 fix-loop-1 (slice δ): apply per-action side effects so
		// the rejection actually does something downstream — not just
		// stamp columns. Errors here log + continue (the rejection
		// itself succeeded; the side effect is best-effort and the
		// operator can re-trigger it).
		applyRejectionSideEffects(r.Context(), db, id, action, rationale, operator)

		_, _ = fmt.Fprintf(w, `{"ok":true,"id":%d,"rejected_by":%q,"action":%q}`,
			id, operator, action)
	}
}

// applyRejectionSideEffects dispatches action-specific downstream
// behaviour that the rejected proposal's columns alone don't trigger.
//
// The handler completes the rejection (column UPDATE + AuditLog) BEFORE
// calling this; on any side-effect failure we log + continue so the
// rejection is durable. Re-running by the operator is possible — the
// inserts/updates here are idempotent on the (proposal_id, action)
// pair.
//
// Action map:
//
//   - leave_as_is — no side effect (operator chose to merely note the
//     rejection; nothing to undo).
//   - clean_revert / cascade_revert / surgical_revert — spawn a
//     RevertTask BountyBoard row pointing at the proposal; ConvoyReview
//     re-trigger lands automatically when the revert task completes
//     (downstream of revert_target_task_id wiring).
//   - escalate — write an Escalation row so the operator inbox shows
//     a hard-block needing attention.
//   - defer_revert — flag the underlying landed task with
//     BountyBoard.deferred_revert=1 so the operator dashboard's
//     "deferred reverts" queue picks it up later.
//   - refile — open a fresh ProposedFeatures row referencing this
//     proposal as evidence; the rule content was wrong but the
//     underlying need was real.
func applyRejectionSideEffects(ctx context.Context, db *sql.DB, proposalID int, action, rationale, operator string) {
	switch action {
	case "leave_as_is":
		return
	case "clean_revert", "cascade_revert", "surgical_revert":
		spawnRevertTaskForProposal(ctx, db, proposalID, action, rationale, operator)
	case "escalate":
		spawnEscalationForProposal(ctx, db, proposalID, rationale)
	case "defer_revert":
		flagDeferredRevertForProposal(ctx, db, proposalID, rationale)
	case "refile":
		refileProposalAsFeature(ctx, db, proposalID, rationale, operator)
	}
}

// spawnRevertTaskForProposal inserts a BountyBoard row of type
// 'RevertTask' targeting the proposal's experiment. revert_task_id on
// the proposal is updated to point at the new BountyBoard row so the
// audit chain is queryable: PromotionProposals → BountyBoard.
//
// Idempotent: if a RevertTask for this proposal already exists, we
// don't spawn a second one. The check is by revert_task_id != 0.
func spawnRevertTaskForProposal(ctx context.Context, db *sql.DB, proposalID int, action, rationale, operator string) {
	// Idempotence check.
	var existing int
	_ = db.QueryRowContext(ctx,
		`SELECT IFNULL(revert_task_id, 0) FROM PromotionProposals WHERE id = ?`,
		proposalID).Scan(&existing)
	if existing != 0 {
		log.Printf("applyRejectionSideEffects: proposal %d already has revert_task_id=%d; skipping respawn", proposalID, existing)
		return
	}

	payload := fmt.Sprintf(
		"Revert PromotionProposal %d (%s) — operator=%s rationale=%s",
		proposalID, action, operator, rationale)
	res, err := db.ExecContext(ctx,
		`INSERT INTO BountyBoard (type, status, payload, parent_id, priority, created_at)
		 VALUES ('RevertTask', 'Pending', ?, 0, 2, datetime('now'))`,
		payload)
	if err != nil {
		log.Printf("applyRejectionSideEffects: spawn RevertTask for proposal %d: %v", proposalID, err)
		return
	}
	revertID, err := res.LastInsertId()
	if err != nil {
		log.Printf("applyRejectionSideEffects: LastInsertId for proposal %d revert: %v", proposalID, err)
		return
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE PromotionProposals SET revert_task_id = ? WHERE id = ?`,
		revertID, proposalID); err != nil {
		log.Printf("applyRejectionSideEffects: link revert_task_id %d on proposal %d: %v", revertID, proposalID, err)
	}
	store.LogAudit(db, operator, "ec.reject.revert-spawned", proposalID,
		fmt.Sprintf("Spawned RevertTask #%d for proposal %d (action=%s)", revertID, proposalID, action))
}

// spawnEscalationForProposal writes an Escalation row so the operator
// inbox surfaces this proposal as a hard-block needing attention.
//
// Idempotent: a second call against the same proposal is a no-op
// (one Open Escalation per proposal is the contract).
func spawnEscalationForProposal(ctx context.Context, db *sql.DB, proposalID int, rationale string) {
	var existing int
	_ = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM Escalations WHERE task_id = ? AND status = 'Open' AND IFNULL(severity,'') != ''`,
		proposalID).Scan(&existing)
	if existing > 0 {
		return
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO Escalations (task_id, severity, message, status, created_at)
		 VALUES (?, 'medium', ?, 'Open', datetime('now'))`,
		proposalID,
		fmt.Sprintf("PromotionProposal %d rejected with action=escalate; rationale=%s", proposalID, rationale),
	); err != nil {
		log.Printf("applyRejectionSideEffects: insert Escalation for proposal %d: %v", proposalID, err)
	}
}

// flagDeferredRevertForProposal marks the proposal's revert_task_id
// as the placeholder "deferred" sentinel by setting deferred_revert=1
// on a stub BountyBoard row. The operator dashboard's deferred-revert
// queue reads BountyBoard.deferred_revert=1 to surface these.
//
// We do NOT spawn an executable RevertTask here — defer_revert is the
// "agree this should revert, but not now" signal. When the operator
// flips the row from deferred to ready, that's a separate UI action
// that promotes the deferred row into a real RevertTask.
func flagDeferredRevertForProposal(ctx context.Context, db *sql.DB, proposalID int, rationale string) {
	var existing int
	_ = db.QueryRowContext(ctx,
		`SELECT IFNULL(revert_task_id, 0) FROM PromotionProposals WHERE id = ?`,
		proposalID).Scan(&existing)
	if existing != 0 {
		return
	}
	payload := fmt.Sprintf("Deferred revert for PromotionProposal %d — rationale=%s", proposalID, rationale)
	res, err := db.ExecContext(ctx,
		`INSERT INTO BountyBoard (type, status, payload, parent_id, priority, deferred_revert, created_at)
		 VALUES ('RevertTask', 'Pending', ?, 0, 0, 1, datetime('now'))`,
		payload)
	if err != nil {
		log.Printf("applyRejectionSideEffects: spawn deferred RevertTask for proposal %d: %v", proposalID, err)
		return
	}
	revertID, err := res.LastInsertId()
	if err != nil {
		log.Printf("applyRejectionSideEffects: LastInsertId for proposal %d deferred revert: %v", proposalID, err)
		return
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE PromotionProposals SET revert_task_id = ? WHERE id = ?`,
		revertID, proposalID); err != nil {
		log.Printf("applyRejectionSideEffects: link deferred revert_task_id %d on proposal %d: %v", revertID, proposalID, err)
	}
}

// refileProposalAsFeature opens a fresh ProposedFeatures row pointing
// back at this proposal as evidence. The proposal's
// refiled_feature_id is updated to the new feature row so the
// dashboard can chain "this proposal was rejected → led to this
// feature → which was promoted/rejected/etc."
//
// Idempotent: if the proposal already has refiled_feature_id != 0, we
// don't double-file.
func refileProposalAsFeature(ctx context.Context, db *sql.DB, proposalID int, rationale, operator string) {
	var existing int
	_ = db.QueryRowContext(ctx,
		`SELECT IFNULL(refiled_feature_id, 0) FROM PromotionProposals WHERE id = ?`,
		proposalID).Scan(&existing)
	if existing != 0 {
		return
	}
	summary := fmt.Sprintf("Refiled from rejected PromotionProposal %d — %s", proposalID, rationale)
	res, err := db.ExecContext(ctx,
		`INSERT INTO ProposedFeatures
		    (observation_summary, category, source, fingerprint, scored_by, status)
		 VALUES (?, 'rule-promotion-refile', ?, ?, ?, 'pending')`,
		summary,
		fmt.Sprintf("ec-rejection-%d", proposalID),
		fmt.Sprintf("ec-rejection-%d", proposalID),
		operator)
	if err != nil {
		log.Printf("applyRejectionSideEffects: insert ProposedFeatures for proposal %d: %v", proposalID, err)
		return
	}
	featureID, err := res.LastInsertId()
	if err != nil {
		log.Printf("applyRejectionSideEffects: LastInsertId for proposal %d refile: %v", proposalID, err)
		return
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE PromotionProposals SET refiled_feature_id = ? WHERE id = ?`,
		featureID, proposalID); err != nil {
		log.Printf("applyRejectionSideEffects: link refiled_feature_id %d on proposal %d: %v", featureID, proposalID, err)
	}
	store.LogAudit(db, operator, "ec.reject.refile", proposalID,
		fmt.Sprintf("Refiled proposal %d as ProposedFeatures #%d", proposalID, featureID))
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
