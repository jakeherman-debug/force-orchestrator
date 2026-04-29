# Claude CLI invocation layering

This document is the durable reference for how Force agents invoke
`claude -p`, what working directory each invocation runs from, what
`CLAUDE.md` files Claude Code auto-loads as a result, and where the
fleet's coordination invariants reach the model. It covers the
state of the world today (post-D1 T0-1) and how that state changes
once D3 Phase 1 ships FleetRules + per-agent rule injection.

## The two invocation patterns

Every agent that calls Claude routes through `internal/claude/claude.go`.
There are exactly two entry-point shapes:

1. **`AskClaudeCLI` / `AskClaudeCLIContext`** — convenience wrappers for
   non-worktree calls. They feed `dir = ""` to the runner, and
   `defaultCLIRunner` skips the `cmd.Dir = dir` assignment when `dir`
   is empty. Result: the spawned `claude` subprocess inherits the
   daemon's CWD (the orchestrator repo root,
   `/Users/jake.herman/code/force-orchestrator`).

2. **`RunCLI` / `RunCLIStreaming` / `RunCLIStreamingContext`** — the
   explicit-directory variants. The caller passes a `dir` argument,
   and the runner sets `cmd.Dir = dir` before invoking the subprocess.
   Astromech is the only production caller that uses this with a
   non-empty `dir`, pointing at its per-agent worktree
   (`.force-worktrees/<repo>/<agent>/`).

Reference call sites:

- `internal/claude/claude.go:201-205` — `defaultCLIRunner` only sets
  `cmd.Dir` when `dir != ""`.
- `internal/claude/claude.go:300-302` — `RunCLIStreamingContext` sets
  `cmd.Dir = dir` for the stream-json path.
- `internal/claude/claude.go:396-409` — `AskClaudeCLI` /
  `AskClaudeCLIContext` always pass `""` to the runner.

## Per-agent invocation table

| Agent | Entry point | `dir` arg | Effective CWD when claude runs |
|---|---|---|---|
| Astromech | `RunCLIStreamingContext` | `worktreeDir` | target-repo worktree (`.force-worktrees/<repo>/<agent>/`) |
| Astromech (foreground) | direct `exec.CommandContext` with `cmd.Dir = worktreeDir` | — | target-repo worktree |
| Captain | `AskClaudeCLI` | `""` | force-orchestrator/ (daemon CWD) |
| Jedi Council | `AskClaudeCLI` | `""` | force-orchestrator/ |
| Medic | `AskClaudeCLI` | `""` | force-orchestrator/ |
| Medic-CI | `AskClaudeCLI` | `""` | force-orchestrator/ |
| Chancellor | `AskClaudeCLI` | `""` | force-orchestrator/ |
| ConvoyReview | `AskClaudeCLI` | `""` | force-orchestrator/ |
| Diplomat | `AskClaudeCLI` | `""` | force-orchestrator/ |
| PR-review-triage | `AskClaudeCLI` | `""` | force-orchestrator/ |
| Commander | `RunCLIStreaming` (no ctx) | `""` (unset) | force-orchestrator/ |
| Investigator | `RunCLI` | `""` | force-orchestrator/ |
| Auditor | `RunCLI` | `""` | force-orchestrator/ |
| Boot | `AskClaudeCLI` | `""` | force-orchestrator/ |
| Librarian | `AskClaudeCLI` | `""` | force-orchestrator/ |
| Memory rerank | `AskClaudeCLI` | `""` | force-orchestrator/ |

The split is binary: only Astromech runs inside a target-repo worktree.
Every other agent inherits the daemon's CWD.

## What CLAUDE.md files Claude auto-loads

Claude Code's documented behaviour is to walk up from CWD looking for
`CLAUDE.md` files, plus loading `~/.claude/CLAUDE.md` (user-level) and
project memory irrespective of CWD.

For review agents (CWD = `force-orchestrator/`):
- `~/.claude/CLAUDE.md` — operator's personal memory
- project memory (under `~/.claude/projects/...`)
- `force-orchestrator/CLAUDE.md` — the Force fleet invariants doc

For astromechs (CWD = target-repo worktree):
- `~/.claude/CLAUDE.md`
- project memory
- `<target-repo>/CLAUDE.md` (when present) — the target's developer
  guidance, NOT Force's
