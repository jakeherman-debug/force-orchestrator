---
audience: operator
scope: force daemon update gates binary rollover behind ~/.force/trusted-binary-hashes with a 4-diff preview and an interactive paranoia prompt.
owner: security
last_reviewed: 2026-05-07
title: Pattern P_DaemonTrustFile — D12 binary-swap trust gate
type: pattern-doc
pattern: P_DaemonTrustFile
---

# Pattern P_DaemonTrustFile — D12 binary-swap trust gate

## Rationale

`force daemon update` swaps the running binary in place and arranges
for launchd / systemd to relaunch it. A swap with no human checkpoint
turns the daemon into a self-promoting RCE channel: any code path
that can drop a binary on disk and call `force daemon update` becomes
remote code execution against the operator's fleet.

D12 P1 closed the hole with a paranoia-default trust gate. Every swap:

1. Computes the candidate's SHA-256.
2. Looks the SHA up in `~/.force/trusted-binary-hashes`.
3. If unknown, prints a 4-diff preview (`git log`, `git diff --stat`,
   `config/*.yaml` drift, `internal/` drift) and prompts
   "Trust this binary? [y/N]". Default is no.
4. On `yes` (or `--assume-yes` for CI), appends a row to the trust
   file and proceeds.

Pattern P_DaemonTrustFile is the AST + grep regression that catches a
refactor of `cmd/force/daemon_cmds.go` that drops the trust lookup,
mangles the preview, or removes the interactive prompt.

Closure narrative:
[`docs/closures/DELIVERABLE-12-CLOSURE.md`](../closures/DELIVERABLE-12-CLOSURE.md)
("`force daemon update` requires explicit operator trust") and
[`docs/subsystems/daemon-lifecycle.md`](../subsystems/daemon-lifecycle.md)
§ "Trust file".

## What it checks

The single test `TestPattern_P_DaemonTrustFile` runs five checks:

1. **`trust.DefaultPath()` shape.** Calls the in-process function,
   asserts the returned path ends in `trusted-binary-hashes`. On any
   HOME-having system the path must contain `.force` (the trust file
   lives next to the PID file and the holocron).
2. **Trust package import.** AST-walks `cmd/force/daemon_cmds.go`
   and confirms the file imports
   `"force-orchestrator/internal/daemon/trust"`. Without the import
   the next two checks would be vacuous (nothing to call).
3. **`trust.Load(` and `trust.Append(` are both called.** Source-level
   grep — the file must contain both substrings. `Load` is the
   lookup; `Append` is the post-confirmation ratification write.
   Either alone is incomplete.
4. **4-diff preview present.** The file must reference each of
   `git log`, `git diff --stat`, `config/*.yaml`, and `internal/`.
   These are the four diffs the operator sees before deciding to
   trust an unknown binary.
5. **Interactive paranoia prompt + `--assume-yes` escape.** The file
   must contain the literal `Trust this binary` (the prompt the
   operator sees), plus both `--assume-yes` (the CLI flag) and
   `assumeYes` (the variable in the body that gates the prompt
   skip). Both spellings are required so a refactor that renames the
   flag or drops the variable trips the audit.

## How it fails

```
Pattern P_DaemonTrustFile: trust.DefaultPath() = "/tmp/foo", want suffix 'trusted-binary-hashes'
Pattern P_DaemonTrustFile: cmd/force/daemon_cmds.go does not import "force-orchestrator/internal/daemon/trust"
Pattern P_DaemonTrustFile: cmd/force/daemon_cmds.go does not call trust.Load( — update flow is incomplete
Pattern P_DaemonTrustFile: 4-diff preview missing "config/*.yaml" from update flow source
Pattern P_DaemonTrustFile: update flow missing the 'Trust this binary' interactive prompt — paranoia mode is the contract
Pattern P_DaemonTrustFile: --assume-yes flag missing — required for non-interactive testing
```

Typical violating snippet (lookup dropped, prompt deleted):

```go
// daemon_cmds.go cmdDaemonUpdate
sha := sha256OfFile(candidate)
if err := os.Rename(candidate, currentBinary); err != nil { return err }
// MISSING: trust.Load lookup, 4-diff preview, "Trust this binary?" prompt, trust.Append.
```

## How to fix

Wire the trust lookup + preview + prompt + append:

```go
import "force-orchestrator/internal/daemon/trust"

sha := sha256OfFile(candidate)
entries, _ := trust.Load(trust.DefaultPath())
if !entries.Contains(sha) {
    printGitLog(oldSHA, newSHA)
    printGitDiffStat(oldSHA, newSHA)
    printConfigYAMLDrift(oldSHA, newSHA)
    printInternalDrift(oldSHA, newSHA)
    if !assumeYes {
        if !askYesNo("Trust this binary? [y/N]") {
            return errors.New("operator declined to trust candidate binary")
        }
    }
    if err := trust.Append(trust.DefaultPath(), trust.Entry{SHA: sha, ...}); err != nil {
        return err
    }
}
// Proceed with swap.
```

The `--assume-yes` flag is required so CI can exercise the path.
Production operator runs MUST default to interactive — never invert
the default.

## Test reference

- File: `internal/audittools/audit_pattern_p_daemon_trust_test.go`
- Core assertion: `TestPattern_P_DaemonTrustFile`
- Helpers: imports `force-orchestrator/internal/daemon/trust` and
  calls `trust.DefaultPath()` directly; AST walks
  `cmd/force/daemon_cmds.go` for the import; uses `strings.Contains`
  for the 4-diff preview and the prompt grep.

## See also

- [P_DaemonProvenance](p-daemon-provenance.md) — produces the
  `provenance.Get().GitSHA` value the trust file records.
- [P_DaemonUpdateHistory](p-daemon-update-history.md) — the
  durable audit-trail companion to the trust file.
- [P_DaemonSingleton](p-daemon-singleton.md) — the flock that
  prevents two updates from racing.
- [`docs/subsystems/daemon-lifecycle.md`](../subsystems/daemon-lifecycle.md)
  § "Trust file" and § "Update flow".
- `internal/daemon/trust/trust.go` — Load / Append / DefaultPath.
