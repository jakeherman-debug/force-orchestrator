---
audience: agent
scope: Auto-generated CLAUDE.md / FIX-LOG.md / per-domain docs byte-equal a fresh audit-slice render.
owner: D13
last_reviewed: 2026-05-05
title: Pattern P18 — Render coherence
type: pattern-doc
pattern: P18
---

# Pattern P18 — Render coherence

## Rationale

The auto-generated files (`CLAUDE.md`, `FIX-LOG.md`, every per-domain
doc) MUST byte-equal what the FleetRules audit slice renders against
a fresh in-memory DB. Drift means an operator edited
`fleet_rules_audit.go` (or its embedded `fixlog/*.md` content)
without re-running `make render-rules` and re-staging the output.

P18 is the third layer of the systemic fix:

- Layer 1: `BootstrapFleetRules` is convergent on `content_hash`
  mismatch — re-bootstrapping refreshes a stale persistent DB.
- Layer 2: `force render-rules` defaults to fresh in-memory — CLI
  output never depends on operator-side DB state.
- Layer 3: this test catches drift in `make test` / CI.

Mirrors the schema-parity pattern (`TestSchemaParity` in
`internal/store/schema_parity_test.go`). Two sources must agree; the
remedy is one command. P18 graduates to a BoS commit-time rule when
D4 ships.

## What it checks

`TestPattern_P18_RenderCoherence`:

1. Initialises `:memory:` DB and bootstraps the FleetRules audit slice.
2. Renders three targets via `agents.RenderClaudeMdFile`,
   `agents.RenderFixLog`, and `agents.RenderPerDomainDocs`.
3. Reads each on-disk file and `bytes.Equal` against the rendered
   content.
4. Reports the first divergent line via `firstDiffLines` for actionable
   diff context.

`TestPattern_P18_DetectsInjectedDrift` proves `firstDiffLines` actually
surfaces differences — without this, a future refactor that neutered
the helper would leave P18 toothless.

## How it fails

```
Render coherence violated:

CLAUDE.md: on-disk content differs from audit-slice render.
  Fix: `make render-rules` and re-stage the resulting files.
  First differing lines:
    --- on-disk lines 14..44 ---
       12: ...
    →  14: STALE LINE
    --- audit-slice lines 14..44 ---
    →  14: FRESHLY RENDERED LINE
```

## How to fix

```bash
make render-rules
git add CLAUDE.md FIX-LOG.md docs/  # whatever changed
git commit
```

## Test reference

- File: `internal/audittools/audit_pattern_p18_render_coherence_test.go`
- Core assertion: `TestPattern_P18_RenderCoherence` (lines 43–104)
- Drift detector self-test: `TestPattern_P18_DetectsInjectedDrift`
  (lines 110–121)
- Helper: `firstDiffLines` (lines 127–176).

## See also

- [P17 — CLAUDE.md size cap](p17-claude-md-size.md)
- `internal/store/fleet_rules_audit.go` — the audit slice.
- `make render-rules` (Makefile target wrapping `force render-rules`).
