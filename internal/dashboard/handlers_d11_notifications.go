// handlers_d11_notifications.go — D11 Phase 2: Notifications dashboard tab.
//
// Endpoint family backing the /notifications SPA tab. The substrate (the
// dispatch resolution chain itself, the YAML config, the SystemConfig keys)
// lives in internal/notify; this file is the read/write surface that the
// SPA's vanilla-JS fetch() calls hit.
//
// Endpoints:
//
//	GET  /api/notifications/catalog                         — per-category roster
//	                                                          + 7d fire-count
//	GET  /api/notifications/state                           — preset / DND /
//	                                                          per-category overrides
//	POST /api/notifications/preset                          — set active preset
//	POST /api/notifications/dnd                             — set DND window
//	POST /api/notifications/dnd/clear                       — clear DND
//	POST /api/notifications/category/<name>                 — per-category override
//	POST /api/notifications/category/<name>/clear           — clear per-category override
//	POST /api/notifications/preset/save                     — export current state
//	                                                          as a YAML diff for
//	                                                          PR review
//
// All state mutations write through internal/notify SystemConfig keys (or
// internal/notify.SetDND / ClearDND). Tier-1 per-category toggles require
// `confirm: true` in the body — the SPA shows a modal, the API enforces.
//
// NB: this file does NOT call notify.Dispatch. It manipulates configuration
// only; the dispatcher runs at every notification call site in the rest of
// the fleet (Pattern P-NotificationDispatch enforces).
package dashboard

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"force-orchestrator/internal/notify"
	"force-orchestrator/internal/store"
)

// ── Catalog ──────────────────────────────────────────────────────────────────

// notifCatalogRow is one entry in the /api/notifications/catalog response.
type notifCatalogRow struct {
	Category        string `json:"category"`
	Tier            int    `json:"tier"`
	YAMLDefault     string `json:"yaml_default"`
	CurrentSetting  string `json:"current_setting"`
	Last7DFireCount int    `json:"last_7d_fire_count"`
	Description     string `json:"description"`
	DNDBypass       bool   `json:"dnd_bypass"`
}

// handleNotificationsCatalog — GET /api/notifications/catalog.
//
// Returns the registered category roster joined with:
//   - the resolved per-category SystemConfig override (empty string when
//     the category falls through to preset/yaml-default — the SPA shows
//     "(default)" in that case);
//   - a count of mail rows in Fleet_Mail with subject prefix "[D11/<cat>]"
//     created in the last 7 days. Cheap, doesn't introduce a new audit
//     table; D11 P1 stamps every dispatched mail with that prefix.
func handleNotificationsCatalog(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"GET required"}`, http.StatusMethodNotAllowed)
			return
		}
		entries, err := notify.ListRegisteredCategories(db)
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
			return
		}
		rows := make([]notifCatalogRow, 0, len(entries))
		// Cutoff for 7-day rolling window. SQLite stores Fleet_Mail
		// created_at as 'YYYY-MM-DD HH:MM:SS' UTC text — a literal
		// string compare against the same shape works.
		cutoff := time.Now().UTC().Add(-7 * 24 * time.Hour).Format("2006-01-02 15:04:05")
		for _, e := range entries {
			ov := store.GetConfig(db, notify.ConfigKeyCategoryPrefix+e.Category, "")
			var fires int
			db.QueryRow(
				`SELECT COUNT(*) FROM Fleet_Mail
				 WHERE subject LIKE ? AND created_at > ?`,
				"[D11/"+e.Category+"]%", cutoff,
			).Scan(&fires)
			rows = append(rows, notifCatalogRow{
				Category:        e.Category,
				Tier:            int(e.Tier),
				YAMLDefault:     string(e.YAMLDefault),
				CurrentSetting:  ov,
				Last7DFireCount: fires,
				Description:     e.Description,
				DNDBypass:       notify.IsDNDBypass(e.Category),
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"rows": rows})
	}
}

// ── State ────────────────────────────────────────────────────────────────────

type notifPresetSummary struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type notifStateResponse struct {
	ActivePreset          string               `json:"active_preset"`
	DNDUntil              string               `json:"dnd_until"`
	DNDReason             string               `json:"dnd_reason"`
	DNDSetBy              string               `json:"dnd_set_by"`
	PerCategoryOverrides  map[string]string    `json:"per_category_overrides"`
	Presets               []notifPresetSummary `json:"presets"`
}

// handleNotificationsState — GET /api/notifications/state.
//
// Reads SystemConfig + the in-memory YAML config. The YAML config is loaded
// at daemon startup (cmd/force/fleet_cmds.go) via notify.SetGlobalConfig;
// tests install a synthetic config the same way.
func handleNotificationsState(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"GET required"}`, http.StatusMethodNotAllowed)
			return
		}
		cfg := notify.GetGlobalConfig()
		resp := notifStateResponse{
			ActivePreset:         store.GetConfig(db, notify.ConfigKeyActivePreset, "default"),
			DNDUntil:             store.GetConfig(db, notify.ConfigKeyDNDUntil, ""),
			DNDReason:            store.GetConfig(db, notify.ConfigKeyDNDReason, ""),
			DNDSetBy:             store.GetConfig(db, notify.ConfigKeyDNDSetBy, ""),
			PerCategoryOverrides: map[string]string{},
			Presets:              []notifPresetSummary{},
		}
		if cfg != nil {
			for _, name := range cfg.PresetNames() {
				resp.Presets = append(resp.Presets, notifPresetSummary{
					Name:        name,
					Description: cfg.Presets[name].Description,
				})
			}
			for _, cat := range cfg.CategoryNames() {
				if v := store.GetConfig(db, notify.ConfigKeyCategoryPrefix+cat, ""); v != "" {
					resp.PerCategoryOverrides[cat] = v
				}
			}
		}
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// ── Preset selection ─────────────────────────────────────────────────────────

