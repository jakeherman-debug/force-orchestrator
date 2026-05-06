# force-orchestrator

A local-first, multi-agent software development factory. You submit a feature request in plain English; a fleet of autonomous AI agents decomposes it into tasks, writes the code, reviews it, and ships a draft PR — while you watch.

Inspired by Steve Yegge's **Gas Town** pattern: all coordination happens through a shared SQLite ledger, not message queues or in-memory state. Agents are stateless workers that compete for tasks; the database is the single source of truth.

The full operator + agent reference lives in [`docs/`](docs/README.md). This page is the front door.

---

## Architecture at a glance

```
                ┌─────────────────────────────────────────────┐
                │            holocron.db (SQLite)             │
                │   the shared ledger — single source of      │
                │   truth; agents poll, never talk directly   │
                └─────────────────────────────────────────────┘
                                    ▲
                  ┌─────────────────┼─────────────────┐
                  │                 │                 │
                  ▼                 ▼                 ▼
              operator         agent fleet        dashboard
              (CLI / web)         (claude -p          (127.0.0.1
              submits work        in worktrees)       only; SSE)

  Feature path:  Commander → Chancellor → convoy created
  Code path:     Astromech → BoS + ISB → Captain → Council → merge
  Ship path:     Pilot ask-branch → Diplomat draft PR → ConvoyReview → operator "Ship it"
  Background:    Inquisitor + ~20 watchdog dogs (spend, CI, drift, cleanup)
```

Agents shell out to `claude -p` (never the Anthropic HTTP API directly), so each one inherits the operator's MCP toolchain. Every agent runs under a static, YAML-declared capability profile (`agents/capabilities/`) — tools the profile does not grant are removed at invocation time. See [`docs/overview.md`](docs/overview.md) and [`docs/architecture/claude-cli-invocation.md`](docs/architecture/claude-cli-invocation.md) for the full layering.

---

## Quickstart

```bash
git clone <this repo> && cd force-orchestrator
make build                                # produces ./force (provenance ldflags injected)
./force add-repo myapp /path/to/myapp "Backend API"
./force daemon foreground                 # start fleet + bundled dashboard on :41977
# or: ./force daemon install               # launchd (macOS) / systemd user-unit (linux)
```

The daemon bundles the dashboard on `127.0.0.1:41977` (Star Wars: A New Hope, 1977 — operator-mnemonic). Standalone `./force dashboard` is still supported for back-compat.

`force doctor` after install verifies `git`, `claude`, `gh`, repo paths, DB integrity, and e-stop state. `force daemon status` surfaces PID, provenance, and trust-file state. Full step-by-step in [`docs/onboarding.md`](docs/onboarding.md); the daemon lifecycle reference is [`docs/subsystems/daemon-lifecycle.md`](docs/subsystems/daemon-lifecycle.md).

---

## Where to go next

| If you want to … | Read |
|---|---|
| Set up Force on a fresh laptop | [`docs/onboarding.md`](docs/onboarding.md) |
| Understand how the fleet fits together | [`docs/overview.md`](docs/overview.md) |
| Recover from a stuck convoy / runaway spend / daemon crash | [`docs/operator-runbook.md`](docs/operator-runbook.md) |
| Tune notifications, dashboard layout, theme | [`docs/subsystems/notification-routing.md`](docs/subsystems/notification-routing.md), [`docs/subsystems/dashboard.md`](docs/subsystems/dashboard.md) |
| Browse the per-subsystem reference | [`docs/subsystems/`](docs/subsystems/README.md) |
| Browse per-agent docs | [`docs/agents/`](docs/agents/README.md) |
| Read the full deliverable list | [`docs/roadmap.md`](docs/roadmap.md) |
| See what shipped in each deliverable | [`docs/closures/`](docs/closures/) |
| Browse the audit-pattern enforcement layer | [`docs/patterns/`](docs/patterns/README.md) |
| See every config knob and YAML file | [`docs/references/`](docs/references/README.md) |
| Read the agent-facing invariants (commit / schema / test discipline) | [`CLAUDE.md`](CLAUDE.md) — auto-rendered, do not hand-edit |

The canonical entry into the docs tree is [`docs/README.md`](docs/README.md). Everything is reachable from there in 1–2 hops.

---

## Contributing

This is a single-operator project, but the hygiene is the same as a team codebase:

- **Conventional commits** (`feat:`, `fix:`, `docs:`, …). Body explains the **why**, not the what.
- **No `--no-verify`.** Pre-commit hooks run for a reason; if one fails, fix the root cause and re-stage. Never `--amend` after a hook failure (the commit didn't happen).
- **Tests gate every phase.** `make test` (with `-tags sqlite_fts5`) is the gate. New code paths need a happy-path test, a per-failure-mode test, and an idempotence test.
- **`CLAUDE.md` is the agent-facing rules contract.** It's auto-rendered from `internal/store/fleet_rules_audit.go` via `make render-rules`. To add a universal-load rule: insert a FleetRules row with `render_to='claude-md-file'`, add the justification comment in the audit file, then re-render.
- **Pattern tests are not "nice-to-haves."** Each grep- or AST-based regression in `internal/audittools/` was earned the hard way; CI failure there means a real invariant has drifted.

Full architecture invariants + commit + schema + testing rules live in [`CLAUDE.md`](CLAUDE.md).

---

## Status

Force is **operator tooling for a single laptop**. There is no multi-tenant story, no public-facing surface, no production-system access. The dashboard binds 127.0.0.1 only; remote access goes through an SSH tunnel. The threat model is prompt injection from ingested content + LLM mistakes + runaway spend, not external attackers — read [`docs/subsystems/security.md`](docs/subsystems/security.md) and the `What this isn't` close in the security overview before evaluating it for any other context.

Roadmap status: D0 through D11 closed; D12 (daemon lifecycle) in flight; D13 (this docs reshard) in progress. See [`docs/roadmap.md`](docs/roadmap.md) and [`docs/closures/`](docs/closures/) for the per-deliverable evidence trails.
