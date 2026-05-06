---
audience: agent
scope: High-stakes auto-execute call sites route through agents.ScheduleCooldown.
owner: D13
last_reviewed: 2026-05-05
title: Pattern P30 — High-stakes auto-execute cooldown
type: pattern-doc
pattern: P30
---

# Pattern P30 — High-stakes auto-execute cooldown

## Rationale

High-stakes auto-execute paths (Council auto-merge on critical
convoy, Medic auto-fix, etc.) must surface a cooldown window the
operator can pause / resume / cancel before the action lands. The
helper layer is `internal/agents/cooldown_scheduler.go`. Originates
in D3 P6A.13. Migration of existing auto-execute sites is tracked
as backlog separately from this audit.

## What it checks

`TestPattern_P30_HighStakesCooldown_HelperExists` reads
`internal/agents/cooldown_scheduler.go` and asserts the load-bearing
exports are present:

- `func ScheduleCooldown(`
- `func PauseCooldown(`
- `func ResumeCooldown(`
- `func CancelCooldown(`
- `func MarkCooldownExecuted(`
- `func ListPendingCooldowns(`
- `const CooldownDuration`

## How it fails

```
Pattern P30: func ScheduleCooldown( missing from cooldown_scheduler.go
```

## How to fix

Restore the missing function or constant. If you need to rename
`CooldownDuration`, update the audit's expected-name list in the same
commit and file a follow-up to update every call site.

## Test reference

- File: `internal/audittools/audit_pattern_p30_cooldown_test.go`
- Core assertion: `TestPattern_P30_HighStakesCooldown_HelperExists`
  (lines 17–37)

## See also

- `internal/agents/cooldown_scheduler.go`
- [P27 — Notification budget routing](p27-notification-budget-routing.md)
