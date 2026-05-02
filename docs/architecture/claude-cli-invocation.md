# Claude CLI invocation layering

This document is the durable reference for how Force agents invoke
`claude -p`, what working directory each invocation runs from, what
`CLAUDE.md` files Claude Code auto-loads as a result, and where the
fleet's coordination invariants reach the model. It reflects the
post-D4 state of the world: D3 Phase 1 (FleetRules bootstrap,
`render_to`-categorised rule registry, per-agent rule injection,
auto-rendered ≤ 20 KB CLAUDE.md) is shipped; D3 Phase 6B added
the `claude.CallWithTranscript*` capture wrapper layered on top of
`AskClaudeCLI` / `RunCLI*` so every LLM call lands a
`LLMCallTranscripts` row (Pattern P31); D4 added the Bureau of
Standards (BoS) and Imperial Security Bureau (ISB) — both are
*non-LLM* commit-time reviewers (pure Go AST / regex / scanner-library
checks, no `claude -p` call) — and the Senate, a repo-scoped
LLM-backed advisory layer with an empty tool surface.

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
| Astromech | `CallWithTranscriptStreaming` (→ `RunCLIStreamingContext`) | `worktreeDir` | target-repo worktree (`.force-worktrees/<repo>/<agent>/`) |
| Astromech (foreground) | direct `exec.CommandContext` with `cmd.Dir = worktreeDir` | — | target-repo worktree |
| Captain | `CallWithTranscript` | `""` | force-orchestrator/ (daemon CWD) |
| Captain proposal judge | `CallWithTranscript` | `""` | force-orchestrator/ |
| Jedi Council | `CallWithTranscript` | `""` | force-orchestrator/ |
| Council critic (adversarial) | `CallWithTranscript` | `""` | force-orchestrator/ |
| Medic | `CallWithTranscript` | `""` | force-orchestrator/ |
| Medic-CI | `CallWithTranscript` | `""` | force-orchestrator/ |
| Medic critic (adversarial) | `CallWithTranscript` | `""` | force-orchestrator/ |
| Chancellor | `CallWithTranscript` | `""` | force-orchestrator/ |
| ConvoyReview | `CallWithTranscript` | `""` | force-orchestrator/ |
| ConvoyReview critic (adversarial) | `CallWithTranscript` | `""` | force-orchestrator/ |
| Diplomat | `CallWithTranscript` | `""` | force-orchestrator/ |
| PR-review-triage | `CallWithTranscript` | `""` | force-orchestrator/ |
| Commander | `CallWithTranscriptStreaming` | `""` | force-orchestrator/ |
| Investigator | `CallWithTranscriptOneShot` | `""` | force-orchestrator/ |
| Auditor | `CallWithTranscriptOneShot` | `""` | force-orchestrator/ |
| Boot | `CallWithTranscript` | `""` | force-orchestrator/ |
| Librarian | `CallWithTranscript` | `""` | force-orchestrator/ |
| Memory rerank | `CallWithTranscript` | `""` | force-orchestrator/ |
| Engineering Corps (experiment_author / metric_author / promotion_author / demotion_author / experiment_monitor / holdout_monitor) | `CallWithTranscript` | `""` | force-orchestrator/ |
| Pilot (FindPRTemplate only) | `CallWithTranscript` | `""` | force-orchestrator/ |
| Narrative renderer (Phase 6A.7) | `CallWithTranscript` (Haiku) | `""` | force-orchestrator/ |
| Briefing renderer (Phase 6A.10) | `CallWithTranscript` (Haiku) | `""` | force-orchestrator/ |
| Learning panel renderer (Phase 6B.12) | `CallWithTranscript` (Haiku) | `""` | force-orchestrator/ |
| Replay (Phase 6B.7) | `CallWithTranscript` (Haiku) | `""` | force-orchestrator/ |
| Ask handler (Phase 6B.10) | `CallWithTranscript` (Haiku, read-only tools) | `""` | force-orchestrator/ |
| Retro generator (Phase 6B.13) | `CallWithTranscript` (Haiku) | `""` | force-orchestrator/ |
| Transcript archive (Phase 6B.3) | `CallWithTranscript` (optional Haiku summarisation) | `""` | force-orchestrator/ |
| Model-availability dog (D3 fix-loop-2 ε.4) | `CallWithTranscript` (one-shot ping) | `""` | force-orchestrator/ |
| Senate (D4 Phase 3 — repo-scoped Senator review) | `CallWithTranscript` (Haiku-gated; deterministic-stub fallback) | `""` | force-orchestrator/ |

