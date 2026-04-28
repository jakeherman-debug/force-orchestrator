// Package capabilities loads per-agent capability profiles from the
// embedded YAML files at agents/capabilities/. Each profile declares
// the builtin tools and MCP servers a single agent is permitted to
// invoke. The fleet-wide REGISTRY enumerates every tool a profile may
// reference; the .forceblocklist names tools no agent may grant
// regardless of profile.
//
// The load path is fail-closed by design: a missing profile, a profile
// listing an unknown tool, or a profile granting a blocklisted tool
// all return errors. There is NO silent fallback to "all tools" — an
// agent without a profile cannot run.
//
// Pattern P13 (internal/audittools/audit_pattern_p13_*) enforces at
// CI time that every AskClaudeCLI / RunCLIStreamingContext call site
// sources its tool args from a Profile and not from a hardcoded
// literal.
package capabilities

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	capprofiles "force-orchestrator/agents/capabilities"

	"gopkg.in/yaml.v3"
)

// Profile is the resolved form of an agents/capabilities/<agent>.yaml
// file: the agent name, its allowed builtin tools, the concrete MCP
// tool names expanded from referenced namespaces, and human-readable
// description fields preserved for audit.
//
// AllowedTools is the union of BuiltinTools + every concrete MCP tool
// name expanded from MCPServers. It is what AllowedToolsArg /
// DisallowedToolsArg operate on.
type Profile struct {
	Agent        string   `yaml:"agent"`
	Description  string   `yaml:"description"`
	BuiltinTools []string `yaml:"builtin_tools"`
	MCPServers   []string `yaml:"mcp_servers"`
	Notes        string   `yaml:"notes"`

	// AllowedTools is BuiltinTools + concrete-expanded MCP tools, in
	// stable sorted order. Populated by LoadProfile after validation.
	AllowedTools []string `yaml:"-"`
}

// registry is the resolved form of REGISTRY.yaml. The loader caches
// it on first use; the YAML is embedded so the cache cannot drift
// from the binary.
type registry struct {
	BuiltinTools  []string            `yaml:"builtin_tools"`
	MCPTools      []string            `yaml:"mcp_tools"`
	MCPNamespaces map[string][]string `yaml:"mcp_namespaces"`
}

// blocklist is the resolved form of .forceblocklist.yaml. The Blocked
// field may contain namespace tokens (mcp:slack-write) or concrete
// tool names; expandedBlocked holds the flattened concrete-tool set
// after resolution against the registry.
type blocklist struct {
	Blocked        []string `yaml:"blocked"`
	expandedBlocks map[string]struct{}
}

var (
	registryOnce sync.Once
	registryVal  *registry
	registryErr  error

	blocklistOnce sync.Once
	blocklistVal  *blocklist
	blocklistErr  error
)

// LoadProfile reads agents/capabilities/<agentName>.yaml from the
// embedded FS, validates it against REGISTRY.yaml + .forceblocklist.yaml,
// and returns the resolved Profile.
//
// Returns an error and a nil Profile if:
//   - the YAML file is missing
//   - the YAML file is malformed
//   - the agent field is empty or doesn't match the filename
//   - any builtin_tools entry isn't in the registry
//   - any mcp_servers entry isn't a known namespace AND isn't a
//     concrete MCP tool name in the registry
//   - any expanded tool is on the fleet blocklist
//
// LoadProfile NEVER falls back to "all tools" on error — a missing
// or invalid profile is a hard failure.
func LoadProfile(agentName string) (*Profile, error) {
	if agentName == "" {
		return nil, fmt.Errorf("capabilities: agent name required")
	}
	reg, err := loadRegistry()
	if err != nil {
		return nil, fmt.Errorf("capabilities: load registry: %w", err)
	}
	bl, err := loadBlocklist(reg)
	if err != nil {
		return nil, fmt.Errorf("capabilities: load blocklist: %w", err)
	}

	raw, err := capprofiles.FS.ReadFile(agentName + ".yaml")
	if err != nil {
		return nil, fmt.Errorf("capabilities: profile %q not found (looked for agents/capabilities/%s.yaml): %w", agentName, agentName, err)
	}
	var p Profile
	if err := yaml.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("capabilities: profile %q malformed YAML: %w", agentName, err)
	}
	if p.Agent == "" {
		return nil, fmt.Errorf("capabilities: profile %q missing required `agent:` field", agentName)
	}
	if p.Agent != agentName {
		return nil, fmt.Errorf("capabilities: profile filename %q does not match `agent:` field %q", agentName, p.Agent)
	}

	allowed, err := resolveAllowedTools(&p, reg, bl)
	if err != nil {
		return nil, fmt.Errorf("capabilities: profile %q: %w", agentName, err)
	}
	p.AllowedTools = allowed
	return &p, nil
}

