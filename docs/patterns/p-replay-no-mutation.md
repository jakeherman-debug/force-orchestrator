---
audience: agent
scope: replay.go must not mutate live state; only ReplayResults + LLMCallTranscripts INSERTs allowed.
owner: D13
last_reviewed: 2026-05-05
title: Pattern P-Replay — replay read-only-on-live-state invariant
type: pattern-doc
pattern: P-Replay
---

# Pattern P-Replay — replay read-only-on-live-state invariant

## Rationale

The ReplayDecision feature lets an operator re-run a past LLM call
deterministically — but it must not mutate live state. A replay that
flipped a bounty's status would conflate "audit" with "operator
action." Originates in D3 P6B.7.

Allowed writes inside `replay.go`:

- `INSERT INTO ReplayResults` — the replay's own audit row.
- `INSERT INTO LLMCallTranscripts` — the replay's own transcript row.

Anything else is a violation. The forbidden mutator set:
`UpdateBountyStatus`, `FailBounty`, `UpsertFleetRule`,
`InsertEscalation`, `EscalateOpen`, `SendMail`,
`SetOperatorTrustDial`, `InsertConvoyReviewCycle`.

## What it checks

`TestPattern_ReplayNoMutation` reads
`internal/agents/replay.go` and asserts:

1. None of the `pReplayForbidden` mutator names appear as a function
   call (substring match `<name>(`).
2. Every `INSERT INTO <table>` matches a table in
   `pReplayAllowedTables` (`ReplayResults` or `LLMCallTranscripts`).
3. No `UPDATE <table> SET` fragment appears.
4. No `DELETE FROM <table>` fragment appears.

## How it fails

```
Pattern P-Replay: replay.go must not call UpdateBountyStatus — replay is read-only on live state
Pattern P-Replay: replay.go writes to forbidden table "Escalations" (allowed: ReplayResults, LLMCallTranscripts)
Pattern P-Replay: replay.go contains an UPDATE — replay must not mutate existing rows
```

## How to fix

Remove the forbidden write. If replay legitimately needs a new
audit-only sink, add the table to `pReplayAllowedTables` AND
document why it's an audit-only target.

## Test reference

- File: `internal/audittools/audit_pattern_replay_no_mutation_test.go`
- Core assertion: `TestPattern_ReplayNoMutation` (lines 41–76)
- Configuration: `pReplayForbidden`, `pReplayAllowedTables` (lines 24–39).

## See also

- [P25 — CLI parity](p25-cli-parity.md) — `force replay` is the CLI.
- [P-AskNoWriteTools](p-ask-no-write-tools.md)
- `internal/agents/replay.go`
