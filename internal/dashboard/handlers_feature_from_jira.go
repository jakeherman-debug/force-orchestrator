package dashboard

// JIRA-from-UI — POST /api/feature/from-jira.
//
// Operator-only entry point that mirrors the `force add-jira` CLI
// command. Calls agents.QueueFeatureFromJira (the reusable core extracted
// from cmdAddJira) so both surfaces share the exact same fetch + payload-
// formatting logic.
//
// Validation matrix (enforced at the handler boundary, before the
// helper sees the input):
//
//   - method must be POST  → 405
//   - JSON body must parse → 400
//   - ticket_id non-empty AND matches ^[A-Z]+-\d+$ → 400 otherwise
//   - priority in [1, 9]   → 400 otherwise (priority=0 is the legal
//     "leave default" sentinel; the CLI uses 0 too)
//   - plan_only is bool    → enforced structurally by the JSON decoder
//
// On success: 200 with {"task_id": N, "summary": "<first 200 chars>"}.
// On helper failure: 500 with a sanitized message — the caller never
// sees raw err.Error() bodies (they may carry CLI stderr fragments).
//
// Pattern P23 (proposer write discipline): this is an operator-routed
// handler, not a proposer-routed one. The dashboard SPA is the only
// consumer; the CSP + same-origin + Origin allow-list middleware in
// dashboard.go gate the actual ingress.

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"force-orchestrator/internal/agents"
)

// jiraTicketIDPattern matches the canonical Atlassian ticket-id shape:
// one-or-more uppercase letters, a dash, one-or-more digits. Refusing
// anything outside this shape at the handler boundary keeps malformed
// payloads out of the BountyBoard and sidesteps any LLM-prompt-injection
// surface that takes ticket_id as a literal in its prompt.
var jiraTicketIDPattern = regexp.MustCompile(`^[A-Z]+-\d+$`)

// featureFromJiraRequest is the JSON body shape the SPA POSTs.
//
// Priority defaults to 0 (helper passes through to BountyBoard's
// default). PlanOnly defaults to false. TicketID is the only required
// field.
type featureFromJiraRequest struct {
	TicketID string `json:"ticket_id"`
	Priority int    `json:"priority"`
	PlanOnly bool   `json:"plan_only"`
}

// featureFromJiraResponse is the success-shape the handler emits.
type featureFromJiraResponse struct {
	TaskID  int    `json:"task_id"`
	Summary string `json:"summary"`
}

// handleFeatureFromJira returns the http.HandlerFunc registered at
// `POST /api/feature/from-jira`. The DB handle is captured by the
// closure; the helper takes it as an argument so this handler stays
// pure-glue (validation + JSON shape).
func handleFeatureFromJira(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		var req featureFromJiraRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid JSON body"}`, http.StatusBadRequest)
			return
		}
		ticket := strings.TrimSpace(req.TicketID)
		if ticket == "" {
			http.Error(w, `{"error":"ticket_id required"}`, http.StatusBadRequest)
			return
		}
		if !jiraTicketIDPattern.MatchString(ticket) {
			http.Error(w, `{"error":"ticket_id must match ^[A-Z]+-\\d+$ (e.g. ABC-123)"}`,
				http.StatusBadRequest)
			return
		}
		// priority 0 means "leave the BountyBoard default in place" —
		// matches the CLI's --priority handling. Any other value must
		// land in [1, 9] (the same range the +Queue Task modal uses).
		if req.Priority < 0 || req.Priority > 9 {
			http.Error(w, `{"error":"priority must be in [1, 9] (or 0 for default)"}`,
				http.StatusBadRequest)
			return
		}

		// Use r.Context() so a client-disconnect cancels the LLM call.
		// agents.QueueFeatureFromJira respects ctx via CallWithTranscript.
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()

		res, err := agents.QueueFeatureFromJira(ctx, db, ticket, req.Priority, req.PlanOnly)
		if err != nil {
			// 500 with a sanitized message. Raw err.Error() may carry
			// CLI stderr fragments (gh tokens, env echoes); the wrapper-
			// level RedactSecrets covers the LLMCallTranscripts row but
			// the HTTP response is a separate egress and we keep it to
			// a class-of-error label here.
			http.Error(w, `{"error":"failed to queue feature from jira ticket"}`,
				http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(featureFromJiraResponse{
			TaskID:  res.TaskID,
			Summary: res.Summary,
		})
	}
}
