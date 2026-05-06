---
audience: both
scope: Investigator — free-form research agent that delivers a written prose report and does not spawn follow-up tasks.
owner: D13
last_reviewed: 2026-05-05
---

# Investigator — Research Agent

## Role

The Investigator is a free-form research agent. Unlike the Auditor it produces *prose*, not structured findings, and does not spawn follow-up tasks. It is the right tool when the question is open-ended ("what would it take to migrate X?", "is feature Y feasible?") and the operator wants a report, not a worklist.

Submit an investigation with `force investigate [--priority N] [--repo <name>] <question>`. Without `--repo`, the agent runs from the force-orchestrator working directory with access to all registered repos; with `--repo myapp`, it runs from that repo's local path and focuses there.

## Responsibilities

- Claim `Investigation` bounties.
- Run the research via Claude with the read-only Investigator capability profile.
- Deliver the full prose report to the operator as fleet mail when complete (`force mail inbox operator` to see it arrive).
- Mark the Investigation task `Completed` once the mail is sent.

## Capability profile

Profile: [`agents/capabilities/investigator.yaml`](../../agents/capabilities/investigator.yaml). Loaded via `capabilities.LoadProfile("investigator")` in `internal/agents/investigator.go`. Same read-only toolset as the Auditor — Read / Glob / Grep, read-only Bash, Jira / Confluence / Glean / SonarQube / Datadog MCP. No Edit / Write.

## Key files

- `internal/agents/investigator.go` — `SpawnInvestigator(ctx, db, name)` claim loop and report-to-mail handoff.
- `agents/capabilities/investigator.yaml` — capability profile.

## Tests

- `internal/audittools/audit_pattern_p13_capability_profiles_test.go` — capability profile invariant (no-write enforcement).
- `internal/audittools/audit_pattern_p31_llm_transcripts_test.go` — Investigator LLM calls write transcripts.
- `internal/audittools/audit_silent_failures_test.go` — Investigator failure paths terminate cleanly.

## See also

- [`docs/agents/auditor.md`](auditor.md) — sibling read-only agent; produces structured findings, spawns a Planned convoy.
- [`docs/agents/librarian.md`](librarian.md) — owner of the mail surface the Investigator ships its report through.
