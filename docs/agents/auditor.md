---
audience: both
scope: Auditor — read-only analysis agent that scans the codebase + external systems and turns findings into a Planned convoy of CodeEdit fixes for operator approval.
owner: D13
last_reviewed: 2026-05-05
---

# Auditor — Codebase Scanner

## Role

The Auditor is a read-only analysis agent. It systematically scans the codebase (and external systems — Jira, Confluence, Glean, SonarQube, Datadog) for issues and produces structured findings. Each finding becomes a discrete `CodeEdit` task in a new convoy — but tasks are created in the **Planned** state, not activated. The operator must review and approve the convoy before any work begins. This anti-cheat shape is deliberate: the Auditor cannot put code-write tasks into `Pending` directly.

Submit an audit with `force scan [--priority N] [--repo <name>] <scope/question>`. Without `--repo`, the agent scans all registered repos; with `--repo myapp`, it scopes to that repo and runs from its local path.

## Responsibilities

- Claim `Audit` bounties.
- Run the scan via Claude with the read-only Auditor capability profile.
- Parse findings into a structured list (severity = `HIGH | MEDIUM | LOW`).
- Create a convoy with one `Planned` `CodeEdit` task per finding (labeled with severity and finding rationale) — never `Pending`.
- Mail the operator the finding summary and convoy ID.
- If no findings are reported, complete the Audit task immediately and mail the operator — no convoy is created.

## Capability profile

Profile: [`agents/capabilities/auditor.yaml`](../../agents/capabilities/auditor.yaml). Loaded via `capabilities.LoadProfile("auditor")` in `internal/agents/auditor.go`. The profile grants Read / Glob / Grep + read-only Bash + Jira / Confluence / Glean / SonarQube / Datadog MCP servers — explicitly no Edit / Write.

## Key files

- `internal/agents/auditor.go` — `SpawnAuditor(ctx, db, name)` claim loop and finding-to-convoy materialization.
- `internal/agents/auditor_test.go` — unit coverage including the no-findings short-circuit.
- `agents/capabilities/auditor.yaml` — capability profile (read-only).

## Tests

- `internal/agents/auditor_test.go` — scan happy path + Planned-not-Pending invariant + no-findings path.
- `internal/audittools/audit_pattern_p13_capability_profiles_test.go` — capability profile invariant (Auditor's no-write surface enforced here).
- `internal/audittools/audit_pattern_p23_proposer_write_discipline_test.go` — Auditor uses the proposer write discipline (Planned convoy, not direct Convoys insert).
- `internal/audittools/audit_pattern_p31_llm_transcripts_test.go` — every Auditor LLM call writes a transcript.

## See also

- [`docs/agents/investigator.md`](investigator.md) — sibling read-only agent; produces prose, not structured findings.
- [`docs/agents/chancellor.md`](chancellor.md) — operator-approved Auditor convoys still pass through the Chancellor.
- [`docs/agents/archaeologist.md`](archaeologist.md) — debt-pattern variant; scans for repeated-pattern hits and proposes migrations through the librarian-emit pipeline.
