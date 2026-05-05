package notify

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNotifYAML_LoadConfig_RealFile parses the canonical
// config/notifications.yaml shipped with the repo. Every assertion below
// is structural — if it fails after a YAML edit, the edit removed a
// category the substrate guarantees exists.
func TestNotifYAML_LoadConfig_RealFile(t *testing.T) {
	root := repoRoot(t)
	cfg, err := LoadConfig(filepath.Join(root, "config", "notifications.yaml"))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Version != 1 {
		t.Errorf("version: got %d, want 1", cfg.Version)
	}
	// Tier-1 categories the substrate guarantees.
	for _, name := range []string{
		"supply_token_expired", "spend_cap_e_stop", "gate_timeout_failed",
		"operator_confirm_required", "consumer_breakage", "senate_dissent_block",
		"promotion_proposal_pending", "task_escalated", "convoy_review_needs_work",
	} {
		spec, ok := cfg.Categories[name]
		if !ok {
			t.Errorf("category %q missing from real YAML", name)
			continue
		}
		if spec.Tier != Tier1 {
			t.Errorf("category %q tier=%d, want 1", name, spec.Tier)
		}
		if spec.Default != SettingMailSlack {
			t.Errorf("category %q default=%q, want mail+slack", name, spec.Default)
		}
	}
	// Tier-2 — at least stage_transition + awaiting_supply_recheck.
	for _, name := range []string{"stage_transition", "awaiting_supply_recheck"} {
		spec, ok := cfg.Categories[name]
		if !ok {
			t.Errorf("category %q missing from real YAML", name)
			continue
		}
		if spec.Tier != Tier2 {
			t.Errorf("category %q tier=%d, want 2", name, spec.Tier)
		}
		if spec.Default != SettingMail {
			t.Errorf("category %q default=%q, want mail", name, spec.Default)
		}
	}
	// Tier-3 supply_token_recovered.
	if spec, ok := cfg.Categories["supply_token_recovered"]; !ok {
		t.Error("category supply_token_recovered missing from real YAML")
	} else {
		if spec.Tier != Tier3 {
			t.Errorf("supply_token_recovered tier=%d, want 3", spec.Tier)
		}
		if spec.Default != SettingOff {
			t.Errorf("supply_token_recovered default=%q, want off", spec.Default)
		}
	}
	// Presets.
	for _, name := range []string{"default", "focus", "verbose"} {
		if _, ok := cfg.Presets[name]; !ok {
			t.Errorf("preset %q missing", name)
		}
	}
	// "default" preset uses tier_defaults.
	if cfg.Presets["default"].RulesToken != "tier_defaults" {
		t.Errorf("default preset RulesToken=%q, want tier_defaults", cfg.Presets["default"].RulesToken)
	}
	// "verbose" preset has wildcard mail+slack.
	if v, ok := cfg.Presets["verbose"].RulesMap["*"]; !ok || v != SettingMailSlack {
		t.Errorf("verbose preset wildcard=%q ok=%v, want mail+slack/true", v, ok)
	}
	// "focus" preset has wildcard off + Tier-1 carve-outs.
	focus := cfg.Presets["focus"]
	if v := focus.RulesMap["*"]; v != SettingOff {
		t.Errorf("focus preset wildcard=%q, want off", v)
	}
	if v := focus.RulesMap["spend_cap_e_stop"]; v != SettingMailSlack {
		t.Errorf("focus preset spend_cap_e_stop=%q, want mail+slack", v)
	}
}

