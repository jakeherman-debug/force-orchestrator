---
audience: operator
scope: New-operator setup — install, first daemon run, first task, smoke-test flows.
owner: D13
last_reviewed: 2026-05-05
---

# Onboarding

For a brand-new operator: from `git clone` to "I've watched a real task land."

## Stub

Currently a placeholder — D13 Phase 2 migrates the README's installation + getting-started sections here and adds the smoke flows from `make smoke`.

## Planned sections (P2 fills)

1. **Prerequisites** — Go 1.21+, `claude` CLI authenticated, `git`, `gh`
2. **Install** — `git clone`, `make build`, `force doctor`
3. **First daemon run** — `force add-repo`, `force daemon`, `force dashboard`, what to look for
4. **First task** — submit via dashboard "+ Queue Task" or `force add`; watch it through Commander → Chancellor → Astromech → Captain → Council → merge
5. **Smoke flows** — what `make smoke` covers; how to run individual smoke targets
6. **Repo write-mode promotion** — new repos default to `read_only`; promotion via `force repo set-mode <name> write`
7. **Spend caps** — defaults, where to inspect, how to override
8. **Where to go next** — pointers into [operator-runbook.md](operator-runbook.md) and the subsystem index

Source content for migration: `README.md` § Installation + § Getting Started + § Plan-only workflow.