// resolveAllowedTools validates every tool/namespace the profile
// requests against the registry, expands namespaces to concrete tool
// names, applies the blocklist, and returns the sorted concrete-tool
// allowlist.
func resolveAllowedTools(p *Profile, reg *registry, bl *blocklist) ([]string, error) {
	regBuiltins := setOf(reg.BuiltinTools)
	regMCPTools := setOf(reg.MCPTools)

	allowed := map[string]struct{}{}

	for _, t := range p.BuiltinTools {
		if _, ok := regBuiltins[t]; !ok {
			return nil, fmt.Errorf("builtin tool %q not in REGISTRY.yaml", t)
		}
		if _, blocked := bl.expandedBlocks[t]; blocked {
			return nil, fmt.Errorf("builtin tool %q is on the fleet blocklist (.forceblocklist.yaml)", t)
		}
		allowed[t] = struct{}{}
	}

	for _, server := range p.MCPServers {
		if strings.HasPrefix(server, "mcp:") {
			expanded, ok := reg.MCPNamespaces[server]
			if !ok {
				return nil, fmt.Errorf("mcp namespace %q not in REGISTRY.yaml mcp_namespaces", server)
			}
			for _, tool := range expanded {
				if _, ok := regMCPTools[tool]; !ok {
					return nil, fmt.Errorf("mcp namespace %q expands to tool %q which is NOT in REGISTRY.yaml mcp_tools — registry is internally inconsistent", server, tool)
				}
				if _, blocked := bl.expandedBlocks[tool]; blocked {
					return nil, fmt.Errorf("mcp namespace %q expands to tool %q which is on the fleet blocklist (.forceblocklist.yaml)", server, tool)
				}
				allowed[tool] = struct{}{}
			}
			continue
		}
		// Concrete tool name — must be in registry mcp_tools.
		if _, ok := regMCPTools[server]; !ok {
			return nil, fmt.Errorf("mcp tool %q not in REGISTRY.yaml mcp_tools (and not a known mcp:<namespace> token)", server)
		}
		if _, blocked := bl.expandedBlocks[server]; blocked {
			return nil, fmt.Errorf("mcp tool %q is on the fleet blocklist (.forceblocklist.yaml)", server)
		}
		allowed[server] = struct{}{}
	}

	out := make([]string, 0, len(allowed))
	for t := range allowed {
		out = append(out, t)
	}
	sort.Strings(out)
	return out, nil
}

// AllowedToolsArg renders the profile's allowed tool set as the
// comma-separated string expected by `claude --allowedTools`. Returns
// "" when the profile grants nothing (in which case the flag should
// be omitted by the CLI builder).
//
// Per Fix #8e empirical findings, --allowedTools is an auto-approve
// hint in --dangerously-skip-permissions mode, not a hard restriction.
// Hard restriction comes from DisallowedToolsArg.
func (p *Profile) AllowedToolsArg() string {
	if p == nil || len(p.AllowedTools) == 0 {
		return ""
	}
	return strings.Join(p.AllowedTools, ",")
}

