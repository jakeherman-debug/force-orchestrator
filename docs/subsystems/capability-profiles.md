---
audience: both
scope: Per-agent YAML capability profiles — how every Claude-invoking agent's tool surface is constrained.
owner: D1 + Fix #8e
last_reviewed: 2026-05-05
subsystem: capability-profiles
type: subsystem-doc
---

# Capability profiles

Every agent that calls `claude` runs under a static, YAML-declared capability profile under `agents/capabilities/`. The profile is the mechanical rail for Claude's tool surface — Captain, Council, Chancellor, Medic, Investigator, Auditor, Boot, Librarian, Diplomat, Pilot, ConvoyReview, and PRReviewTriage **cannot call `Bash`** because their YAML doesn't grant it. Astromech and Medic-CI are the only agents that can.

## Overview

The fleet ships with one profile per agent plus two coordinating files:

- `agents/capabilities/<agent>.yaml` — per-agent grants (one per agent + `cli-jira.yaml` for the operator's `force add-jira` CLI).
- `agents/capabilities/REGISTRY.yaml` — fleet-wide vocabulary; every tool name an agent profile may grant must appear here first.
- `agents/capabilities/.forceblocklist.yaml` — never-allowed denylist (Slack-write namespace, Confluence-write tools, destructive Jira ops, destructive Sonar ops). Removing an entry requires explicit operator action with audit trail.

`capabilities.LoadProfile(agentName)` is the only ingress; it fails closed on missing YAML, unknown tool reference, or blocklisted grant. There is no silent fallback to "all tools."

The empirical Fix #8e finding: `--allowedTools` is an auto-approve hint in `--dangerously-skip-permissions` mode, **not enforcement**. `--disallowedTools` is the actual hard restriction. Profiles emit both, but only the second is the security boundary.

## Components

- **`internal/capabilities/`** — `LoadProfile`, profile-vs-registry validation, `AllowedToolsArg` / `DisallowedToolsArg` / `MCPConfigArg` accessors.
- **`agents/capabilities/<agent>.yaml`** — per-agent grants:
  - `astromech.yaml`, `captain.yaml`, `council.yaml`, `chancellor.yaml`, `medic.yaml`, `medic-ci.yaml`, `investigator.yaml`, `auditor.yaml`, `boot.yaml`, `librarian.yaml`, `diplomat.yaml`, `pilot.yaml`, `convoy-review.yaml`, `pr-review-triage.yaml`, `commander.yaml`, `bos.yaml`, `isb.yaml`, `senate.yaml`.
- **`agents/capabilities/REGISTRY.yaml`** — fleet-wide vocabulary (every tool name).
- **`agents/capabilities/.forceblocklist.yaml`** — global denylist.
- **Pattern P13** (`internal/audittools/audit_pattern_p13_capability_profiles_test.go`) — AST-based regression: every `claude.RunCLIStreamingContext` / `AskClaudeCLI` call site must source tool args from `capabilities.LoadProfile(agentName)`. Hardcoded literals are rejected.

## Invariants

1. **Profile is mandatory at every call site.** Pattern P13 walks AST nodes and rejects any Claude CLI call with hardcoded tool args.
2. **Loader fails closed.** Missing YAML / unknown tool / blocklisted grant → error; agent does not start.
3. **`--disallowedTools` is the hard restriction.** Per Fix #8e, `--allowedTools` alone is not enforcement.
4. **Blocklist overrides per-agent grants.** A profile granting a blocklisted tool fails to load.
5. **REGISTRY-gated vocabulary.** A profile referencing a tool not in `REGISTRY.yaml` fails to load. Adding a new tool requires explicitly extending the registry first.
6. **No silent "all tools."** There is no fallback path that yields `--allowedTools=*` if the YAML is incomplete.

## Configuration

Profile shape (`agents/capabilities/captain.yaml`):

```yaml
agent: captain
description: Plan-coherence reviewer; strict read-only diff inspection.
builtin_tools:
  - Read
  - Glob
  - Grep
mcp:
  servers: []        # captain doesn't need MCP
extends: []          # no inheritance
```

Astromech grants `Bash` plus the full target-repo edit toolchain; review agents do not.

`agents/capabilities/REGISTRY.yaml` lists every legal tool name with a one-line rationale; new entries require an audit-trail commit message and (per the architecture invariant in CLAUDE.md) a justification in `internal/store/fleet_rules_audit.go` if the rule needs to render to docs.

`.forceblocklist.yaml` covers:
- Slack write namespace (`mcp__plugin_slack_slack__slack_send_message`, `…schedule_message`, `…create_canvas`, `…update_canvas`).
- Confluence write tools (`createConfluencePage`, `updateConfluencePage`, comment creators).
- Destructive Jira ops (`transitionJiraIssue` to terminal states, mass-edit ops).
- Destructive Sonar ops (admin-tier mutations).

## Operator surface

```bash
force agents capabilities show astromech    # render effective profile + diff vs blocklist
force agents capabilities lint              # validate every YAML against registry + blocklist
force agents capabilities diff astromech captain   # compare two profiles
```

The dashboard's Agents tab surfaces each agent's effective tool count and any blocklist conflicts. Profile changes require restarting the daemon (profiles are loaded once at agent spawn).

When an agent attempts to call a tool the profile rejects, Claude returns a `tool not available` error inline. The error is captured into the task transcript via Pattern P31's `LLMCallTranscripts` substrate, so the failure is auditable post-hoc.

## See also

- `security.md` (planned) — capability profiles in the broader security posture.
- [`mcp-registry.md`](mcp-registry.md) — MCP server allowlist + injection.
- [`cli-shelling.md`](cli-shelling.md) — how profiles flow into the `claude -p` invocation.
- [`../../FIX-LOG.md`](../../FIX-LOG.md) — Fix #8e narrative on `--disallowedTools` enforcement.
