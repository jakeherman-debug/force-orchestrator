---
audience: both
scope: Fleet Memory + RAG + Librarian curator — placeholder until subsystem reference is authored.
owner: librarian
last_reviewed: 2026-05-05
---

# Fleet memory + RAG

## Status: Stub

This page is a placeholder reserved by [D13 P1](../closures/) for the fleet-memory + RAG + Librarian-curator reference.

## What this will cover

- The `FleetMemory` table — schema, write ownership, retention.
- The `FleetMemoryEmbeddings` (or equivalent) FTS5 + vector index that backs RAG.
- Librarian curator pattern — how memory is filtered, summarized, and emitted to other agents.
- The `librarian.Client` cross-agent interface ([Pattern P33](../patterns/p33-agent-memory-via-librarian-client.md)) — agents read FleetMemory via this surface only.

## Until then

Read:

- [`agents/librarian.md`](../agents/librarian.md) — the curator agent itself.
- [`patterns/p33-agent-memory-via-librarian-client.md`](../patterns/p33-agent-memory-via-librarian-client.md) — the read-discipline contract.

## See also

- [`subsystems/holocron-schema.md`](holocron-schema.md) — schema parity + migration discipline.

## When this page lands

The next round of subsystem-doc authoring fills this in once the FleetMemory shape stabilizes after D11+D12.
