---
audience: both
scope: Librarian-mediated fleet memory — FleetMemory rows, weighted retrieval, transcripts, replay, learning panels.
owner: infrastructure
last_reviewed: 2026-05-07
---

# Fleet memory

Force's cross-agent memory is a SQLite + FTS5 substrate behind a single Go-side service interface (`librarian.Client`). Agents write lessons through the Librarian's queue; agents read weighted memories through the Librarian's read methods. Bypassing the interface skips weighting, retrieval logging, and rerank — Pattern P33 catches it at CI time.

## Overview

The substrate carries five categories of fleet-wide knowledge:

1. **`FleetMemory`** — task-outcome lessons (success and failure), per-repo, with composite quality score (freshness × validation × scope-relevance).
2. **`FleetMemory_fts`** — FTS5 virtual table over `summary`, `files_changed`, `topic_tags` for keyword recall.
3. **`LLMCallTranscripts`** — every Claude CLI call, redacted at write time (Pattern P31). Bodies offload to disk after 30 days via the `transcript-archive` dog.
4. **`ReplayResults`** — pure-read replays of historical decisions (Captain ruling, Council ruling, Medic decision, ConvoyReviewCycle) under the current prompt version. Pattern P-Replay forbids any mutation other than `INSERT INTO ReplayResults` and `INSERT INTO LLMCallTranscripts`.
5. **`FleetLearningPanels`** — weekly auto-rendered fleet-self-narrative; reads PromotionProposals + ProposedFeatures + ConvoyReviewCycles + FleetRules + BountyBoard over the trailing 7 days.

The Librarian curates the write path: agents enqueue `WriteMemory` bounties (sync) or `WriteMemoryTx` (in-transaction), the in-process Librarian Spawn loop consumes them, summarises diffs, and writes the `FleetMemory` row plus its FTS5 index entry. The on-wire payload (`writeMemoryPayload` in `inprocess.go`) is intentionally duplicated between the client package and `internal/agents/librarian.go` so the client package does not import the agents package — both definitions must stay in lockstep when the wire shape changes.

D4 Phase 0 added quality scoring (`freshness_score`, `validation_score`, `retrieval_count`, `last_retrieved_at`, `canonical_id`, `hypothesis_emitted_at`) and four maintenance dogs (`librarian-dedup-watch`, `librarian-quality-recompute`, `librarian-conflict-watch`, `librarian-hypothesis-emit`). D6 collapsed repo-digest assembly into `BuildRepoDigest`; D10 added `BuildArchitectureDoc` for the `dogArchitectureDocRender` dog.

The composite quality score is `freshness_score * (1.0 + validation_score)` computed in SQL so the sort is index-friendly. `validation_score` is clamped to `[-1, 1]` at write time, so the multiplier lives in `[0, 2]`: a memory with `validation_score = 0` ranks at freshness alone, a fully-positive memory ranks 2× freshness, a fully-negative one ranks 0 (effectively excluded). `GetWeightedMemories(scope, k)` is the Client method; `k <= 0` defaults to 20 (the historic `GetFleetMemories` cap). An empty `Scope` is rejected with `ErrEmptyScope` so callers cannot accidentally fan a global scan through this entry point.

`SummarizeForContextOverflow(ctx, prompt, targetBytes)` (D2 T1-2) is the Librarian's context-overflow lifeboat. The fleet calls this from `internal/claude/claude.go` ingress when an agent's assembled prompt exceeds the per-agent byte cap; the Librarian makes a single-turn Claude call (cheapest model available) and returns the shortened prompt. Implementations MUST NOT silently truncate to a smaller value than `targetBytes` — return the prompt as-is or an error so the caller's overflow path fires correctly. This is the second of two structural Pattern P13 / P31 exemptions (the first is `claude.go` itself).

## Components

