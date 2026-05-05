// internal/notify/dispatcher.go — D11 Phase 1 substrate.
//
// Central dispatcher routes a notification through the resolution chain:
//
//  1. Per-convoy override   (ConvoyNotificationOverrides row, mode + custom_json)
//  2. DND active            (notification_dnd_until > now AND category is NOT
//                            in dndBypassCategories: spend_cap_e_stop and
//                            consumer_breakage always dispatch)
//  3. Active preset         (notification_active_preset SystemConfig key)
//  4. Per-category override (notification_category_<name> SystemConfig key)
//  5. YAML default          (NotificationCategoryRegistry.yaml_default)
//
// The resolved setting is one of: "off" | "mail" | "slack" | "mail+slack".
// Dispatch then writes to Fleet_Mail (always for "mail" + "mail+slack")
// and/or fires notify-after Slack (for "slack" + "mail+slack").
//
// Pattern P-NotificationDispatch enforces that no production code outside
// this package calls the existing notifyAfterFn / realNotifyAfter / Slack
// shell-out paths directly. New deliverables MUST dispatch via
// notify.Dispatch or fail the audit.
package notify

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"force-orchestrator/internal/store"
)

// SystemConfig keys owned by the dispatcher. The full namespace:
//
//	notification_active_preset           — name of the preset SystemConfig
//	                                       resolves through (default: "default")
//	notification_dnd_until               — ISO8601 UTC timestamp; empty = no DND
//	notification_dnd_reason              — operator-facing reason (audit only)
//	notification_dnd_set_by              — operator name (audit only)
//	notification_category_<name>         — per-category override; absent = use
//	                                       preset/yaml-default. Value is one of
//	                                       off|mail|slack|mail+slack.
const (
	ConfigKeyActivePreset   = "notification_active_preset"
	ConfigKeyDNDUntil       = "notification_dnd_until"
	ConfigKeyDNDReason      = "notification_dnd_reason"
	ConfigKeyDNDSetBy       = "notification_dnd_set_by"
	ConfigKeyCategoryPrefix = "notification_category_"
)

// MaxDNDDays caps how far into the future an operator can set DND.
// Server-side validators (e.g. the dashboard endpoint) reject values
// past now+MaxDNDDays. The cap exists so a fat-fingered "DND for 14
// MONTHS" doesn't silently suppress the fleet for a year.
const MaxDNDDays = 14

// dndBypassCategories names categories that dispatch even when DND is
// active. The carve-out is "but for real emergencies" — a Tier-1
// category that signifies the fleet is bleeding money or breaking
// downstream consumers must not be silenced by DND. The set is
// declared in code (not YAML) so it can't be silently disabled by a
// config edit; operators who want to silence one of these must turn
// off the category explicitly via per-category override.
var dndBypassCategories = map[string]struct{}{
	"spend_cap_e_stop":  {},
	"consumer_breakage": {},
}

// IsDNDBypass reports whether the given category bypasses DND.
// Exported so tests + the dashboard can render "this category will
// fire even during DND" labels.
func IsDNDBypass(category string) bool {
	_, ok := dndBypassCategories[category]
	return ok
}

// configHolder owns the in-process YAML config. The daemon-startup
// seeder calls SetConfig once; the dispatcher reads it on every call.
// A mutex guards the swap so tests can install a fresh config without
// racing the dispatcher.
type configHolder struct {
	mu  sync.RWMutex
	cfg *Config
}

var globalConfig configHolder

// SetGlobalConfig stores the parsed config in the package-level
// holder. Called once from the daemon-startup seeder; tests call it
// per-test to install a synthetic Config.
func SetGlobalConfig(cfg *Config) {
	globalConfig.mu.Lock()
	defer globalConfig.mu.Unlock()
	globalConfig.cfg = cfg
}

