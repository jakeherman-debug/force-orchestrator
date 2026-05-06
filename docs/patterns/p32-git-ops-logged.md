---
audience: agent
scope: Every git/gh subprocess invocation routes through internal/git's LogAndRun (GitOperationLog).
owner: D13
last_reviewed: 2026-05-05
title: Pattern P32 — Git ops logged
type: pattern-doc
pattern: P32
---

# Pattern P32 — Git ops logged

## Rationale

Every `git` and `gh` subprocess invocation must land in
`GitOperationLog` so the operator's drill / replay surfaces can see
what the agent ran, against which repo, and what came back. Direct
`exec.Command("git", …)` or `exec.CommandContext(ctx, "gh", …)` calls
bypass that capture.

D3 P6B.2 introduced `internal/git::LogAndRun`. The migration backlog
is recorded in `p32Allowlist` — each entry pairs a file path with the
reason routing-through-LogAndRun is deferred (typically: pre-DB boot
helpers, ExecRunner-wrapped CLIs, or comment-only references). D3
polish-pass iteration 2 (B4r) burned the 8-file backlog down to its
current 5 (4 internal/git + internal/gh).

## What it checks

`TestPattern_P32_GitOpsLogged` walks `internal/` and `cmd/` for
`*.go` (non-test). For each non-comment line:

1. Matches `p32CallPattern`:
   `exec\.Command(?:Context)?\([^)]*"(?:git|gh)"`.
2. If the file is in `p32Allowlist`, skip.
3. Otherwise the file:line is a violation.

Allowlist entries are validated for non-empty rationale.

## How it fails

```
Pattern P32 violation: direct git/gh exec calls outside igit.LogAndRun:

  internal/agents/foo.go:42 cmd := exec.CommandContext(ctx, "git", "fetch")

Fix: replace with igit.LogAndRun(ctx, igit.OpContext{...}, op, "git"|"gh", args...)
OR: add an allowlist entry to p32Allowlist with a one-line truthful rationale.
```

## How to fix

Replace the direct exec call with the wrapper:

```go
import igit "force-orchestrator/internal/git"

out, err := igit.LogAndRun(ctx, igit.OpContext{
    Agent:    "captain",
    Repo:     repoSlug,
    ConvoyID: convoyID,
}, "fetch", "git", "fetch", "origin")
```

`LogAndRun` writes the `GitOperationLog` row, threads ctx for
cancellation, and returns the combined stdout/stderr. Degrades
gracefully when no DB is attached (best-effort logging) — safe for
CLI-invoked entry points.

## Test reference

- File: `internal/audittools/audit_pattern_p32_git_ops_test.go`
- Core assertion: `TestPattern_P32_GitOpsLogged` (lines 90–169)
- Regex: `p32CallPattern` (lines 86–88).

## See also

- [P11 — exec.CommandContext threading](p11-exec-context.md)
- [P31 — LLM transcripts captured](p31-llm-transcripts.md)
- `internal/git/oplog.go` — the LogAndRun wrapper.
