---
audience: agent
scope: Only operator-facing code paths may write OperatorEventAnnotations rows.
owner: D13
last_reviewed: 2026-05-05
title: Pattern P-AnnotationsOperatorOnly — operator-only annotation writes
type: pattern-doc
pattern: P-AnnotationsOperatorOnly
---

# Pattern P-AnnotationsOperatorOnly — operator-only annotation writes

## Rationale

OperatorEventAnnotations are the operator's freeform notes attached
to fleet events. Agents must NEVER write to this table — annotations
must come from the operator UI or the CLI, not from a system path
that masks agent action as an operator note. Originates in D3 P6B.8.

Allowed roots:

- `internal/store` — the CRUD layer itself.
- `internal/dashboard` — operator-facing HTTP handlers.
- `cmd/force` — CLI parity (`force annotate`).
- `internal/audittools` — this test file references the table name.

## What it checks

`TestPattern_AnnotationsOperatorOnly` walks `internal/` and `cmd/`
for `*.go` (non-test). For each file:

1. Skips files whose path begins with one of the allowed roots.
2. Reads the file and checks if it mentions
   `OperatorEventAnnotations` at all; if not, skip.
3. For each non-comment line, flags any of:
   - `INSERT INTO OPERATOREVENTANNOTATIONS`
   - `UPDATE OPERATOREVENTANNOTATIONS`
   - `DELETE FROM OPERATOREVENTANNOTATIONS`
   - `InsertAnnotation(`

## How it fails

```
Pattern P-AnnotationsOperatorOnly: non-operator paths writing OperatorEventAnnotations:
  internal/agents/foo.go:42

Fix: route through store.InsertAnnotation from an operator-facing path only (dashboard handler or CLI command).
```

## How to fix

Move the annotation write into the operator surface:

- Dashboard: `internal/dashboard/handlers_annotations.go` calls
  `store.InsertAnnotation` from the POST handler.
- CLI: `force annotate <kind> <ref> <flag> <text>` invokes the same
  helper from `cmd/force/`.

Agents that need to surface a note to the operator should emit a
notification (via `notify.Dispatch`) or write to a different table
that already carries operator-vs-agent provenance.

## Test reference

- File: `internal/audittools/audit_pattern_annotations_operator_only_test.go`
- Core assertion: `TestPattern_AnnotationsOperatorOnly` (lines 31–113)
- Configuration: `pAnnotationsAllowedDirs` (lines 24–29).

## See also

- [P-Replay](p-replay-no-mutation.md) — replay handler is read-only.
- [P-AskNoWriteTools](p-ask-no-write-tools.md) — Ask handler is read-only.
- [P-TrustDialsOperatorWriteDiscipline](p-trust-dials-operator-write.md)
