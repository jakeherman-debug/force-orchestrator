// Package notify is the central operator-notification dispatcher (D11
// Phase 1 — Substrate).
//
// The package owns:
//
//  1. The YAML schema (config/notifications.yaml) — the canonical
//     declaration of every operator-facing notification category in
//     the fleet, plus named presets.
//  2. The dispatch resolution chain (per-convoy override → DND →
//     active preset → per-category override → YAML default).
//  3. The single Dispatch entrypoint that callers use to fire a
//     notification. Direct calls to notifyAfterFn / realNotifyAfter
//     are forbidden outside this package (Pattern P-NotificationDispatch
//     enforces).
//
// This file (config.go) defines the YAML model and the parser. The
// dispatcher itself lives in dispatcher.go; the seeding logic that
// upserts categories into NotificationCategoryRegistry lives in
// seed.go.
package notify

import (
	"errors"
	"fmt"
	"os"
	"sort"

	"gopkg.in/yaml.v3"
)

// Setting is the resolved notification routing. Exactly four values are
// legal across every layer of the resolution chain (YAML defaults, preset
// rules, per-category overrides, per-convoy overrides):
//
//	"off"        — no mail, no slack
//	"mail"       — Fleet_Mail row only
//	"slack"      — notify-after Slack ping only
//	"mail+slack" — both
//
// Setting values are deliberately strings (not int constants) so they
// round-trip through SystemConfig + YAML + JSON without an intermediate
// enum table. The parser rejects unknown values; the dispatcher's resolve
// step fails closed if any layer returns an empty string.
type Setting string

const (
	SettingOff       Setting = "off"
	SettingMail      Setting = "mail"
	SettingSlack     Setting = "slack"
	SettingMailSlack Setting = "mail+slack"
)

// IsValid reports whether s is one of the four legal Setting values.
func (s Setting) IsValid() bool {
	switch s {
	case SettingOff, SettingMail, SettingSlack, SettingMailSlack:
		return true
	}
	return false
}

// Tier is the category's default-routing class. Tier values flow into
// presets via the special `tier_defaults` token: Tier 1 → mail+slack,
// Tier 2 → mail, Tier 3 → off. Tier is also surfaced on the dashboard
// so operators can bulk-edit by severity class.
type Tier int

const (
	Tier1 Tier = 1 // operator must act
	Tier2 Tier = 2 // informational
	Tier3 Tier = 3 // debug-trace
)

// IsValid reports whether t is one of the three legal tier values.
func (t Tier) IsValid() bool {
	return t == Tier1 || t == Tier2 || t == Tier3
}

// DefaultForTier returns the routing setting that the special preset
// token `tier_defaults` resolves to for a given tier. Used both at
// preset-resolution time and at YAML validation time (we cross-check
// each category's `default:` against this map and warn if they diverge —
// but do not fail; the YAML field is authoritative for the per-category
// fallback layer).
func DefaultForTier(t Tier) Setting {
	switch t {
	case Tier1:
		return SettingMailSlack
	case Tier2:
		return SettingMail
	case Tier3:
		return SettingOff
	}
	return SettingOff
}

// CategorySpec is a single row from the YAML `categories:` map.
type CategorySpec struct {
	Name        string  `yaml:"-"` // injected from the map key
	Tier        Tier    `yaml:"tier"`
	Default     Setting `yaml:"default"`
	Description string  `yaml:"description"`
}

// PresetSpec is a single row from the YAML `presets:` map.
//
// `Rules` is either:
//   - the literal string "tier_defaults" (special token meaning "use
//     each category's tier default"), captured by RulesToken == "tier_defaults"
//     with RulesMap == nil; OR
//   - a map of category-name → setting, with the special wildcard "*"
//     applying to all unlisted categories.
//
// The YAML can express the special token either as `rules: tier_defaults`
// (a bare scalar) or as the literal map; we accept both shapes and
// canonicalise during parse.
type PresetSpec struct {
	Name        string             `yaml:"-"` // injected from the map key
	Description string             `yaml:"description"`
	RulesToken  string             `yaml:"-"` // "tier_defaults" or "" (means use RulesMap)
	RulesMap    map[string]Setting `yaml:"-"` // category → setting; "*" wildcard
}

