// Package agents — D3 polish-pass iteration 2 live Haiku gating.
//
// The 7 renderers (narrative, briefing, learning-panel, replay, ask,
// retro, plus the transcript-archive summariser which is text-shape-
// only and stays deterministic) each guard their LLM call site with
// liveHaikuDisabled(). When the env flag is "1" or "true", the
// renderer returns the deterministic synthesise* stub output. When
// unset, the renderer routes through claude.CallWithTranscript with
// a per-renderer capability profile (loaded once, cached for the
// process lifetime).
//
// Tests pin to deterministic mode via TestMain (testmain_test.go) so
// no unit test ever spends an LLM call. Production daemons leave the
// flag unset; renderers default to live Haiku once the dashboard or
// dog ticks the entry point.
//
// Wrapper choice: every renderer calls CallWithTranscript (one-shot,
// no streaming). Streaming is reserved for astromech tail output;
// renderers want a single prose blob landing in the DB row.
package agents

import (
	"fmt"
	"os"
	"sync"

	"force-orchestrator/internal/agents/capabilities"
)

// liveHaikuDisabled reports whether the LIVE_HAIKU_DISABLED env flag
// pins renderers to deterministic synth mode. The flag accepts the
// shapes "1" and "true" — matching the convention used by Go's
// strconv.ParseBool minus the locale-y forms.
//
// Why an env flag rather than a SystemConfig key: tests need to pin
// the mode BEFORE any DB connection exists (TestMain runs before
// each test's :memory: init). A SystemConfig key would require every
// test to seed the row. The env flag is a single t.Setenv call in
// TestMain — strictly simpler, and equivalent at the runtime tier
// (production daemons leave the flag unset).
func liveHaikuDisabled() bool {
	v := os.Getenv("LIVE_HAIKU_DISABLED")
	return v == "1" || v == "true"
}

// rendererProfileCache memoises capabilities.LoadProfile per agent
// name. Profiles are immutable (the YAML is embedded) so caching is
// safe; the alternative would be a load-per-call which is wasteful
// at the tick rate the renderers run at.
var (
	rendererProfileMu    sync.Mutex
	rendererProfileCache = map[string]rendererProfile{}
)

type rendererProfile struct {
	allowedTools    string
	disallowedTools string
	mcpConfig       string
}

// loadRendererProfile returns the cached AllowedTools / DisallowedTools
// / MCPConfig args for a renderer agent. On miss it loads via the
// capabilities loader. A failed load is fatal-shape (the live path
// can't proceed without a profile per Pattern P13); callers fall
// back to the deterministic synth in that case.
func loadRendererProfile(agentName string) (rendererProfile, error) {
	rendererProfileMu.Lock()
	defer rendererProfileMu.Unlock()
	if rp, ok := rendererProfileCache[agentName]; ok {
		return rp, nil
	}
	prof, err := capabilities.LoadProfile(agentName)
	if err != nil {
		return rendererProfile{}, fmt.Errorf("loadRendererProfile: %w", err)
	}
	mcpConfig, mcpErr := prof.MCPConfigArg()
	if mcpErr != nil {
		// Log-only: an MCPConfigArg failure is recoverable — the live
		// CLI call still works without --mcp-config. Per the existing
		// pattern in captain.go / medic.go.
		mcpConfig = ""
	}
	rp := rendererProfile{
		allowedTools:    prof.AllowedToolsArg(),
		disallowedTools: prof.DisallowedToolsArg(),
		mcpConfig:       mcpConfig,
	}
	rendererProfileCache[agentName] = rp
	return rp, nil
}
