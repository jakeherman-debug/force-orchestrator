---
audience: agent
scope: Dashboard keyboard bindings and the help-overlay row set agree exactly.
owner: D13
last_reviewed: 2026-05-05
title: Pattern P26 — Keyboard shortcut consistency
type: pattern-doc
pattern: P26
---

# Pattern P26 — Keyboard shortcut consistency

## Rationale

Every binding registered in `keymap.js` must appear in
`help-overlay.html`, and vice versa. Drift between the two — a
shortcut bound but not documented, or documented but not bound —
silently degrades the discoverability of the dashboard's keyboard
surface. Originates in D3 P6A.3.

## What it checks

`TestPattern_P26_KeyboardShortcutConsistency`:

1. Reads `internal/dashboard/static/keymap.js` and extracts every
   `bind('<key>', …)` first argument via the regex
   `bind\(\s*'([^']+)'`.
2. Reads `internal/dashboard/static/help-overlay.html` and extracts
   every `data-help-key="<key>"` attribute value.
3. Deduplicates each set (a key may bind in multiple contexts but
   only needs one help-overlay row).
4. Asserts the two sets are equal — reports any keys present in one
   side but missing from the other.

`resolveStaticDir` walks candidate paths so the test runs from any
package working directory.

## How it fails

```
Pattern P26 violation: bindings in keymap.js but not in help-overlay.html:
  Cmd-K, g r
Add a row with data-help-key="<key>" for each, or remove the binding.
```

Or in the other direction:

```
Pattern P26 violation: keys documented in help-overlay.html but not bound in keymap.js:
  ?
Add a bind('<key>', ...) call for each, or remove the help-overlay row.
```

## How to fix

When you add a binding, also add a help-overlay row in the same commit
(and vice versa). The help-overlay's `data-help-key="<key>"` value
must match the `bind('<key>', …)` argument exactly.

## Test reference

- File: `internal/audittools/audit_pattern_p26_keyboard_shortcuts_test.go`
- Core assertion: `TestPattern_P26_KeyboardShortcutConsistency` (lines 40–85)
- Helpers: `resolveStaticDir`, `keysFromMatches`, `uniqueSet`,
  `setDiff`.

## See also

- [P25 — CLI parity](p25-cli-parity.md)
- `internal/dashboard/static/keymap.js`
- `internal/dashboard/static/help-overlay.html`
