package capabilities

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"
)

// TestLoadProfile_AllAgentsValid is the happy-path sweep: every YAML
// profile in agents/capabilities/ must parse, validate, and resolve
// against the registry + blocklist without error. This is the
// regression that fires if a profile drifts from the registry (typo,
// removed namespace, etc.).
func TestLoadProfile_AllAgentsValid(t *testing.T) {
	names, err := ListProfiles()
	if err != nil {
		t.Fatalf("ListProfiles: %v", err)
	}
	if len(names) < 16 {
		t.Fatalf("expected at least 16 agent profiles (per D1 T0-1 spec), got %d: %v", len(names), names)
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			p, err := LoadProfile(name)
			if err != nil {
				t.Fatalf("LoadProfile(%q): %v", name, err)
			}
			if p.Agent != name {
				t.Errorf("profile.Agent = %q, want %q", p.Agent, name)
			}
			// AllowedTools is sorted + deduplicated by resolveAllowedTools.
			if !sort.StringsAreSorted(p.AllowedTools) {
				t.Errorf("AllowedTools not sorted: %v", p.AllowedTools)
			}
		})
	}
}

// TestLoadProfile_NonexistentReturnsError exercises the fail-closed
// rule: a missing profile MUST fail loudly, never fall back to "all
// tools". An agent without a profile cannot run.
func TestLoadProfile_NonexistentReturnsError(t *testing.T) {
	_, err := LoadProfile("definitely-not-a-real-agent")
	if err == nil {
		t.Fatalf("LoadProfile of nonexistent agent must return error — fail-closed contract")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention not-found; got %v", err)
	}
}

// TestLoadProfile_EmptyAgentName covers the explicit empty-string
// guard.
func TestLoadProfile_EmptyAgentName(t *testing.T) {
	_, err := LoadProfile("")
	if err == nil {
		t.Fatal("LoadProfile(\"\") must return error")
	}
}

// TestRegistryConsistent walks every namespace expansion in
// REGISTRY.yaml and asserts each expanded tool is listed in mcp_tools.
// This is the registry's own internal-consistency invariant; without
// it a profile granting a namespace might silently grant a non-
// existent tool.
func TestRegistryConsistent(t *testing.T) {
	reg, err := loadRegistry()
	if err != nil {
		t.Fatalf("loadRegistry: %v", err)
	}
	mcp := setOf(reg.MCPTools)
	for ns, expansion := range reg.MCPNamespaces {
		for _, tool := range expansion {
			if _, ok := mcp[tool]; !ok {
				t.Errorf("namespace %q expands to %q which is not in mcp_tools", ns, tool)
			}
		}
	}
}

// TestBlocklistTuplesAreNotInProfiles asserts the load-bearing
// blocklist invariant: no per-agent profile can grant a tool that's
// on .forceblocklist.yaml. This already runs as part of LoadProfile
// validation; here we surface it as an explicit regression so the
// "what does the blocklist actually do" question has a single answer.
func TestBlocklistTuplesAreNotInProfiles(t *testing.T) {
	reg, err := loadRegistry()
	if err != nil {
		t.Fatalf("loadRegistry: %v", err)
	}
	bl, err := loadBlocklist(reg)
	if err != nil {
		t.Fatalf("loadBlocklist: %v", err)
	}
	if len(bl.expandedBlocks) == 0 {
		t.Fatal("blocklist is empty — at least the Slack-write namespace must remain")
	}

	names, err := ListProfiles()
	if err != nil {
		t.Fatalf("ListProfiles: %v", err)
	}
	for _, name := range names {
		p, err := LoadProfile(name)
		if err != nil {
			t.Fatalf("LoadProfile(%q): %v", name, err)
		}
		for _, granted := range p.AllowedTools {
			if _, blocked := bl.expandedBlocks[granted]; blocked {
				t.Errorf("profile %q grants blocklisted tool %q — LoadProfile should have rejected this", name, granted)
			}
		}
	}
}

// TestDisallowedToolsArg_Captain_BlocksBash is the smoke test the
// closure note calls out: Captain's profile lists no builtin tools,
// so DisallowedToolsArg must contain Bash. This is the empirical
// proof that the complement logic actually produces a Bash-less
// argv for read-only review agents.
func TestDisallowedToolsArg_Captain_BlocksBash(t *testing.T) {
	captain, err := LoadProfile("captain")
	if err != nil {
		t.Fatalf("LoadProfile(captain): %v", err)
	}
	allowed := captain.AllowedToolsArg()
	if strings.Contains(allowed, "Bash") {
		t.Fatalf("Captain.AllowedToolsArg unexpectedly contains Bash: %s", allowed)
	}
	disallowed := captain.DisallowedToolsArg()
	if !strings.Contains(disallowed, "Bash") {
		t.Fatalf("Captain.DisallowedToolsArg must contain Bash; got: %s", disallowed)
	}
	// Sanity: allowed and disallowed must be disjoint.
	allowedSet := setOf(strings.Split(allowed, ","))
	for _, d := range strings.Split(disallowed, ",") {
		if _, both := allowedSet[d]; both && d != "" {
			t.Errorf("tool %q appears in both AllowedToolsArg and DisallowedToolsArg", d)
		}
	}
}

