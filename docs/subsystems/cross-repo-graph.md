---
audience: both
scope: D8 cross-repo dependency graph — how cross-repo edges are discovered, stored, and surfaced.
owner: D8
last_reviewed: 2026-05-05
subsystem: cross-repo-graph
type: subsystem-doc
---

# Cross-repo dependency graph

When a feature spans multiple registered repos, the fleet needs to know which repos depend on which so Commander can decompose, Chancellor can sequence, and ConvoyReview can verify the cross-repo diff. D8 ships the cross-repo dependency graph: a discovered, persisted, queryable model of the edges between every registered repo.

## Overview

The graph is built from three signal classes:

1. **Manifest declarations** — `go.mod` `replace` directives, `package.json` deps, `requirements.txt` references, etc.
2. **Source-text matches** — import / require statements that resolve to one of the operator's registered repos.
3. **Operator-authored** — `force repo link <a> <b> --reason "..."` for edges the discovery layer cannot infer.

A weekly `cross-repo-discovery` dog refreshes the auto-discovered edges; operator-authored edges are sticky.

## Components

- **`internal/repo/graph/`** — graph builder, discovery scanners, persistence layer.
- **`Repositories` table** — extended with `dependency_metadata_json` for cached scan output.
- **`RepoDependencyGraph` table** — edge list with `from_repo_id`, `to_repo_id`, `kind` (`manifest` / `import` / `operator`), `confidence`, `last_seen`.
- **`internal/repo/graph/discoverer.go`** — per-ecosystem discoverers (Go modules, npm, Python, Ruby, …).
- **`cross-repo-discovery` dog** (weekly) — refreshes auto-discovered edges; respects `--repo` scoping.
- **Commander integration** — Commander reads the graph during decomposition to assign tasks to dependency-leaf repos first.
- **Chancellor integration** — Chancellor reads the graph to detect cross-convoy conflicts (two pending convoys touching the same repo or a repo + its dependent).

## Invariants

1. **Graph is observational, not authoritative.** A missing edge does not block work; the operator can always force a cross-repo task. The graph is a planning aid.
2. **Operator-authored edges are sticky.** The discovery refresh never deletes or downgrades an `operator`-kind edge.
3. **Discovery is per-repo and pure.** Each discoverer is a function `(repo) → []Edge`; no cross-repo state in the discoverer itself. Composition happens at the graph layer.
4. **Confidence is monotonic on refresh.** A repeated observation raises confidence; a missed observation does not lower it (it bumps `last_seen` instead). Removal requires explicit operator action.
5. **Idempotent refresh.** Running the discovery dog twice produces the same edge set.

## Configuration

SystemConfig knobs:

- `cross_repo_graph_enabled` (default true) — global kill switch.
- `cross_repo_discovery_cron` — when the discovery dog fires (default weekly).
- `cross_repo_max_scan_bytes` — per-repo scan budget; oversized repos skip with a warning.

Per-discoverer ecosystem support is wired in code (Go modules, npm, Python pip, Ruby bundler at minimum). Adding a discoverer requires:

1. Implement the `Discoverer` interface in `internal/repo/graph/`.
2. Register in the discoverer factory.
3. Test fixture under `testdata/repo-graph/<ecosystem>/`.

## Operator surface

```bash
force repo graph                                  # render the full graph
force repo graph --repo myapp                     # subgraph rooted at myapp
force repo graph --json                           # JSON for piping
force repo link <repo-a> <repo-b> --reason "..."  # operator-authored edge
force repo unlink <repo-a> <repo-b>               # remove (operator edges only by default; --force for discovered)
force repo graph refresh [--repo myapp]           # manual discovery run
```

Dashboard:
- **Repos tab** — graph badge per repo showing in-degree / out-degree.
- **Convoy detail** — when a convoy spans multiple repos, the graph view highlights edges that the convoy's diff actually touches.

When the graph is wrong:

- **Missing edge**: `force repo link a b --reason "..."` writes an `operator` edge.
- **Wrong edge**: `force repo unlink a b --force` removes a discovered edge; the next discovery may re-add it. To suppress, add an explicit operator-suppression flag (per-edge `dispositions` JSON in `RepoDependencyGraph`).

## See also

- [`gas-town.md`](gas-town.md) — `Repositories` and `RepoDependencyGraph` are part of the coordination substrate.
- [`convoy-lifecycle.md`](convoy-lifecycle.md) — Chancellor uses the graph during conflict detection.
- `arch-health.md` (planned) — D9 surfaces graph properties (cycle detection, dependency depth) as health metrics.
- [`../closures/DELIVERABLE-8-CLOSURE.md`](../closures/DELIVERABLE-8-CLOSURE.md) — D8 closure report.