// handleNotificationsPreset — POST /api/notifications/preset.
//
// Body: {"preset_name": "focus"}. Validates against the YAML's preset list.
// Writes notification_active_preset SystemConfig key + AuditLog row.
func handleNotificationsPreset(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"POST required"}`, http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			PresetName string `json:"preset_name"`
			Operator   string `json:"operator"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, `{"error":"bad JSON"}`, http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(body.PresetName) == "" {
			http.Error(w, `{"error":"preset_name required"}`, http.StatusBadRequest)
			return
		}
		cfg := notify.GetGlobalConfig()
		if cfg == nil {
			http.Error(w, `{"error":"notification config not loaded"}`, http.StatusInternalServerError)
			return
		}
		if _, ok := cfg.Presets[body.PresetName]; !ok {
			http.Error(w, fmt.Sprintf(`{"error":"unknown preset %q"}`, body.PresetName), http.StatusBadRequest)
			return
		}
		store.SetConfig(db, notify.ConfigKeyActivePreset, body.PresetName)
		actor := body.Operator
		if actor == "" {
			actor = "operator"
		}
		store.LogAudit(db, actor, "notify_preset_set", 0, body.PresetName)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "active_preset": body.PresetName})
	}
}

// ── DND ──────────────────────────────────────────────────────────────────────

