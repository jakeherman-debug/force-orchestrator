// internal/dashboard/handlers_convoy_watch.go — D11 Phase 2 Sub-task B.
//
// Per-convoy notification override (the "👁 Watch" surface on a convoy
// card). Three endpoints scoped under /api/convoys/<id>/watch:
//
//   GET  /api/convoys/<id>/watch
//        Returns the current ConvoyNotificationOverrides row (or `{}` if
//        none) plus the list of registered notification categories so
//        the popover can render a per-category toggle grid.
//
//   POST /api/convoys/<id>/watch
//        Body: {"mode":"verbose|quiet|custom_json","custom_json":"{...}",
//               "operator":"jake","reason":"tracking ZDM migration"}
//        Validates mode + (when custom) the JSON shape, then upserts via
//        store.UpsertConvoyNotificationOverride. Reason + operator land
//        in the audit trail.
//
//   POST /api/convoys/<id>/watch/clear
//        Body: {"operator":"jake","reason":"no longer needed"}
//        Clears the override (deletes the row). Audit trail.
//
// All overrides write through store helpers — no inline SQL in this
// handler. Pattern P-Convoy-Inline-SQL would catch a regression.

package dashboard

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"force-orchestrator/internal/notify"
	"force-orchestrator/internal/store"
)

// watchSetBody is the POST /watch JSON body.
type watchSetBody struct {
	Mode       string `json:"mode"`
	CustomJSON string `json:"custom_json,omitempty"`
	Operator   string `json:"operator"`
	Reason     string `json:"reason"`
}

// watchClearBody is the POST /watch/clear JSON body.
type watchClearBody struct {
	Operator string `json:"operator"`
	Reason   string `json:"reason"`
}

// watchGetResponse is the GET /watch payload. Override is the parsed
// ConvoyNotificationOverrides row (zero-value when absent); HasOverride
// disambiguates "no row" from "row with mode='verbose' and empty reason"
// for the SPA.
type watchGetResponse struct {
	HasOverride bool                          `json:"has_override"`
	Override    *watchOverrideShape           `json:"override,omitempty"`
	Categories  []watchCategoryShape          `json:"categories"`
	Settings    []string                      `json:"settings"`
}

type watchOverrideShape struct {
	ConvoyID       int               `json:"convoy_id"`
	Mode           string            `json:"mode"`
	CustomJSON     map[string]string `json:"custom_json,omitempty"`
	CustomJSONRaw  string            `json:"custom_json_raw"`
	SetAt          string            `json:"set_at"`
	SetBy          string            `json:"set_by"`
	Reason         string            `json:"reason"`
	ConvoyClosedAt string            `json:"convoy_closed_at,omitempty"`
}

type watchCategoryShape struct {
	Name        string `json:"name"`
	Tier        int    `json:"tier"`
	YAMLDefault string `json:"yaml_default"`
	Description string `json:"description"`
}

// validWatchSettings is the set of legal values for a custom_json entry.
// Mirrors notify.Setting.IsValid but inlined here so we don't import an
// internal-only helper.
var validWatchSettings = map[string]struct{}{
	"off":        {},
	"mail":       {},
	"slack":      {},
	"mail+slack": {},
}

// handleConvoyWatch dispatches the three /watch routes off the
// /api/convoys/<id>/ subtree. Returns ok=true if the request was handled
// (caller should not fall through to the default 404).
//
// parts is the segments after /api/convoys/<id>/ — caller already split.
// Expected: ["watch"] or ["watch", "clear"].
func handleConvoyWatch(db *sql.DB, w http.ResponseWriter, r *http.Request, convoyID int, parts []string) bool {
	if len(parts) == 0 || parts[0] != "watch" {
		return false
	}
	switch len(parts) {
	case 1:
		// /api/convoys/<id>/watch
		switch r.Method {
		case http.MethodGet:
			getConvoyWatch(db, w, convoyID)
		case http.MethodPost:
			postConvoyWatch(db, w, r, convoyID)
		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
		return true
	case 2:
		// /api/convoys/<id>/watch/clear
		if parts[1] != "clear" {
			return false
		}
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return true
		}
		clearConvoyWatch(db, w, r, convoyID)
		return true
	}
	return false
}

