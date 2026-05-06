---
audience: agent
scope: Astromech Claude sessions route Bash through force-bash-guard via PATH+SHELL env shim.
owner: D13
last_reviewed: 2026-05-05
title: Pattern P15 — Bash-guard wiring + env
type: pattern-doc
pattern: P15
---

# Pattern P15 — Bash-guard wiring + env

## Rationale

Astromechs run `claude` inside target-repo worktrees. The Bash tool the
session can invoke must be constrained to a curated allowlist of safe
programs; `force-bash-guard` is the binary the shim points at. The two
load-bearing pieces are:

1. The astromech's `runAstromechTask` must install the per-worktree
   PATH override AND pass the bash-guard env into the Claude
   subprocess.
2. The shim itself must export BOTH a `PATH=` entry AND a `SHELL=`
   entry. PATH-only wiring was the gap the 2026-04-29 empirical
   investigation surfaced — Claude CLI's Bash tool resolves the shell
   via `$SHELL` as an absolute path, so PATH-only never reaches the
   shim.

Pattern P15 is the static guard — `force-bash-guard`'s own test suite
plus the integration test in `bash_guard_setup_test.go` cover the
runtime exec semantics.

## What it checks

Two sub-tests:

1. `TestPattern_P15_BashGuardIntegrity` — three production files must
   contain specific load-bearing substrings:
   - `internal/agents/astromech.go`: `setupBashGuardShim`,
     `force-bash-guard`, `bashGuardEnv`.
   - `internal/agents/bash_guard_setup.go`: `force-bash-guard`,
     `BashGuardBinaryName`, `bashGuardShimDirName`,
     `setupBashGuardShim`, `bashShimSource`,
     `FORCE_BASH_GUARD_BIN`.
   - `cmd/force-bash-guard/main.go`: `allowedPrograms`,
     `deniedPrograms`, `evaluateCompound`.
2. `TestPattern_P15_BashGuardEnvWiring` — `bash_guard_setup.go` must
   contain the literal snippets that build the PATH and SHELL env
   entries and return them together.

## How it fails

```
Pattern P15 violation: internal/agents/astromech.go does not contain "bashGuardEnv" (bash-guard wiring missing)
```

Or for env-wiring:

```
Pattern P15 env-wiring: internal/agents/bash_guard_setup.go missing required snippet `shellEntry := fmt.Sprintf("SHELL=%s"`
  -> the bash-guard shim is unreachable in production without both PATH and SHELL entries
```

## How to fix

Restore the missing wiring; do not delete one of the env entries.
The expected end-of-`setupBashGuardShim` shape is:

```go
pathEntry := fmt.Sprintf("PATH=%s:%s", shimDir, os.Getenv("PATH"))
shellEntry := fmt.Sprintf("SHELL=%s", shimShellPath)
return []string{pathEntry, shellEntry}, nil
```

## Test reference

- File: `internal/audittools/audit_pattern_p15_bash_guard_test.go`
- Core assertions:
  - `TestPattern_P15_BashGuardIntegrity` (lines 30–83)
  - `TestPattern_P15_BashGuardEnvWiring` (lines 98–123)

## See also

- `cmd/force-bash-guard/main.go` — the shim binary.
- `internal/agents/bash_guard_setup.go` — the shim materializer.
