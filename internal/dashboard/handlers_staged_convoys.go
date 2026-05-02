// Package dashboard — D5.5 P4 staged-convoys operator surface.
//
// Three endpoints back the SPA's staged-convoy view:
//
//   GET  /api/convoys/<id>/stages
//        ConvoyStages list, ordered by stage_num. For staging_mode='single'
//        convoys the response is a single forward-compat row with
//        gate_type=null so the SPA can render either shape uniformly.
//
//   GET  /api/convoys/<id>/stages/<stage_num>
//        Stage detail: the row plus its ConvoyAskBranches (filtered by
//        stage_id) and per-branch AskBranchPRs (state, checks, mergedAt).
//        Includes the stage audit-log so the SPA can render the
//        "view history" panel.
//
//   POST /api/convoys/<id>/stages/<stage_num>/advance
//        Body: {"operator":"<name>","reason":"<text>","audit_id":"AUDIT-NNN"?}
//        Two modes:
//
//          - Normal advance (no audit_id): writes
//            SystemConfig.stage_advance_<convoy>_<stage_num> = "<name>:<rfc3339>"
//            — the operator-confirm gate's rendezvous key. Idempotent:
//            repeated POSTs overwrite the timestamp; the gate evaluator
//            only checks "non-empty" so a fast double-click is safe.
//            Audit row appended via store.LogStageAudit (action=stage_advance).
//
//          - Emergency bypass (audit_id matches `^AUDIT-\d+$`): skips gate
//            evaluation entirely, transitions the stage directly to
//            GatePassed via store.BypassStage. Audit row appended with
//            action=stage_bypass and the AUDIT id + reason in detail.
//            Required for D5.5 exit criterion #10 (production-on-fire
//            cut-through for soak / threshold gates that can't wait).
//
//   POST /api/convoys/<id>/stages/<stage_num>/abort
//        Body: {"operator":"<name>","reason":"<text>"}
//        Forces the stage to status='Failed' (terminal). The convoy itself
//        is left in DraftPROpen — the operator separately decides whether
//        to revert the merged stages or accept-as-shipped.
//
// All four are routed through the existing /api/convoys/ subroute mux —
// see RegisterStagedConvoyRoutes for the dispatcher used by dashboard.go.

package dashboard

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"force-orchestrator/internal/store"
)

// StageRowResponse is the per-stage payload shape the dashboard SPA renders.
// Mirrors store.ConvoyStage with the JSON-friendly nullable shape for
// gate_type and a parsed gate_config.
type StageRowResponse struct {
	ID                    int             `json:"id"`
	ConvoyID              int             `json:"convoy_id"`
	StageNum              int             `json:"stage_num"`
	IntentText            string          `json:"intent_text"`
	Status                string          `json:"status"`
	GateType              *string         `json:"gate_type"` // nil when DB column is NULL
	GateConfig            json.RawMessage `json:"gate_config,omitempty"`
	GateTimeoutMinutes    int             `json:"gate_timeout_minutes"`
	OpenedAt              string          `json:"opened_at"`
	AllPRsMergedAt        string          `json:"all_prs_merged_at"`
	GatePassedAt          string          `json:"gate_passed_at"`
	CompletedAt           string          `json:"completed_at"`
	GateEvaluationStatus  string          `json:"gate_evaluation_status"`
}

// StageAskBranchResponse is one ask-branch row for stage detail.
type StageAskBranchResponse struct {
	Repo          string                  `json:"repo"`
	AskBranch     string                  `json:"ask_branch"`
	DraftPRURL    string                  `json:"draft_pr_url"`
	DraftPRNumber int                     `json:"draft_pr_number"`
	DraftPRState  string                  `json:"draft_pr_state"`
	PRs           []StagePRResponse       `json:"prs"`
}

// StagePRResponse is one sub-PR row scoped to a stage's ask-branch.
type StagePRResponse struct {
	ID          int    `json:"id"`
	TaskID      int    `json:"task_id"`
	PRNumber    int    `json:"pr_number"`
	PRURL       string `json:"pr_url"`
	State       string `json:"state"`
	ChecksState string `json:"checks_state"`
	MergedAt    string `json:"merged_at"`
}