// Resolve returns the setting this preset assigns to the named category,
// given the registered category-spec set (used for the `tier_defaults`
// token to look up the category's tier).
//
// Precedence inside a single preset:
//
//  1. Exact category-name match in RulesMap
//  2. Wildcard "*" in RulesMap
//  3. tier_defaults token → DefaultForTier(category.Tier)
//
// If none of the above resolves, returns ("", false). The dispatcher
// treats that as "this preset doesn't speak for this category — fall
// through to the next layer."
func (p PresetSpec) Resolve(category string, specs map[string]CategorySpec) (Setting, bool) {
	if p.RulesMap != nil {
		if s, ok := p.RulesMap[category]; ok {
			return s, true
		}
		if s, ok := p.RulesMap["*"]; ok {
			return s, true
		}
	}
	if p.RulesToken == "tier_defaults" {
		spec, ok := specs[category]
		if !ok {
			return "", false
		}
		return DefaultForTier(spec.Tier), true
	}
	return "", false
}

// Config is the parsed notifications.yaml document.
type Config struct {
	Version    int                     `yaml:"version"`
	Categories map[string]CategorySpec `yaml:"-"`
	Presets    map[string]PresetSpec   `yaml:"-"`
}

// rawConfig is the on-disk YAML shape (used only by the parser).
type rawConfig struct {
	Version    int                            `yaml:"version"`
	Categories map[string]rawCategory         `yaml:"categories"`
	Presets    map[string]rawPresetUnmarshall `yaml:"presets"`
}

type rawCategory struct {
	Tier        int    `yaml:"tier"`
	Default     string `yaml:"default"`
	Description string `yaml:"description"`
}

// rawPresetUnmarshall captures both shapes for `rules:` — the bare
// scalar `tier_defaults` and the map form. yaml.Node lets us inspect
// the kind without committing to a Go shape up front.
type rawPresetUnmarshall struct {
	Description string    `yaml:"description"`
	Rules       yaml.Node `yaml:"rules"`
}

// LoadConfig reads + parses notifications.yaml at the given path. Returns
// a fully validated Config or an error describing the first defect
// encountered (missing file, malformed YAML, unknown tier, unknown
// setting, unknown preset rule shape, etc.).
//
// LoadConfig is fail-closed: if anything is off, no Config is returned.
// Callers (the daemon-startup seeder + the dispatcher's lazy reload
// path) treat a parse failure as a hard error — surfacing through the
// startup-error mail path rather than silently routing to YAML
// defaults that no longer exist on disk.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("notify: read %s: %w", path, err)
	}
	return ParseConfig(data, path)
}

// ParseConfig parses an in-memory YAML byte slice. Used by tests that
// want to drive parser variations without touching the filesystem.
//
// `sourceLabel` is a free-text label (typically the file path) that
// appears in error messages so a synthetic test that passes
// "fake-test.yaml" gets a readable diagnostic.
func ParseConfig(data []byte, sourceLabel string) (*Config, error) {
	var raw rawConfig
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("notify: parse %s: %w", sourceLabel, err)
	}
	if raw.Version != 1 {
		return nil, fmt.Errorf("notify: %s: unsupported version %d (want 1)", sourceLabel, raw.Version)
	}
	if len(raw.Categories) == 0 {
		return nil, fmt.Errorf("notify: %s: no categories declared", sourceLabel)
	}

	cfg := &Config{
		Version:    raw.Version,
		Categories: make(map[string]CategorySpec, len(raw.Categories)),
		Presets:    make(map[string]PresetSpec, len(raw.Presets)),
	}

	// Sort category names to keep validation order deterministic so
	// error messages don't flap between runs.
	catNames := make([]string, 0, len(raw.Categories))
	for k := range raw.Categories {
		catNames = append(catNames, k)
	}
	sort.Strings(catNames)
	for _, name := range catNames {
		rc := raw.Categories[name]
		tier := Tier(rc.Tier)
		if !tier.IsValid() {
			return nil, fmt.Errorf("notify: %s: category %q has invalid tier %d (want 1, 2, or 3)", sourceLabel, name, rc.Tier)
		}
		def := Setting(rc.Default)
		if !def.IsValid() {
			return nil, fmt.Errorf("notify: %s: category %q has invalid default %q (want off, mail, slack, or mail+slack)", sourceLabel, name, rc.Default)
		}
		cfg.Categories[name] = CategorySpec{
			Name:        name,
			Tier:        tier,
			Default:     def,
			Description: rc.Description,
		}
	}

	presetNames := make([]string, 0, len(raw.Presets))
	for k := range raw.Presets {
		presetNames = append(presetNames, k)
	}
	sort.Strings(presetNames)
	for _, name := range presetNames {
		rp := raw.Presets[name]
		ps, err := decodePreset(name, rp, sourceLabel, cfg.Categories)
		if err != nil {
			return nil, err
		}
		cfg.Presets[name] = ps
	}

	// Sanity check: the "default" preset should exist (the dispatcher
	// uses it as the active-preset fallback when SystemConfig points at
	// an unknown name). Missing "default" is a config bug we surface
	// immediately rather than silently fall through to YAML defaults.
	if _, ok := cfg.Presets["default"]; !ok {
		return nil, fmt.Errorf("notify: %s: missing required preset %q", sourceLabel, "default")
	}

	return cfg, nil
}

