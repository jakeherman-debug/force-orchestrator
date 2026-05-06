---
audience: both
scope: Commander Cody — the planner agent that decomposes Feature tasks into per-repo CodeEdit subtasks and proposes a convoy.
owner: D13
last_reviewed: 2026-05-05
---

# Commander Cody — Planner

## Role

Commander is the planner. It claims `Feature` tasks, reads each registered repo's description and README to ground itself in the codebase, then decomposes the feature into concrete `CodeEdit` subtasks targeting specific repos with explicit `blocked_by` dependency ordering. The output is a *proposed* plan — Commander does not create the convoy directly; it writes a `ProposedConvoys` row and transitions the Feature to `AwaitingChancellorReview` so the Chancellor can resolve it against the rest of the in-flight plan space.

## Responsibilities

- Claim `Feature` bounties from `BountyBoard`.
- Read repo metadata + READMEs to inform repo selection per subtask.
- Emit a structured plan (`ProposedConvoys` row) — one task per atom of work, each tagged with the target repo and `blocked_by` predecessors.
- Mail the operator a summary of which repos each subtask targets when the Chancellor approves and the convoy materializes.
- React to `[SHARD_NEEDED]` signals from astromechs by re-claiming the offending task as a `Decompose` request and producing finer-grained subtasks.
- Mark the source `Feature` task `Completed` once the convoy is created.

Commander is one of the review agents that runs with daemon CWD = `force-orchestrator/`, so its prompt auto-loads the project `CLAUDE.md` directives (see [`docs/architecture/claude-cli-invocation.md`](../architecture/claude-cli-invocation.md)).

## Capability profile

Profile: [`agents/capabilities/commander.yaml`](../../agents/capabilities/commander.yaml). Loaded at spawn time via `capabilities.LoadProfile("commander")` in `internal/agents/commander.go`; the Claude CLI invocation sources `--allowedTools` / `--disallowedTools` / `--mcp-config` from the loaded profile (no hardcoded literals, per Pattern P13).

## Key files

- `internal/agents/commander.go` — `SpawnCommander(ctx, db, name)` claim loop and plan-emission flow.
- `internal/agents/commander_test.go` — unit coverage for plan shape, `[SHARD_NEEDED]` handling, and Feature-to-AwaitingChancellorReview transition.
- `internal/store/proposed_convoys.go` — write surface Commander uses for plan rows.
- `agents/capabilities/commander.yaml` — Commander's capability profile.

## Tests

- `internal/agents/commander_test.go` — happy path + decomposition + shard handoff.
- `internal/audittools/audit_pattern_p13_capability_profiles_test.go` — enforces that Commander's Claude CLI call sites source `--allowedTools` / `--disallowedTools` from `capabilities.LoadProfile`.
- `internal/audittools/audit_pattern_p23_proposer_write_discipline_test.go` — proposer write discipline (Commander writes `ProposedConvoys`, not `Convoys` directly).
- `internal/audittools/audit_pattern_p31_llm_transcripts_test.go` — every Commander LLM call writes a transcript via `claude.CallWithTranscript`.

## See also

- [`docs/agents/chancellor.md`](chancellor.md) — Commander's downstream gate; APPROVE/SEQUENCE/REJECT/MERGE rulings on the `ProposedConvoys` row.
- [`docs/agents/captain.md`](captain.md) — sibling review agent; Captain validates plan coherence after each commit.
- [`docs/architecture/claude-cli-invocation.md`](../architecture/claude-cli-invocation.md) — Claude CLI invocation layering (Commander runs with `force-orchestrator/` as CWD).