// GetGlobalConfig returns the currently installed Config, or nil if
// SetGlobalConfig has never been called. The dispatcher tolerates a
// nil config by returning ErrNoConfig — the daemon should never reach
// that path in production (the seeder runs at startup), but tests
// that don't bother seeding still get a clear error.
func GetGlobalConfig() *Config {
	globalConfig.mu.RLock()
	defer globalConfig.mu.RUnlock()
	return globalConfig.cfg
}

// ErrNoConfig is returned by Dispatch when no Config has been installed
// via SetGlobalConfig. The daemon-startup seeder is responsible for
// installing a config before any agent dispatches.
var ErrNoConfig = fmt.Errorf("notify: no Config installed (call SetGlobalConfig at daemon startup)")

// ErrUnknownCategory is returned by Dispatch when the category isn't
// registered. Unknown categories never silently drop; the dispatcher
// fails the call so the offending caller surfaces in fleet-mail or
// the dog's error path (per CLAUDE.md "no silent failures").
type ErrUnknownCategory struct {
	Category string
}

func (e ErrUnknownCategory) Error() string {
	return fmt.Sprintf("notify: unknown category %q (not in YAML registry)", e.Category)
}

// ConvoyOverride is the parsed shape of a ConvoyNotificationOverrides
// row. The dispatcher resolves it during the per-convoy step.
type ConvoyOverride struct {
	ConvoyID   int
	Mode       string            // 'verbose' | 'quiet' | 'custom_json'
	CustomJSON map[string]string // category → setting; only populated when Mode == 'custom_json'
}

// Dispatch routes a notification through the resolution chain and
// writes to Fleet_Mail and/or fires the Slack shell-out as resolved.
// Returns nil on success even when the resolved setting is "off"; only
// errors on real failures (mail-write returning 0, malformed config,
// unknown category, etc.).
//
// `convoyID` may be 0 for fleet-wide notifications; the per-convoy
// resolution layer is skipped when convoyID <= 0.
//
// `label` is the short Slack-style header. `body` is the longer mail
// body. The dispatcher constructs the structured subject as:
//
//	[D11/<category>] <label>
//
// so operator inbox filters can trigger on the prefix.
func Dispatch(ctx context.Context, db *sql.DB, category string, convoyID int, label, body string) error {
	cfg := GetGlobalConfig()
	if cfg == nil {
		return ErrNoConfig
	}
	spec, ok := cfg.Categories[category]
	if !ok {
		return ErrUnknownCategory{Category: category}
	}

	resolved, layer, dndActive, err := resolveSetting(db, cfg, spec, convoyID)
	if err != nil {
		return err
	}

	log.Printf("notify: category=%s convoyID=%d resolved_setting=%s layer=%s dnd_active=%v",
		category, convoyID, resolved, layer, dndActive)

	// "off" is a successful no-op — the resolution chain decided this
	// notification should not fire.
	if resolved == SettingOff {
		return nil
	}

	subject := fmt.Sprintf("[D11/%s] %s", category, label)

	// Mail side effect.
	if resolved == SettingMail || resolved == SettingMailSlack {
		// SendMail returns the inserted row id; 0 means the INSERT
		// failed. Surface as an error so the caller can decide whether
		// the failure is fatal to its own task.
		if id := store.SendMail(db, "notify", "operator", subject, body, 0, store.MailTypeAlert); id == 0 {
			return fmt.Errorf("notify: SendMail failed for category=%s (mail row not inserted)", category)
		}
	}

	// Slack side effect. Best-effort: a Slack failure is logged but
	// doesn't poison the dispatch result — the mail row already
	// landed (or wasn't requested), and a transient webhook hiccup
	// shouldn't surface as a hard error to the caller.
	if resolved == SettingSlack || resolved == SettingMailSlack {
		if err := SlackNotify(ctx, label); err != nil {
			log.Printf("notify: SlackNotify failed for category=%s (continuing): %v", category, err)
		}
	}

	return nil
}

