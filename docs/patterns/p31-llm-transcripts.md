---
audience: agent
scope: Every Claude CLI call site flows through claude.CallWithTranscript* (LLMCallTranscripts capture).
owner: D13
last_reviewed: 2026-05-05
title: Pattern P31 — LLM transcripts captured
type: pattern-doc
pattern: P31
---

# Pattern P31 — LLM transcripts captured

## Rationale

Every Claude CLI call must land in `LLMCallTranscripts` so the
operator's drill / replay / audit surfaces have a row to look at.
Direct calls to `AskClaudeCLI` / `AskClaudeCLIContext` /
`RunCLI` / `RunCLIStreaming` / `RunCLIStreamingContext` from agent
code would bypass that capture.

D3 P6B.1 introduced the wrapper layer
(`internal/claude/transcript.go::CallWithTranscript*`). D3
polish-pass B3 burned the 19-entry migration backlog: every
production agent now routes through one of the wrappers. Only two
structural exemptions remain — the wrapper file itself and
`claude.go` (where `AskClaudeCLI` is defined).

P31 also verifies the allowlist's rationale set never empties so a
future commit can't silently neutralise the audit.

## What it checks

`TestPattern_P31_AllLLMCallsCaptured` walks `internal/agents` and
`internal/claude` for `*.go` (non-test):

1. Skips comment lines.
2. Matches the regex
   `claude\.(?:AskClaudeCLI(?:Context)?|RunCLI(?:Streaming(?:Context)?)?)\b`
   on each remaining line.
3. For every hit, asserts the file is in `p31Allowlist`. The
   allowlist holds two structural entries:
   - `internal/claude/claude.go` (defines the helpers — wrapping
     would be infinite recursion).
   - `internal/claude/transcript.go` (IS the wrapper).
4. Asserts every allowlist entry's rationale is non-empty.

## How it fails

```
Pattern P31 violation: direct claude.* CLI calls outside the LLMCallTranscripts wrapper:

  internal/agents/foo.go:42 claude.AskClaudeCLIContext(ctx, sys, prompt, …)

Fix: route the call through claude.CallWithTranscript(ctx, claude.CallDescriptor{...}, ...)
OR: add an allowlist entry to p31Allowlist with a one-line truthful rationale.
```

## How to fix

Replace the direct call with the wrapper:

```go
res, err := claude.CallWithTranscript(ctx, claude.CallDescriptor{
    Agent:        "captain",
    ConvoyID:     convoyID,
    TaskID:       taskID,
    SystemPrompt: sys,
    UserPrompt:   prompt,
    Profile:      profile,
}, ...)
```

For streaming variants, use `CallWithTranscriptStreaming` /
`CallWithTranscriptOneShot` as appropriate. The wrapper handles
profile-arg expansion and writes the `LLMCallTranscripts` row before
returning.

## Test reference

- File: `internal/audittools/audit_pattern_p31_llm_transcripts_test.go`
- Core assertion: `TestPattern_P31_AllLLMCallsCaptured` (lines 86–178)
- Regex: `p31CallPattern` (lines 82–84).

## See also

- [P13 — Capability profiles](p13-capability-profiles.md)
- [P32 — Git ops logged](p32-git-ops-logged.md)
- `internal/claude/transcript.go` — the wrapper layer.