**Non-LLM reviewers (D4).** BoS (`internal/agents/bos.go` + capability profile `agents/capabilities/bos.yaml`) and ISB (`internal/agents/isb.go` + capability profile `agents/capabilities/isb.yaml`) are commit-time reviewers that do **not** invoke `claude -p`. Their work is pure Go AST / regex / scanner-library analysis against the post-commit diff. Their capability profiles grant minimal tool surfaces (Read / Grep / Glob / Bash) for parity with the foreground `force bos review --file <path>` use case — the in-process reviewer doesn't actually shell out at runtime. They are listed here for completeness so the table is comprehensive; they sit outside the per-agent invocation matrix because there is no Claude invocation to characterise. The Senate (`agents/capabilities/senate.yaml`) is the LLM-backed counterpart and appears as a regular row above; its capability profile is `builtin_tools: []` — the Senator review is a pure-reasoning LLM call with no Read / Edit / Write / Bash surface, and the per-Senator context is assembled in-process from FleetRules + `SenateMemory` + `librarian.RecentCommitsDigest`.

The split is binary: only Astromech runs inside a target-repo worktree.
Every other Claude-invoking agent inherits the daemon's CWD.

**Live Haiku gating.** Every Phase-6 renderer (narrative / briefing /
learning-panel / replay / ask / retro / transcript-archive) plus the
model-availability dog gates its live `CallWithTranscript` invocation
behind `liveHaikuDisabled()` (env flag `LIVE_HAIKU_DISABLED`).
Production daemons leave the flag unset; tests pin to `"1"` via the
package-level `TestMain` so the deterministic synth fallback runs
instead of burning real Haiku tokens. The model-availability dog
additionally honours `FORCE_MODEL_AVAILABILITY_LIVE_PROBE=0` as a
per-dog operator kill-switch (e.g. during a known Anthropic outage).

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

**Current state (post-D3, fix-loop-2 closed):**
- All agents (astromechs included) receive fleet rules via a
  FleetRules-rendered `--append-system-prompt` content block scoped
  to that agent (`agent_scope='all' OR agent_scope='<agent>'`).
  Wiring lives in `internal/store/fleet_rules.go` (`AppendFleetRulesToPrompt`,
  `InjectFleetRulesAgentPrompt`); call-site fan-out covers Captain,
  Council, Medic, Chancellor, ConvoyReview, PR-review-triage,
  Astromech, Pilot, Engineering Corps. Fail-open: a missing
  FleetRules table or query error logs but does not stop agent
  startup.
- `force-orchestrator/CLAUDE.md` is **auto-rendered** from FleetRules
  rows where `render_to='claude-md-file'` (currently 11 rows; 6,616
  bytes rendered, well under the 20 KB hard cap enforced by Pattern
  P17 + the pre-commit hook). It changes meaning: it is now the doc
  that applies when an *agent operates on Force itself* — review
  agents auto-load it from their CWD; astromechs do not (their CWD
  is a target-repo worktree).
- Review agents still pay the `force-orchestrator/CLAUDE.md` token
  cost on every call (now ~6.6 KB, down from the pre-D3 ~46 KB);
  astromechs receive the same FleetRules content via the
  `--append-system-prompt` injection path, so coordination rules
  reach them without auto-load.
- Astromechs continue to auto-load target-repo `CLAUDE.md` from their
  worktree CWD as developer guidance. The asymmetry on the
  *target-repo* surface is preserved by design — Force does not
  publish rules into target repos.

**Pattern enforcement layer (D3 Phase 6B + polish-iter2):**
- `TestPattern_P31_AllLLMCallsCaptured` (audit) walks every Claude
  CLI call site and rejects un-wrapped `AskClaudeCLI` /
  `RunCLIStreamingContext` invocations — every prod LLM call MUST
  route through one of the `CallWithTranscript*` helpers in
  `internal/claude/transcript.go`, which writes a redacted
  `LLMCallTranscripts` row at write time.
- `TestPattern_P13_CapabilityProfiles` (AST) rejects hardcoded tool
  literals at any call site — every site sources `--allowedTools` /
  `--disallowedTools` / MCP config from
  `capabilities.LoadProfile(agentName)`. Pattern P13 was extended
  during fix-loop-2 to also cover the model-availability dog and
  the seven Phase-6 renderers.

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
- D4 BoS / ISB / Senate plan: `docs/roadmap.md` § "Deliverable 4 —
  Bureau of Standards + Imperial Security Bureau + Senate"; closure
  in `docs/closures/DELIVERABLE-4-CLOSURE.md`. BoS rule pack:
  `internal/bos/rules/bos_001..011.go`; ISB rule pack:
  `internal/isb/rules/isb_001..010.go`; Senate package:
  `internal/agents/senate.go`. Capability profiles: `bos.yaml`,
  `isb.yaml`, `senate.yaml`.