// DisallowedToolsArg renders the COMPLEMENT of the profile (every
// fleet-known tool NOT in the profile, plus every blocklist entry)
// for `claude --disallowedTools`. This IS the hard restriction —
// listed tools are removed from Claude's catalog entirely.
func (p *Profile) DisallowedToolsArg() string {
	reg, err := loadRegistry()
	if err != nil {
		return ""
	}
	bl, err := loadBlocklist(reg)
	if err != nil {
		bl = &blocklist{expandedBlocks: map[string]struct{}{}}
	}
	allowed := map[string]struct{}{}
	if p != nil {
		for _, t := range p.AllowedTools {
			allowed[t] = struct{}{}
		}
	}

	universe := map[string]struct{}{}
	for _, t := range reg.BuiltinTools {
		universe[t] = struct{}{}
	}
	for _, t := range reg.MCPTools {
		universe[t] = struct{}{}
	}

	disallowed := map[string]struct{}{}
	for t := range universe {
		if _, granted := allowed[t]; granted {
			continue
		}
		disallowed[t] = struct{}{}
	}
	for t := range bl.expandedBlocks {
		// Belt-and-suspenders: the blocklist is already excluded from
		// allowed during LoadProfile validation, but if a future code
		// path bypasses validation we still want the blocklist to win.
		if _, granted := allowed[t]; granted {
			continue
		}
		disallowed[t] = struct{}{}
	}

	out := make([]string, 0, len(disallowed))
	for t := range disallowed {
		out = append(out, t)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// MCPConfigArg writes a JSON file naming the MCP servers/tools this
// profile grants and returns the path. Intended for use with
// `claude --mcp-config <path>` once the fleet adopts strict MCP server
// scoping (currently the operator's Claude Code installation supplies
// MCP server credentials; the file is generated for audit/forward-
// compat and does not yet drive runtime restriction — DisallowedToolsArg
// is the hard restriction today).
//
// The file is regenerated per call under ~/.force/cache/mcp-configs/.
// Returns ("", nil) when the profile grants no MCP servers (no file
// is written; caller should omit --mcp-config).
func (p *Profile) MCPConfigArg() (string, error) {
	if p == nil || len(p.MCPServers) == 0 {
		return "", nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("capabilities: resolve home dir: %w", err)
	}
	dir := filepath.Join(home, ".force", "cache", "mcp-configs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("capabilities: mkdir %s: %w", dir, err)
	}
	path := filepath.Join(dir, p.Agent+".json")

	// Filter the profile's allowed tool list down to MCP tools only;
	// builtin tools don't belong in --mcp-config.
	mcpTools := []string{}
	for _, t := range p.AllowedTools {
		if strings.HasPrefix(t, "mcp__") {
			mcpTools = append(mcpTools, t)
		}
	}
	doc := map[string]any{
		"agent":              p.Agent,
		"profile_description": "Generated from agents/capabilities/" + p.Agent + ".yaml — do not edit by hand.",
		"mcp_servers":         p.MCPServers,
		"mcp_tools_allowed":   mcpTools,
	}
	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", fmt.Errorf("capabilities: marshal mcp config: %w", err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return "", fmt.Errorf("capabilities: write %s: %w", path, err)
	}
	return path, nil
}

// loadRegistry reads REGISTRY.yaml from the embedded FS once per
// process. The cache is keyed by the binary's embedded copy, so cache
// invalidation is impossible by design — a profile change requires
// recompile.
func loadRegistry() (*registry, error) {
	registryOnce.Do(func() {
		raw, err := capprofiles.FS.ReadFile("REGISTRY.yaml")
		if err != nil {
			registryErr = fmt.Errorf("read REGISTRY.yaml: %w", err)
			return
		}
		var r registry
		if err := yaml.Unmarshal(raw, &r); err != nil {
			registryErr = fmt.Errorf("unmarshal REGISTRY.yaml: %w", err)
			return
		}
		if len(r.BuiltinTools) == 0 {
			registryErr = fmt.Errorf("REGISTRY.yaml has no builtin_tools")
			return
		}
		registryVal = &r
	})
	if registryErr != nil {
		return nil, registryErr
	}
	return registryVal, nil
}

// loadBlocklist reads .forceblocklist.yaml from the embedded FS once
// per process and expands any namespace tokens against the registry.
func loadBlocklist(reg *registry) (*blocklist, error) {
	blocklistOnce.Do(func() {
		raw, err := capprofiles.FS.ReadFile(".forceblocklist.yaml")
		if err != nil {
			blocklistErr = fmt.Errorf("read .forceblocklist.yaml: %w", err)
			return
		}
		var bl blocklist
		if err := yaml.Unmarshal(raw, &bl); err != nil {
			blocklistErr = fmt.Errorf("unmarshal .forceblocklist.yaml: %w", err)
			return
		}
		bl.expandedBlocks = map[string]struct{}{}
		for _, entry := range bl.Blocked {
			if strings.HasPrefix(entry, "mcp:") {
				expansion, ok := reg.MCPNamespaces[entry]
				if !ok {
					blocklistErr = fmt.Errorf(".forceblocklist.yaml references unknown namespace %q", entry)
					return
				}
				for _, tool := range expansion {
					bl.expandedBlocks[tool] = struct{}{}
				}
				continue
			}
			bl.expandedBlocks[entry] = struct{}{}
		}
		blocklistVal = &bl
	})
	if blocklistErr != nil {
		return nil, blocklistErr
	}
	return blocklistVal, nil
}

// ListProfiles returns the agent-name list (filename without .yaml
// extension) of every profile in the embedded FS, sorted. Used by
// Pattern P13 to assert that every Spawn* / LoadProfile reference
// has a corresponding YAML.
func ListProfiles() ([]string, error) {
	entries, err := capprofiles.FS.ReadDir(".")
	if err != nil {
		return nil, fmt.Errorf("capabilities: read embedded FS: %w", err)
	}
	var names []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".yaml") {
			continue
		}
		base := strings.TrimSuffix(name, ".yaml")
		// Skip the registry and blocklist — they are not agent profiles.
		if base == "REGISTRY" || base == ".forceblocklist" {
			continue
		}
		names = append(names, base)
	}
	sort.Strings(names)
	return names, nil
}

func setOf(items []string) map[string]struct{} {
	m := make(map[string]struct{}, len(items))
	for _, s := range items {
		m[s] = struct{}{}
	}
	return m
}
