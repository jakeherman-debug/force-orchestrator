// Package capabilities defines the client interface for agent
// capability profiles — per-agent allowed/disallowed tools, MCP server
// configurations, and the CLAUDE.md fragments injected at claim time.
//
// Implementation timeline:
//   - D0 (this commit): interface definition + ErrNotImplemented stubs.
//   - D1 (capability profiles deliverable, T0-1): the real in-process
//     implementation lands here, reading per-agent profiles from
//     `agent_profiles/<agent>.toml` and exposing them through this
//     interface. Agents (Astromech, Captain, Medic, etc.) consume the
//     interface in their claim-time prompt assembly.
//   - Later: a gRPC backing for multi-tenant Force operation.
//
// Pattern P16 (audit_pattern_p16_clients_interfaces_test.go) enforces
// that production agent code references the Client interface, not any
// concrete struct type. Construction is via NewInProcess / NewGRPC /
// NewMock factory functions only.
package capabilities

import (
	"context"
	"errors"
)

// Client is the contract between agents and the capability-profile
// service. The interface is small on purpose — the D1 design carves it
// to the four operations agent code actually performs at claim time.
type Client interface {
	// LoadProfile returns the capability profile for the given agent
	// name (e.g. "Astromech-1", "Yoda"). Returns ErrProfileNotFound if
	// the profile does not exist.
	LoadProfile(ctx context.Context, agentName string) (*Profile, error)

	// AllowedTools returns the Claude CLI --allowedTools list for the
	// given agent. Empty slice (not nil) means "no allowlist; defer to
	// disallow list."
	AllowedTools(ctx context.Context, agentName string) ([]string, error)

	// DisallowedTools returns the Claude CLI --disallowedTools list
	// for the given agent. Empty slice means "no denylist."
	DisallowedTools(ctx context.Context, agentName string) ([]string, error)

	// MCPConfigPath returns the absolute path to the per-agent MCP
	// server config file. Returns ErrProfileNotFound when the agent
	// has no profile; returns "" with nil error when the agent has a
	// profile but no MCP servers configured.
	MCPConfigPath(ctx context.Context, agentName string) (string, error)
}

// Profile is the in-memory representation of one agent's capability
// profile. Field shapes are provisional until D1 freezes the on-disk
// schema; agents that read individual fields should expect minor
// renames.
type Profile struct {
	AgentName       string
	AllowedTools    []string
	DisallowedTools []string
	MCPConfigPath   string

	// SystemPromptFragment is the chunk of CLAUDE.md (or equivalent)
	// that gets injected into the agent's prompt at claim time. D1
	// owns the formatting contract.
	SystemPromptFragment string
}

// Sentinel errors. Compare with errors.Is.
var (
	// ErrProfileNotFound — no profile exists for the requested agent.
	ErrProfileNotFound = errors.New("capabilities: profile not found")

	// ErrNotImplemented — the in-process backing's stub methods return
	// this until D1 fills in real bodies. Callers should not gate on
	// this error in production paths; it exists so D0 ships a real
	// interface contract instead of a permissive nop.
	ErrNotImplemented = errors.New("capabilities: not implemented (D1 deliverable)")
)