// handleNotificationsDND — POST /api/notifications/dnd.
//
// Body: {"until":"2026-05-20T23:59:00Z","reason":"vacation","operator":"jake"}.
// All three fields required. Server-side enforces until <= now+14d (in
// addition to the client-side picker cap) — notify.SetDND returns the
// validated error which we surface as a 400.
func handleNotificationsDND(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"POST required"}`, http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Until    string `json:"until"`
			Reason   string `json:"reason"`
			Operator string `json:"operator"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, `{"error":"bad JSON"}`, http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(body.Until) == "" || strings.TrimSpace(body.Reason) == "" || strings.TrimSpace(body.Operator) == "" {
			http.Error(w, `{"error":"until, reason, and operator are all required"}`, http.StatusBadRequest)
			return
		}
		until, err := time.Parse(time.RFC3339, body.Until)
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"until must be RFC3339 (got %q): %s"}`, body.Until, err.Error()), http.StatusBadRequest)
			return
		}
		if err := notify.SetDND(db, until, body.Reason, body.Operator); err != nil {
			// Validator returned an error — surface as 400 with the
			// explicit message (e.g. "exceeds the 14-day cap").
			http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
			return
		}
		store.LogAudit(db, body.Operator, "notify_dnd_set", 0,
			fmt.Sprintf("until=%s reason=%s", until.UTC().Format(time.RFC3339), body.Reason))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":     true,
			"until":  until.UTC().Format(time.RFC3339),
			"reason": body.Reason,
		})
	}
}

// handleNotificationsDNDClear — POST /api/notifications/dnd/clear.
func handleNotificationsDNDClear(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"POST required"}`, http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Operator string `json:"operator"`
		}
		// Empty body is fine; operator may be implicit. Decode only if
		// there's a Content-Length, otherwise fall through to defaults.
		_ = json.NewDecoder(r.Body).Decode(&body)
		actor := body.Operator
		if actor == "" {
			actor = "operator"
		}
		notify.ClearDND(db)
		store.LogAudit(db, actor, "notify_dnd_clear", 0, "")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}
}

// ── Per-category override ────────────────────────────────────────────────────

// handleNotificationsCategory dispatches POST /api/notifications/category/<name>
// and POST /api/notifications/category/<name>/clear.
func handleNotificationsCategory(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"POST required"}`, http.StatusMethodNotAllowed)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/api/notifications/category/")
		path = strings.TrimSuffix(path, "/")
		if path == "" {
			http.Error(w, `{"error":"category name required in path"}`, http.StatusBadRequest)
			return
		}
		// Detect the /clear suffix.
		isClear := false
		if strings.HasSuffix(path, "/clear") {
			isClear = true
			path = strings.TrimSuffix(path, "/clear")
		}
		if path == "" {
			http.Error(w, `{"error":"category name required"}`, http.StatusBadRequest)
			return
		}
		cfg := notify.GetGlobalConfig()
		if cfg == nil {
			http.Error(w, `{"error":"notification config not loaded"}`, http.StatusInternalServerError)
			return
		}
		spec, known := cfg.Categories[path]
		if !known {
			http.Error(w, fmt.Sprintf(`{"error":"unknown category %q"}`, path), http.StatusBadRequest)
			return
		}
		var body struct {
			Setting  string `json:"setting"`
			Operator string `json:"operator"`
			Reason   string `json:"reason"`
			Confirm  bool   `json:"confirm"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		actor := body.Operator
		if actor == "" {
			actor = "operator"
		}
		if isClear {
			store.SetConfig(db, notify.ConfigKeyCategoryPrefix+path, "")
			store.LogAudit(db, actor, "notify_category_clear", 0, path)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "category": path})
			return
		}
		s := notify.Setting(body.Setting)
		if !s.IsValid() {
			http.Error(w, fmt.Sprintf(`{"error":"setting must be one of off|mail|slack|mail+slack (got %q)"}`, body.Setting), http.StatusBadRequest)
			return
		}
		// Tier-1 confirm gate. Server-side enforcement (the SPA shows
		// a modal; this is the belt-and-suspenders).
		if spec.Tier == notify.Tier1 && !body.Confirm {
			http.Error(w, `{"error":"Tier-1 categories require confirm:true to silence — operator must acknowledge the carve-out"}`, http.StatusBadRequest)
			return
		}
		store.SetConfig(db, notify.ConfigKeyCategoryPrefix+path, body.Setting)
		store.LogAudit(db, actor, "notify_category_set", 0,
			fmt.Sprintf("category=%s setting=%s reason=%s", path, body.Setting, body.Reason))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":       true,
			"category": path,
			"setting":  body.Setting,
		})
	}
}

// ── Save-as-preset (export-for-review) ───────────────────────────────────────

