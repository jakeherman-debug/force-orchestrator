---
audience: operator
scope: D9 Archaeologist — operator-gated incident archive synthesizing past failures into searchable signals.
owner: D9
last_reviewed: 2026-05-05
subsystem: archaeologist
type: subsystem-doc
---

# Archaeologist

The Archaeologist is the operator-gated agent that synthesizes the fleet's incident archive — escalations, failed convoys, parse-failure clusters, sustained Medic interventions — into a searchable signal layer. It does not run automatically; the operator triggers it for a specific scope, and the output flows into the arch-health report and fleet memory RAG layer (both planned subsystem docs).

## Overview

The fleet accumulates failure data faster than any human can read it: every escalation, every Medic verdict, every `[CHANCELLOR FAIL-CLOSED]`, every retry-cap hit, every reshard cascade. The Archaeologist answers a specific operator question — "what's been going wrong in the last N days, scoped to repo X?" — by:

1. Reading the windowed failure data: `Escalations`, `BountyBoard.error_log`, `MedicReviews`, `[…]` mail subjects.
2. Clustering by signature (file path, error class, agent, rule_key).
3. Calling Claude with a strict synthesis prompt to produce a per-cluster narrative.
4. Writing the result as a `ProposedFeatures` row tagged `source=archaeologist` so it flows into the operator's triage queue.

The Archaeologist is **not** a dog — it doesn't auto-fire — because incident synthesis is high-stakes and cost-sensitive. The operator picks the moment.

## Components

- **`internal/agents/archaeologist.go`** — claim loop + synthesis prompt.
- **`Archaeologist` task type** — operator-spawned only.
- **`ArchaeologyReports` table** — one row per run with scope, window, narrative, links to source rows.
- **`ProposedFeatures` cross-emit** — Archaeologist findings appear in the Investigator → Captain → ConvoyReview aggregation queue at value/complexity score.
- **Capability profile**: `agents/capabilities/archaeologist.yaml` — read-only tool surface; no Bash, no MCP write.

## Invariants

1. **Operator-gated.** No auto-spawn. No dog triggers Archaeologist runs. The operator runs `force archaeology` explicitly.
2. **Read-only.** Capability profile grants Read / Glob / Grep + read-only MCP. No git ops, no DB mutations beyond writing the `ArchaeologyReports` row and `ProposedFeatures` cross-emit.
3. **Scoped windows are mandatory.** Every run requires a time window AND a scope (repo / agent / convoy / rule_key). Unscoped runs are rejected at CLI parse.
4. **Narrative cites real evidence.** Pattern P29 (`audit_pattern_p29_briefing_cites_real_evidence_test.go`) — the same family that gates Briefing prose — applies to Archaeologist output. Every claim resolves to a real `BountyBoard` / `Escalations` / `ConvoyReviewCycles` row.
5. **No cross-emit cheating.** The `ProposedFeatures` cross-emit must include real source-row references; Pattern P22 fingerprint determinism applies (sort-order-invariant, byte-deterministic).
6. **Cost cap.** Archaeologist runs route through the per-task spend caps (`per_task_spend_alert_usd` / `per_task_spend_escalate_usd`) like every other agent.

## Configuration

SystemConfig knobs:

- `archaeology_max_window_days` (default 90) — refuses windows beyond this.
- `archaeology_max_clusters` (default 20) — caps the cluster count to keep the narrative coherent.
- `archaeology_min_signals` (default 3) — refuses scopes with fewer signals than this (not enough material to synthesize).

Capability profile (`agents/capabilities/archaeologist.yaml`) declares:

- `builtin_tools: [Read, Glob, Grep]`
- `mcp.servers:` — read-only MCP allowlist (Glean for cross-team context, Datadog for runtime correlation).

## Operator surface

```bash
force archaeology --repo myapp --window 30d                     # synthesize last 30 days for myapp
force archaeology --convoy 7                                    # synthesize a specific convoy
force archaeology --agent astromech --window 7d                 # per-agent failure synthesis
force archaeology list                                          # past report headlines
force archaeology show <id>                                     # full report
```

Dashboard:
- **Reflection tab** — past Archaeologist reports surface alongside calibration scoreboard and learning panel.
- **ProposedFeatures triage queue** — `source=archaeologist` rows appear scored by value/complexity.

The output narrative is plain prose, not a spreadsheet. It's intended to answer "what's the pattern?" not "what's the count?" — for counts, use `force stats`, `force costs`, and `force audit`.

## See also

- `arch-health.md` (planned) — D9 sibling: continuous health metrics (Archaeologist is the on-demand qualitative complement).
- `fleet-memory.md` (planned) — Archaeologist findings can be promoted into FleetMemory by the operator.
- [`escalation-and-medic.md`](escalation-and-medic.md) — `Escalations` is the primary input.
- [`../closures/DELIVERABLE-9-CLOSURE.md`](../closures/DELIVERABLE-9-CLOSURE.md) — D9 closure report.
