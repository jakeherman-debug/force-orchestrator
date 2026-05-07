---
audience: operator
scope: Security posture — capability profiles, inbound scrubbing, repo-mode gating, .forceignore, and the astromech bash guard.
owner: security
last_reviewed: 2026-05-07
---

# Security posture

Force is single-laptop operator tooling. There is no multi-tenant story, no public-facing surface, and no production-system credentials in scope. The threat model the security stack defends against is **operator mistakes, prompt injection from ingested repo content, and runaway LLM spend** — not a hostile authenticated attacker on the operator's machine. Capability profiles, the bash guard, inbound scrubbing, repo-mode gating, and `.forceignore` are layered so a failure in any one of them is caught by another.

## Overview

The fleet's security boundary is built from five reinforcing layers:

1. **Per-agent capability profiles** (D1 T0-1) — every Claude session runs under a static YAML profile that declares the exact tool surface it may invoke. Hardcoded literals at call sites are rejected by Pattern P13 at CI time.
2. **Inbound prompt scrubbing** (D1 T0-2) — every prompt assembled by the orchestrator is run through `claude.ScrubInbound` before reaching `claude -p`, redacting PEM blocks, GCP service-account JSON, AWS access keys, GH PATs, and `.env`-shape secret assignments.
3. **`.forceignore` gating** (D1 T0-2) — repo-side opt-in policy file (gitignore syntax) that blocks secret-bearing files from being read into prompts at the file-load boundary, before scrubbing even runs.
4. **Repo-mode gating** (D1 / D2) — every registered repo carries a permanent `mode` (`read_only` / `write` / `quarantined`); destructive git ops refuse to run unless the mode is `write`.
5. **Astromech bash guard** (D2 T1-3) — the only agents that can run `Bash` (`astromech`, `medic-ci`) execute commands through a per-worktree shell shim that delegates to the `force-bash-guard` binary. Compound commands are split, segments are evaluated against an allowlist + denylist, and rejected commands never reach `/bin/bash`.

D1's closure (`docs/closures/DELIVERABLE-1-CLOSURE.md`) is the historical ledger; D2 layered the bash guard and per-task spend escalation on top.

## Components