// getConvoyWatch returns the current override (if any) plus the
// registered category list so the popover can render its custom grid.
func getConvoyWatch(db *sql.DB, w http.ResponseWriter, convoyID int) {
	resp := watchGetResponse{
		Categories: []watchCategoryShape{},
		Settings:   []string{"off", "mail", "slack", "mail+slack"},
	}

	ov, err := store.GetConvoyNotificationOverride(db, convoyID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Leave HasOverride=false; the SPA renders "Default" state.
	case err != nil:
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
		return
	default:
		shape := watchOverrideShape{
			ConvoyID:       ov.ConvoyID,
			Mode:           ov.Mode,
			CustomJSONRaw:  ov.CustomJSON,
			SetAt:          ov.SetAt,
			SetBy:          ov.SetBy,
			Reason:         ov.Reason,
			ConvoyClosedAt: ov.ConvoyClosedAt,
		}
		if ov.Mode == "custom_json" && ov.CustomJSON != "" && ov.CustomJSON != "{}" {
			parsed := map[string]string{}
			// Tolerate already-validated DB content: malformed JSON here
			// would only happen via direct DB write (not through this
			// handler), so we surface a 500 rather than silently dropping.
			if jerr := json.Unmarshal([]byte(ov.CustomJSON), &parsed); jerr != nil {
				http.Error(w, fmt.Sprintf(`{"error":"stored custom_json malformed: %s"}`, jerr.Error()), http.StatusInternalServerError)
				return
			}
			shape.CustomJSON = parsed
		}
		resp.HasOverride = true
		resp.Override = &shape
	}

	cats, err := notify.ListRegisteredCategories(db)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
		return
	}
	for _, c := range cats {
		resp.Categories = append(resp.Categories, watchCategoryShape{
			Name:        c.Category,
			Tier:        int(c.Tier),
			YAMLDefault: string(c.YAMLDefault),
			Description: c.Description,
		})
	}

	writeJSON(w, resp)
}

// postConvoyWatch validates and upserts the override for a convoy.
func postConvoyWatch(db *sql.DB, w http.ResponseWriter, r *http.Request, convoyID int) {
	var body watchSetBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		if writeBodyReadError(w, err) {
			return
		}
		http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
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
	switch body.Mode {
	case "verbose", "quiet", "custom_json":
	default:
		http.Error(w, `{"error":"mode must be verbose|quiet|custom_json"}`, http.StatusBadRequest)
		return
	}

	customJSON := strings.TrimSpace(body.CustomJSON)
	if body.Mode == "custom_json" {
		if customJSON == "" {
			http.Error(w, `{"error":"custom_json required when mode=custom_json"}`, http.StatusBadRequest)
			return
		}
		// Validate: must parse as map[string]string AND each value must
		// be a legal Setting. The "*" wildcard key is allowed.
		var parsed map[string]string
		if err := json.Unmarshal([]byte(customJSON), &parsed); err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"custom_json malformed: %s"}`, err.Error()), http.StatusBadRequest)
			return
		}
		if len(parsed) == 0 {
			http.Error(w, `{"error":"custom_json must contain at least one entry"}`, http.StatusBadRequest)
			return
		}
		for k, v := range parsed {
			if k == "" {
				http.Error(w, `{"error":"custom_json key must be non-empty"}`, http.StatusBadRequest)
				return
			}
			if _, ok := validWatchSettings[v]; !ok {
				http.Error(w, fmt.Sprintf(`{"error":"custom_json key %q has invalid setting %q (want off|mail|slack|mail+slack)"}`, k, v), http.StatusBadRequest)
				return
			}
		}
		// Re-marshal so what we store is a normalised JSON form
		// (whitespace-stripped, key-ordered by the encoder). This
		// mirrors the dispatcher's loadConvoyOverride expectation.
		buf, _ := json.Marshal(parsed)
		customJSON = string(buf)
	} else {
		// For verbose/quiet, ignore any client-supplied custom_json
		// — it's meaningless and would just clutter the row.
		customJSON = "{}"
	}

	if err := store.UpsertConvoyNotificationOverride(db, store.ConvoyNotificationOverride{
		ConvoyID:   convoyID,
		Mode:       body.Mode,
		CustomJSON: customJSON,
		SetBy:      body.Operator,
		Reason:     body.Reason,
	}); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
		return
	}

	// Audit trail: who, why, what mode. Detail field carries the
	// reason verbatim so audit-log searches can filter by free text.
	store.LogAudit(db, body.Operator, "convoy-watch-set", convoyID,
		fmt.Sprintf("mode=%s reason=%s", body.Mode, body.Reason))

	writeJSON(w, map[string]any{
		"ok":        true,
		"convoy_id": convoyID,
		"mode":      body.Mode,
	})
}

// clearConvoyWatch deletes the override row.
func clearConvoyWatch(db *sql.DB, w http.ResponseWriter, r *http.Request, convoyID int) {
	var body watchClearBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		if writeBodyReadError(w, err) {
			return
		}
		http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.Operator) == "" {
		http.Error(w, `{"error":"operator required"}`, http.StatusBadRequest)
		return
	}
	// Reason is optional for clear (the operator might just be
	// reverting an accidental override; "no longer needed" is fine).
	if err := store.ClearConvoyNotificationOverride(db, convoyID); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
		return
	}
	store.LogAudit(db, body.Operator, "convoy-watch-clear", convoyID,
		fmt.Sprintf("reason=%s", body.Reason))
	writeJSON(w, map[string]any{
		"ok":        true,
		"convoy_id": convoyID,
		"cleared":   true,
	})
}
