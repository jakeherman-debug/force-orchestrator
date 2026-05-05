package notify

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"testing"
	"time"

	"force-orchestrator/internal/store"
)

// makeTestConfig returns a tiny but full-shape Config covering all three
// tiers + the three presets the resolution chain depends on.
func makeTestConfig(t *testing.T) *Config {
	t.Helper()
	cfg, err := ParseConfig([]byte(`
version: 1
categories:
  tier1_act:
    tier: 1
    default: mail+slack
    description: tier1
  tier2_info:
    tier: 2
    default: mail
    description: tier2
  tier3_trace:
    tier: 3
    default: off
    description: tier3
  spend_cap_e_stop:
    tier: 1
    default: mail+slack
    description: dnd-bypass
  consumer_breakage:
    tier: 1
    default: mail+slack
    description: dnd-bypass
presets:
  default:
    description: defaults
    rules: tier_defaults
  focus:
    description: quiet-mode
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
	return cfg
}

// freshDB wires a fresh in-memory holocron + installs the test config
// + installs a counting Slack stub. Returns the DB, a pointer to the
// captured slack labels, and a cleanup closure.
func freshDB(t *testing.T) (*sql.DB, *[]string, func()) {
	t.Helper()
	db := store.InitHolocronDSN(":memory:")
	cfg := makeTestConfig(t)
	SetGlobalConfig(cfg)

	var (
		mu     sync.Mutex
		calls  []string
	)
	restore := SetSlackNotifierForTest(func(_ context.Context, label string) error {
		mu.Lock()
		calls = append(calls, label)
		mu.Unlock()
		return nil
	})

	cleanup := func() {
		restore()
		SetGlobalConfig(nil)
		db.Close()
	}
	return db, &calls, cleanup
}

func mailRowCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM Fleet_Mail`).Scan(&n); err != nil {
		t.Fatalf("count Fleet_Mail: %v", err)
	}
	return n
}

// TestNotifyDispatcher_NoConfig confirms Dispatch errors clearly when
// SetGlobalConfig has never been called.
func TestNotifyDispatcher_NoConfig(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	SetGlobalConfig(nil)
	err := Dispatch(context.Background(), db, "tier1_act", 0, "x", "y")
	if err == nil || !strings.Contains(err.Error(), "no Config") {
		t.Errorf("want no-config error, got %v", err)
	}
}

// TestNotifyDispatcher_UnknownCategory confirms unknown categories error
// rather than silently dropping.
func TestNotifyDispatcher_UnknownCategory(t *testing.T) {
	db, _, cleanup := freshDB(t)
	defer cleanup()
	err := Dispatch(context.Background(), db, "nonsuch_category", 0, "x", "y")
	if err == nil {
		t.Fatal("want unknown-category error")
	}
	var ec ErrUnknownCategory
	if !errorAs(err, &ec) {
		t.Errorf("want ErrUnknownCategory, got %T %v", err, err)
	}
}

// TestNotifyDispatcher_YAMLDefault_Tier1 — no overrides anywhere; falls
// through to YAML default (mail+slack).
func TestNotifyDispatcher_YAMLDefault_Tier1(t *testing.T) {
	db, slack, cleanup := freshDB(t)
	defer cleanup()
	if err := Dispatch(context.Background(), db, "tier1_act", 0, "lab", "body"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if mailRowCount(t, db) != 1 {
		t.Errorf("mail rows=%d, want 1", mailRowCount(t, db))
	}
	if len(*slack) != 1 || (*slack)[0] != "lab" {
		t.Errorf("slack=%v, want [lab]", *slack)
	}
}

// TestNotifyDispatcher_YAMLDefault_Tier2 — Tier-2 default is mail only.
func TestNotifyDispatcher_YAMLDefault_Tier2(t *testing.T) {
	db, slack, cleanup := freshDB(t)
	defer cleanup()
	if err := Dispatch(context.Background(), db, "tier2_info", 0, "l", "b"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if mailRowCount(t, db) != 1 {
		t.Errorf("mail rows=%d, want 1", mailRowCount(t, db))
	}
	if len(*slack) != 0 {
		t.Errorf("slack=%v, want []", *slack)
	}
}

// TestNotifyDispatcher_YAMLDefault_Tier3 — Tier-3 default is off.
func TestNotifyDispatcher_YAMLDefault_Tier3(t *testing.T) {
	db, slack, cleanup := freshDB(t)
	defer cleanup()
	if err := Dispatch(context.Background(), db, "tier3_trace", 0, "l", "b"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if mailRowCount(t, db) != 0 {
		t.Errorf("mail rows=%d, want 0", mailRowCount(t, db))
	}
	if len(*slack) != 0 {
		t.Errorf("slack=%v, want []", *slack)
	}
}

// TestNotifyDispatcher_PerCategoryOverride — set notification_category_<name>
// = "off" and confirm the layer wins over YAML default.
func TestNotifyDispatcher_PerCategoryOverride(t *testing.T) {
	db, slack, cleanup := freshDB(t)
	defer cleanup()
	store.SetConfig(db, "notification_category_tier1_act", "off")
	if err := Dispatch(context.Background(), db, "tier1_act", 0, "l", "b"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if mailRowCount(t, db) != 0 {
		t.Errorf("mail rows=%d, want 0", mailRowCount(t, db))
	}
	if len(*slack) != 0 {
		t.Errorf("slack=%v, want []", *slack)
	}
}

// TestNotifyDispatcher_PerCategoryOverride_BadValue — invalid setting
// surfaces an error.
func TestNotifyDispatcher_PerCategoryOverride_BadValue(t *testing.T) {
	db, _, cleanup := freshDB(t)
	defer cleanup()
	store.SetConfig(db, "notification_category_tier1_act", "wibble")
	err := Dispatch(context.Background(), db, "tier1_act", 0, "l", "b")
	if err == nil || !strings.Contains(err.Error(), "invalid value") {
		t.Errorf("want invalid-value error, got %v", err)
	}
}

// TestNotifyDispatcher_ActivePreset_Focus — focus preset routes Tier-2/Tier-3
// to off and Tier-1 carve-outs to mail+slack.
func TestNotifyDispatcher_ActivePreset_Focus(t *testing.T) {
	db, slack, cleanup := freshDB(t)
	defer cleanup()
	store.SetConfig(db, ConfigKeyActivePreset, "focus")
	// tier2_info — wildcard "*": off
	if err := Dispatch(context.Background(), db, "tier2_info", 0, "l1", "b"); err != nil {
		t.Fatalf("Dispatch tier2: %v", err)
	}
	if mailRowCount(t, db) != 0 {
		t.Errorf("mail rows tier2=%d, want 0", mailRowCount(t, db))
	}
	// tier1_act — explicit mail+slack
	if err := Dispatch(context.Background(), db, "tier1_act", 0, "l2", "b"); err != nil {
		t.Fatalf("Dispatch tier1: %v", err)
	}
	if mailRowCount(t, db) != 1 {
		t.Errorf("mail rows tier1=%d, want 1", mailRowCount(t, db))
	}
	if len(*slack) != 1 || (*slack)[0] != "l2" {
		t.Errorf("slack=%v, want [l2]", *slack)
	}
}

// TestNotifyDispatcher_ActivePreset_Verbose — verbose routes everything
// (including Tier-3) to mail+slack.
func TestNotifyDispatcher_ActivePreset_Verbose(t *testing.T) {
	db, slack, cleanup := freshDB(t)
	defer cleanup()
	store.SetConfig(db, ConfigKeyActivePreset, "verbose")
	if err := Dispatch(context.Background(), db, "tier3_trace", 0, "l", "b"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if mailRowCount(t, db) != 1 {
		t.Errorf("mail rows=%d, want 1", mailRowCount(t, db))
	}
	if len(*slack) != 1 {
		t.Errorf("slack=%v, want 1 ping", *slack)
	}
}

// TestNotifyDispatcher_DefaultPreset_FallsThrough — when active_preset
// is "default" (tier_defaults), per_category_override should still get
// a chance.
func TestNotifyDispatcher_DefaultPreset_FallsThrough(t *testing.T) {
	db, slack, cleanup := freshDB(t)
	defer cleanup()
	store.SetConfig(db, ConfigKeyActivePreset, "default")
	store.SetConfig(db, "notification_category_tier1_act", "mail")
	if err := Dispatch(context.Background(), db, "tier1_act", 0, "l", "b"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if mailRowCount(t, db) != 1 {
		t.Errorf("mail rows=%d, want 1", mailRowCount(t, db))
	}
	if len(*slack) != 0 {
		t.Errorf("slack=%v, want []", *slack)
	}
}

// TestNotifyDispatcher_DND_SuppressesNonBypass — DND active suppresses
// regular Tier-1 categories but bypass categories still fire.
func TestNotifyDispatcher_DND_SuppressesNonBypass(t *testing.T) {
	db, slack, cleanup := freshDB(t)
	defer cleanup()
	until := time.Now().UTC().Add(1 * time.Hour).Format(time.RFC3339)
	store.SetConfig(db, ConfigKeyDNDUntil, until)

	// tier1_act — should be suppressed.
	if err := Dispatch(context.Background(), db, "tier1_act", 0, "l1", "b"); err != nil {
		t.Fatalf("Dispatch tier1: %v", err)
	}
	if mailRowCount(t, db) != 0 {
		t.Errorf("mail rows after DND tier1=%d, want 0", mailRowCount(t, db))
	}
	if len(*slack) != 0 {
		t.Errorf("slack after DND tier1=%v, want []", *slack)
	}

	// spend_cap_e_stop — DND-bypass; should fire.
	if err := Dispatch(context.Background(), db, "spend_cap_e_stop", 0, "estop", "b"); err != nil {
		t.Fatalf("Dispatch spend_cap_e_stop: %v", err)
	}
	if mailRowCount(t, db) != 1 {
		t.Errorf("mail rows after DND bypass=%d, want 1", mailRowCount(t, db))
	}
	if len(*slack) != 1 {
		t.Errorf("slack after DND bypass=%v, want 1 ping", *slack)
	}
}

// TestNotifyDispatcher_DND_BypassConsumerBreakage — second bypass.
func TestNotifyDispatcher_DND_BypassConsumerBreakage(t *testing.T) {
	db, slack, cleanup := freshDB(t)
	defer cleanup()
	until := time.Now().UTC().Add(1 * time.Hour).Format(time.RFC3339)
	store.SetConfig(db, ConfigKeyDNDUntil, until)
	if err := Dispatch(context.Background(), db, "consumer_breakage", 0, "brk", "b"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if mailRowCount(t, db) != 1 {
		t.Errorf("mail rows=%d, want 1", mailRowCount(t, db))
	}
	if len(*slack) != 1 {
		t.Errorf("slack=%v, want 1", *slack)
	}
}

// TestNotifyDispatcher_DND_PastTimestamp — DND with a past until value
// is treated as inactive.
func TestNotifyDispatcher_DND_PastTimestamp(t *testing.T) {
	db, _, cleanup := freshDB(t)
	defer cleanup()
	store.SetConfig(db, ConfigKeyDNDUntil, time.Now().UTC().Add(-1*time.Hour).Format(time.RFC3339))
	if err := Dispatch(context.Background(), db, "tier1_act", 0, "l", "b"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if mailRowCount(t, db) != 1 {
		t.Errorf("mail rows=%d, want 1 (DND past = inactive)", mailRowCount(t, db))
	}
}

// TestNotifyDispatcher_DND_Malformed — malformed DND value treated as
// inactive (logged but doesn't break the call).
func TestNotifyDispatcher_DND_Malformed(t *testing.T) {
	db, _, cleanup := freshDB(t)
	defer cleanup()
	store.SetConfig(db, ConfigKeyDNDUntil, "not-an-iso-timestamp")
	if err := Dispatch(context.Background(), db, "tier1_act", 0, "l", "b"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if mailRowCount(t, db) != 1 {
		t.Errorf("mail rows=%d, want 1", mailRowCount(t, db))
	}
}

// TestNotifyDispatcher_PerConvoyOverride_Quiet — convoy-specific quiet
// suppresses everything (including Tier-1).
func TestNotifyDispatcher_PerConvoyOverride_Quiet(t *testing.T) {
	db, slack, cleanup := freshDB(t)
	defer cleanup()
	insertConvoyOverride(t, db, 42, "quiet", `{}`)
	if err := Dispatch(context.Background(), db, "tier1_act", 42, "l", "b"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if mailRowCount(t, db) != 0 {
		t.Errorf("mail rows=%d, want 0", mailRowCount(t, db))
	}
	if len(*slack) != 0 {
		t.Errorf("slack=%v, want []", *slack)
	}
}

// TestNotifyDispatcher_PerConvoyOverride_Verbose — convoy-specific verbose
// fires Tier-3 to mail+slack.
func TestNotifyDispatcher_PerConvoyOverride_Verbose(t *testing.T) {
	db, slack, cleanup := freshDB(t)
	defer cleanup()
	insertConvoyOverride(t, db, 42, "verbose", `{}`)
	if err := Dispatch(context.Background(), db, "tier3_trace", 42, "l", "b"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if mailRowCount(t, db) != 1 {
		t.Errorf("mail rows=%d, want 1", mailRowCount(t, db))
	}
	if len(*slack) != 1 {
		t.Errorf("slack=%v, want 1", *slack)
	}
}

// TestNotifyDispatcher_PerConvoyOverride_CustomJSON — per-category map
// resolves; missing keys fall through.
func TestNotifyDispatcher_PerConvoyOverride_CustomJSON(t *testing.T) {
	db, slack, cleanup := freshDB(t)
	defer cleanup()
	insertConvoyOverride(t, db, 42, "custom_json", `{"tier1_act":"slack"}`)

	// tier1_act — explicit slack.
	if err := Dispatch(context.Background(), db, "tier1_act", 42, "l1", "b"); err != nil {
		t.Fatalf("Dispatch tier1: %v", err)
	}
	if mailRowCount(t, db) != 0 {
		t.Errorf("mail rows tier1=%d, want 0", mailRowCount(t, db))
	}
	if len(*slack) != 1 {
		t.Errorf("slack tier1=%v, want 1", *slack)
	}

	// tier2_info — not in the JSON; falls through to YAML default (mail).
	if err := Dispatch(context.Background(), db, "tier2_info", 42, "l2", "b"); err != nil {
		t.Fatalf("Dispatch tier2: %v", err)
	}
	if mailRowCount(t, db) != 1 {
		t.Errorf("mail rows tier2=%d, want 1 (fell through to YAML default)", mailRowCount(t, db))
	}
}

// TestNotifyDispatcher_PerConvoyOverride_Wildcard — "*" inside custom_json.
func TestNotifyDispatcher_PerConvoyOverride_Wildcard(t *testing.T) {
	db, _, cleanup := freshDB(t)
	defer cleanup()
	insertConvoyOverride(t, db, 42, "custom_json", `{"*":"off","tier1_act":"mail"}`)

	// tier2_info — wildcard suppresses.
	if err := Dispatch(context.Background(), db, "tier2_info", 42, "l", "b"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if mailRowCount(t, db) != 0 {
		t.Errorf("mail rows=%d, want 0", mailRowCount(t, db))
	}

	// tier1_act — explicit override wins over wildcard.
	if err := Dispatch(context.Background(), db, "tier1_act", 42, "l", "b"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if mailRowCount(t, db) != 1 {
		t.Errorf("mail rows=%d, want 1", mailRowCount(t, db))
	}
}

// TestNotifyDispatcher_PerConvoyOverride_PrecedesDND — per-convoy beats
// DND. A convoy-specific verbose override fires even if DND is active.
func TestNotifyDispatcher_PerConvoyOverride_PrecedesDND(t *testing.T) {
	db, slack, cleanup := freshDB(t)
	defer cleanup()
	store.SetConfig(db, ConfigKeyDNDUntil, time.Now().UTC().Add(1*time.Hour).Format(time.RFC3339))
	insertConvoyOverride(t, db, 42, "verbose", `{}`)
	if err := Dispatch(context.Background(), db, "tier1_act", 42, "l", "b"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if mailRowCount(t, db) != 1 {
		t.Errorf("mail rows=%d, want 1 (per-convoy override beats DND)", mailRowCount(t, db))
	}
	if len(*slack) != 1 {
		t.Errorf("slack=%v, want 1", *slack)
	}
}

// TestNotifyDispatcher_LayerComposition_PerCategoryBeatsYAML — multi-layer
// resolution: per-category override beats YAML default.
func TestNotifyDispatcher_LayerComposition_PerCategoryBeatsYAML(t *testing.T) {
	db, slack, cleanup := freshDB(t)
	defer cleanup()
	store.SetConfig(db, "notification_category_tier3_trace", "slack")
	if err := Dispatch(context.Background(), db, "tier3_trace", 0, "l", "b"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if mailRowCount(t, db) != 0 {
		t.Errorf("mail rows=%d, want 0 (slack-only)", mailRowCount(t, db))
	}
	if len(*slack) != 1 {
		t.Errorf("slack=%v, want 1", *slack)
	}
}

// TestNotifyDispatcher_LayerComposition_PresetBeatsPerCategory — when
// non-default preset speaks for the category, it wins over per-category
// override.
func TestNotifyDispatcher_LayerComposition_PresetBeatsPerCategory(t *testing.T) {
	db, slack, cleanup := freshDB(t)
	defer cleanup()
	store.SetConfig(db, ConfigKeyActivePreset, "verbose")
	store.SetConfig(db, "notification_category_tier1_act", "off")
	if err := Dispatch(context.Background(), db, "tier1_act", 0, "l", "b"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if mailRowCount(t, db) != 1 {
		t.Errorf("mail rows=%d, want 1 (verbose preset wins)", mailRowCount(t, db))
	}
	if len(*slack) != 1 {
		t.Errorf("slack=%v, want 1", *slack)
	}
}

// TestNotifySetDND_Validates — SetDND rejects > now+14d.
func TestNotifySetDND_Validates(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// > 14 days.
	farFuture := time.Now().UTC().Add(15 * 24 * time.Hour)
	if err := SetDND(db, farFuture, "vacation", "ops"); err == nil {
		t.Errorf("want error for 15-day DND, got nil")
	}

	// in the past.
	past := time.Now().UTC().Add(-1 * time.Hour)
	if err := SetDND(db, past, "vacation", "ops"); err == nil {
		t.Errorf("want error for past DND, got nil")
	}

	// valid: 7 days.
	ok := time.Now().UTC().Add(7 * 24 * time.Hour)
	if err := SetDND(db, ok, "out of office", "ops"); err != nil {
		t.Errorf("valid DND rejected: %v", err)
	}
	if v := store.GetConfig(db, ConfigKeyDNDUntil, ""); v == "" {
		t.Error("DND until not stored")
	}
	ClearDND(db)
	if v := store.GetConfig(db, ConfigKeyDNDUntil, ""); v != "" {
		t.Errorf("ClearDND failed: %q", v)
	}
}

// TestNotifyIsDNDBypass — the bypass set is exactly the two documented
// categories.
func TestNotifyIsDNDBypass(t *testing.T) {
	for _, name := range []string{"spend_cap_e_stop", "consumer_breakage"} {
		if !IsDNDBypass(name) {
			t.Errorf("IsDNDBypass(%s)=false, want true", name)
		}
	}
	for _, name := range []string{"tier1_act", "stage_transition", "supply_token_expired"} {
		if IsDNDBypass(name) {
			t.Errorf("IsDNDBypass(%s)=true, want false", name)
		}
	}
}

// TestNotifySeedRegistryFromYAML — seeds rows; re-seeds upserts; new
// daemon-start with edited YAML updates fields.
func TestNotifySeedRegistryFromYAML(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	cfg := makeTestConfig(t)

	if err := SeedRegistryFromYAML(db, cfg); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rows, err := ListRegisteredCategories(db)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 5 {
		t.Errorf("registry rows=%d, want 5", len(rows))
	}
	// Re-seed (idempotent).
	if err := SeedRegistryFromYAML(db, cfg); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	rows2, _ := ListRegisteredCategories(db)
	if len(rows2) != 5 {
		t.Errorf("after re-seed rows=%d, want 5", len(rows2))
	}

	// Edit YAML default for one category and re-seed → upsert flips it.
	tier1Spec := cfg.Categories["tier1_act"]
	tier1Spec.Default = SettingMail
	cfg.Categories["tier1_act"] = tier1Spec
	if err := SeedRegistryFromYAML(db, cfg); err != nil {
		t.Fatalf("re-seed edited: %v", err)
	}
	rows3, _ := ListRegisteredCategories(db)
	for _, r := range rows3 {
		if r.Category == "tier1_act" && r.YAMLDefault != SettingMail {
			t.Errorf("tier1_act yaml_default=%q, want mail (post-edit)", r.YAMLDefault)
		}
	}
}

// TestNotifySeedRegistryFromYAML_PreservesUnknownCategories — categories
// in DB but absent from YAML survive a re-seed (operators may have
// removed-then-re-added a category mid-rollout).
func TestNotifySeedRegistryFromYAML_PreservesUnknownCategories(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	cfg := makeTestConfig(t)

	// Manually insert a row not in the YAML.
	if _, err := db.Exec(
		`INSERT INTO NotificationCategoryRegistry (category, tier, yaml_default, description, yaml_version)
		 VALUES ('legacy_cat', 1, 'mail', 'legacy', 1)`,
	); err != nil {
		t.Fatalf("insert legacy: %v", err)
	}

	if err := SeedRegistryFromYAML(db, cfg); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rows, _ := ListRegisteredCategories(db)
	var sawLegacy bool
	for _, r := range rows {
		if r.Category == "legacy_cat" {
			sawLegacy = true
		}
	}
	if !sawLegacy {
		t.Error("legacy_cat removed by seeder; should be preserved")
	}
}

// insertConvoyOverride is a test helper that writes a row directly.
func insertConvoyOverride(t *testing.T, db *sql.DB, convoyID int, mode, customJSON string) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO ConvoyNotificationOverrides (convoy_id, mode, custom_json, set_at, set_by, reason)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		convoyID, mode, customJSON, store.NowSQLite(), "test", "test override",
	)
	if err != nil {
		t.Fatalf("insert convoy override: %v", err)
	}
}

// errorAs is a tiny shim around errors.As — saves importing errors here.
func errorAs(err error, target interface{}) bool {
	if err == nil {
		return false
	}
	if e, ok := err.(ErrUnknownCategory); ok {
		if t, ok := target.(*ErrUnknownCategory); ok {
			*t = e
			return true
		}
	}
	return false
}
