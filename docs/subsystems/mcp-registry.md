---
audience: agent
scope: MCP server allowlist + per-agent injection — how Claude's MCP toolchain is gated.
owner: D1 + Fix #8e
last_reviewed: 2026-05-05
subsystem: mcp-registry
type: subsystem-doc
---

# MCP server registry

Force agents shell out to `claude -p` (see [cli-shelling.md](cli-shelling.md)), which preserves access to the operator's MCP toolchain. The MCP registry is what controls **which** MCP servers each agent can see — it is the per-agent gating layer on top of the operator's global Claude config.

## Overview

Every Claude CLI invocation receives three tool-related flags from the agent's [capability profile](capability-profiles.md):

- `--allowedTools` (auto-approve hint, NOT enforcement per Fix #8e).
- `--disallowedTools` (the hard restriction).
- `--mcp-config` (path to the per-agent MCP config the loader synthesizes).

The MCP config is a JSON file that lists exactly the MCP servers this agent is allowed to talk to, deduplicated against the operator's environment. Agents that don't need MCP (Senate, BoS, ISB) get an empty config and run without any MCP server reachable.

## Components

- **`internal/capabilities/mcp.go`** — synthesizes `--mcp-config` JSON from the profile + operator env.
- **`agents/capabilities/<agent>.yaml`** — per-agent `mcp.servers:` list naming which MCP servers are exposed.
- **`agents/capabilities/REGISTRY.yaml`** — fleet-wide vocabulary; every MCP server name must appear here first.
- **`agents/capabilities/.forceblocklist.yaml`** — global never-allowed denylist (Slack-write namespace, Confluence-write tools, destructive Jira/Sonar ops). Applies regardless of agent grant.
- **`internal/audittools/audit_pattern_p13_*`** — Pattern P13 enforces that every Claude call site sources its MCP config from the profile.

## Invariants

1. **Per-agent MCP config is mandatory.** A Claude call site cannot pass an operator-global MCP config; it must use `profile.MCPConfigArg()`.
2. **REGISTRY-gated server names.** Profiles cannot reference an MCP server not declared in `REGISTRY.yaml`.
3. **Blocklist applies even when granted.** A profile granting `mcp__plugin_slack_slack__slack_send_message` fails to load because the blocklist denies it.
4. **Loader fails closed.** Missing operator env entry for a granted server → error; agent does not start. There is no silent "skip the missing one."
5. **No silent fallback to operator-global.** If the profile declares zero MCP servers, the agent runs with `--mcp-config /dev/null` (or equivalent empty), not with the operator's full config.

## Configuration

Profile shape (`agents/capabilities/investigator.yaml`):

```yaml
agent: investigator
description: Read-only research agent; broad MCP read surface.
builtin_tools:
  - Read
  - Glob
  - Grep
  - Bash      # narrow allowlist via force-bash-guard
mcp:
  servers:
    - mcp__plugin_dev-tools_atlassian      # Jira/Confluence read
    - mcp__plugin_glean_glean               # Glean search
    - mcp__plugin_dev-tools_datadog-mcp     # Datadog
    - mcp__plugin_databricks-sql_databricks-sql-prod   # read-only SQL
```

`REGISTRY.yaml` lists every legal MCP server name with a one-line rationale. Adding a new MCP server requires:

1. Operator wires the server into their global Claude env.
2. Author appends to `REGISTRY.yaml` with rationale.
3. Each agent that needs the server adds it to its `mcp.servers:` list.
4. `make test` confirms `TestPattern_P13` still passes.

`.forceblocklist.yaml` covers (verbatim):

- Slack write namespace: `slack_send_message`, `slack_schedule_message`, `slack_send_message_draft`, `slack_create_canvas`, `slack_update_canvas`.
- Confluence write tools: `createConfluencePage`, `updateConfluencePage`, comment creators.
- Destructive Jira ops: terminal-state transitions, mass edits.
- Destructive Sonar ops: admin-tier mutations.

## Operator surface

```bash
force agents capabilities show investigator    # render effective profile + MCP config preview
force agents capabilities lint                 # validate every YAML against registry + blocklist
force agents capabilities mcp-diff             # show registry vs operator env reachability
```

If an agent fails to start with `mcp server X not reachable` it means the operator's Claude env doesn't have that server configured; either remove the grant from the profile or wire the server.

When an agent attempts to call an MCP tool it isn't granted, Claude returns `tool not available` inline and the failure is captured into `LLMCallTranscripts` (Pattern P31).

## See also

- [`capability-profiles.md`](capability-profiles.md) — sibling layer for built-in tools.
- [`cli-shelling.md`](cli-shelling.md) — how the synthesized config flows into `claude -p`.
- `security.md` (planned) — capability profiles in the broader security posture.
- [`../CLAUDE.md`](../../CLAUDE.md) — Per-agent capability profiles invariant.
