---
audience: both
scope: Fleet Medic — failure-triage agent that requeues, shards, or escalates permanently-failed tasks instead of dumping them straight on the operator.
owner: D13
last_reviewed: 2026-05-05
---

# Fleet Medic — Failure Triage

## Role

The Medic triages tasks that have permanently failed — exhausted all retry attempts or hit an unrecoverable infra failure. Instead of immediately escalating to the operator, the Medic examines the full attempt history, all council/captain rejection feedback, and the last git diff, then renders one of three verdicts:

| Verdict | Action |
|---|---|
| `requeue` | The task is valid but needed clearer guidance. Resets to `Pending` and mails astromechs with specific corrective guidance. |
| `shard` | The task was too broad for a single agent. Cancels the original and inserts 2–5 focused sub-tasks into the same convoy. |
| `escalate` | The failure reveals an architectural ambiguity, missing dependency, or problem a coding agent cannot resolve. Creates an escalation and mails the operator. |

The Medic is biased toward `requeue` or `shard` — `escalate` is a last resort. On Claude failure the Medic escalates directly without looping. The Medic also handles `CIFailureTriage` under the PR flow — classifying CI failures as Flaky / RealBug / Environmental / BranchProtection / Unfixable.

Roster: Bacta, Kolto, Stim.

## Responsibilities

- Claim permanently-failed `CodeEdit` tasks for triage.
- Claim `CIFailureTriage` bounties under the PR flow; classify and either retrigger or spawn a fix task on the astromech branch.
- Call Claude with the Medic capability profile; on failure, escalate directly.
- Apply the verdict — requeue with corrective mail, shard into sub-tasks, or escalate.
- Honor the per-repo CI circuit breaker: 5 Environmental failures in 1 hour pauses sub-PR creation for that repo for 30 minutes.
- The adversarial `medic-critic` profile provides a second-pass adversarial check on close calls.

## Capability profile

Profile: [`agents/capabilities/medic.yaml`](../../agents/capabilities/medic.yaml). The CI-triage variant uses [`agents/capabilities/medic-ci.yaml`](../../agents/capabilities/medic-ci.yaml); the adversarial critic uses [`agents/capabilities/medic-critic.yaml`](../../agents/capabilities/medic-critic.yaml). Loaded via `capabilities.LoadProfile("medic")` in `internal/agents/medic.go`.

## Key files

- `internal/agents/medic.go` — `SpawnMedic(ctx, db, name)` claim loop and verdict-application flow.
- `internal/agents/adversarial_wiring.go` — wires the medic-critic adversarial second pass.
- `agents/capabilities/medic.yaml`, `agents/capabilities/medic-ci.yaml`, `agents/capabilities/medic-critic.yaml` — capability profiles.

## Tests

- `internal/agents/adversarial_wiring_test.go`, `internal/agents/adversarial_hotpath_test.go` — adversarial critic + hot-path coverage; the medic-critic shape is exercised here.
- `internal/audittools/audit_pattern_p13_capability_profiles_test.go` — capability profile invariant.
- `internal/audittools/audit_pattern_p31_llm_transcripts_test.go` — Medic LLM calls write transcripts.
- `internal/audittools/audit_silent_failures_test.go` — Medic Claude-failure path escalates directly; no silent log+continue.

## See also

- [`docs/agents/council.md`](council.md) — upstream gate; tasks the Council permanently fails land in the Medic's queue.
- [`docs/agents/diplomat.md`](diplomat.md) — coordinates the PR-flow CI signals the Medic triages.
- [`docs/agents/inquisitor.md`](inquisitor.md) — sweeps stalled tasks to permanent-failed state when retries exhaust.