// StageDetailResponse is the GET /stages/<num> payload.
type StageDetailResponse struct {
	Stage       StageRowResponse           `json:"stage"`
	AskBranches []StageAskBranchResponse   `json:"ask_branches"`
	AuditLog    []store.AuditEntry         `json:"audit_log"`
}

// stageAdvanceBody is the JSON body for POST /advance and /abort.
//
// AuditID is the optional emergency-bypass key (D5.5 exit criterion #10).
// When non-empty, the advance handler validates the value matches
// `^AUDIT-\d+$`, then bypasses the gate evaluation entirely and lands the
// stage in GatePassed. The reason field remains required so the audit
// trail captures human context, not just the AUDIT id.
type stageAdvanceBody struct {
	Operator string `json:"operator"`
	Reason   string `json:"reason"`
	AuditID  string `json:"audit_id,omitempty"`
}

// stageBypassAuditIDRe is the strict shape required of the bypass key.
// Mirrors internal/isb/bypass.go's `AUDIT-\d+` discipline so a typo'd id
// (e.g. AUDIT-FOO, audit-12) fails parse rather than slipping through.
var stageBypassAuditIDRe = regexp.MustCompile(`^AUDIT-\d+$`)

// handleConvoyStages dispatches the four staged-convoy routes off the
// /api/convoys/<id>/stages... path. Returns ok=true if it handled the
// request (caller should not fall through), false otherwise so the
// existing handleConvoysSubroutes default branches keep working.
//
// Path shapes accepted:
//
//   /api/convoys/<id>/stages                        GET    list
//   /api/convoys/<id>/stages/<stage_num>            GET    detail
//   /api/convoys/<id>/stages/<stage_num>/advance    POST   advance
//   /api/convoys/<id>/stages/<stage_num>/abort      POST   abort
func handleConvoyStages(db *sql.DB, w http.ResponseWriter, r *http.Request, convoyID int, parts []string) bool {
	// parts is the segments after /api/convoys/<id>/. Caller already split.
	// Expected: ["stages"] or ["stages", "<num>"] or ["stages", "<num>", "advance"|"abort"].
	if len(parts) == 0 || parts[0] != "stages" {
		return false
	}
	switch len(parts) {
	case 1:
		// /api/convoys/<id>/stages
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return true
		}
		listStages(db, w, convoyID)
		return true
	case 2:
		// /api/convoys/<id>/stages/<stage_num>
		stageNum, err := strconv.Atoi(parts[1])
		if err != nil || stageNum <= 0 {
			http.Error(w, `{"error":"invalid stage_num"}`, http.StatusBadRequest)
			return true
		}
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return true
		}
		getStageDetail(db, w, convoyID, stageNum)
		return true
	case 3:
		// /api/convoys/<id>/stages/<stage_num>/{advance,abort}
		stageNum, err := strconv.Atoi(parts[1])
		if err != nil || stageNum <= 0 {
			http.Error(w, `{"error":"invalid stage_num"}`, http.StatusBadRequest)
			return true
		}
		switch parts[2] {
		case "advance":
			if r.Method != http.MethodPost {
				http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
				return true
			}
			advanceStageHandler(db, w, r, convoyID, stageNum)
			return true
		case "abort":
			if r.Method != http.MethodPost {
				http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
				return true
			}
			abortStageHandler(db, w, r, convoyID, stageNum)
			return true
		}
	}
	return false
}

// listStages returns the stages for a convoy, ordered by stage_num.
// Returns 404 if the convoy doesn't exist or has zero stage rows. The
// forward-compat migration ensures every convoy has at least one stage,
// so the no-rows path indicates the convoy id itself is bogus.
func listStages(db *sql.DB, w http.ResponseWriter, convoyID int) {
	stages, err := store.ListStages(db, convoyID)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
		return
	}
	if len(stages) == 0 {
		// Distinguish "convoy missing" from "convoy with zero stages": if
		// the Convoys row exists, return an empty list; otherwise 404.
		var exists int
		_ = db.QueryRow(`SELECT COUNT(*) FROM Convoys WHERE id = ?`, convoyID).Scan(&exists)
		if exists == 0 {
			http.NotFound(w, nil)
			return
		}
	}
	out := make([]StageRowResponse, 0, len(stages))
	for _, s := range stages {
		out = append(out, stageToResponse(s))
	}
	writeJSON(w, map[string]any{"stages": out})
}

