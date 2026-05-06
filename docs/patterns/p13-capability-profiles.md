---
audience: agent
scope: Every Claude CLI call site sources its tool args from a *capabilities.Profile.
owner: D13
last_reviewed: 2026-05-05
title: Pattern P13 — Capability profiles
type: pattern-doc
pattern: P13
---

# Pattern P13 — Capability profiles

## Rationale

Per-agent capability profiles are the load-bearing layer that limits
what each Claude session can touch. A hardcoded tool-args literal at a
call site bypasses the YAML profile + blocklist, defeating the purpose
of `capabilities.LoadProfile`. Pattern P13 enforces, at CI time, that
every entry-point Claude call sources its `--allowedTools`,
`--disallowedTools`, and `--mcp-config` arguments from the loader.
Originates in D1 Track T0-1.

P13 graduates to a BoS commit-time rule when D4 ships, alongside the
P16 cross-agent service interface rule.

## What it checks

Three phases:

1. AST walk over `cmd/` and `internal/` for every CallExpr to
   `AskClaudeCLI`, `AskClaudeCLIContext`, `RunCLI`, `RunCLIStreaming`,
   or `RunCLIStreamingContext`. The three tool-arg slots (positions
   depend on the function — see `profileToolArgs`) must each be either:
   - a method call on a `*capabilities.Profile`
     (`AllowedToolsArg` / `DisallowedToolsArg` / `MCPConfigArg`), or
   - one of the allowed local-variable names: `mcpConfig`,
     `allowedTools`, `disallowedTools`.
2. Every `LoadProfile("<name>")` (or `mustLoadCapProfile(t, "<name>")`)
   string-literal name must correspond to a YAML file under
   `agents/capabilities/`.
3. Every YAML profile in `agents/capabilities/` must validate via
   `capabilities.LoadProfile(name)` (delegates to the loader's own
   blocklist + namespace checks).

`p13Allowlist` carries two structural exemptions
(`internal/claude/claude.go`, `internal/clients/librarian/summarize_call.go`);
each entry must have a rationale ≥20 chars matching one of the
descriptors `loader / internals / helper / classifier / contract /
implementation / wraps / ARE the / profile-sourced / from the caller`
(enforced by `TestPattern_P13_AllowlistReasonsTruthful`).

## How it fails

```
Pattern P13 (D1 T0-1): N production Claude call site(s) source tool args from a non-profile expression. Use *capabilities.Profile.AllowedToolsArg() / .DisallowedToolsArg() / .MCPConfigArg() instead:
  internal/agents/foo.go:42 — AskClaudeCLI
      non-profile tool arg in AskClaudeCLI: literal "Edit,Write"
...
Fix: load the profile via capabilities.LoadProfile("<agent>") at Spawn time, then pass profile.AllowedToolsArg() / profile.DisallowedToolsArg() / mcpConfig (from profile.MCPConfigArg()) to the Claude call.
```

Typical violating snippet:

```go
res, err := claude.AskClaudeCLI(sys, prompt, "Edit,Write", "", "", 5)
```

## How to fix

Load a profile at Spawn time and pass its accessor methods:

```go
profile, err := capabilities.LoadProfile("captain")
if err != nil { return err }
mcpConfig, _ := profile.MCPConfigArg()
res, err := claude.AskClaudeCLIContext(
    ctx, sys, prompt,
    profile.AllowedToolsArg(),
    profile.DisallowedToolsArg(),
    mcpConfig,
    5,
)
```

If the agent is new, also add `agents/capabilities/<name>.yaml` and an
audit comment in `internal/store/fleet_rules_audit.go` if applicable.

## Test reference

- File: `internal/audittools/audit_pattern_p13_capability_profiles_test.go`
- Core assertions:
  - `TestPattern_P13_CapabilityProfiles` (lines 92–232)
  - `TestPattern_P13_AllowlistReasonsTruthful` (lines 443–476)
- Helpers: `claudeCallName`, `profileToolArgs`,
  `p13ArgIsProfileSourced`, `collectLoadProfileNames`.

## See also

- [P16 — Cross-agent service interfaces](p16-clients-interfaces.md)
- [P31 — LLM transcripts captured](p31-llm-transcripts.md)
- `agents/capabilities/REGISTRY.yaml` and `.forceblocklist.yaml`
- CLAUDE.md "Per-agent capability profiles" section.
