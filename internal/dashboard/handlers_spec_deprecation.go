// D3 fix-loop-1 / γ3 — Operator endpoint for spec-item deprecation
// (concern #9, exit criterion 14d).
//
// POST /api/convoy/<id>/deprecate-spec-item
//
// Body shape:
//
//	{
//	  "item_id": "AT-3",
//	  "item_kind": "at" | "ec",
//	  "rationale": "≥ 20 chars explanation",
//	  "removal_kind": "mistake|superseded|satisfied|out_of_scope",
//	  "operator_email": "op@example.com",
//	  "superseded_by_kind": "at|fleet_rule" (optional),
//	  "superseded_by_ref": "AT-99" (optional),
//	  "inflight_disposition": "cancel_and_remove" | "complete_then_remove" | "cancel_removal" (optional)
//	}
//
// Response:
//
//	200 — { "ok": true, "inflight_task_ids": [...] }
//	409 — { "error": "...", "inflight_task_ids": [...] } (when in-flight tasks
//	      exist and no inflight_disposition was supplied)
//	400 — validation error
//
// Pattern P21 wiring (slice α — AT-removal-is-operator-only):
//   - The handler requires operator_email in the body. The store helper
//     DeprecateSpecItem refuses an empty email, so the agent path that
//     somehow acquired this entry point (impossible without a routing
//     bug) still cannot complete the deprecation.
//   - This file is the ONLY caller of DeprecateSpecItem. Pattern P21 in
//     slice α walks the LLM proposal schemas (Captain proposed_action_json,
//     ConvoyReview amendment proposals, EC promotion proposals) to
//     ensure none of them carry a "remove" or "deprecate" intent on AT
//     references — closing the AST-level door from the proposal side.
//     Together they form a runtime + AST-level barrier.
package dashboard

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"force-orchestrator/internal/store"
)

// handleSpecDeprecation routes /api/convoy/<id>/deprecate-spec-item.
func handleSpecDeprecation(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}

		// Path: /api/convoy/<id>/deprecate-spec-item
		path := strings.TrimPrefix(r.URL.Path, "/api/convoy/")
		// path: "<id>/deprecate-spec-item"
		parts := strings.Split(path, "/")
		if len(parts) != 2 || parts[1] != "deprecate-spec-item" {
			http.Error(w, `{"error":"path: /api/convoy/<id>/deprecate-spec-item"}`, http.StatusBadRequest)
			return
		}
		convoyID, err := strconv.Atoi(parts[0])
		if err != nil || convoyID <= 0 {
			http.Error(w, `{"error":"bad convoy id"}`, http.StatusBadRequest)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, `{"error":"body read"}`, http.StatusRequestEntityTooLarge)
			return
		}
		var in struct {
			ItemID              string `json:"item_id"`
			ItemKind            string `json:"item_kind"`
			Rationale           string `json:"rationale"`
			RemovalKind         string `json:"removal_kind"`
			OperatorEmail       string `json:"operator_email"`
			SupersededByKind    string `json:"superseded_by_kind"`
			SupersededByRef     string `json:"superseded_by_ref"`
			InflightDisposition string `json:"inflight_disposition"`
		}
		if err := json.Unmarshal(body, &in); err != nil {
			http.Error(w, `{"error":"bad JSON"}`, http.StatusBadRequest)
			return
		}
		if in.ItemKind == "" {
			in.ItemKind = "at"
		}

		// In-flight check (concern #9 — modal forces operator to pick a
		// disposition). Without an explicit disposition supplied, refuse
		// with 409 and surface the task IDs the operator must address.
		inflight, ierr := store.InflightTasksForAT(db, convoyID, in.ItemID)
		if ierr != nil {
			http.Error(w, `{"error":"`+ierr.Error()+`"}`, http.StatusInternalServerError)
			return
		}
		if len(inflight) > 0 && in.InflightDisposition == "" {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error":             "inflight tasks exist; pass inflight_disposition",
				"inflight_task_ids": inflight,
			})
			return
		}

		// Apply the disposition before the deprecation lands so the spec
		// edit is the last write the operator sees succeed.
		switch in.InflightDisposition {
		case "", "cancel_removal":
			// no-op; either no in-flight, or operator chose to cancel.
			if in.InflightDisposition == "cancel_removal" {
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"ok":                true,
					"action":            "cancel_removal",
					"inflight_task_ids": inflight,
				})
				return
			}
		case "cancel_and_remove":
			for _, taskID := range inflight {
				_ = store.UpdateBountyStatus(db, taskID, "Cancelled")
			}
		case "complete_then_remove":
			// Mark them with a flag so ConvoyReview keeps evaluating
			// until they land. The schema doesn't have a dedicated
			// pending_deprecation column, so we stash the marker in
			// error_log to keep the change additive.
			for _, taskID := range inflight {
				db.Exec(`UPDATE BountyBoard
					SET error_log = COALESCE(error_log,'') || char(10) || '[PENDING_DEPRECATION:' || ? || ']'
					WHERE id = ?`, in.ItemID, taskID)
			}
		default:
			http.Error(w, `{"error":"unknown inflight_disposition"}`, http.StatusBadRequest)
			return
		}

		args := store.DeprecateSpecItemArgs{
			ConvoyID:        convoyID,
			ItemID:          in.ItemID,
			ItemKind:        store.SpecItemKind(in.ItemKind),
			Rationale:       in.Rationale,
			RemovalKind:     in.RemovalKind,
			RemovedByEmail:  in.OperatorEmail,
			SupersededByRef: in.SupersededByRef,
			SupersededByKnd: in.SupersededByKind,
		}
		if err := store.DeprecateSpecItem(db, args); err != nil {
			http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
			return
		}

		// Captain proposal re-justification trigger (concern #9 / roadmap
		// 1190): proposals citing the now-deprecated AT need to be
		// re-justified. We mark them via an idempotent SQL update — the
		// existing proposal-management surface picks up the flag in the
		// next operator pass.
		db.Exec(`UPDATE BountyBoard
			SET error_log = COALESCE(error_log,'') || char(10) || '[NEEDS_REJUSTIFICATION:' || ? || ']'
			WHERE convoy_id = ?
			  AND IFNULL(proposed_action_json,'') LIKE '%' || ? || '%'
			  AND status NOT IN ('Completed','Cancelled','Failed')`,
			in.ItemID, convoyID, in.ItemID)

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":                true,
			"inflight_task_ids": inflight,
			"disposition":       in.InflightDisposition,
		})
	}
}