// getStageDetail returns one stage row + its ask-branches + sub-PRs +
// the per-stage audit log.
func getStageDetail(db *sql.DB, w http.ResponseWriter, convoyID, stageNum int) {
	stage, err := store.GetStageByNum(db, convoyID, stageNum)
	if err != nil {
		// GetStageByNum returns sql.ErrNoRows for a missing row.
		if err == sql.ErrNoRows {
			http.NotFound(w, nil)
			return
		}
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
		return
	}

	// Ask-branches scoped to this stage. Pre-D5.5 rows backfilled to
	// stage_id=<stage1>; D5.5 staged convoys have one ask-branch row per
	// (repo, stage). For staging_mode='single' convoys the implicit
	// stage 1 owns every ask-branch.
	branches := store.ListConvoyAskBranchesByStage(db, convoyID, stage.ID)
	if len(branches) == 0 {
		// Single-mode fallback: stage 1's ask-branches may have been
		// inserted before the stage_id backfill; fall back to the
		// convoy-wide list so the SPA never renders an empty stage 1.
		if stageNum == 1 {
			branches = store.ListConvoyAskBranches(db, convoyID)
		}
	}

	// Roll up sub-PRs per (convoy, repo). Cheap to materialise — a single
	// list query per convoy followed by an in-memory partition by repo.
	allPRs := store.ListAskBranchPRsByConvoy(db, convoyID)
	prsByRepo := make(map[string][]store.AskBranchPR)
	for _, p := range allPRs {
		prsByRepo[p.Repo] = append(prsByRepo[p.Repo], p)
	}

	branchOut := make([]StageAskBranchResponse, 0, len(branches))
	for _, b := range branches {
		prs := make([]StagePRResponse, 0, len(prsByRepo[b.Repo]))
		for _, p := range prsByRepo[b.Repo] {
			prs = append(prs, StagePRResponse{
				ID:          p.ID,
				TaskID:      p.TaskID,
				PRNumber:    p.PRNumber,
				PRURL:       p.PRURL,
				State:       p.State,
				ChecksState: p.ChecksState,
				MergedAt:    p.MergedAt,
			})
		}
		branchOut = append(branchOut, StageAskBranchResponse{
			Repo:          b.Repo,
			AskBranch:     b.AskBranch,
			DraftPRURL:    b.DraftPRURL,
			DraftPRNumber: b.DraftPRNumber,
			DraftPRState:  b.DraftPRState,
			PRs:           prs,
		})
	}

	auditLog, alErr := store.ListStageAuditLog(db, convoyID, stageNum)
	if alErr != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, alErr.Error()), http.StatusInternalServerError)
		return
	}

	writeJSON(w, StageDetailResponse{
		Stage:       stageToResponse(stage),
		AskBranches: branchOut,
		AuditLog:    auditLog,
	})
}

