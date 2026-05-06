---
audience: both
scope: Imperial Security Bureau (ISB) — post-commit security gate sibling to BoS, plus host of the SUPPLY rule pack for supply-chain hygiene.
owner: D13
last_reviewed: 2026-05-05
---

# Imperial Security Bureau — Commit-Time Security Scanner

## Role

The Imperial Security Bureau (ISB) is the post-commit security gate, sibling to [BoS](bos.md). Same hook point, same `SecurityFindings` table, same bypass mechanism (`// ISB-BYPASS: <AUDIT-NNN> <reason>`). ISB rules cover hardcoded secrets, shell-injection, concatenated SQL, outbound-URL validation, HTTP-handler hardening, file-mode hygiene, destructive-file-op containment, LLM prompt-injection sentinels, unbounded `io.ReadAll`, and `DisallowUnknownFields` on LLM-response unmarshals.

All 10 baseline ISB rules ship at `severity=advise` per the D4 anti-cheat directive (no block-default on new rules; 30-clean-firings warm-up window precedes promotion to block). Context-sensitive rules (ISB-005, ISB-008, ISB-010) attempt a deterministic check first and only fall through to the LLM layer when the deterministic gate cannot resolve — per the "no LLM-layer ISB rule without a deterministic fallback attempt" directive.

Roster: ISB-Tarkin, ISB-Krennic, ISB-Yularen.

ISB also hosts the **SUPPLY rule pack** (D5) for supply-chain hygiene.

## Responsibilities

- Claim `ISBReview` bounties.
- Run ISB-001..010 against the post-commit diff. Examples: ISB-001 (hardcoded secrets — gitleaks + regex fallback), ISB-002 (shell injection — `exec.Command` arg discipline), ISB-003 (concatenated SQL), ISB-004 (outbound-URL validation), ISB-005 (HTTP-handler hardening), ISB-006 (file-mode hygiene), ISB-007 (destructive file-op containment), ISB-008 (LLM prompt-injection sentinels), ISB-009 (unbounded `io.ReadAll`), ISB-010 (`DisallowUnknownFields`).
- Run the SUPPLY pack (SUPPLY-001..005) on the same claim loop with `category='isb'` FleetRules gating: SUPPLY-001 (hallucinated package via CodeArtifact `DescribePackageVersion`), SUPPLY-002 (typosquat against per-ecosystem CodeArtifact-derived allowlist), SUPPLY-003 (stale package via `PublishedAt` threshold), SUPPLY-004 (SPDX license-compatibility against `internal/isb/rules/license_matrix.yaml`), SUPPLY-005 (known-CVE blocking via vendored osv-scanner). Manifest-gating dispatch filters out source-only commits before any registry hit.
- Route AWS CodeArtifact auth errors through the deferral path (`disposition='token_expired'`); the `supply-token-recheck` dog plus the convoy-level `AwaitingSupplyRecheck` gate replay.
- Honor `// ISB-BYPASS:` and `// SUPPLY-BYPASS:` comments (the parser is comment-prefix-agnostic — `#` for Ruby/Python, `<!--` for XML — and rejects bypasses with `< 10`-char reasons).
- Approve the `ISBReview` only when no rule fires at `severity=block`.

## Capability profile

Profile: [`agents/capabilities/isb.yaml`](../../agents/capabilities/isb.yaml). Loaded via `capabilities.LoadProfile("isb")` in `internal/agents/isb.go`. Like BoS, the profile is a minimal Read / Grep / Glob / Bash surface for foreground-CLI parity; the in-process reviewer's deterministic checks do not shell out at runtime, but the LLM-fallback layer for ISB-005 / ISB-008 / ISB-010 routes through the standard `claude -p` invocation under this profile.

## Key files

- `internal/agents/isb.go` — `SpawnISB(ctx, db, name)` claim loop.
- `internal/isb/rules/` — ISB-001..010 + SUPPLY-001..005 rule implementations.
- `internal/isb/rules/license_matrix.yaml` — SPDX license-compatibility matrix consumed by SUPPLY-004.
- `internal/agents/dogs_supply_token_recheck.go` — supply-deferral replay dog.
- `agents/capabilities/isb.yaml` — capability profile.

## Tests

- `internal/audittools/audit_pattern_p13_capability_profiles_test.go` — capability profile invariant.
- `internal/audittools/audit_pattern_p_supply_deferral_test.go` — SUPPLY deferral / token-recheck contract.
- `internal/audittools/audit_pattern_p_stage_gate_test.go` — convoy-stage gating including `AwaitingSupplyRecheck`.
- `internal/agents/dogs_supply_allowlist_test.go`, `dogs_supply_token_recheck_test.go` — supply-pack-specific dog coverage.
- `internal/audittools/audit_pattern_p31_llm_transcripts_test.go` — ISB's LLM-fallback layer writes transcripts.

## See also

- [`docs/agents/bos.md`](bos.md) — sibling commit-time gate; together they form the dual-gate that precedes Captain.
- [`docs/closures/DELIVERABLE-5-CLOSURE.md`](../closures/DELIVERABLE-5-CLOSURE.md) — full SUPPLY-pack design + anti-cheat self-check (D5 closure narrative).
- [`docs/agents/captain.md`](captain.md) — runs only after both BoS and ISB approve.