// decodePreset translates the rawPresetUnmarshall (which owns a yaml.Node
// for the `rules` field) into a PresetSpec. Handles the two shapes:
//
//   - scalar "tier_defaults" → RulesToken="tier_defaults"
//   - map → RulesMap (with each value validated as a Setting)
func decodePreset(name string, rp rawPresetUnmarshall, sourceLabel string, cats map[string]CategorySpec) (PresetSpec, error) {
	out := PresetSpec{
		Name:        name,
		Description: rp.Description,
	}
	switch rp.Rules.Kind {
	case yaml.ScalarNode:
		if rp.Rules.Value != "tier_defaults" {
			return out, fmt.Errorf("notify: %s: preset %q has unknown scalar rules %q (only %q is supported)", sourceLabel, name, rp.Rules.Value, "tier_defaults")
		}
		out.RulesToken = "tier_defaults"
	case yaml.MappingNode:
		var raw map[string]string
		if err := rp.Rules.Decode(&raw); err != nil {
			return out, fmt.Errorf("notify: %s: preset %q rules decode failed: %w", sourceLabel, name, err)
		}
		out.RulesMap = make(map[string]Setting, len(raw))
		for k, v := range raw {
			s := Setting(v)
			if !s.IsValid() {
				return out, fmt.Errorf("notify: %s: preset %q has invalid setting %q for key %q (want off, mail, slack, or mail+slack)", sourceLabel, name, v, k)
			}
			// Wildcard "*" is always allowed; non-wildcard keys must be
			// real category names so a typo at config-author time fails
			// loudly rather than silently routing to YAML defaults.
			if k != "*" {
				if _, ok := cats[k]; !ok {
					return out, fmt.Errorf("notify: %s: preset %q references unknown category %q", sourceLabel, name, k)
				}
			}
			out.RulesMap[k] = s
		}
	case 0:
		// rules omitted entirely. Treat as empty map so the preset
		// resolves "" (false) for every category, which causes the
		// dispatcher to fall through to the next layer. We disallow
		// this at parse time — a preset with no rules has no meaning.
		return out, errors.New("notify: " + sourceLabel + ": preset " + name + " has no rules")
	default:
		return out, fmt.Errorf("notify: %s: preset %q has unsupported rules kind %d", sourceLabel, name, rp.Rules.Kind)
	}
	return out, nil
}

// CategoryNames returns the registered category names in sorted order.
// Used by tests + the seeder to drive deterministic iteration.
func (c *Config) CategoryNames() []string {
	out := make([]string, 0, len(c.Categories))
	for k := range c.Categories {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// PresetNames returns the registered preset names in sorted order.
func (c *Config) PresetNames() []string {
	out := make([]string, 0, len(c.Presets))
	for k := range c.Presets {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
