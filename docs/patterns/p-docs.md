---
audience: agent
scope: docs/ tree structure invariants — README size cap, sub-index files, metadata blocks.
owner: D13
last_reviewed: 2026-05-05
title: Pattern P-Docs — documentation structure substrate
type: pattern-doc
pattern: P-Docs
---

# Pattern P-Docs — documentation structure substrate

## Rationale

D13 Phase 1 introduced four guards that pin the docs/ tree's shape so
content migration (P2), drift detection (P3), and verifier (P4) can
build on a stable substrate. The four guards are:

1. The top-level `README.md` stays under a hard cap (200 lines) — the
   front door is not the manual.
2. `docs/README.md` exists as the canonical navigation entry point.
3. Each new docs subdirectory (`agents/`, `subsystems/`, `patterns/`,
   `references/`) carries its own `README.md` mini-index.
4. Every `*.md` file under those subdirectories carries a YAML front-
   matter block with `audience`, `scope`, `owner`, `last_reviewed`.

P3 will broaden this surface (broken-link checker, orphan-doc
checker, render-coherence on docs); P1's scope is the four guards.

## What it checks

Four sub-tests:

1. `TestReadmeSizeUnder200Lines` — counts `\n` bytes in the
   top-level `README.md` (matching `wc -l` semantics) and asserts
   the count is ≤ 200.
2. `TestDocsIndexExists` — `docs/README.md` exists and is non-empty.
3. `TestDocsSubdirsHaveIndex` — every directory in
   `docsSubdirsRequiringIndex` (`agents`, `subsystems`, `patterns`,
   `references`) has a non-empty `README.md`.
4. `TestMetadataBlockOnAllNewDocs` — every `*.md` under those four
   subdirectories has a leading YAML front-matter block (allowing
   blank lines or HTML comments before the opening `---`) that
   contains the four required keys: `audience`, `scope`, `owner`,
   `last_reviewed`. The block is scanned in the first 30 lines;
   formatting is lenient (indented metadata still counts).

## How it fails

```
README.md is 1460 lines; hard cap is 200.
Move long-form content into docs/subsystems/ or docs/agents/ or docs/references/.
The top-level README is the front door, not the manual.
```

Or:

```
docs/patterns/foo.md: missing leading `---` YAML front-matter block.
Every doc under docs/agents/, docs/subsystems/, docs/patterns/, docs/references/ must start with:
---
audience: operator | agent | both
scope: <one-sentence description>
owner: <deliverable id or subsystem>
last_reviewed: YYYY-MM-DD
---
```

## How to fix

For oversized README: move long-form content into the appropriate
subdirectory (`docs/agents/`, `docs/subsystems/`, `docs/patterns/`,
`docs/references/`) and link to it from the top-level README.

For missing front matter: prepend the standard block:

```markdown
---
audience: agent
scope: <one-sentence description>
owner: <deliverable id or subsystem>
last_reviewed: 2026-05-05
---
```

## Test reference

- File: `internal/audittools/audit_pattern_p_docs_test.go`
- Core assertions:
  - `TestReadmeSizeUnder200Lines` (lines 41–57)
  - `TestDocsIndexExists` (lines 61–71)
  - `TestDocsSubdirsHaveIndex` (lines 85–99)
  - `TestMetadataBlockOnAllNewDocs` (lines 122–142)
- Helpers: `checkMetadataBlock`, `blockContainsKey`,
  `missingFrontMatterError`, `missingKeyError`.
- Constants: `readmeHardCapLines = 200`,
  `docsSubdirsRequiringIndex`, `metadataKeys`.

## See also

- [P17 — CLAUDE.md size cap](p17-claude-md-size.md)
- [P18 — Render coherence](p18-render-coherence.md)
- `docs/README.md` — the canonical navigation entry point.