// handleNotificationsPresetSave — POST /api/notifications/preset/save.
//
// Writes a YAML diff to /tmp/notifications-preset-<timestamp>.yaml.diff
// capturing the preset definition the operator wants to commit. Returns
// the path; the operator pastes it into a real PR via the normal `git add
// config/notifications.yaml` workflow. We deliberately do NOT edit the
// YAML on disk: that would conflate "operator clicked save" with "operator
// merged a PR" and skip review.
//
// The output is a real, parseable notify.Config YAML block (preset only)
// so a reviewer can copy-paste under the existing `presets:` section. The
// test asserts notify.ParseConfig accepts it after embedding.
func handleNotificationsPresetSave(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"POST required"}`, http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Name        string            `json:"name"`
			Description string            `json:"description"`
			Rules       map[string]string `json:"rules"`
			Operator    string            `json:"operator"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, `{"error":"bad JSON"}`, http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(body.Name) == "" || strings.TrimSpace(body.Description) == "" {
			http.Error(w, `{"error":"name and description required"}`, http.StatusBadRequest)
			return
		}
		if len(body.Rules) == 0 {
			http.Error(w, `{"error":"rules must be non-empty"}`, http.StatusBadRequest)
			return
		}
		cfg := notify.GetGlobalConfig()
		if cfg == nil {
			http.Error(w, `{"error":"notification config not loaded"}`, http.StatusInternalServerError)
			return
		}
		// Validate every rule. Wildcard "*" allowed; non-wildcard keys
		// must match a registered category. Settings must parse.
		ruleNames := make([]string, 0, len(body.Rules))
		for k := range body.Rules {
			ruleNames = append(ruleNames, k)
		}
		sort.Strings(ruleNames)
		for _, k := range ruleNames {
			v := body.Rules[k]
			if !notify.Setting(v).IsValid() {
				http.Error(w, fmt.Sprintf(`{"error":"rule %q has invalid setting %q"}`, k, v), http.StatusBadRequest)
				return
			}
			if k != "*" {
				if _, ok := cfg.Categories[k]; !ok {
					http.Error(w, fmt.Sprintf(`{"error":"rule references unknown category %q"}`, k), http.StatusBadRequest)
					return
				}
			}
		}

		// Compose the YAML preset block. Ordering: keys sorted so the
		// diff is deterministic across runs.
		var sb strings.Builder
		sb.WriteString("# Generated by /api/notifications/preset/save — paste under `presets:` in\n")
		sb.WriteString("# config/notifications.yaml and commit via the normal review path.\n")
		fmt.Fprintf(&sb, "  %s:\n", body.Name)
		fmt.Fprintf(&sb, "    description: %s\n", yamlEscape(body.Description))
		sb.WriteString("    rules:\n")
		for _, k := range ruleNames {
			fmt.Fprintf(&sb, "      %s: %s\n", yamlKey(k), body.Rules[k])
		}

		// Write to a temp file so the operator can `cat` it and copy to
		// the real config in a PR. The path includes a timestamp so
		// successive saves don't clobber.
		ts := time.Now().UTC().Format("20060102T150405Z")
		fname := fmt.Sprintf("notifications-preset-%s-%s.yaml.diff", body.Name, ts)
		fpath := filepath.Join(os.TempDir(), fname)
		if err := os.WriteFile(fpath, []byte(sb.String()), 0o644); err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"write %s failed: %s"}`, fpath, err.Error()), http.StatusInternalServerError)
			return
		}
		actor := body.Operator
		if actor == "" {
			actor = "operator"
		}
		store.LogAudit(db, actor, "notify_preset_save", 0, fmt.Sprintf("name=%s path=%s", body.Name, fpath))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":      true,
			"path":    fpath,
			"content": sb.String(),
		})
	}
}

// yamlKey wraps a key in quotes if it's the wildcard or contains characters
// that need quoting (mirrors the on-disk style of config/notifications.yaml
// where "*" is quoted).
func yamlKey(k string) string {
	if k == "*" {
		return `"*"`
	}
	return k
}

// yamlEscape returns a YAML-safe scalar for free-text descriptions. We
// quote unconditionally to dodge the "value starts with a special character"
// trap (a description starting with "-" or ":" would parse as something
// else otherwise).
func yamlEscape(s string) string {
	// Replace embedded quotes with escaped form.
	return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
}
