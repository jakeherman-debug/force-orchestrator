---
audience: agent
scope: Long-running subprocesses must thread a daemon-cancellable ctx through exec.CommandContext.
owner: D13
last_reviewed: 2026-05-05
title: Pattern P11 — exec.CommandContext threading
type: pattern-doc
pattern: P11
---

# Pattern P11 — `exec.CommandContext` threading

## Rationale

Long-running subprocess invocations must be cancellable when the daemon
is shutting down (SIGINT / e-stop). A bare `exec.Command(...)` detaches
the child from any caller context, and the cheat shape
`exec.CommandContext(context.WithTimeout(context.Background(), …), …)`
synthesizes a context that no caller can cancel. Fix #8d shipped a
ratio-based check that would still pass with half the sites regressed;
Fix #8e tightened it to a per-site check. The directive originates from
AUDIT-127 / AUDIT-158 / AUDIT-165.

D3 P1 follow-up B added a sub-test for the next layer up: agent code
calling `context.Background()` for an LLM call when the caller already
has a daemon ctx in scope.

## What it checks

Three sub-tests:

1. `TestPattern_P11_ExecCommandsUseContext` — for every production
   `*.go` file, rejects:
   - bare `exec.Command(` outside `shortExecAllowlist`,
   - `exec.CommandContext(context.WithTimeout(context.Background(), …)`
     anywhere (no allowlist exemption),
   - `exec.CommandContext(context.Background(), …)` anywhere.
2. `TestPattern_P11_FabricatedContextRejected` — fixture-driven proof
   the cheat-shape regex matches both fabricated forms and rejects
   legitimate ctx variables / wrapped caller ctx / unrelated bare
   exec.Command.
3. `TestPattern_P11_AllowlistReasonsTruthful` — every entry in
   `shortExecAllowlist` must mention either a network-op descriptor
   (push/fetch/ls-remote/clone) or a cancellation-mechanism descriptor
   (sub-second / dog-level / Ctrl-C / runner-layer / runWithTimeout /
   process group).
4. `TestPattern_P11_AgentCodeBackgroundCtx` — under `internal/agents/`,
   bare `context.Background()` is rejected (with carve-outs for
   `RespectNotificationBudget` / `emitOperatorMail*` wrappers, which
   do short SQLite queries, not subprocess spawns).

## How it fails

```
Pattern P11 (Fix #8e): N disallowed exec call(s) in production code:
  internal/foo/bar.go:42 — bare exec.Command in non-allowlisted production file — use exec.CommandContext(ctx, …) and thread a caller-supplied ctx
      exec.Command("git", "fetch")
...
Fix: thread a caller-supplied ctx through exec.CommandContext(ctx, …). If the caller has no ctx, surface that in the closure report — do NOT default to context.Background() silently.
```

Typical violating snippet:

```go
cmd := exec.Command("git", "fetch", "origin")
if err := cmd.Run(); err != nil { ... }
```

## How to fix

Thread `ctx` from the caller through `exec.CommandContext`:

```go
func (a *agent) refresh(ctx context.Context) error {
    cmd := exec.CommandContext(ctx, "git", "fetch", "origin")
    return cmd.Run()
}
```

For agent code calling Claude, use `claude.AskClaudeCLIContext(ctx, …)`
instead of `claude.AskClaudeCLI(…)`. See `chancellor.go`'s D3 P1
follow-up B (`runChancellorReview`, `synthesizeMergedPlan`) for the
canonical shape.

## Test reference

- File: `internal/audittools/audit_pattern_p11_exec_context_test.go`
- Core assertions:
  - `TestPattern_P11_ExecCommandsUseContext` (lines 116–205)
  - `TestPattern_P11_FabricatedContextRejected` (lines 213–253)
  - `TestPattern_P11_AllowlistReasonsTruthful` (lines 261–303)
  - `TestPattern_P11_AgentCodeBackgroundCtx` (lines 332–436)
- Key regexes: `fabricatedCtxRe`, `directBackgroundRe` (lines 86, 90).

## See also

- [P32 — Git ops logged](p32-git-ops-logged.md)
- [P31 — LLM transcripts captured](p31-llm-transcripts.md)
- [Fix #8d / #8e narrative in FIX-LOG.md](../../FIX-LOG.md)
