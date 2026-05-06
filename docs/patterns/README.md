---
audience: agent
scope: Index of audit-pattern reference docs — one entry per Pattern test in internal/audittools/.
owner: D13
last_reviewed: 2026-05-05
---

# Audit patterns

Pattern tests are grep- or AST-based regressions that fail CI when a specific architectural invariant drifts. Each entry below links the per-pattern doc on this page to the Go test that enforces it. The contract: every Pattern test in `internal/audittools/` has a doc here, and every doc here points at a real test. (D13 P3 adds a drift-checker enforcing this invariant.)

## Numeric Pattern tests

- [P1 — rows.Scan error checks](p1-rows-scan.md) — every `rows.Scan` inside a `for-rows.Next()` loop observes its error.
- [P1.1 — rows.Err() after iteration](p1_1-rows-err.md) — every `for-rows.Next()` loop observes `rows.Err()` after the close brace.
- [P11 — exec.CommandContext threading](p11-exec-context.md) — long-running subprocesses thread a daemon-cancellable ctx; no fabricated `context.Background()`.
- [P13 — Capability profiles](p13-capability-profiles.md) — every Claude CLI call site sources tool args from a `*capabilities.Profile`.
- [P14 — BoS rules cover CLAUDE.md invariants](p14-bos-claudemd-coverage.md) — every CLAUDE.md invariant has a BoS rule (or an allowlisted process-only exemption).
- [P15 — Bash-guard wiring + env](p15-bash-guard.md) — astromech Bash sessions route through `force-bash-guard` via PATH+SHELL env shim.
- [P16 — Cross-agent service interfaces](p16-clients-interfaces.md) — `clients/<svc>/Client` is an interface; agents construct via factory, never composite literal.
- [P17 — CLAUDE.md size cap](p17-claude-md-size.md) — `CLAUDE.md` ≤ 20 KB hard cap (Phase 1 target ≤ 10 KB).
- [P18 — Render coherence](p18-render-coherence.md) — auto-generated docs byte-equal a fresh audit-slice render.
- [P20 — AT-id scope integrity](p20-at-id-scope-integrity.md) — every AT lookup scopes by compound `(convoy_id, at_id)`.
- [P21 — AT removal is operator-only](p21-at-removal-operator-only.md) — LLM proposal schemas may not declare a remove/deprecate intent on AT references.
- [P22 — Fingerprint determinism](p22-fingerprint-determinism.md) — ProposedFeatures fingerprint is deterministic + sort-idempotent + sensitive to topic.
- [P23 — Proposer write discipline](p23-proposer-write-discipline.md) — proposers only INSERT; archive/suppression writes are operator-only.
- [P24 — Score-distribution monitor](p24-score-distribution-monitor.md) — per-source score histogram skew >70% in any single bucket triggers a warning.
- [P25 — CLI parity](p25-cli-parity.md) — every mutating dashboard handler has a matching `force <verb>` CLI command.
- [P26 — Keyboard shortcut consistency](p26-keyboard-shortcuts.md) — `keymap.js` bindings and `help-overlay.html` rows agree exactly.
- [P27 — Notification budget routing](p27-notification-budget-routing.md) — forward-going `SendMail` call sites route through `RespectNotificationBudget` or an `emitOperatorMail*` wrapper.
- [P28 — Narrative is generated](p28-narrative.md) — `NarrativeRenders` insert lives in exactly one file; prompt template lives in code.
- [P29 — Briefing cites real evidence](p29-briefing.md) — briefing renderer emits IDs only from input; prompt in code; safe-llm marker required.
- [P30 — High-stakes auto-execute cooldown](p30-cooldown.md) — `agents.ScheduleCooldown` and its sibling helpers exist and are exported.
- [P31 — LLM transcripts captured](p31-llm-transcripts.md) — every Claude CLI call flows through `claude.CallWithTranscript*`.
- [P32 — Git ops logged](p32-git-ops-logged.md) — every `git`/`gh` exec routes through `internal/git`'s `LogAndRun`.
- [P33 — Agent memory via Librarian Client](p33-agent-memory-via-librarian-client.md) — agents read FleetMemory rows via the Librarian Client surface only.
- [P34 — Senate no self-promote](p34-senate-no-self-promote.md) — Senate code MUST NOT mutate FleetRules directly.

## Named Pattern tests

- [P-Docs — documentation structure substrate](p-docs.md) — README size cap, sub-index files, metadata blocks.
- [P-StageGate — staged-convoy gate enforcement](p-stage-gate.md) — D5.5 dispatch SQL includes `stage_id IS NULL` predicate; package wiring present.
- [P-StagingPromotionConfirm — post-hoc promotion gate](p-staging-promotion-confirm.md) — `SetConvoyStaging` has zero ungated production callers.
- [P-NotificationDispatch — D11 dispatch routing](p-notification-dispatch.md) — every operator notification routes through `notify.Dispatch`.
- [P-SupplyDeferral — token-expired non-passthrough](p-supply-deferral.md) — every `ErrTokenExpired` branch records a `supplydeferral.RecordDeferral`.
- [P-ArchaeologistOperatorGated — archaeologist proposal discipline](p-archaeologist-operator-gated.md) — archaeologist's only emission seam is `librarian.Client.EmitCandidate`.
- [P-AnnotationsOperatorOnly — operator-only annotation writes](p-annotations-operator-only.md) — only operator-facing paths may write `OperatorEventAnnotations`.
- [P-AskNoWriteTools — Ask handler read-only](p-ask-no-write-tools.md) — `ask_handler.go` calls no store mutators and contains no write SQL.
- [P-Replay — replay read-only on live state](p-replay-no-mutation.md) — `replay.go` may only INSERT into `ReplayResults` / `LLMCallTranscripts`.
- [P-TrustDialsOperatorWriteDiscipline — operator trust-dial writes](p-trust-dials-operator-write.md) — `SetTrustDial` with `SetBy=TrustDialOperator` runs only from operator-routed handlers/CLI.

## Authoring a new pattern doc

When you add a new `internal/audittools/audit_pattern_*_test.go`, add a sibling Markdown doc here with the standard six H2 sections (`Rationale` / `What it checks` / `How it fails` / `How to fix` / `Test reference` / `See also`) plus the metadata block (`audience` / `scope` / `owner` / `last_reviewed`). Then add a one-line bullet to one of the two lists above so the test is reachable from this index.
