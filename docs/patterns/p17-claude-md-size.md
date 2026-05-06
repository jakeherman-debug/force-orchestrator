---
audience: agent
scope: CLAUDE.md must remain ≤ 20 KB (Phase 1 target ≤ 10 KB).
owner: D13
last_reviewed: 2026-05-05
title: Pattern P17 — CLAUDE.md size cap
type: pattern-doc
pattern: P17
---

# Pattern P17 — CLAUDE.md size cap

## Rationale

D3 Phase 1 pivoted CLAUDE.md from a hand-edited 50 KB rulebook to a
renderer-managed file containing only `render_to='claude-md-file'`
content. The Phase 1 target is ≤ 10 KB; the absolute upper bound is
20 KB. Keeping the file small preserves prompt-cache efficiency and
forces long-form content into the sharded `docs/` tree.

Bumping the cap is a deliberate operator action — the constant lives
in three places (this test, `agents.ClaudeMdHardCapBytes`,
`scripts/pre-commit/claude-md-size-check.sh`); they must move together
in a single commit with rationale.

## What it checks

`os.Stat(CLAUDE.md).Size() <= 20 * 1024` bytes. The constant
`claudeMdHardCapBytes` is duplicated locally so audittools doesn't
import `internal/agents` (audittools reads source on disk only).

## How it fails

```
CLAUDE.md is 24576 bytes; hard cap is 20480 (Phase 1 target ≤ 10240).
  Either:
    - move content out via the FleetRules render_to enum
      (agent-prompt / fix-log / pattern-test-docstring / per-domain-doc:*),
      then `make render-rules`, OR
    - bump claudeMdHardCapBytes + agents.ClaudeMdHardCapBytes +
      scripts/pre-commit/claude-md-size-check.sh together with a
      commit message that justifies the growth.
```

## How to fix

Pick the right `render_to` target for the content:

- `claude-md-file` — agent universal-load (the tightly capped file).
- `agent-prompt` — per-agent prompt (injected via `--append-system-prompt`).
- `fix-log` — historical narrative (`FIX-LOG.md`).
- `pattern-test-docstring` — co-located with the pattern test.
- `per-domain-doc:<name>` — sharded `docs/` page.

Move the row in `internal/store/fleet_rules_audit.go`, run
`make render-rules`, and re-stage the resulting files (Pattern P18
verifies coherence).

## Test reference

- File: `internal/audittools/audit_pattern_p17_claude_md_size_test.go`
- Core assertion: `TestPattern_P17_ClaudeMdSize` (lines 29–49)
- Constant: `claudeMdHardCapBytes = 20 * 1024` (line 15).

## See also

- [P18 — Render coherence](p18-render-coherence.md)
- `scripts/pre-commit/claude-md-size-check.sh` — the matching pre-commit
  guard.