- `internal/clients/librarian/client.go` — `Client` interface (the contract); `Memory`, `Scope`, `MemoryUpdate`, `Candidate`, `CommitsDigest`, `RepoDigest`, `ArchitectureDoc`, `SenatorDigest`, `CandidateRule`, `APISymbol`, `DigestCommit` types; sentinel errors (`ErrTxNotSupported`, `ErrEmptyScope`, `ErrNotFound`, `ErrInvalidLimit`). Pattern P16 (`internal/audittools/audit_pattern_p16_clients_interfaces_test.go`) forbids agent code from importing the concrete `inProcessClient` struct — only the `Client` interface and the `NewInProcess`/`NewGRPC`/`NewShared`/`NewMock` factory functions.
- `internal/clients/librarian/inprocess.go` — `inProcessClient` backing; `NewInProcess(db)`; `WriteMemory` / `WriteMemoryTx` / `GetMemoriesForTask` / `GetMemoriesByScope` / `UpdateMemory` / `RemoveMemory` / `EmitCandidate` / `ListPendingCandidates` / `SummarizeForContextOverflow`.
- `internal/clients/librarian/inprocess_d4.go` — D4 Phase 0 read path: `GetWeightedMemories` (composite-score sort), `RecentCommitsDigest`, `BootstrapSenatorRules`, `RefreshSenatorMemoryDigest`. The two LLM-backed methods are LIVE_HAIKU-gated (`LIVE_HAIKU_DISABLED` toggles a deterministic fixture for tests).
- `internal/clients/librarian/inprocess_d6.go` — D6 `BuildRepoDigest`; the shared knowledge-synthesis primitive both `SenatorOnboarding` and `force onboard <repo>` consume.
- `internal/clients/librarian/inprocess_d10.go` — D10 `BuildArchitectureDoc`; reuses `BuildRepoDigest` and emits the `<!-- AUTO-GENERATED by dogArchitectureDocRender -->`-stamped Markdown body for `<repo-root>/ARCHITECTURE.md`.
- `internal/clients/librarian/summarize_call.go` — `SummarizeForContextOverflow` LLM call (cheapest model; one of the two structural P13 exemptions).
- `internal/agents/transcript_archive.go` — daily housekeeping dog; transcripts >30d (or for convoys closed >7d) get summarised and offloaded to `~/.force/transcripts/<year>/<month>/<id>.txt.gz`.
- `internal/agents/replay.go` — `ReplayDecision` re-runs the historical event under the current prompt version; writes ONLY to `ReplayResults` + `LLMCallTranscripts` (Pattern P-Replay enforces).
- `internal/agents/retro_generator.go` — convoy-retro renderer; reads transcripts + memory + briefing for the post-mortem.
- `internal/agents/learning_panel_renderer.go` — weekly fleet-learning panel; `LearningPanelStats` + deterministic prose synthesiser; called by the `learning-panel-render` dog (7 d cadence). Live-Haiku swap is structured to be mechanical: replace `synthesisesProse(stats)` with a `CallWithTranscript` call that hands `stats` to Haiku.
- `internal/store/fleet_memory_dedup.go`, `fleet_memory_quality.go`, `fleet_memory_hypothesis.go` — D4 Phase 0 dog handlers (DedupAndMerge, RecomputeFreshnessScores, EmitHypothesisCandidates). Direct `store.GetFleetMemories` / `store.StoreFleetMemory` (in `internal/store/tasks.go`) live here; agent code reaches them only through the Librarian Client per Pattern P33.

D6's `BuildRepoDigest` is the shared knowledge-synthesis primitive both `SenatorOnboarding` and the `force onboard <repo>` CLI consume. The fields (`READMESample`, `TopLevelDirs`, `PublicAPISymbols`, `RecentCommits`, `Conventions`, `FragilityMemories`, `GeneratedAt`) are pure-data; rendering decisions live in the consumers, not in the digest builder. D10's `BuildArchitectureDoc` reuses `BuildRepoDigest` so a single edit moves both the SenatorOnboarding and the `dogArchitectureDocRender` paths in lockstep — the explicit anti-cheat seam called out in roadmap §D6.

## Invariants