// TestNotifYAML_ParseConfig_Synthetic exercises the parser with crafted
// inputs covering valid + each invalid category.
func TestNotifYAML_ParseConfig_Synthetic(t *testing.T) {
	cases := []struct {
		name      string
		yaml      string
		wantErr   bool
		errSubstr string
	}{
		{
			name: "minimal-valid",
			yaml: `
version: 1
categories:
  foo:
    tier: 1
    default: mail+slack
    description: minimal foo
presets:
  default:
    description: defaults
    rules: tier_defaults
`,
		},
		{
			name: "missing-version",
			yaml: `
categories:
  foo:
    tier: 1
    default: mail
    description: x
presets:
  default:
    description: d
    rules: tier_defaults
`,
			wantErr:   true,
			errSubstr: "unsupported version",
		},
		{
			name: "wrong-version",
			yaml: `
version: 99
categories:
  foo:
    tier: 1
    default: mail
    description: x
presets:
  default:
    description: d
    rules: tier_defaults
`,
			wantErr:   true,
			errSubstr: "unsupported version",
		},
		{
			name: "no-categories",
			yaml: `
version: 1
categories: {}
presets:
  default:
    description: d
    rules: tier_defaults
`,
			wantErr:   true,
			errSubstr: "no categories declared",
		},
		{
			name: "invalid-tier",
			yaml: `
version: 1
categories:
  foo:
    tier: 9
    default: mail
    description: x
presets:
  default:
    description: d
    rules: tier_defaults
`,
			wantErr:   true,
			errSubstr: "invalid tier 9",
		},
		{
			name: "invalid-setting",
			yaml: `
version: 1
categories:
  foo:
    tier: 1
    default: bogus
    description: x
presets:
  default:
    description: d
    rules: tier_defaults
`,
			wantErr:   true,
			errSubstr: `invalid default "bogus"`,
		},
		{
			name: "preset-unknown-scalar",
			yaml: `
version: 1
categories:
  foo:
    tier: 1
    default: mail
    description: x
presets:
  default:
    description: d
    rules: bogus_token
`,
			wantErr:   true,
			errSubstr: "unknown scalar rules",
		},
		{
			name: "preset-invalid-setting-in-map",
			yaml: `
version: 1
categories:
  foo:
    tier: 1
    default: mail
    description: x
presets:
  default:
    description: d
    rules:
      foo: bogus
`,
			wantErr:   true,
			errSubstr: `invalid setting "bogus"`,
		},
		{
			name: "preset-unknown-category",
			yaml: `
version: 1
categories:
  foo:
    tier: 1
    default: mail
    description: x
presets:
  default:
    description: d
    rules:
      bar: mail
`,
			wantErr:   true,
			errSubstr: `unknown category "bar"`,
		},
		{
			name: "missing-default-preset",
			yaml: `
version: 1
categories:
  foo:
    tier: 1
    default: mail
    description: x
presets:
  custom:
    description: d
    rules:
      foo: off
`,
			wantErr:   true,
			errSubstr: `missing required preset "default"`,
		},
		{
			name: "preset-wildcard-allowed",
			yaml: `
version: 1
categories:
  foo:
    tier: 1
    default: mail
    description: x
presets:
  default:
    description: d
    rules:
      "*": off
      foo: mail+slack
`,
		},
		{
			name: "malformed-yaml",
			yaml: `
version: 1
categories:
  foo
    tier: 1
`,
			wantErr:   true,
			errSubstr: "parse",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseConfig([]byte(tc.yaml), "synthetic-"+tc.name+".yaml")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error containing %q, got nil", tc.errSubstr)
				}
				if !strings.Contains(err.Error(), tc.errSubstr) {
					t.Errorf("err=%q, want substring %q", err.Error(), tc.errSubstr)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			}
		})
	}
}

// TestNotifYAML_LoadConfig_MissingFile confirms a missing path surfaces
// a clear file-not-found error rather than silently returning a nil
// config.
func TestNotifYAML_LoadConfig_MissingFile(t *testing.T) {
	tmp := t.TempDir()
	_, err := LoadConfig(filepath.Join(tmp, "nope.yaml"))
	if err == nil {
		t.Fatal("want error for missing file, got nil")
	}
	if !strings.Contains(err.Error(), "read") {
		t.Errorf("err=%q, want substring \"read\"", err.Error())
	}
}

// TestNotifYAML_PresetResolve exercises the per-preset resolution
// (exact-match → wildcard → tier_defaults → none).
func TestNotifYAML_PresetResolve(t *testing.T) {
	cfg, err := ParseConfig([]byte(`
version: 1
categories:
  alpha:
    tier: 1
    default: mail+slack
    description: a
  beta:
    tier: 2
    default: mail
    description: b
  gamma:
    tier: 3
    default: off
    description: g
presets:
  default:
    description: d
    rules: tier_defaults
  noisy:
    description: noisy
    rules:
      "*": mail+slack
  focused:
    description: focused
    rules:
      "*": off
      alpha: mail+slack
  exact_only:
    description: exact_only
    rules:
      alpha: slack
`), "syn.yaml")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	tests := []struct {
		preset   string
		category string
		want     Setting
		ok       bool
	}{
		{"default", "alpha", SettingMailSlack, true},
		{"default", "beta", SettingMail, true},
		{"default", "gamma", SettingOff, true},
		{"noisy", "alpha", SettingMailSlack, true},
		{"noisy", "beta", SettingMailSlack, true},
		{"noisy", "gamma", SettingMailSlack, true},
		{"focused", "alpha", SettingMailSlack, true}, // exact-match wins over wildcard
		{"focused", "beta", SettingOff, true},        // wildcard
		{"exact_only", "alpha", SettingSlack, true},
		{"exact_only", "beta", "", false}, // exact_only doesn't speak; falls through
	}
	for _, tc := range tests {
		got, ok := cfg.Presets[tc.preset].Resolve(tc.category, cfg.Categories)
		if got != tc.want || ok != tc.ok {
			t.Errorf("preset=%s cat=%s: got (%q, %v), want (%q, %v)",
				tc.preset, tc.category, got, ok, tc.want, tc.ok)
		}
	}
}

// repoRoot finds the repo root by walking up from the current test's
// working directory until a go.mod is found.
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	dir := wd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s", wd)
		}
		dir = parent
	}
}
