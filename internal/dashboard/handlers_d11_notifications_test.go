// handlers_d11_notifications_test.go — D11 Phase 2 dashboard handler tests.
//
// Each endpoint is tested with httptest + an in-memory holocron + a synthetic
// notify.Config installed via SetGlobalConfig. SlackNotifier is stubbed via
// SetSlackNotifierForTest so a regression that accidentally fires a real
// dispatch is caught (the count stays at zero — these handlers configure
// state, they do not dispatch).
package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"force-orchestrator/internal/notify"
	"force-orchestrator/internal/store"
)

// notifInstallConfig installs a small synthetic notify.Config covering all
// three tiers + a DND-bypass category + the three canonical presets. Every
// test in this file uses this shape so a bug in one test's seed doesn't
// cascade into another's setup.
func notifInstallConfig(t *testing.T) func() {
	t.Helper()
	cfg, err := notify.ParseConfig([]byte(`
version: 1
categories:
  tier1_act:
    tier: 1
    default: mail+slack
    description: tier1 must-act
  tier2_info:
    tier: 2
    default: mail
    description: tier2 informational
  tier3_trace:
    tier: 3
    default: off
    description: tier3 debug
  spend_cap_e_stop:
    tier: 1
    default: mail+slack
    description: dnd-bypass
presets:
  default:
    description: per-tier defaults
    rules: tier_defaults
  focus:
    description: quiet
    rules:
      "*": off
      tier1_act: mail+slack
  verbose:
    description: loud
    rules:
      "*": mail+slack
`), "test.yaml")
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	notify.SetGlobalConfig(cfg)
	return func() { notify.SetGlobalConfig(nil) }
}

func notifInstallSlackStub(t *testing.T) (calls *[]string, restore func()) {
	t.Helper()
	var (
		mu sync.Mutex
		c  []string
	)
	r := notify.SetSlackNotifierForTest(func(_ context.Context, label string) error {
		mu.Lock()
		c = append(c, label)
		mu.Unlock()
		return nil
	})
	return &c, r
}

// ── Catalog ─────────────────────────────────────────────────────────────────

