---
audience: both
scope: Fleet Librarian — memory curator that turns each successful task into a high-quality 2–4 sentence memory nugget and feeds Fleet Memory RAG.
owner: D13
last_reviewed: 2026-05-05
---

# Fleet Librarian — Memory Curator

## Role

The Librarian writes high-quality memory entries after each successful task. When the Jedi Council approves a task it spawns a `WriteMemory` bounty containing the task description, files changed, council feedback, and the git diff. The Librarian claims this bounty and calls Claude with a strict prompt that produces a 2–4 sentence memory nugget covering: (1) what was built or fixed (specific — named function, file, or component), (2) what was non-obvious or tricky about the implementation, and (3) patterns / gotchas / pitfalls not obvious from reading the code. The result is injected into future astromech prompts via the Fleet Memory FTS5 RAG system.

Roster: Jocasta-Nu, Huyang, Dexter-Jettster.

The Librarian also exposes a Go interface (`librarian.Client`) consumed by other agents (Astromechs, Archaeologist, Senate) for memory reads, candidate emission, and recent-commits digests — per the cross-agent-service-interfaces invariant in CLAUDE.md.

## Responsibilities

- Claim `WriteMemory` bounties.
- Render the memory nugget via Claude with the Librarian capability profile.
- Insert the result into `FleetMemory` (the FTS5 backing table).
- On Claude failure, fall back to a truncated task description so no memory slot is lost (no silent failure, no skipped slot).
- Serve the in-process `librarian.Client` interface (`Lookup`, `EmitCandidate`, `RecentCommitsDigest`, etc.) — see `internal/clients/librarian/`.
- Run as the upstream of `BootstrapSenatorRules` candidate proposals (the Senate's no-self-promote pipeline routes through the Librarian; see [`docs/agents/senate.md`](senate.md)).

## Capability profile

Profile: [`agents/capabilities/librarian.yaml`](../../agents/capabilities/librarian.yaml). Loaded via `capabilities.LoadProfile("librarian")` in `internal/agents/librarian.go`.

## Key files

- `internal/agents/librarian.go` — `SpawnLibrarian(ctx, db, name)` claim loop + memory-rendering flow.
- `internal/clients/librarian/` — the cross-agent service interface (`Client`, `NewInProcess`, etc.) per the CLAUDE.md cross-agent-service-interfaces invariant.
- `agents/capabilities/librarian.yaml` — capability profile.

## Tests

- `internal/audittools/audit_pattern_p13_capability_profiles_test.go` — capability profile invariant.
- `internal/audittools/audit_pattern_p16_clients_interfaces_test.go` — enforces the `librarian.Client` interface shape (no concrete-struct imports across packages).
- `internal/audittools/audit_pattern_p33_agent_memory_via_librarian_client_test.go` — every cross-agent memory read routes through the librarian client interface, never a direct `FleetMemory` query.
- `internal/audittools/audit_pattern_p31_llm_transcripts_test.go` — Librarian LLM calls write transcripts.
- `internal/audittools/audit_silent_failures_test.go` — Claude-failure path falls back to a truncated description, never a silent skip.

## See also

- [`docs/agents/council.md`](council.md) — spawns the `WriteMemory` bounties the Librarian claims.
- [`docs/agents/astromech.md`](astromech.md) — primary consumer of `FleetMemory` via the `librarian.Client` interface (FTS5-ranked Fleet Memory hits inject into the astromech prompt).
- [`docs/agents/senate.md`](senate.md) — Librarian emits `BootstrapSenatorRules` candidates the Senate ratification pipeline consumes.
- [`docs/agents/archaeologist.md`](archaeologist.md) — calls `librarian.EmitCandidate` for migration proposals.