- the daemon's `force-orchestrator/CLAUDE.md` is **not** auto-loaded;
  it sits above the worktree's parent chain only if the worktree path
  walks back to a directory that contains it, which it does not in
  the standard `.force-worktrees/...` layout

## Where Force's coordination invariants reach the agent

**Today (post-D1 T0-1):**
- Review agents: invariants reach them via auto-loaded
  `force-orchestrator/CLAUDE.md`. No `--append-system-prompt` is in
  use.
- Astromechs: invariants reach them via in-process system-prompt
  composition (`AstromechSystemPrompt` const + per-task context blocks
  in `runAstromechTask`). Astromechs do **not** see
  `force-orchestrator/CLAUDE.md` content, so any rule that lives
  exclusively there is invisible to them.

This asymmetry is load-bearing: review agents pay the
`force-orchestrator/CLAUDE.md` token cost on every call (currently
~46 KB), while astromechs do not.

**After D3 Phase 1:**
- All agents (astromechs included) receive fleet rules via a
  FleetRules-rendered `--append-system-prompt` content block
  scoped to that agent (`agent_scope='all' OR agent_scope='<agent>'`).
  See `docs/roadmap.md` § "Phase 1 — Foundations + Rule Audit" for
  the full design.
- `force-orchestrator/CLAUDE.md` shrinks to ≤ 20 KB and changes
  meaning: it becomes the doc that applies when an *agent operates on
  Force itself* (build/maintenance work in this repo), not the
  authoritative source of fleet-coordination rules.
- Astromechs continue to auto-load target-repo `CLAUDE.md` from their
  worktree CWD as developer guidance. The asymmetry on the
  *target-repo* surface is preserved by design — Force does not
  publish rules into target repos.

## Security implication: target-repo CLAUDE.md is a prompt-injection surface

Because Claude Code auto-loads `<target-repo>/CLAUDE.md` from the
worktree CWD, and astromechs are the only agents that operate inside
that CWD, target-repo CLAUDE.md is a prompt-injection surface for
astromechs only.

It is **outside Fix #8.5's `<user_content>` sentinel discipline** —
Claude Code reads the file directly during context assembly, before
the orchestrator-supplied system prompt reaches the model. Force
cannot wrap target CLAUDE.md in `<user_content>` tags after the fact;
the file is loaded by Claude Code, not by Force.

The mitigation lives in two layers:

1. **Static rail (capability profile).** Astromech's tool grant is
   declared in `agents/capabilities/astromech.yaml` and enforced via
   `--disallowedTools`. A target CLAUDE.md instruction to use a tool
   absent from the profile cannot be followed because the tool is
   mechanically removed from Claude's catalog.
2. **Runtime rail (system-prompt clause).** `AstromechTargetCLAUDEMDClause`
   in `internal/agents/astromech.go` is appended to every astromech
   system prompt. It frames target CLAUDE.md as advisory dev guidance
   and tells the model how to surface conflicts to the operator
   (via the `[TARGET_CLAUDE_MD_OBSERVATION: ...]` signal, which is
   itself a reserved token in `llmSignalTokens` — Force's own LLMs
   cannot smuggle the same token into a downstream payload).

The clause is added ONLY to astromech, because it is the only agent
that auto-loads target CLAUDE.md. Adding it to review agents would
be confusing noise.

## Cross-references

- Capability profiles: `agents/capabilities/<agent>.yaml` (one per
  agent, plus `REGISTRY.yaml` and `.forceblocklist.yaml`)
- Claude CLI dispatch: `internal/claude/claude.go`
  (`defaultCLIRunner`, `RunCLIStreamingContext`, `AskClaudeCLI*`)
- Astromech system prompt: `internal/agents/astromech.go`
  (`AstromechSystemPrompt`, `AstromechTargetCLAUDEMDClause`,
  `runAstromechTask`)
- LLM-boundary discipline: `internal/agents/llm_boundary.go`
  (`promptInjectionClause`, `WrapUserContent`, `llmSignalTokens`,
  `SanitizeLLMPayload`)
- D3 Phase 1 plan: `docs/roadmap.md` § "Phase 1 — Foundations + Rule
  Audit"
