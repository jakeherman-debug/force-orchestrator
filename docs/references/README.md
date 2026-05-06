---
audience: operator
scope: Index of configuration files and tunable knobs — pointers into the YAML and SystemConfig surfaces.
owner: D13
last_reviewed: 2026-05-05
---

# References

Pointers to every shipped configuration file and the operator-facing knob surface. The configs are the source of truth — this doc points at them and explains what each one does.

Currently a stub directory — D13 Phase 2 fills the per-config explainers.

## Configuration files

| File | Subsystem | Notes |
|---|---|---|
| [`config/notifications.yaml`](../../config/notifications.yaml) | D11 notification routing | Channel → severity routing rules; loaded by `notify.Dispatch`. Per-rule operator overrides via `SystemConfig`. |
| [`config/dashboard.yaml`](../../config/dashboard.yaml) | D11 dashboard personalization | Tab visibility/order, theme + density, refresh cadence; per-operator overrides via `SystemConfig`. |
| [`docs/arch-health-weights.yaml`](../arch-health-weights.yaml) | D9 architecture health report | Per-signal weights for the composite arch-health score. |
| [`agents/capabilities/REGISTRY.yaml`](../../agents/capabilities/REGISTRY.yaml) | D1 capability profiles | Fleet-wide vocabulary of allowed tools — every per-agent profile draws from this. |
| [`agents/capabilities/.forceblocklist.yaml`](../../agents/capabilities/.forceblocklist.yaml) | D1 capability profiles | Fleet-wide denylist; overrides any per-agent grant. |
| [`agents/capabilities/<agent>.yaml`](../../agents/capabilities/) | D1 capability profiles | Per-agent capability profile (one per agent — Astromech, Captain, …). |
| [`internal/isb/rules/license_matrix.yaml`](../../internal/isb/rules/license_matrix.yaml) | D5 SUPPLY-004 | SPDX license-compatibility matrix. |
| [`schema/schema.sql`](../../schema/schema.sql) | Store | Canonical SQLite schema; parity-checked against `createSchema` + `runMigrations` by `TestSchemaParity`. |

## SystemConfig knob index

The `SystemConfig` table is a key/value store of runtime-tunable knobs. The full operator-facing list lives in [`README.md` § Configuration](../../README.md#configuration); D13 P2 migrates that table into a per-knob explainer here.

## .forceignore

[`.forceignore.example`](../../.forceignore.example) — gitignore-style pattern file that filters secrets / sensitive files out of Force's own file readers. See [`internal/repo/forceignore.go`](../../internal/repo/forceignore.go) for the loader.