- `internal/agents/capabilities/loader.go` — `LoadProfile`, fail-closed YAML loader; `AllowedToolsArg` / `DisallowedToolsArg` / `MCPConfigArg` accessors. `Profile.AllowedTools` is the union of `BuiltinTools` + the concrete-expanded MCP tool names from `MCPServers` (resolved against `REGISTRY.yaml`'s `mcp_namespaces`).
- `agents/capabilities/<agent>.yaml` — one per agent (astromech, captain, council, chancellor, medic, medic-ci, investigator, auditor, boot, librarian, diplomat, pilot, convoy-review, pr-review-triage, commander, bos, isb, senate, replay, retro, transcript-archive, learning-panel, …).
- `agents/capabilities/REGISTRY.yaml` — fleet-wide vocabulary (every legal tool name).
- `agents/capabilities/.forceblocklist.yaml` — fleet denylist; entries here cannot be granted by any profile.
- `internal/claude/inbound_redact.go` — `ScrubInbound`, `SetInboundRedactDB`, `recordInboundRedact`. Patterns: `pemBlockRe`, `gcpPrivateKeyRe`, `envAssignmentRe`, `awsAccessKeyRe`, `bearerOrPATRe`. Outbound dual lives in `store.RedactSecrets`.
- `internal/repo/forceignore.go` — `LoadForceIgnore`, `ForceIgnore.IsIgnored`, `ReadRepoFileGated`. Symlink-resolves before matching so `link.txt -> .env` cannot bypass policy.
- `.forceignore.example` — the canonical template operators copy into target repos (covers `.env*`, `*.pem`, `*_rsa`, GCP service-account JSON, `.aws/`, `.ssh/`).
- `scripts/pre-commit/forceignore-check.sh` — operator-side gate that refuses commits adding files matching the repo's own `.forceignore`.
- `internal/store/repo_mode.go` — `RepoMode` constants (`ModeReadOnly`, `ModeWrite`, `ModeQuarantined`), `GetRepoMode`, `SetRepoMode` (audit-logged).
- `internal/git/repo_mode_guard.go` — `AssertRepoWritable`; called as the second check (after `AssertNotDefaultBranch`) in every destructive git op (`ForcePushBranch`, `TriggerCIRerun`, `DeleteAskBranch`, `MergeAndCleanup`, `completeAskBranchResolution`).
- `cmd/force-bash-guard/main.go` — the standalone gatekeeper binary; allowlist / denylist / per-program rules / path denylist / curl host allowlist; logs to `bash.log`.
- `internal/agents/bash_guard_setup.go` — `setupBashGuardShim`, writes the per-worktree `bash` shim and exports both `PATH=` and `SHELL=` env entries (the latter is load-bearing per the 2026-04-29 empirical investigation; Claude CLI's Bash tool resolves the shell via `$SHELL`, not `PATH`).

The bash guard's allowlist (`allowedPrograms` in `cmd/force-bash-guard/main.go`) covers `git`, `gh`, `go`, `gofmt`, the popular package managers (`npm`, `yarn`, `pnpm`, `cargo`, `bun`, `deno`), test runners (`pytest`, `make`, `rustc`, `rustfmt`, `jest`, `vitest`, `mocha`, `phpunit`, `rspec`), read-only inspection (`ls`, `cat`, `grep`, `rg`, `head`, `tail`, `wc`, `diff`, `cmp`, `find`), light transforms (`awk`, `sed`, `jq`, `yq`), and the controlled-flag programs (`chmod`, `kill`, `curl`, `wget`). The denylist (`deniedPrograms`) covers `sudo`, `su`, `doas`, `dd`, `mkfs`, `shutdown`, `reboot`, `halt`, `poweroff`, `chown`, `passwd`. `rm` is intentionally NOT on the allowlist — operators use `git rm` for tracked files and explicit operator action for everything else.

## Invariants

1. **Profile is mandatory at every Claude call site.** Pattern P13 (`internal/audittools/audit_pattern_p13_capability_profiles_test.go`) walks AST nodes for every `AskClaudeCLI` / `AskClaudeCLIContext` / `RunCLI` / `RunCLIStreaming` / `RunCLIStreamingContext` call and rejects hardcoded tool-arg literals. Sourced args must come from `*capabilities.Profile.AllowedToolsArg()` / `.DisallowedToolsArg()` / `.MCPConfigArg()`.
2. **`LoadProfile` fails closed.** Missing YAML, unknown tool reference, blocklisted grant — all return errors and the agent does not start. There is no "all tools" fallback path.
3. **`--disallowedTools` is the hard restriction.** Per Fix #8e empirical findings, `--allowedTools` is an auto-approve hint under `--dangerously-skip-permissions`; the actual mechanical restriction comes from `--disallowedTools` (the COMPLEMENT of the profile + every blocklist entry).
4. **Inbound scrubbing is enforcement, not advisory.** `ScrubInbound` is fail-closed: when anything matches, the redacted prompt is what flows downstream; there is no "warn but send original" path (T0-2 anti-cheat).
5. **`.forceignore` symlink resolution.** `IsIgnored` evaluates patterns against the symlink-resolved path, so a `link.txt -> .env` alias does not bypass an `.env` rule (T0-2 anti-cheat).
6. **Bash guard wiring is verified.** Pattern P15 (`docs/patterns/p15-bash-guard.md`, `internal/audittools/audit_pattern_p15_bash_guard_test.go`) asserts astromech's `runAstromechTask` installs the shim AND that the shim exports both `PATH=` and `SHELL=`.
7. **Default repo mode is read-only.** Newly added repos start at `ModeReadOnly`; the operator must explicitly promote via `SetRepoMode` (audit-logged) before astromechs can do destructive git work.
8. **No silent failures.** Capability load errors, scrub-redaction state-update failures, repo-mode lookup errors, and bash-guard rejections all surface in operator-visible logs or mail; they never get swallowed (architecture invariant in CLAUDE.md).

## Configuration

- `agents/capabilities/<agent>.yaml` — per-agent grants. Profile shape: `agent`, `description`, `builtin_tools`, `mcp_servers`, `notes`. Adding a tool requires the corresponding entry in `REGISTRY.yaml`; profile reload requires daemon restart.
- `agents/capabilities/REGISTRY.yaml` — fleet vocabulary; `builtin_tools`, `mcp_tools`, `mcp_namespaces`. Adding a new tool is the first commit; granting it is a separate commit.
- `agents/capabilities/.forceblocklist.yaml` — `blocked` list of namespace tokens (`mcp:slack-write`) and concrete tool names. Removing an entry requires explicit operator action with audit-trail commit.
- Per-target-repo `.forceignore` — gitignore-syntax patterns. Symbol-resolved against repo root. Absent file = "no policy" (permits everything; the inbound scrubber is the safety net).
- SystemConfig keys (read by `cmd/force-bash-guard/main.go` at startup):
  - `bash_guard_curl_hosts` — comma-separated allowlist of hosts `curl`/`wget` may contact (default empty: every host rejected until the operator populates the list).
  - `bash_guard_log_max_bytes` — per-session `bash.log` cap (default 10 MiB; rotates to `bash.log.1`).
  - `inbound_redact_total_count` — running redaction total (auto-managed).
  - `inbound_redact_alert_threshold` — N (default 10) redactions between operator alerts.
  - `inbound_redact_last_alert_count` — last-emit dedup state.
- MCP config writes land under `~/.force/cache/mcp-configs/<agent>.json` (regenerated per call).
- Per-worktree shim writes to `<worktree>/.force-bash-guard-shim/bash`; bash log lives next to it as `<worktree>/bash.log`.

## Bash-guard rules in detail

`force-bash-guard` evaluates each compound segment (split on `&&`, `||`, `;`, `|`) as an independent argv vector and reaches a single allowed/denied verdict for the whole command — any rejected segment fails the entire input. Three structural rejections fire before per-program rules:

- `$(...)` command substitution.
- `<(...)` / `>(...)` process substitution.
- Backtick substitution.

Plus the canonical `:(){` fork-bomb pattern. Inline env-var assignments are stripped (`FOO=bar baz`), but `BASH_ENV`, `ENV`, and `PROMPT_COMMAND` cannot be set inline. Per-program rules (in `perProgramRules`) cover: no `rm` (use `git rm`), no `sed -i`, no `find -exec`/`-execdir`/`-delete`/`-ok`/`-okdir`, no `chmod -R`, no `kill -9 1`, and `curl`/`wget` host-allowlisted via `bash_guard_curl_hosts`. Path arguments are evaluated against `pathDenylist` (`/etc/`, `/var/`, `/usr/`, `/bin/`, `/sbin/`, `/boot/`, `/dev/`) AND a credential read denylist (`.ssh`, `.aws`, `.config/gh/hosts.yml`); both lexically-cleaned and symlink-resolved forms are checked so `..`-traversal and macOS `/etc → /private/etc` indirection both miss.

## Operator surface

```bash
force agents capabilities show <agent>      # render effective profile + diff vs blocklist
force agents capabilities lint              # validate every YAML against registry + blocklist
force agents capabilities diff a b          # compare two profiles
force repo mode <repo> [read_only|write|quarantined] [--reason ...]
force repo list                              # surface per-repo mode
make build-bash-guard                       # build the gatekeeper binary
```

Operator mail subjects (filterable):

- `[INBOUND REDACT — repo hygiene]` — fired once every `inbound_redact_alert_threshold` redactions; lists agent + task + redaction count and points the operator at `.forceignore`.
- `[FORCEIGNORE SKIP]` — log-line prefix on every gated file read (no mail; visible in daemon log).
- `force-bash-guard: REJECTED — …` — stderr emit when a Bash tool call is rejected; the rejection is also captured in `bash.log` and the agent's `LLMCallTranscripts` row.

The dashboard's Agents tab surfaces each agent's effective tool count and any blocklist conflicts; the Repos tab surfaces per-repo `mode`. When `inbound_redact_total_count` crosses the threshold, the operator-mail dedup logic in `recordInboundRedact` emits one summary mail rather than per-event spam.

Failure-mode behaviour (what happens when an enforcement layer trips):

| Layer | Trip | Operator-visible signal |
|---|---|---|
| Capability profile load | Missing YAML / unknown tool / blocklisted grant | Daemon log + agent does not start; cycle aborts |
| Inbound scrub | PEM / GCP key / `.env` line surfaced | Redacted in-place; `[INBOUND REDACT]` log; mail every N |
| `.forceignore` | Repo file matches a rule | `[FORCEIGNORE SKIP]` log line; file content not read |
| Repo-mode guard | Destructive op on `read_only` / `quarantined` repo | `store.ErrRepoNotWritable`; agent routes to `handleInfraFailure` |
| Bash guard | Disallowed program / host / path | `bash.log` entry; `force-bash-guard: REJECTED — …` to stderr; non-zero exit prevents `/bin/bash` exec |

## What this isn't

Force is not a hardened public service. The five layers above are defense in depth against operator mistakes and prompt-injection-via-ingested-content; they are not designed to repel an authenticated attacker on the operator's laptop. If you are evaluating Force for a context with adversarial ground truth, the answer today is "no" — the threat model needs revisiting first.

The dashboard binds to `127.0.0.1` only — there is no remote-listener configuration. To reach it from another machine, use an SSH tunnel; do not punch a hole in the loopback bind. The Holocron file (`holocron.db`) and its `-shm` / `-wal` siblings carry transcripts, redacted prompts, and operator email — `.forceignore.example` excludes them by default, but if the operator copies a target-repo `.forceignore` from elsewhere they should ensure the same exclusions are present.

## See also

- [`capability-profiles.md`](capability-profiles.md) — per-agent YAML deep dive.
- [`mcp-registry.md`](mcp-registry.md) — MCP server allowlist + injection.
- [`cli-shelling.md`](cli-shelling.md) — how profiles flow into the `claude -p` invocation.
- [`worktree-isolation.md`](worktree-isolation.md) — per-task worktree boundary the bash guard wraps.
- [`../patterns/p13-capability-profiles.md`](../patterns/p13-capability-profiles.md) — Pattern P13 (capability-profile call-site enforcement).
- [`../patterns/p15-bash-guard.md`](../patterns/p15-bash-guard.md) — Pattern P15 (bash-guard wiring + env).
- [`../closures/DELIVERABLE-1-CLOSURE.md`](../closures/DELIVERABLE-1-CLOSURE.md) — D1 pre-restart security closure (historical).
- [`../../FIX-LOG.md`](../../FIX-LOG.md) — Fix #8e narrative on `--disallowedTools` enforcement.