// TestComplementCovers_AllRegistryTools asserts the universe property:
// for every tool in REGISTRY (builtin + mcp), it is either in
// AllowedToolsArg or DisallowedToolsArg, never neither. Astromech is
// chosen as the agent with the broadest grant — if it leaks one,
// every narrower profile would too.
func TestComplementCovers_AllRegistryTools(t *testing.T) {
	astromech, err := LoadProfile("astromech")
	if err != nil {
		t.Fatalf("LoadProfile(astromech): %v", err)
	}
	allowedSet := setOf(splitNonEmpty(astromech.AllowedToolsArg(), ","))
	disallowedSet := setOf(splitNonEmpty(astromech.DisallowedToolsArg(), ","))

	reg, err := loadRegistry()
	if err != nil {
		t.Fatalf("loadRegistry: %v", err)
	}
	for _, tool := range append(append([]string{}, reg.BuiltinTools...), reg.MCPTools...) {
		_, inAllowed := allowedSet[tool]
		_, inDisallowed := disallowedSet[tool]
		if !inAllowed && !inDisallowed {
			t.Errorf("registry tool %q in neither AllowedToolsArg nor DisallowedToolsArg", tool)
		}
		if inAllowed && inDisallowed {
			t.Errorf("registry tool %q in BOTH AllowedToolsArg and DisallowedToolsArg", tool)
		}
	}
}

// TestMCPConfigArg_GeneratesFile asserts MCPConfigArg writes a JSON
// file under ~/.force/cache/mcp-configs/ for a profile with MCP
// servers, and returns ("", nil) for a profile with none.
func TestMCPConfigArg_GeneratesFile(t *testing.T) {
	// Astromech grants 4 namespaces; should produce a file.
	astromech, err := LoadProfile("astromech")
	if err != nil {
		t.Fatalf("LoadProfile(astromech): %v", err)
	}
	path, err := astromech.MCPConfigArg()
	if err != nil {
		t.Fatalf("astromech.MCPConfigArg: %v", err)
	}
	if path == "" {
		t.Fatal("expected non-empty MCP config path for astromech")
	}
	if !strings.Contains(path, "/.force/cache/mcp-configs/") {
		t.Errorf("expected path under ~/.force/cache/mcp-configs/, got %s", path)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read mcp config file: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("mcp config file is not valid JSON: %v", err)
	}
	if doc["agent"] != "astromech" {
		t.Errorf("agent field = %v, want astromech", doc["agent"])
	}
	servers, ok := doc["mcp_servers"].([]any)
	if !ok || len(servers) != 4 {
		t.Errorf("expected 4 mcp_servers, got %v", doc["mcp_servers"])
	}

	// Chancellor grants no MCP servers; should return empty path.
	chancellor, err := LoadProfile("chancellor")
	if err != nil {
		t.Fatalf("LoadProfile(chancellor): %v", err)
	}
	cpath, err := chancellor.MCPConfigArg()
	if err != nil {
		t.Fatalf("chancellor.MCPConfigArg: %v", err)
	}
	if cpath != "" {
		t.Errorf("expected empty path for chancellor (no MCP servers); got %q", cpath)
	}
}

// TestResolveAllowedTools_RejectsUnknownBuiltin uses an in-test fake
// profile to confirm the validator rejects a builtin tool not in the
// registry.
func TestResolveAllowedTools_RejectsUnknownBuiltin(t *testing.T) {
	reg, _ := loadRegistry()
	bl, _ := loadBlocklist(reg)
	p := &Profile{Agent: "fake", BuiltinTools: []string{"NotARealTool"}}
	_, err := resolveAllowedTools(p, reg, bl)
	if err == nil {
		t.Fatal("resolveAllowedTools must reject unknown builtin")
	}
	if !strings.Contains(err.Error(), "NotARealTool") {
		t.Errorf("error should name offending tool; got %v", err)
	}
}

// TestResolveAllowedTools_RejectsUnknownNamespace covers the
// counterpart for unknown MCP namespaces.
func TestResolveAllowedTools_RejectsUnknownNamespace(t *testing.T) {
	reg, _ := loadRegistry()
	bl, _ := loadBlocklist(reg)
	p := &Profile{Agent: "fake", MCPServers: []string{"mcp:not-a-real-namespace"}}
	_, err := resolveAllowedTools(p, reg, bl)
	if err == nil {
		t.Fatal("resolveAllowedTools must reject unknown namespace")
	}
	if !strings.Contains(err.Error(), "not-a-real-namespace") {
		t.Errorf("error should name offending namespace; got %v", err)
	}
}

// TestResolveAllowedTools_RejectsBlocklistedTool fabricates a profile
// that grants a blocklisted Slack-write tool and confirms validation
// fails. Without this guard a typo in blocklist resolution could
// allow privilege escalation by per-agent profile edit.
func TestResolveAllowedTools_RejectsBlocklistedTool(t *testing.T) {
	reg, _ := loadRegistry()
	bl, _ := loadBlocklist(reg)
	// slack_send_message is in the blocklist via mcp:slack-write namespace.
	p := &Profile{Agent: "fake", MCPServers: []string{"mcp__plugin_slack_slack__slack_send_message"}}
	_, err := resolveAllowedTools(p, reg, bl)
	if err == nil {
		t.Fatal("resolveAllowedTools must reject blocklisted tool")
	}
	if !strings.Contains(err.Error(), "blocklist") {
		t.Errorf("error should mention blocklist; got %v", err)
	}
}

func splitNonEmpty(s, sep string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, sep)
	out := parts[:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