// advanceStageHandler is the operator advance endpoint. Two modes:
//
//   - Normal advance (no audit_id): writes the rendezvous key in
//     SystemConfig that the operator_confirm gate evaluator reads, and
//     records an AuditLog entry. Stage transitions on the dog's next
//     tick. Idempotent: re-posting overwrites the SystemConfig timestamp.
//
//   - Emergency bypass (audit_id set, must match `^AUDIT-\d+$`): skips
//     gate evaluation entirely, transitions the stage directly to
//     GatePassed via store.BypassStage, and records the bypass in the
//     audit trail with the AUDIT id and reason in the detail blob. This
//     is the operator-only "production is on fire, cut through the
//     gate" path required by D5.5 exit criterion #10. Works regardless
//     of the stage's gate type (soak_minutes, threshold gates, etc.).
func advanceStageHandler(db *sql.DB, w http.ResponseWriter, r *http.Request, convoyID, stageNum int) {
	body, ok := decodeStageActionBody(w, r)
	if !ok {
		return
	}
	if strings.TrimSpace(body.Operator) == "" {
		http.Error(w, `{"error":"operator required"}`, http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.Reason) == "" {
		http.Error(w, `{"error":"reason required"}`, http.StatusBadRequest)
		return
	}

	// Bypass shape: operator provided an audit_id. Validate it strictly
	// before doing anything else so a malformed key never lands in the
	// audit trail (a "AUDIT-FOO" row would be worse than no row).
	bypass := strings.TrimSpace(body.AuditID) != ""
	if bypass && !stageBypassAuditIDRe.MatchString(body.AuditID) {
		http.Error(w, `{"error":"audit_id must match ^AUDIT-\\d+$"}`, http.StatusBadRequest)
		return
	}

	// Confirm the stage exists before writing — gives us a 404 path
	// (rather than a silent SystemConfig write that points at nothing).
	stage, err := store.GetStageByNum(db, convoyID, stageNum)
	if err != nil {
		if err == sql.ErrNoRows {
			http.NotFound(w, nil)
			return
		}
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
		return
	}
	if stage.Status == store.StageStatusVerified || stage.Status == store.StageStatusFailed {
		http.Error(w, fmt.Sprintf(`{"error":"stage in terminal status %q; cannot advance"}`, stage.Status), http.StatusBadRequest)
		return
	}

	if bypass {
		prevStatus := stage.Status
		if bErr := store.BypassStage(db, stage.ID); bErr != nil {
			http.Error(w, fmt.Sprintf(`{"error":%q}`, bErr.Error()), http.StatusInternalServerError)
			return
		}
		// Reason carries audit_id + free-text so the durable trail is
		// searchable post-incident. Action discriminator separates this
		// from the normal advance row in dashboard / Slack rollups.
		auditReason := fmt.Sprintf("%s %s", body.AuditID, body.Reason)
		if alErr := store.LogStageAudit(db, body.Operator, store.AuditActionStageBypass,
			convoyID, stageNum, prevStatus, store.StageStatusGatePassed, auditReason, ""); alErr != nil {
			// State already moved; surface the audit-write failure so the
			// operator can replay (the bypass itself is durable).
			http.Error(w, fmt.Sprintf(`{"error":%q}`, alErr.Error()), http.StatusInternalServerError)
			return
		}
		updated, _ := store.GetStageByNum(db, convoyID, stageNum)
		writeJSON(w, map[string]any{
			"ok":     true,
			"bypass": true,
			"stage":  stageToResponse(updated),
		})
		return
	}

	key := fmt.Sprintf("stage_advance_%d_%d", convoyID, stageNum)
	value := fmt.Sprintf("%s:%s", body.Operator, time.Now().UTC().Format(time.RFC3339))
	store.SetConfig(db, key, value)

	if alErr := store.LogStageAudit(db, body.Operator, store.AuditActionStageAdvance,
		convoyID, stageNum, stage.Status, stage.Status, body.Reason, ""); alErr != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, alErr.Error()), http.StatusInternalServerError)
		return
	}

	// Re-read so the response carries any state mutation (in this code
	// path the stage doesn't transition synchronously — the dog picks
	// up the SystemConfig key on its next tick — but returning the row
	// keeps the SPA in sync with what the DB believes).
	updated, _ := store.GetStageByNum(db, convoyID, stageNum)
	writeJSON(w, map[string]any{
		"ok":    true,
		"stage": stageToResponse(updated),
	})
}

