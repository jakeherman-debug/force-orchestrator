---
audience: agent
scope: Index of audit-pattern reference docs — one entry per Pattern test in internal/audittools/.
owner: D13
last_reviewed: 2026-05-05
---

# Audit patterns

Pattern tests are grep- or AST-based regressions that fail CI when a specific architectural invariant drifts. Each entry below points at the Go test that enforces the rule plus a markdown doc explaining what the rule is, why it exists, and how to obey it without tripping the test.

The Pattern index in [`README.md`](../../README.md#pattern-test-enforcement-layer) is currently the source of truth — D13 Phase 2 migrates each row to its own doc here, then `../README.md` becomes the canonical entry.

## Planned contents (D13 P2 fills)

Numeric Pattern tests (P1..P34):

- `P1.md` / `P1_1.md` — `rows.Scan` error checks; `rows.Err()` after iteration
- `P2.md` — Idempotency-key UNIQUE coverage; race-safe insert paths
- `P3.md` — Convoy-scoped queries use `convoy_id` not `payload LIKE`
- `P4.md` — Hot-table indexes present; claim queries use `idx_bounty_*`
- `P6.md` — `Escalations.status` only takes documented values
- `P7.md` — State-transition CAS via `UpdateBountyStatusFrom`
- `P8.md` — Dashboard binds 127.0.0.1; no wildcard CORS
- `P9.md` — Secret-literal scrubbing on outbound channels
- `P10.md` — Branch validators + `git --` separator
- `P11.md` — `exec.CommandContext(ctx, ...)` for long-running commands
- `P12.md` — LLM prompt injection: `<user_content>` wrapping + `strictJSONUnmarshal`
- `P13.md` — Capability profiles sourced via `capabilities.LoadProfile`
- `P14.md` — BoS rules cover CLAUDE.md invariant headings
- `P15.md` — Bash-guard wiring + env (PATH and SHELL)
- `P16.md` — Cross-agent service interfaces in `internal/clients/`
- `P17.md` — Rendered `CLAUDE.md` ≤ 20 KB hard cap
- `P18.md` — Render coherence (FleetRules → CLAUDE.md / FIX-LOG.md / docs)
- `P20.md` — AT-id scope integrity (compound `(convoy_id, at_id)` key)
- `P21.md` — AT removal is operator-only
- `P22.md` — Fingerprint determinism
- `P23.md` — Proposer write discipline
- `P24.md` — Score-distribution monitor
- `P25.md` — CLI parity for every mutating dashboard handler
- `P26.md` — Keyboard shortcut consistency
- `P27.md` — Notification-budget routing (`RespectNotificationBudget`)
- `P28.md` — Narrative is generated (no hardcoded prose)
- `P29.md` — Briefing cites real evidence; prompt in code
- `P30.md` — High-stakes cooldown helper exists
- `P31.md` — All LLM calls captured (`CallWithTranscript*`)
- `P32.md` — Git ops logged (every `git`/`gh` exec routes through `internal/git`)
- `P33.md` — Agent memory injection via `librarian.Client`
- `P34.md` — Senate package contains no direct `INSERT INTO FleetRules`

Named Pattern tests:

- `P-StageGate.md` — D5.5 staged convoys gate enforcement
- `P-NotificationDispatch.md` — D11 every operator notification routes through `notify.Dispatch`
- `P-SupplyDeferral.md` — D5 every `ErrTokenExpired` branch records deferral

The contract: every Pattern test in `internal/audittools/` MUST have a corresponding doc here, and every doc here MUST point at a real test. P3 (D13 Phase 3) will add a drift-checker enforcing this invariant.
