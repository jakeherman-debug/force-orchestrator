---
audience: operator
scope: Security posture overview — capability profiles, bash guard, scrubbing, repo-mode gating; placeholder until subsystem reference is authored.
owner: security
last_reviewed: 2026-05-05
---

# Security posture

## Status: Stub

This page is a placeholder reserved by [D13 P1](../closures/) for the security-posture overview that the top-level [`README.md`](../../README.md) `## Status` paragraph forward-references. The threat model — prompt injection from ingested content, LLM mistakes, runaway spend — is the same shape for every layer; this page will cover all of them in one place.

## What this will cover

- The threat model: single-laptop operator tooling, no multi-tenant story, no public-facing surface, no production-system access.
- Capability profiles — per-agent YAML tool grants ([`subsystems/capability-profiles.md`](capability-profiles.md)).
- The bash guard — astromech subprocess gating ([`patterns/p15-bash-guard.md`](../patterns/p15-bash-guard.md)).
- Inbound scrubbing — `internal/claude/inbound_redact.go` redacts secrets before content reaches the model.
- Repo-mode gating — read-only / proposal / commit modes per repo.
- The `.forceignore` file contract and the [`scripts/pre-commit/forceignore-check.sh`](../../scripts/pre-commit/forceignore-check.sh) gate.
- Dashboard binding (127.0.0.1 only; SSH-tunnel for remote access).

## Until then

Read the constituent pieces individually:

- [`subsystems/capability-profiles.md`](capability-profiles.md)
- [`patterns/p15-bash-guard.md`](../patterns/p15-bash-guard.md)
- [`patterns/p13-capability-profiles.md`](../patterns/p13-capability-profiles.md)

## What this isn't

Force is not a hardened public service. Capability profiles + bash guard + scrubbing are defense in depth against operator mistakes and prompt-injection-via-ingested-content; they are not designed to repel an authenticated attacker on the operator's laptop. If you are evaluating Force for a context with adversarial ground truth, the answer is currently "no" — the threat model needs revisiting first.

## See also

- [`README.md`](../../README.md) `## Status` — the top-level framing this page expands.
- [`closures/DELIVERABLE-1-CLOSURE.md`](../closures/DELIVERABLE-1-CLOSURE.md) — D1 pre-restart security closure.

## When this page lands

The next round of subsystem-doc authoring lifts the per-layer prose into this consolidated stub.
