---
audience: both
scope: Fleet Captain — plan-coherence gate that sits between each astromech commit and the Jedi Council, keeping a convoy plan valid as implementation diverges.
owner: D13
last_reviewed: 2026-05-05
---

# Fleet Captain — Plan Coherence Gate

## Role

The Captain is **not** a code reviewer — it is a plan-coherence check. After every astromech commit, a Captain agent inspects the convoy's full state (already-merged tasks, pending tasks, current diff) and the new commit's diff, then decides whether the convoy plan needs no adjustment, downstream rewrites, or an escalation. The Captain is biased strongly toward approval; minor stylistic deviation is fine, only genuine plan-incoherence triggers a non-approve ruling. Only convoys created by Commander run the Captain; ad-hoc tasks added with `force add-task` go straight to the Jedi Council.

Roster: Captain-Rex, Captain-Wolffe, Captain-Bly, Captain-Gree, Captain-Ponds.

## Responsibilities

- Claim `CaptainReview` bounties enqueued after each astromech commit.
- Load convoy state + diff and call Claude (via `claude -p`) for the plan-coherence ruling.
- Emit one of: `Approve` (forward to Council), `UpdateDownstream` (rewrite pending task payloads to match the implementation), `InsertNew` (add tasks Commander didn't anticipate), `Reject` (return to astromech for rework), or `Escalate`.
- The LLM-judge layer (`captain_proposal_judge.go`) checks the captain's reasoning against cited evidence and emits a `consistent | inconsistent | ambiguous` verdict; inconsistent rulings are retried with a critic note (per Pattern P31 / D3 fix-loop-1 β1).

## Capability profile

Profile: [`agents/capabilities/captain.yaml`](../../agents/capabilities/captain.yaml). The judge layer additionally uses [`agents/capabilities/captain-proposal-judge.yaml`](../../agents/capabilities/captain-proposal-judge.yaml). Loaded via `capabilities.LoadProfile("captain")` in `internal/agents/captain.go`.

## Key files

- `internal/agents/captain.go` — `SpawnCaptain(ctx, db, name)` claim loop and ruling emission.
- `internal/agents/captain_proposal_judge.go` — LLM-judge layer that validates captain reasoning vs cited AT-IDs / FleetRule texts.
- `internal/agents/captain_scope_guard_test.go` — scope-guard covering convoy-only Captain involvement.
- `internal/agents/captain_proposal_emit_test.go`, `internal/agents/captain_proposal_judge_test.go` — judge-layer + emission unit tests.
- `internal/agents/captain_test.go` — primary captain coverage.
- `agents/capabilities/captain.yaml`, `agents/capabilities/captain-proposal-judge.yaml` — capability profiles.

## Tests

- `internal/agents/captain_test.go`, `captain_scope_guard_test.go`, `captain_proposal_emit_test.go`, `captain_proposal_judge_test.go` — full captain + judge coverage.
- `internal/audittools/audit_pattern_p13_capability_profiles_test.go` — capability-profile invariant.
- `internal/audittools/audit_pattern_p31_llm_transcripts_test.go` — judge-layer transcripts.
- `internal/audittools/audit_pattern_p23_proposer_write_discipline_test.go` — Captain's downstream-rewrite path stays within the proposer write surface.

## See also

- [`docs/agents/commander.md`](commander.md) — produces the original plan the Captain validates.
- [`docs/agents/council.md`](council.md) — Captain forwards approved commits here.
- [`docs/architecture/claude-cli-invocation.md`](../architecture/claude-cli-invocation.md) — invocation layering.