1. **Read path goes through the Librarian Client.** Pattern P33 (`docs/patterns/p33-agent-memory-via-librarian-client.md`, `internal/audittools/audit_pattern_p33_agent_memory_via_librarian_client_test.go`) AST-walks `internal/agents/*.go` and rejects direct `store.GetFleetMemories` / `store.GetFleetMemoriesByIDs` / `store.ListAllFleetMemories` calls. The Client owns weighting + retrieval logging + rerank.
2. **Every Claude CLI call captured.** Pattern P31 (`docs/patterns/p31-llm-transcripts.md`) requires every entry-point Claude call to flow through `claude.CallWithTranscript*`, which inserts into `LLMCallTranscripts` (pre-redacted at write time per Fix #10). Two structural exemptions (`internal/claude/claude.go`, `internal/clients/librarian/summarize_call.go`).
3. **Replay is pure-read.** Pattern P-Replay (`internal/audittools/audit_pattern_replay_no_mutation_test.go`) walks the replay code path and rejects any reach into a non-replay mutator: no `BountyBoard` updates, no `FleetRules` writes, no `Escalations` inserts, no mail.
4. **Write path is queue-mediated.** `WriteMemory` returns the `BountyBoard.id` of the queued task, not the eventual `FleetMemory.id`. The Librarian Spawn loop owns the actual row insert + FTS5 sync.
5. **`WriteMemoryTx` is in-process only.** Remote backings (gRPC, shared) return `ErrTxNotSupported` because a `*sql.Tx` cannot cross a process boundary.
6. **Composite-score retrieval excludes merged rows.** `GetWeightedMemories` filters `canonical_id != 0` so dedup-merged rows don't surface; `validation_score` is clamped to `[-1, 1]` so the multiplier is `[0, 2]`.
7. **No `--no-edit` on rebases, no `--amend` after hook failure.** Replay and retro-rendering consume the same transcripts; mutating history would rewrite the replay's own substrate.
8. **FTS5 build tag required.** `FleetMemory_fts` is a virtual table; tests and binaries must build with `-tags sqlite_fts5` (per CLAUDE.md testing rules).

Replay-loop discipline (Pattern P-Replay) lives at the boundary between auditability and safety. Allowed writes are exactly two:

- `INSERT INTO ReplayResults` — the replay's audit row.
- `INSERT INTO LLMCallTranscripts` — the replay's OWN transcript row, stamped with `agent='<agent>-replay'`.

Forbidden: `BountyBoard.UpdateStatus`, `FailBounty`, any `FleetRules` write, `ConvoyReviewCycles` insert, `Escalations` insert, `Fleet_Mail` send, `SystemConfig` write, `OperatorTrustDials` write. The pattern test walks the replay path and rejects any reach into a non-replay mutator.

## Configuration

`schema/schema.sql` columns of interest:

- `FleetMemory` — `id`, `repo`, `task_id`, `outcome` (`success`|`failure`), `summary`, `files_changed`, `topic_tags`, `embedding` (reserved for sqlite-vec), `created_at`, `freshness_score`, `validation_score`, `retrieval_count`, `last_retrieved_at`, `canonical_id`, `hypothesis_emitted_at`. Index: `idx_fleet_memory_repo_created (repo, created_at)`.
- `FleetMemory_fts` — virtual FTS5 table over `summary, files_changed, topic_tags`. Synced explicitly by `StoreFleetMemory` (not a content table).
- `LLMCallTranscripts` — `id`, `task_id`, `agent`, `prompt_version`, `call_started_at`, `call_completed_at`, `system_prompt`, `user_prompt`, `response_text`, `tool_calls_json`, `cost_usd`, `input_tokens`, `output_tokens`, `cache_read_tokens`, `cache_creation_tokens`, `archived_at`. Indexes: `idx_llmct_task (task_id, call_started_at)`, `idx_llmct_agent (agent, call_started_at)`.
- `ReplayResults` — `id`, `original_event_id`, `original_event_kind` (`captain_ruling`|`council_ruling`|`convoy_review_cycle`|`medic_decision`), `replay_prompt_version`, `replay_started_at`, `replay_response`, `decision_changed`, `cost_usd`, `triggered_by_email`.
- `FleetLearningPanels` — `id`, `rendered_at`, `prose`, `cost_usd`, `prompt_version`, `source_event_refs_json`.

Environment / filesystem:

- `LIVE_HAIKU_DISABLED=1` — gates `BootstrapSenatorRules` and `RefreshSenatorMemoryDigest` to deterministic fixture paths (used by tests; production daemons leave it unset).
- `~/.force/transcripts/<year>/<month>/<id>.txt.gz` — `transcript-archive` dog offload root. Path template is constant; operator-supplied paths cannot pivot it.
- `maxArchivesPerRun = 1000` — per-tick offload cap so the DB transaction window stays small under backlog.

## Write path detail

The write path is queue-mediated by design: `WriteMemory` returns a `BountyBoard.id` (the queued task), not the eventual `FleetMemory.id`. This decouples the agent's hot path (success-handler, council-emit) from the Librarian's actual ingestion work — which includes diff truncation, summary generation, topic-tag extraction, and FTS5 sync. The Librarian Spawn loop in `internal/agents/librarian.go` consumes `WriteMemory` bounties; `writeMemoryPayload` is the on-wire JSON shape (`task`, `files`, `feedback`, `diff`, `repo`).

For in-transaction writes (e.g. recording a memory atomically with a status transition), use `WriteMemoryTx(ctx, tx, m)` so the queue write rides the same `*sql.Tx`. Remote backings (gRPC, shared) reject the tx form with `ErrTxNotSupported` because a `*sql.Tx` cannot meaningfully cross a process boundary. The in-process backing is the only one that supports it today.

## Operator surface

```bash
force memories list <repo>                    # recent FleetMemory rows for a repo
force memories show <id>                      # full memory body
force transcripts list <task-id>              # LLMCallTranscripts for one task
force replay <event-kind> <event-id>          # re-run a historical decision under current prompt
```

Dashboard surfaces:

- **Memory tab** — per-repo `FleetMemory` rows with composite-score ranking; click-through to the originating task.
- **Drill view (per-task)** — `LLMCallTranscripts` timeline; offloaded bodies load lazily on expand.
- **Reflection panel** — weekly `FleetLearningPanels` row; Sunday-night auto-render via `learning-panel-render` dog with on-demand "Refresh now" trigger (deterministic synth is cheap).
- **Operator-event annotations** — `OperatorEventAnnotations` rows let the operator flag transcripts/git ops/cycles as `problem`/`interesting`/`follow_up`.

Replay specifically: `force replay <event-kind> <event-id>` re-runs the historical decision under the current prompt version (Captain ruling, Council ruling, Medic decision, ConvoyReviewCycle). The replay's own LLM call writes a new transcript stamped with `agent='<agent>-replay'` so it does not pollute the original transcript stream. The "decision changed" comparison is the first 80 chars of the response trimmed; with the deterministic synth the comparison is meaningful (the response carries a structured tag, not free text). Live-Haiku swap is mechanical and does not change the contract.

The maintenance dogs run on the Inquisitor's 5-min heartbeat (subject to per-dog cooldowns):

- `librarian-dedup-watch` (12h) — folds near-identical rows into a single canonical row (sets `canonical_id`).
- `librarian-quality-recompute` (24h) — decays `freshness_score`.
- `librarian-conflict-watch` (24h) — surfaces contradictory memories as operator-actionable tickets.
- `librarian-hypothesis-emit` (24h) — emits candidate `PromotionProposals` from high-quality memories (handoff to Engineering Corps).
- `claude-md-drift-watch` (7d) — scans CLAUDE.md invariants vs FleetRules and emits drift candidates.
- `transcript-archive` (24h) — bounded offload of stale transcripts.
- `learning-panel-render` (7d) — weekly fleet-self-narrative.

## See also

- [`../agents/librarian.md`](../agents/librarian.md) — the curator agent itself.
- [`../patterns/p33-agent-memory-via-librarian-client.md`](../patterns/p33-agent-memory-via-librarian-client.md) — Pattern P33 (read-path discipline).
- [`../patterns/p31-llm-transcripts.md`](../patterns/p31-llm-transcripts.md) — Pattern P31 (transcript capture).
- [`holocron-schema.md`](holocron-schema.md) — schema parity + migration discipline.
- [`gas-town.md`](gas-town.md) — coordination substrate the Librarian's queue rides on.
- [`dogs.md`](dogs.md) — the maintenance dog roster.