// resolveSetting walks the resolution chain and returns the resolved
// setting, the chain layer that won, and whether DND is active. Pure
// function modulo the SystemConfig + ConvoyNotificationOverrides reads;
// no side effects on any layer.
//
// Layers, top-of-chain first:
//
//	per_convoy_override
//	dnd
//	preset
//	per_category_override
//	yaml_default
//
// The "winning" layer is the first one that returns a non-empty
// setting. When DND is active and the category is on the DND-bypass
// list, layer is "dnd_bypass" and resolution continues through the
// remaining layers (preset → per_category_override → yaml_default).
func resolveSetting(db *sql.DB, cfg *Config, spec CategorySpec, convoyID int) (Setting, string, bool, error) {
	// 1. Per-convoy override. Skipped when convoyID <= 0.
	if convoyID > 0 {
		ov, err := loadConvoyOverride(db, convoyID)
		if err != nil {
			return "", "", false, err
		}
		if ov != nil {
			s, ok := convoyOverrideResolve(ov, spec, cfg)
			if ok {
				return s, "per_convoy_override", false, nil
			}
		}
	}

	// 2. DND check. If active and not bypassed, return SettingOff.
	dndActive := isDNDActive(db)
	if dndActive && !IsDNDBypass(spec.Name) {
		return SettingOff, "dnd", true, nil
	}

	// 3. Active preset.
	presetName := store.GetConfig(db, ConfigKeyActivePreset, "default")
	if presetName == "" {
		presetName = "default"
	}
	if preset, ok := cfg.Presets[presetName]; ok {
		if s, ok2 := preset.Resolve(spec.Name, cfg.Categories); ok2 {
			// Only consume the preset layer if it's NOT the "default"
			// preset's tier_defaults — that case is logically the
			// same as "fall through to yaml_default", and reporting
			// it as preset/yaml_default makes operator UX clearer.
			//
			// However — for non-default presets, "tier_defaults"-style
			// resolutions are still preset-layer wins, so we only
			// special-case this when preset.Name == "default".
			if preset.Name == "default" && preset.RulesToken == "tier_defaults" {
				// Fall through to per_category_override + yaml_default.
			} else {
				return s, "preset", dndActive, nil
			}
		}
	}

	// 4. Per-category override.
	overrideKey := ConfigKeyCategoryPrefix + spec.Name
	if v := store.GetConfig(db, overrideKey, ""); v != "" {
		s := Setting(v)
		if !s.IsValid() {
			return "", "", false, fmt.Errorf("notify: SystemConfig key %s has invalid value %q", overrideKey, v)
		}
		return s, "per_category_override", dndActive, nil
	}

	// 5. YAML default.
	return spec.Default, "yaml_default", dndActive, nil
}

// loadConvoyOverride reads ConvoyNotificationOverrides for the given
// convoy. Returns (nil, nil) if no row exists (the dispatcher treats
// that as "no override; fall through"). Errors only on a real DB error.
func loadConvoyOverride(db *sql.DB, convoyID int) (*ConvoyOverride, error) {
	var (
		mode    string
		jsonRaw string
	)
	err := db.QueryRow(
		`SELECT mode, IFNULL(custom_json, '{}') FROM ConvoyNotificationOverrides WHERE convoy_id = ?`,
		convoyID,
	).Scan(&mode, &jsonRaw)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("notify: load ConvoyNotificationOverrides convoy=%d: %w", convoyID, err)
	}
	out := &ConvoyOverride{ConvoyID: convoyID, Mode: mode}
	if mode == "custom_json" && jsonRaw != "" {
		var m map[string]string
		if err := json.Unmarshal([]byte(jsonRaw), &m); err != nil {
			return nil, fmt.Errorf("notify: ConvoyNotificationOverrides convoy=%d custom_json malformed: %w", convoyID, err)
		}
		out.CustomJSON = m
	}
	return out, nil
}

