---
audience: both
scope: Boot Agent — lightweight Claude-backed triage agent the Inquisitor calls when a stall is detected.
owner: D13
last_reviewed: 2026-05-05
---

# Boot Agent — Stall Triage

## Role

Boot is a lightweight, single-call triage agent invoked by the Inquisitor when a task is detected as stalled past `stallEscTimeout`. Boot inspects the task details, agent history, and recent error logs, then returns one of four verdicts: `RESET`, `ESCALATE`, `WARN`, or `IGNORE`. The Inquisitor acts on the verdict — Boot itself only reasons.

| Verdict | Action |
|---|---|
| `RESET` | Return the task to `Pending` — the agent is likely hung. |
| `ESCALATE` | Create an escalation — needs human review. |
| `WARN` | Log a warning but take no action yet. |
| `IGNORE` | The agent is still making progress. |

## Responsibilities

- Receive a stall-triage request from the Inquisitor (in-process call, not a claim loop).
- Assemble a small prompt containing the task payload, agent history, and recent error excerpts.
- Call Claude with the Boot capability profile and parse the verdict + rationale.
- Return the verdict to the Inquisitor for action.

## Capability profile

Profile: [`agents/capabilities/boot.yaml`](../../agents/capabilities/boot.yaml). Loaded by the Inquisitor at spawn time and passed in for each Boot call (no per-call profile reload).

## Key files

- `internal/agents/boot.go` — Boot triage entrypoint and verdict parser.
- `internal/agents/boot_test.go` — unit coverage for each of the four verdicts and parsing failure paths.
- `internal/agents/inquisitor.go` — caller (the only caller).
- `agents/capabilities/boot.yaml` — capability profile.

## Tests

- `internal/agents/boot_test.go` — verdict parse + each verdict's downstream effect on the holocron.
- `internal/agents/claude_error_excerpt_test.go` — ensures the error-log slice Boot sees stays bounded.
- `internal/audittools/audit_pattern_p13_capability_profiles_test.go` — capability profile invariant.
- `internal/audittools/audit_pattern_p31_llm_transcripts_test.go` — Boot's LLM call writes a transcript.

## See also

- [`docs/agents/inquisitor.md`](inquisitor.md) — the only caller; runs every 5 minutes and decides when to invoke Boot.
- [`docs/agents/medic.md`](medic.md) — handles permanent failures Boot's `RESET` verdict cannot fix on retry.
