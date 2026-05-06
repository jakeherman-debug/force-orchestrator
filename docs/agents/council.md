---
audience: both
scope: Jedi Council — code-review agents that evaluate astromech commits against the stated task and either merge or send back rework.
owner: D13
last_reviewed: 2026-05-05
---

# Jedi Council — Code Reviewers

## Role

Council members watch for tasks in `AwaitingCouncilReview`, compute the git diff against the current default branch, and ask Claude to evaluate whether the diff *correctly and completely* accomplishes the stated task. The council prompt is structured to return JSON of shape `{"approved": bool, "feedback": string}`. Council also reads its inbox before reviewing so operator directives ("always require tests") are respected. The Council is the gate between commit and merge / draft-PR-on-the-ask-branch (depending on flow).

## Responsibilities

- Claim `AwaitingCouncilReview` bounties.
- Compute diff vs default (or vs the convoy's ask-branch under PR flow).
- Read inbox + load directives and call Claude with the Council profile.
- On **approve**: merge the branch (legacy flow) or open the sub-PR with auto-merge (PR flow), unblock dependent tasks, spawn a `WriteMemory` bounty for the Librarian, and mail the operator on whole-convoy completion.
- On **reject**: append feedback to the task payload, reset to `Pending`, mail the next astromech that picks it up, and increment the retry counter. After `maxRetries` rejections, the task is permanently failed and the Medic takes over.
- The adversarial Council critic (`council-critic` profile) provides a second-pass adversarial check on close calls.

## Capability profile

Profile: [`agents/capabilities/council.yaml`](../../agents/capabilities/council.yaml). The adversarial critic loads [`agents/capabilities/council-critic.yaml`](../../agents/capabilities/council-critic.yaml). Both are wired in via `capabilities.LoadProfile` in `internal/agents/jedi_council.go` / `internal/agents/adversarial_wiring.go`.

## Key files

- `internal/agents/jedi_council.go` — `SpawnJediCouncil(ctx, db, cfg)` and the review claim loop.
- `internal/agents/jedi_council_test.go` — primary unit coverage.
- `internal/agents/adversarial_wiring.go` — wires the council-critic adversarial second pass.
- `internal/agents/adversarial_hotpath.go` — hot-path invocation of the critic on close calls.
- `agents/capabilities/council.yaml`, `agents/capabilities/council-critic.yaml` — capability profiles.

## Tests

- `internal/agents/jedi_council_test.go` — happy-path approve + reject + retry-counter.
- `internal/agents/adversarial_wiring_test.go`, `internal/agents/adversarial_hotpath_test.go` — adversarial critic wiring + hot-path.
- `internal/audittools/audit_pattern_p13_capability_profiles_test.go` — capability profile invariant.
- `internal/audittools/audit_pattern_p31_llm_transcripts_test.go` — Council LLM calls produce transcripts.
- `internal/audittools/audit_pattern_p32_git_ops_test.go` — git-merge ops discipline.

## See also

- [`docs/agents/captain.md`](captain.md) — upstream gate that forwards to Council on approve.
- [`docs/agents/librarian.md`](librarian.md) — claims the `WriteMemory` bounty Council spawns on approval.
- [`docs/agents/medic.md`](medic.md) — handles tasks the Council permanently fails.
- [`docs/agents/diplomat.md`](diplomat.md) — under PR flow, opens the convoy-level draft PR after Council clears all sub-PRs.