// abortStageHandler forces the stage to Failed (terminal). The convoy
// itself is left in its current state; the operator decides revert /
// accept-as-shipped separately.
func abortStageHandler(db *sql.DB, w http.ResponseWriter, r *http.Request, convoyID, stageNum int) {
	body, ok := decodeStageActionBody(w, r)
	if !ok {
		return
	}
	if strings.TrimSpace(body.Operator) == "" {
		http.Error(w, `{"error":"operator required"}`, http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.Reason) == "" {
		http.Error(w, `{"error":"reason required"}`, http.StatusBadRequest)
		return
	}

	stage, err := store.GetStageByNum(db, convoyID, stageNum)
	if err != nil {
		if err == sql.ErrNoRows {
			http.NotFound(w, nil)
			return
		}
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
		return
	}
	if stage.Status == store.StageStatusVerified || stage.Status == store.StageStatusFailed {
		http.Error(w, fmt.Sprintf(`{"error":"stage already in terminal status %q"}`, stage.Status), http.StatusBadRequest)
		return
	}

	prevStatus := stage.Status
	if aErr := store.AdvanceStage(db, stage.ID, store.StageStatusFailed); aErr != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, aErr.Error()), http.StatusInternalServerError)
		return
	}

	if alErr := store.LogStageAudit(db, body.Operator, store.AuditActionStageAbort,
		convoyID, stageNum, prevStatus, store.StageStatusFailed, body.Reason, ""); alErr != nil {
		// Audit insert failed but the state already moved. Surface the
		// error so the operator can re-write the audit row (the abort
		// itself is durable).
		http.Error(w, fmt.Sprintf(`{"error":%q}`, alErr.Error()), http.StatusInternalServerError)
		return
	}

	updated, _ := store.GetStageByNum(db, convoyID, stageNum)
	writeJSON(w, map[string]any{
		"ok":    true,
		"stage": stageToResponse(updated),
	})
}

// decodeStageActionBody parses the JSON body for advance/abort. Writes the
// 4xx response itself when the body is malformed and returns ok=false.
func decodeStageActionBody(w http.ResponseWriter, r *http.Request) (stageAdvanceBody, bool) {
	var body stageAdvanceBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		if writeBodyReadError(w, err) {
			return body, false
		}
		http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
		return body, false
	}
	return body, true
}

// stageToResponse marshals a store.ConvoyStage into the JSON shape the SPA
// expects. Notably:
//   - gate_type is nullable — the JSON column is `null` for legacy single-mode
//     convoys' implicit stage 1 (and for any future "no gate" terminal stage).
//   - gate_config is forwarded as a json.RawMessage so the SPA gets the
//     parsed object rather than a stringified blob.
//   - gate_evaluation_status is derived from status + gate-stamp columns,
//     since we don't store the per-tick evaluation history.
func stageToResponse(s store.ConvoyStage) StageRowResponse {
	resp := StageRowResponse{
		ID:                   s.ID,
		ConvoyID:             s.ConvoyID,
		StageNum:             s.StageNum,
		IntentText:           s.IntentText,
		Status:               s.Status,
		GateTimeoutMinutes:   s.GateTimeoutMinutes,
		OpenedAt:             s.OpenedAt,
		AllPRsMergedAt:       s.AllPRsMergedAt,
		GatePassedAt:         s.GatePassedAt,
		CompletedAt:          s.CompletedAt,
		GateEvaluationStatus: deriveGateEvaluationStatus(s),
	}
	if !s.GateTypeIsNull {
		gt := s.GateType
		resp.GateType = &gt
	}
	cfg := s.GateConfigJSON
	if cfg == "" {
		cfg = "{}"
	}
	resp.GateConfig = json.RawMessage(cfg)
	return resp
}

// deriveGateEvaluationStatus rolls up the latest evaluation outcome from
// the row's status and stamp columns. Mirrors the dashboard's needs:
//
//   passed   — stage flipped to GatePassed (or beyond)
//   failed   — stage flipped to Failed
//   pending  — stage is AwaitingGate (the dog hasn't yet declared a winner)
//   n/a      — stage doesn't have a gate, or is too early to evaluate
func deriveGateEvaluationStatus(s store.ConvoyStage) string {
	if s.GateTypeIsNull {
		return "n/a"
	}
	switch s.Status {
	case store.StageStatusGatePassed, store.StageStatusVerified:
		return "passed"
	case store.StageStatusFailed:
		return "failed"
	case store.StageStatusAwaitingGate:
		return "pending"
	default:
		return "n/a"
	}
}