// convoyOverrideResolve maps a ConvoyOverride row's mode to a Setting
// for the given category. Returns (s, true) if the override speaks for
// the category; (_, false) if not (dispatcher continues to next layer).
//
// Mode semantics:
//
//	verbose      — every category fires mail+slack
//	quiet        — every category is off (DND-bypass still bypasses at
//	               the dispatcher's DND step, not here — verbose/quiet
//	               are convoy-scoped, not DND-scoped, so a "quiet" convoy
//	               STILL silences a Tier-1 category. That's intentional:
//	               operators who set quiet on a convoy mean it.)
//	custom_json  — per-category map; missing keys fall through to next
//	               layer (we don't claim authority for unspecified ones)
func convoyOverrideResolve(ov *ConvoyOverride, spec CategorySpec, cfg *Config) (Setting, bool) {
	switch ov.Mode {
	case "verbose":
		return SettingMailSlack, true
	case "quiet":
		return SettingOff, true
	case "custom_json":
		if v, ok := ov.CustomJSON[spec.Name]; ok {
			s := Setting(v)
			if !s.IsValid() {
				// Malformed value — log + treat as "no override
				// for this category" so a single bad entry doesn't
				// poison every category for the convoy.
				log.Printf("notify: ConvoyNotificationOverrides convoy=%d category=%s invalid setting %q (skipping override)",
					ov.ConvoyID, spec.Name, v)
				return "", false
			}
			return s, true
		}
		// Wildcard support inside custom_json mirrors the YAML preset
		// shape — operators can write {"*": "off"} to mute everything
		// for the convoy and then opt categories back in.
		if v, ok := ov.CustomJSON["*"]; ok {
			s := Setting(v)
			if s.IsValid() {
				return s, true
			}
		}
		return "", false
	}
	return "", false
}

// isDNDActive returns true when the operator has set
// notification_dnd_until to a future timestamp. Empty / unset / past
// values all mean DND inactive.
func isDNDActive(db *sql.DB) bool {
	until := store.GetConfig(db, ConfigKeyDNDUntil, "")
	if strings.TrimSpace(until) == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, until)
	if err != nil {
		log.Printf("notify: %s has malformed value %q (treating as inactive)", ConfigKeyDNDUntil, until)
		return false
	}
	return time.Now().UTC().Before(t)
}

// SetDND validates and stores the operator's DND request. Returns an
// error if `until` is past `now+MaxDNDDays` (the server-side cap that
// prevents fat-fingered year-long muting).
//
// The setter does NOT check whether DND is currently active before
// overwriting — operators may freely extend or shorten an active
// window. AuditLog rows for DND changes live elsewhere (D11 Phase 2
// will wire the dashboard endpoint that calls this).
func SetDND(db *sql.DB, until time.Time, reason, setBy string) error {
	now := time.Now().UTC()
	maxAllowed := now.Add(MaxDNDDays * 24 * time.Hour)
	if until.After(maxAllowed) {
		return fmt.Errorf("notify: DND until %s exceeds the %d-day cap (max %s)",
			until.UTC().Format(time.RFC3339), MaxDNDDays, maxAllowed.Format(time.RFC3339))
	}
	if until.Before(now) {
		return fmt.Errorf("notify: DND until %s is in the past (current time %s)",
			until.UTC().Format(time.RFC3339), now.Format(time.RFC3339))
	}
	store.SetConfig(db, ConfigKeyDNDUntil, until.UTC().Format(time.RFC3339))
	store.SetConfig(db, ConfigKeyDNDReason, reason)
	store.SetConfig(db, ConfigKeyDNDSetBy, setBy)
	return nil
}

// ClearDND removes any active DND. Idempotent.
func ClearDND(db *sql.DB) {
	store.SetConfig(db, ConfigKeyDNDUntil, "")
	store.SetConfig(db, ConfigKeyDNDReason, "")
	store.SetConfig(db, ConfigKeyDNDSetBy, "")
}