func TestHandleNotificationsCatalog_HappyPath(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer notifInstallConfig(t)()
	_, restore := notifInstallSlackStub(t)
	defer restore()

	cfg := notify.GetGlobalConfig()
	if err := notify.SeedRegistryFromYAML(db, cfg); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Seed a few mail rows so the 7d count is non-zero for one category.
	for i := 0; i < 3; i++ {
		store.SendMail(db, "notify", "operator", "[D11/tier1_act] x", "body", 0, store.MailTypeAlert)
	}
	// Add a category override so current_setting is reflected.
	store.SetConfig(db, "notification_category_tier2_info", "off")

	req := httptest.NewRequest(http.MethodGet, "/api/notifications/catalog", nil)
	rr := httptest.NewRecorder()
	handleNotificationsCatalog(db)(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Rows []notifCatalogRow `json:"rows"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Rows) != 4 {
		t.Errorf("rows=%d, want 4", len(resp.Rows))
	}
	var sawTier1, sawTier2 bool
	for _, r := range resp.Rows {
		if r.Category == "tier1_act" {
			sawTier1 = true
			if r.Last7DFireCount != 3 {
				t.Errorf("tier1_act fires=%d, want 3", r.Last7DFireCount)
			}
			if r.Tier != 1 {
				t.Errorf("tier1_act tier=%d, want 1", r.Tier)
			}
		}
		if r.Category == "tier2_info" {
			sawTier2 = true
			if r.CurrentSetting != "off" {
				t.Errorf("tier2_info current=%q, want off", r.CurrentSetting)
			}
		}
		if r.Category == "spend_cap_e_stop" && !r.DNDBypass {
			t.Errorf("spend_cap_e_stop should be DND-bypass")
		}
	}
	if !sawTier1 || !sawTier2 {
		t.Errorf("missing rows in response: %+v", resp.Rows)
	}
}

func TestHandleNotificationsCatalog_RejectsNonGET(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer notifInstallConfig(t)()
	_, restore := notifInstallSlackStub(t)
	defer restore()

	req := httptest.NewRequest(http.MethodPost, "/api/notifications/catalog", nil)
	rr := httptest.NewRecorder()
	handleNotificationsCatalog(db)(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("code=%d, want 405", rr.Code)
	}
}

// ── State ───────────────────────────────────────────────────────────────────

func TestHandleNotificationsState_HappyPath(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer notifInstallConfig(t)()
	_, restore := notifInstallSlackStub(t)
	defer restore()

	store.SetConfig(db, notify.ConfigKeyActivePreset, "focus")
	store.SetConfig(db, "notification_category_tier1_act", "mail")
	until := time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339)
	store.SetConfig(db, notify.ConfigKeyDNDUntil, until)
	store.SetConfig(db, notify.ConfigKeyDNDReason, "lunch")
	store.SetConfig(db, notify.ConfigKeyDNDSetBy, "jake")

	req := httptest.NewRequest(http.MethodGet, "/api/notifications/state", nil)
	rr := httptest.NewRecorder()
	handleNotificationsState(db)(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp notifStateResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.ActivePreset != "focus" {
		t.Errorf("active_preset=%q, want focus", resp.ActivePreset)
	}
	if resp.DNDReason != "lunch" || resp.DNDSetBy != "jake" {
		t.Errorf("DND fields wrong: %+v", resp)
	}
	if resp.PerCategoryOverrides["tier1_act"] != "mail" {
		t.Errorf("per-cat overrides=%v, want tier1_act:mail", resp.PerCategoryOverrides)
	}
	if len(resp.Presets) < 3 {
		t.Errorf("presets=%v, want at least 3 (default/focus/verbose)", resp.Presets)
	}
}

func TestHandleNotificationsState_RejectsNonGET(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer notifInstallConfig(t)()
	_, restore := notifInstallSlackStub(t)
	defer restore()

	req := httptest.NewRequest(http.MethodPost, "/api/notifications/state", nil)
	rr := httptest.NewRecorder()
	handleNotificationsState(db)(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("code=%d, want 405", rr.Code)
	}
}

// ── Preset selection ────────────────────────────────────────────────────────

func TestHandleNotificationsPreset_HappyPath(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer notifInstallConfig(t)()
	slack, restore := notifInstallSlackStub(t)
	defer restore()

	body := strings.NewReader(`{"preset_name":"focus","operator":"jake"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/preset", body)
	rr := httptest.NewRecorder()
	handleNotificationsPreset(db)(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
	if got := store.GetConfig(db, notify.ConfigKeyActivePreset, ""); got != "focus" {
		t.Errorf("active_preset=%q, want focus", got)
	}
	if len(*slack) != 0 {
		t.Errorf("preset switch fired Slack: %v (must not — handlers configure, dispatcher dispatches)", *slack)
	}
	// Audit row.
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM AuditLog WHERE action = 'notify_preset_set'`).Scan(&n)
	if n != 1 {
		t.Errorf("audit rows=%d, want 1", n)
	}
}

func TestHandleNotificationsPreset_UnknownPreset(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer notifInstallConfig(t)()
	_, restore := notifInstallSlackStub(t)
	defer restore()

	body := strings.NewReader(`{"preset_name":"nonsuch"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/preset", body)
	rr := httptest.NewRecorder()
	handleNotificationsPreset(db)(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("code=%d, want 400", rr.Code)
	}
}

func TestHandleNotificationsPreset_RejectsNonPOST(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer notifInstallConfig(t)()
	_, restore := notifInstallSlackStub(t)
	defer restore()

	req := httptest.NewRequest(http.MethodGet, "/api/notifications/preset", nil)
	rr := httptest.NewRecorder()
	handleNotificationsPreset(db)(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("code=%d, want 405", rr.Code)
	}
}

// ── DND ─────────────────────────────────────────────────────────────────────

func TestHandleNotificationsDND_HappyPath(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer notifInstallConfig(t)()
	_, restore := notifInstallSlackStub(t)
	defer restore()

	until := time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339)
	body := strings.NewReader(fmt.Sprintf(`{"until":%q,"reason":"lunch","operator":"jake"}`, until))
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/dnd", body)
	rr := httptest.NewRecorder()
	handleNotificationsDND(db)(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
	if got := store.GetConfig(db, notify.ConfigKeyDNDUntil, ""); got == "" {
		t.Errorf("DND until not stored")
	}
	if got := store.GetConfig(db, notify.ConfigKeyDNDReason, ""); got != "lunch" {
		t.Errorf("DND reason=%q, want lunch", got)
	}
	if got := store.GetConfig(db, notify.ConfigKeyDNDSetBy, ""); got != "jake" {
		t.Errorf("DND set_by=%q, want jake", got)
	}
}

func TestHandleNotificationsDND_RejectsBeyond14Days(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer notifInstallConfig(t)()
	_, restore := notifInstallSlackStub(t)
	defer restore()

	until := time.Now().UTC().Add(15 * 24 * time.Hour).Format(time.RFC3339)
	body := strings.NewReader(fmt.Sprintf(`{"until":%q,"reason":"too long","operator":"jake"}`, until))
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/dnd", body)
	rr := httptest.NewRecorder()
	handleNotificationsDND(db)(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("code=%d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "14-day cap") {
		t.Errorf("body lacks 14-day cap message: %s", rr.Body.String())
	}
}

func TestHandleNotificationsDND_AcceptsJustUnder14Days(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer notifInstallConfig(t)()
	_, restore := notifInstallSlackStub(t)
	defer restore()

	until := time.Now().UTC().Add(14*24*time.Hour - 1*time.Minute).Format(time.RFC3339)
	body := strings.NewReader(fmt.Sprintf(`{"until":%q,"reason":"vacation","operator":"jake"}`, until))
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/dnd", body)
	rr := httptest.NewRecorder()
	handleNotificationsDND(db)(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("code=%d body=%s, want 200", rr.Code, rr.Body.String())
	}
}

func TestHandleNotificationsDND_RejectsPastTimestamp(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer notifInstallConfig(t)()
	_, restore := notifInstallSlackStub(t)
	defer restore()

	until := time.Now().UTC().Add(-1 * time.Minute).Format(time.RFC3339)
	body := strings.NewReader(fmt.Sprintf(`{"until":%q,"reason":"past","operator":"jake"}`, until))
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/dnd", body)
	rr := httptest.NewRecorder()
	handleNotificationsDND(db)(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("code=%d, want 400", rr.Code)
	}
}

func TestHandleNotificationsDND_RequiresAllFields(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer notifInstallConfig(t)()
	_, restore := notifInstallSlackStub(t)
	defer restore()

	until := time.Now().UTC().Add(1 * time.Hour).Format(time.RFC3339)
	body := strings.NewReader(fmt.Sprintf(`{"until":%q,"reason":"","operator":"jake"}`, until))
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/dnd", body)
	rr := httptest.NewRecorder()
	handleNotificationsDND(db)(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("code=%d, want 400 (missing reason)", rr.Code)
	}
}

func TestHandleNotificationsDNDClear_HappyPath(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer notifInstallConfig(t)()
	_, restore := notifInstallSlackStub(t)
	defer restore()

	until := time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339)
	store.SetConfig(db, notify.ConfigKeyDNDUntil, until)
	store.SetConfig(db, notify.ConfigKeyDNDReason, "lunch")

	body := strings.NewReader(`{"operator":"jake"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/dnd/clear", body)
	rr := httptest.NewRecorder()
	handleNotificationsDNDClear(db)(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
	if got := store.GetConfig(db, notify.ConfigKeyDNDUntil, ""); got != "" {
		t.Errorf("DND until not cleared: %q", got)
	}
	if got := store.GetConfig(db, notify.ConfigKeyDNDReason, ""); got != "" {
		t.Errorf("DND reason not cleared: %q", got)
	}
}

// ── Per-category override ───────────────────────────────────────────────────

func TestHandleNotificationsCategory_Tier2_HappyPath(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer notifInstallConfig(t)()
	_, restore := notifInstallSlackStub(t)
	defer restore()

	body := strings.NewReader(`{"setting":"slack","operator":"jake","reason":"tuning"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/category/tier2_info", body)
	rr := httptest.NewRecorder()
	handleNotificationsCategory(db)(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
	if got := store.GetConfig(db, "notification_category_tier2_info", ""); got != "slack" {
		t.Errorf("override=%q, want slack", got)
	}
}

func TestHandleNotificationsCategory_Tier1_RequiresConfirm(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer notifInstallConfig(t)()
	_, restore := notifInstallSlackStub(t)
	defer restore()

	// No confirm:true.
	body := strings.NewReader(`{"setting":"off","operator":"jake","reason":"tuning"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/category/tier1_act", body)
	rr := httptest.NewRecorder()
	handleNotificationsCategory(db)(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("code=%d, want 400 (Tier-1 without confirm)", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "confirm") {
		t.Errorf("body lacks confirm message: %s", rr.Body.String())
	}
	if got := store.GetConfig(db, "notification_category_tier1_act", ""); got != "" {
		t.Errorf("override leaked despite Tier-1 reject: %q", got)
	}
}

func TestHandleNotificationsCategory_Tier1_WithConfirm(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer notifInstallConfig(t)()
	_, restore := notifInstallSlackStub(t)
	defer restore()

	body := strings.NewReader(`{"setting":"off","operator":"jake","reason":"tuning","confirm":true}`)
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/category/tier1_act", body)
	rr := httptest.NewRecorder()
	handleNotificationsCategory(db)(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
	if got := store.GetConfig(db, "notification_category_tier1_act", ""); got != "off" {
		t.Errorf("override=%q, want off", got)
	}
}

func TestHandleNotificationsCategory_InvalidSetting(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer notifInstallConfig(t)()
	_, restore := notifInstallSlackStub(t)
	defer restore()

	body := strings.NewReader(`{"setting":"loud","operator":"jake"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/category/tier2_info", body)
	rr := httptest.NewRecorder()
	handleNotificationsCategory(db)(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("code=%d, want 400", rr.Code)
	}
}

func TestHandleNotificationsCategory_UnknownCategory(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer notifInstallConfig(t)()
	_, restore := notifInstallSlackStub(t)
	defer restore()

	body := strings.NewReader(`{"setting":"off","operator":"jake"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/category/nonsuch", body)
	rr := httptest.NewRecorder()
	handleNotificationsCategory(db)(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("code=%d, want 400", rr.Code)
	}
}

func TestHandleNotificationsCategory_ClearRestoresDefault(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer notifInstallConfig(t)()
	_, restore := notifInstallSlackStub(t)
	defer restore()

	store.SetConfig(db, "notification_category_tier2_info", "off")

	body := strings.NewReader(`{"operator":"jake"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/category/tier2_info/clear", body)
	rr := httptest.NewRecorder()
	handleNotificationsCategory(db)(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
	if got := store.GetConfig(db, "notification_category_tier2_info", ""); got != "" {
		t.Errorf("override=%q, want empty (cleared)", got)
	}
}

func TestHandleNotificationsCategory_RejectsNonPOST(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer notifInstallConfig(t)()
	_, restore := notifInstallSlackStub(t)
	defer restore()

	req := httptest.NewRequest(http.MethodGet, "/api/notifications/category/tier2_info", nil)
	rr := httptest.NewRecorder()
	handleNotificationsCategory(db)(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("code=%d, want 405", rr.Code)
	}
}

// ── Save-as-preset ──────────────────────────────────────────────────────────

func TestHandleNotificationsPresetSave_HappyPath(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer notifInstallConfig(t)()
	_, restore := notifInstallSlackStub(t)
	defer restore()

	body := strings.NewReader(`{
		"name": "weekend_quiet",
		"description": "Suppress most categories on weekends",
		"rules": {"tier2_info":"off","tier1_act":"mail+slack"},
		"operator": "jake"
	}`)
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/preset/save", body)
	rr := httptest.NewRecorder()
	handleNotificationsPresetSave(db)(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		OK      bool   `json:"ok"`
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.OK || resp.Path == "" {
		t.Fatalf("response missing fields: %+v", resp)
	}
	// File on disk matches.
	disk, err := os.ReadFile(resp.Path)
	if err != nil {
		t.Fatalf("read written file %s: %v", resp.Path, err)
	}
	if string(disk) != resp.Content {
		t.Errorf("on-disk content drifted from response.content")
	}

	// Compose a full YAML doc by wrapping the preset block under
	// `presets:`, plus a minimal categories section. ParseConfig must
	// accept the result.
	full := "version: 1\ncategories:\n" +
		"  tier1_act:\n    tier: 1\n    default: mail+slack\n    description: t1\n" +
		"  tier2_info:\n    tier: 2\n    default: mail\n    description: t2\n" +
		"presets:\n" +
		"  default:\n    description: defaults\n    rules: tier_defaults\n" +
		string(disk)
	if _, err := notify.ParseConfig([]byte(full), "save-test.yaml"); err != nil {
		t.Errorf("written preset block does not parse via notify.ParseConfig: %v\nFULL:\n%s", err, full)
	}
}

func TestHandleNotificationsPresetSave_ValidatesRules(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer notifInstallConfig(t)()
	_, restore := notifInstallSlackStub(t)
	defer restore()

	body := strings.NewReader(`{"name":"x","description":"y","rules":{"tier2_info":"loud"}}`)
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/preset/save", body)
	rr := httptest.NewRecorder()
	handleNotificationsPresetSave(db)(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("code=%d, want 400", rr.Code)
	}
}

func TestHandleNotificationsPresetSave_RejectsUnknownCategory(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer notifInstallConfig(t)()
	_, restore := notifInstallSlackStub(t)
	defer restore()

	body := strings.NewReader(`{"name":"x","description":"y","rules":{"nonsuch":"off"}}`)
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/preset/save", body)
	rr := httptest.NewRecorder()
	handleNotificationsPresetSave(db)(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("code=%d, want 400", rr.Code)
	}
}

func TestHandleNotificationsPresetSave_RequiresFields(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer notifInstallConfig(t)()
	_, restore := notifInstallSlackStub(t)
	defer restore()

	// Missing rules.
	body := strings.NewReader(`{"name":"x","description":"y","rules":{}}`)
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/preset/save", body)
	rr := httptest.NewRecorder()
	handleNotificationsPresetSave(db)(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("code=%d, want 400 (empty rules)", rr.Code)
	}
}

func TestHandleNotificationsPresetSave_RejectsNonPOST(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer notifInstallConfig(t)()
	_, restore := notifInstallSlackStub(t)
	defer restore()

	req := httptest.NewRequest(http.MethodGet, "/api/notifications/preset/save", nil)
	rr := httptest.NewRecorder()
	handleNotificationsPresetSave(db)(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("code=%d, want 405", rr.Code)
	}
}

// ── No-Slack-fired guard ────────────────────────────────────────────────────

// TestHandleNotifications_NoSlackFiredFromConfig confirms that none of the
// state-mutating endpoints accidentally fire a real notification dispatch.
// The handlers configure SystemConfig only; the dispatcher fires when an
// agent calls notify.Dispatch elsewhere in the fleet.
func TestHandleNotifications_NoSlackFiredFromConfig(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer notifInstallConfig(t)()
	slack, restore := notifInstallSlackStub(t)
	defer restore()

	// preset, dnd, dnd/clear, category, category/clear, preset/save —
	// run them all back to back.
	until := time.Now().UTC().Add(1 * time.Hour).Format(time.RFC3339)
	calls := []struct {
		path    string
		body    string
		handler http.HandlerFunc
	}{
		{"/api/notifications/preset", `{"preset_name":"focus","operator":"jake"}`, handleNotificationsPreset(db)},
		{"/api/notifications/dnd", fmt.Sprintf(`{"until":%q,"reason":"r","operator":"jake"}`, until), handleNotificationsDND(db)},
		{"/api/notifications/dnd/clear", `{"operator":"jake"}`, handleNotificationsDNDClear(db)},
		{"/api/notifications/category/tier2_info", `{"setting":"slack","operator":"jake"}`, handleNotificationsCategory(db)},
		{"/api/notifications/category/tier2_info/clear", `{"operator":"jake"}`, handleNotificationsCategory(db)},
		{"/api/notifications/preset/save", `{"name":"a","description":"b","rules":{"tier2_info":"off"}}`, handleNotificationsPresetSave(db)},
	}
	for _, c := range calls {
		req := httptest.NewRequest(http.MethodPost, c.path, strings.NewReader(c.body))
		rr := httptest.NewRecorder()
		c.handler(rr, req)
		if rr.Code >= 400 {
			t.Errorf("%s returned %d: %s", c.path, rr.Code, rr.Body.String())
		}
	}
	if len(*slack) != 0 {
		t.Errorf("config endpoints fired Slack: %v (these handlers must not dispatch)", *slack)
	}
}
