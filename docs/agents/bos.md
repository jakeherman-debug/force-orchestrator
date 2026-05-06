---
audience: both
scope: Bureau of Standards (BoS) — post-commit invariant gate that runs deterministic Go AST checks against every astromech diff.
owner: D13
last_reviewed: 2026-05-05
---

# Bureau of Standards — Commit-Time Invariant Enforcer

## Role

The Bureau of Standards (BoS) is the post-commit invariant gate. After every astromech commit, a `BoSReview` infrastructure task is enqueued in parallel with the next-stage review. BoS runs every registered rule (BOS-001..011) against the diff via Go AST analysis — pure deterministic checks, **no LLM call**. Findings land in `SecurityFindings` with disposition `flagged` or `overridden` (the latter for `// BOS-BYPASS: <AUDIT-NNN> <reason>` comments).

BoS rules graduate D0 invariants to commit-time enforcement (e.g. BOS-011 graduates Pattern P16 from CI-time to commit-time block). Per the D4 anti-cheat directive, every new rule ships at `severity=advise` for 30 clean firings before being promoted to `block`. BOS-011 is the documented exception: it ships at block because Pattern P16 already had zero false positives across D0–D3.

Together with [ISB](isb.md), BoS is one half of a dual-gate: the source `CodeEdit` task only forwards to Captain after both `BoSReview` and `ISBReview` approve.

Roster: BoS-Phasma, BoS-Pyre, BoS-Cardinal.

## Responsibilities

- Claim `BoSReview` bounties.
- Run BOS-001..011 (and any new rules at `severity=advise`) against the post-commit diff via Go AST.
- Write findings to `SecurityFindings` with `flagged` / `overridden` disposition.
- Honor `// BOS-BYPASS: <AUDIT-NNN> <reason>` comments (rejects bypasses with reasons under 10 chars).
- Approve the `BoSReview` only when no rule fires at `severity=block`.

## Capability profile

Profile: [`agents/capabilities/bos.yaml`](../../agents/capabilities/bos.yaml). Loaded via `capabilities.LoadProfile("bos")` in `internal/agents/bos.go`. The profile grants minimal tools (Read / Grep / Glob / Bash) for parity with the foreground `force bos review --file <path>` use case — the in-process reviewer doesn't actually shell out at runtime, but the surface is documented for the CLI parallel.

## Key files

- `internal/agents/bos.go` — `SpawnBoS(ctx, db, name)` claim loop and rule dispatcher.
- `internal/agents/bos_integration_test.go` — full integration coverage.
- `internal/bos/rules/` — BOS-001..011 rule implementations (one file per rule).
- `agents/capabilities/bos.yaml` — capability profile.

## Tests

- `internal/agents/bos_integration_test.go` — end-to-end commit-to-finding coverage.
- `internal/audittools/audit_pattern_p13_capability_profiles_test.go` — capability profile invariant.
- `internal/audittools/audit_pattern_p14_bos_claudemd_coverage_test.go` — BoS rule coverage of CLAUDE.md invariants.
- `internal/audittools/audit_pattern_p15_bash_guard_test.go` — bash-guard rule (BoS-relevant).
- `internal/audittools/audit_pattern_p11_exec_context_test.go` — exec-context discipline (BoS-relevant invariant).

## See also

- [`docs/agents/isb.md`](isb.md) — sibling commit-time gate; together they form the dual-gate that precedes Captain.
- [`docs/agents/captain.md`](captain.md) — downstream gate that runs only after both BoS and ISB approve.
- [`docs/architecture/claude-cli-invocation.md`](../architecture/claude-cli-invocation.md) — see "Non-LLM reviewers (D4)" — BoS sits outside the per-agent invocation matrix because it does not invoke `claude -p`.
