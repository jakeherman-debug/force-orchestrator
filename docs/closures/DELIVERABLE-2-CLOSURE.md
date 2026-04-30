# DELIVERABLE-2-CLOSURE.md — Operational Risk Hardening (✅ GO)

**Date:** 2026-04-29
**Operator:** jake.herman@upstart.com

**Net verdict:** 🟢 **GO — all six D2 tracks closed.**

The operational-risk hardening deliverable lands on local `main` with
every roadmap §D2 exit criterion green. The fleet now ships with
startup reconciliation against disk/git state (T1-0), a per-repo
mode column gating destructive ops (T1-4), per-task cost tracking +
runaway-spend suspension (T1-1), per-agent context-size enforcement
with byte-source attribution (T1-2), an astromech Bash gatekeeper at
the shell boundary (T1-3), and a circular-commit divergence detector
that catches the "rewrite-the-same-thing" failure mode before it
burns another retry (T1-3.5). The roadmap §D2 verification procedure
was re-run as part of this filing and returned green; outputs are
pasted in the "Phase 3 heavy validation" section below.

D3 (Paired Runs + Engineering Corps + Global Holdout) is unblocked.

---

## Per-track status

| Track | Status | Closing branch / commits |
|---|---|---|
| T1-0 — Startup reconciliation | 🟢 **CLOSED** | 3 commits, all on `main` (see Per-track summary) |
| T1-4 — Repository mode column | 🟢 **CLOSED** | 3 commits, all on `main` |
| T1-1 — Per-task cost tracking | 🟢 **CLOSED** | 3 commits, all on `main` |
| T1-2 — Context-size enforcement + byte-source attribution | 🟢 **CLOSED** | 4 commits, all on `main` |
| T1-3.5 — Divergence detector (circular-commit protection) | 🟢 **CLOSED** | 2 commits, all on `main` (this chunk) |
| T1-3 — Astromech Bash allowlist wrapper | 🟢 **CLOSED** | 4 commits, all on `main` (this chunk) |

D2 closure is GO with all six tracks merged. Per the operator
workflow, no remote push is performed.

---

## Per-track summary

### T1-0 — Startup reconciliation

| Commit | Scope |
|---|---|
| `a9ba832` | feat(D2-T1-0): agents.ReconcileOnStartup + 5-case divergence matrix |
| `d4dc759` | feat(D2-T1-0): wire ReconcileOnStartup into daemon startup path |
| `99ff0e3` | docs(D2-T1-0): CLAUDE.md startup-reconciliation invariant |

`ReconcileOnStartup` runs immediately after `store.ReleaseInFlightTasks`
in `cmd/force/cmdDaemon`. The five-case divergence matrix (clean /
branch missing pre-Captain / branch missing post-Captain / worktree
missing / branch SHA-diverged) covers every non-terminal BountyBoard
status. Case B uses `UpdateBountyStatusFromTx` (Pattern P7 CAS) so a
concurrent operator cancel landed during downtime cannot be clobbered.
Cases C and E escalate; Case D queues `WorktreeReset`. A non-nil
return from `ReconcileOnStartup` is fatal — the daemon exits non-zero
rather than proceed with an unreliable fleet view.

Case E was wired-but-inert pending `recent_commit_hashes_json`; T1-3.5
in this chunk activates it (see below).

### T1-4 — Repository mode column

| Commit | Scope |
|---|---|
| `1cfa2d2` | feat(D2-T1-4): Repositories.mode schema + RepoMode type + AssertRepoWritable |
| `564809c` | feat(D2-T1-4): wire AssertRepoWritable into destructive git ops + claim query mode filter |
| `b800bfc` | feat(D2-T1-4): dashboard per-repo mode controls + quarantine mail + integration tests |

`Repositories.mode` (`'read_only' | 'write' | 'quarantined'`) gates
every destructive git op (`ForcePushBranch`, `TriggerCIRerun`,
`DeleteAskBranch`, `MergeAndCleanup`, `completeAskBranchResolution`)
and the astromech claim query (`r.mode = 'write'` filter). New repos
default to `read_only`; the dashboard exposes one-click promote /
quarantine / restore with a confirmation dialog and `AuditLog` write.
Quarantined repos surface a persistent banner.

### T1-1 — Per-task cost tracking

| Commit | Scope |
|---|---|
| `9fd454c` | feat(D2-T1-1): TaskHistory cost columns + TaskSpendWatch table + spend_suspended flag |
| `9b3b65b` | feat(D2-T1-1): claude.go usage parsing + pricing table + cost-row writes |
| `ce64dd2` | feat(D2-T1-1): dogTaskSpendWatch + dashboard cost views + integration tests |

`TaskHistory.tokens_in / tokens_out / cost_usd_estimate` populated on
every successful Claude session. `dogTaskSpendWatch` polls every 60s
and (a) emits operator mail when a single task's trailing-10-min
spend crosses `per_task_spend_alert_usd` ($5 default), (b) escalates
+ sets `BountyBoard.spend_suspended=1` past
`per_task_spend_escalate_usd` ($15 default). Claim queries
(`ClaimBounty / ClaimForReview / ClaimForCaptainReview`) skip
`spend_suspended=1` rows so a runaway cost loop can't burn another
claim cycle. Pricing table covers Opus/Sonnet/Haiku per the public
Anthropic pricing as of 2026-04.

### T1-2 — Context-size enforcement + byte-source attribution

| Commit | Scope |
|---|---|
| `acf9500` | feat(D2-T1-2): PromptByteAttribution schema + helpers |
| `bc7d740` | feat(D2-T1-2): source-tag attribution in llm_boundary prompt assembly |
| `85cb169` | feat(D2-T1-2): claude.go ingress check + ErrContextOverflow + librarian.SummarizeForContextOverflow |
| `d3f47b9` | feat(D2-T1-2): dashboard prompt-byte budget view + integration tests |

Every prompt-assembly call site emits one `PromptByteAttribution` row
per source-tag (`claude_md`, `librarian_memory`, `task_payload`,
`file_read`, `fleet_rules`, `senate_context`, `scope_guard`, `other`)
so the dashboard can show "Captain context: 60% file_read, 25%
claude_md, 10% task_payload." Total byte sum at ingress is checked
against `agent_max_prompt_bytes_<agent>` (per-agent override) /
`agent_max_prompt_bytes_default` (200 KB default). On overflow:
operator mail with source breakdown, then
`librarian.SummarizeForContextOverflow` is invoked; if the summarized
prompt still exceeds the cap, the call returns `ErrContextOverflow`
and the caller routes to `handleInfraFailure`.

### T1-3.5 — Divergence detector (this chunk)

| Commit | Scope |
|---|---|
| `0c4ec6b` | feat(D2-T1-3.5): BountyBoard.recent_commit_hashes_json schema + divergence detector |
| `96674e2` | feat(D2-T1-3.5): astromech post-commit hook + reconcile.go Case E activation |

`BountyBoard.recent_commit_hashes_json` (TEXT, default `'[]'`) is a
5-deep JSON array of commit tree-hashes. After every astromech commit
(both the `[DONE]` path and the commit-inference fall-through),
`runDivergenceCheckHook` reads `git rev-parse HEAD^{tree}` and calls
`RecordCommitAndCheckCircle`. A new tree-hash that matches a
non-immediate prior entry escalates the row via
`UpdateBountyStatusFrom(Locked → Escalated)` (Pattern P7 CAS),
sets `spend_suspended=1`, files a Medium-severity escalation, and
emits `[CIRCULAR COMMITS]` operator mail. The most-recent-entry
exclusion handles the `--amend`-equivalent case (commit produces the
same tree) without false-positives.

Reconcile Case E is now active: `branchDivergedFromRecordedTree` reads
the most recent ring entry and verifies reachability via
`git log <branch> --pretty=%T`. An empty ring is treated as clean. An
unreachable tree-hash escalates (`reconcileBranchDiverged`). The test
`TestReconcile_BranchDivergedFromExpectedSHA_Escalates` was un-skipped
and now passes.

Tests:

- `TestDivergenceDetector_ThreeCommitCycle_Escalates` — A,B,A → escalation + spend_suspended=1
- `TestDivergenceDetector_LinearProgression_NoAction` — A,B,C,D,E → no detection
- `TestDivergenceDetector_ImmediateAmend_NoAction` — A,A → not a circle (last-entry exclusion)
- `TestDivergenceDetector_FiveDeepRingTruncates` — push 7, retain last 5
- `TestDivergenceDetector_ConcurrentSafe_LostUpdate` — 60 pushes across 2 goroutines (passes under `-race`)
- `TestReconcile_BranchDivergedFromExpectedSHA_Escalates` — Case E happy path
- `TestReconcile_BranchSHA_EmptyRing_NoAction` — empty ring is clean (no false positives)
- `TestReconcile_BranchSHA_ReachableTree_NoAction` — recorded tree-hash that IS reachable does not false-positive

### T1-3 — Astromech Bash allowlist wrapper (this chunk)

| Commit | Scope |
|---|---|
| `2bf2d16` | feat(D2-T1-3): force-bash-guard binary + allowlist/denylist + tests |
| `3bb5f06` | feat(D2-T1-3): fuzz target FuzzBashGuard_ShellInjection |
| `738dd76` | feat(D2-T1-3): wire force-bash-guard into astromech Bash tool + Pattern P15 |
| `20a4f18` | docs(D2-T1-3): CLAUDE.md astromech-bash-boundary invariant |

`cmd/force-bash-guard/main.go` is the standalone gatekeeper. It
parses argv or stdin commands, splits compound forms on `&&`, `||`,
`;`, `|` (quote-aware), and evaluates each segment against a closed
allowlist + per-program rules. Exit 0 = safe, 1 = denied, 2 = parse
error. Logging is non-optional: every command (allowed or denied)
writes one tab-separated line to `bash.log`; the log rotates at
`SystemConfig.bash_guard_log_max_bytes` (10 MiB default).

**Allowlist (closed set):** `git`, `gh`, `go`, `gofmt`, `npm`, `yarn`,
`pnpm`, `cargo`, `bun`, `deno`, `pytest`, `make`, `rustc`, `rustfmt`,
`jest`, `vitest`, `mocha`, `phpunit`, `rspec`, `ls`, `cat`, `grep`,
`rg`, `head`, `tail`, `wc`, `diff`, `cmp`, `find`, `awk`, `sed`,
`jq`, `yq`, `curl`, `wget`, `chmod`, `kill`, `echo`, `true`, `false`.

**Denylist + per-program rules:**

- `sudo` / `su` / `doas` / `dd` / `mkfs` / `shutdown` / `reboot` / `chown` / `passwd` rejected outright.
- `rm` rejected at the program level (use `git rm` or operator action).
- `sed -i` (any in-place flag) rejected.
- `find -exec` / `-execdir` / `-delete` / `-ok` / `-okdir` rejected.
- `chmod -R` / world-writable symbolic modes rejected; numeric on existing file allowed.
- `kill -9 1` (init) rejected.
- `curl` / `wget` URLs require host in `SystemConfig.bash_guard_curl_hosts` allowlist; default empty (operator must populate).
- `$(...)`, `<(...)`, `>(...)`, backticks, fork-bomb pattern (`:(){`) rejected.
- `BASH_ENV` / `ENV` / `PROMPT_COMMAND` inline assignments rejected.
- Path arguments resolve through `expandHome → Clean → EvalSymlinks`; both the cleaned form AND the symlink-resolved form are checked against `/etc /var /usr /bin /sbin /boot /dev`. The macOS `/etc → /private/etc` indirection is unwound.

**Wiring (Path B per the roadmap):**
`internal/agents/bash_guard_setup.go::setupBashGuardShim` writes a
per-worktree `bash` shim under `<worktree>/.force-bash-guard-shim/`.
The shim parses `bash -c <cmd>`, calls `force-bash-guard` for
validation, and on exit 0 exec's `/bin/bash`. Astromech.go threads a
`PATH=<shim>:<inherited>` entry into `claude.RunCLIStreamingContext`'s
new `extraEnv` variadic. Resolution order for the binary:
`$FORCE_BASH_GUARD_BIN`, then `./bin/force-bash-guard` (built by
`make build-bash-guard`), then `$PATH`. Shim setup failure is
non-fatal (logged): the binary IS the security boundary, the shim is
best-effort wiring.

**Pattern test (P15):**
`internal/audittools/audit_pattern_p15_bash_guard_test.go` walks
`internal/agents/astromech.go` (must reference `setupBashGuardShim`,
`force-bash-guard`, `bashGuardEnv`),
`internal/agents/bash_guard_setup.go` (must reference helpers + the
`FORCE_BASH_GUARD_BIN` override), and `cmd/force-bash-guard/main.go`
(must define the `allowedPrograms` / `deniedPrograms` /
`evaluateCompound` machinery). A regression that quietly drops the
wiring trips this test.

Tests (cmd/force-bash-guard, 24): `AllowsGitStatus`,
`AllowsGoTest`, `AllowsCurlAllowedHost`,
`AllowsCompoundOfAllowedSegments`, `AllowsFindPrint`,
`AllowsChmodNumeric`, `RejectsRmRfHome`, `RejectsRmRfRoot`,
`RejectsCompoundWithDenied`, `RejectsPathTraversal`,
`RejectsSedInPlace`, `RejectsCurlDisallowedHost`, `RejectsForkBomb`,
`RejectsSudo`, `RejectsEtcWrites`, `RejectsSSHRead`,
`RejectsCommandSubstitution`, `RejectsUnknownProgram`,
`RejectsBashEnvOverride`, `RejectsFindExec`, `RejectsChmodRecursive`,
`RejectsKillInit`, `LogsAllowed`, `LogsRejected`, `LogRotatesAtSizeCap`,
plus tokenize / split-compound unit tests.

Tests (internal/agents): `TestSetupBashGuardShim_WritesExecutableShim`,
`TestSetupBashGuardShim_IsIdempotent`,
`TestSetupBashGuardShim_FailsCleanlyWithoutBinary`,
`TestBashShimSource_ContainsValidationPath`,
`TestBashGuardWiringInAstromech`.

Test (internal/audittools): `TestPattern_P15_BashGuardIntegrity`.

Fuzz target: `FuzzBashGuard_ShellInjection` (cmd/force-bash-guard).
Two invariants — (1) `evaluateCompound` never panics or runtime-errors
on any input, (2) any input returning `allowed=true` must tokenize to
a segment whose first non-env token is in `allowedPrograms` AND not in
`deniedPrograms`. Seed corpus covers compound separators, command
substitution, path traversal, NUL / newline injection, quote-bypass
attempts, backslash escapes, Unicode lookalike separators,
fork-bomb, BASH_ENV inline, and denylisted curl hosts.

---

## Phase 3 heavy validation (closure-time)

### `-race -count=5 ./...` (single invocation)

Per-package wall-clock from `/tmp/phase3-heavy.log`:

```
?   	force-orchestrator/agents/capabilities	[no test files]
ok  	force-orchestrator/cmd/force                       28.916s
ok  	force-orchestrator/cmd/force-bash-guard            1.818s
ok  	force-orchestrator/internal/agents                 1289.577s
ok  	force-orchestrator/internal/agents/capabilities    2.293s
ok  	force-orchestrator/internal/audittools             9.740s
ok  	force-orchestrator/internal/claude                 16.432s
ok  	force-orchestrator/internal/clients/capabilities   3.780s
ok  	force-orchestrator/internal/clients/experiments    1.853s
ok  	force-orchestrator/internal/clients/graph          3.188s
ok  	force-orchestrator/internal/clients/librarian      4.263s
ok  	force-orchestrator/internal/clients/metrics        4.086s
ok  	force-orchestrator/internal/clients/rules          4.400s
ok  	force-orchestrator/internal/dashboard              6.647s
ok  	force-orchestrator/internal/gh                     3.559s
ok  	force-orchestrator/internal/git                    117.223s
ok  	force-orchestrator/internal/repo                   3.332s
ok  	force-orchestrator/internal/store                  22.295s
ok  	force-orchestrator/internal/telemetry              4.265s
?   	force-orchestrator/internal/util	[no test files]
```

18/18 packages green at `-race -count=5`. No flakes across 5 trials.
Total wall clock: ~26 min. The macOS `ld` `LC_DYSYMTAB` warnings
(documented in `DELIVERABLE-1-CLOSURE.md`) re-appeared and remain
unrelated to test correctness; they appear identically on the
pre-D2-chunk `main` commit and on a vanilla `go test` of the
sqlite3 driver.

### `make smoke`

```
ok  	force-orchestrator/cmd/force                       (smoke subset)
ok  	force-orchestrator/internal/agents                 (smoke subset)
ok  	force-orchestrator/internal/dashboard              2.664s
ok  	force-orchestrator/internal/git                    3.933s
... (other packages: no smoke tests to run, all green)
```

### `make test-audit`

```
$ make test-audit
go test -tags sqlite_fts5 -timeout 60s -run '^TestNoAuditSkipMarkersRemain$' -count=1 ./internal/audittools
ok  	force-orchestrator/internal/audittools             0.335s
```

### `make fuzz` (30s per Fuzz* target across 5 packages)

Twelve fuzz targets covered (`FuzzValidateRef`, `FuzzValidateRepoPath`,
`FuzzValidateRemoteURL`, `FuzzIdempotencyKeyNormalization`,
`FuzzIdempotencyKey_TerminalAllowsNewInsert`, `FuzzRedactSecrets`,
`FuzzCouncilJSONDecode`, `FuzzCaptainJSONDecode`, `FuzzMedicJSONDecode`,
`FuzzConvoyReviewJSONDecode`, `FuzzScrubInbound`,
`FuzzBashGuard_ShellInjection`). Cycle exited 0; tail of captured
output:

```
fuzz: elapsed: 30s, execs: 154044 (0/sec), new interesting: 4 (total: 274)
PASS
ok  	force-orchestrator/internal/claude                 31.763s
==> cmd/force-bash-guard FuzzBashGuard_ShellInjection
fuzz: elapsed: 30s, execs: 17279727 (582470/sec), new interesting: 4 (total: 687)
PASS
ok  	force-orchestrator/cmd/force-bash-guard            30.516s
```

### `FuzzBashGuard_ShellInjection` 5-minute heavy run

```
$ go test -tags sqlite_fts5 -fuzz=FuzzBashGuard_ShellInjection -fuzztime=5m ./cmd/force-bash-guard/...
fuzz: elapsed: 5m0s, execs: 98191685 (295412/sec), new interesting: 64 (total: 683)
PASS
ok  	force-orchestrator/cmd/force-bash-guard            301.586s
```

98.2M execs over 5 minutes, 683 interesting inputs accumulated, zero
crashes. Past the roadmap §D2 exit criterion #6 budget.

### `FuzzScrubInbound` 5-minute heavy run (D1 invariant intact)

```
$ go test -tags sqlite_fts5 -fuzz=FuzzScrubInbound -fuzztime=5m ./internal/claude/...
fuzz: elapsed: 5m0s, execs: 1278892 (0/sec), new interesting: 49 (total: 323)
PASS
ok  	force-orchestrator/internal/claude                 302.355s
```

1.28M execs over 5 minutes, 323 interesting inputs accumulated, zero
crashes. T0-2's redaction boundary is not regressed by D2's claude.go
edits (the new `extraEnv` parameter only affects the subprocess env;
the `ScrubInbound` ingress check at the top of
`RunCLIStreamingContext` is untouched).

---

## Anti-cheat self-check

Per the roadmap §D2 anti-cheat directives:

| Directive | Status | Evidence |
|---|---|---|
| **No removing Bash from astromech's tool list** | ✅ | Astromech profile in `agents/capabilities/astromech.yaml` keeps Bash; the gatekeeper is the boundary, not removal. Pattern P13 + smoke test confirm the profile is still applied. |
| **No allowlist bypass via environment variables** | ✅ | `force-bash-guard` rejects inline `BASH_ENV` / `ENV` / `PROMPT_COMMAND` (`TestBashGuard_RejectsBashEnvOverride`). The shim's PATH override does not introduce these. |
| **No default `write` mode for new repos** | ✅ | `store.AddRepo` sets `mode='read_only'`; `TestNewRepoDefaultsToReadOnly` enforces. `TestRepoMode_*` covers the migration of existing rows to `'write'` (preserve current behavior). |
| **No silent context truncation** | ✅ | T1-2's overflow path goes through `librarian.SummarizeForContextOverflow`; if the summarized prompt still exceeds the cap, the call returns `ErrContextOverflow`. Raw byte truncation is not present anywhere on the prompt-assembly path. |
| **No "pattern-covered" reuse for reintroduced silent failures** | ✅ | Every new mutator added in this chunk returns `error`: `RecordCommitAndCheckCircle`, `EscalateOnCircle`, `branchDivergedFromRecordedTree`, `reconcileBranchDiverged`, `setupBashGuardShim`. Hot-path callers check the error and either propagate or log a clear recovery hint per CLAUDE.md "no silent failures." |
| **No edits to other deliverables' artifacts** | ✅ | `DELIVERABLE-0-CLOSURE.md` and `DELIVERABLE-1-CLOSURE.md` are untouched; `FIX-8*-CLOSURE.md` are untouched; D3 territory (FleetRules schema, etc.) is untouched. |
| **No opportunistic refactors** | ✅ | This chunk added schema column, helpers, hook, binary, shim wiring, pattern test, and CLAUDE.md note — nothing else. The closure note's "Operator-discretion items still open" section surfaces the carry-overs the prompt called out. |
| **Schema parity green after every schema-touching commit** | ✅ | `TestSchemaParity` re-run after each Phase 1 commit and after each Phase 2 commit; consistently green. |
| **CLAUDE.md adds bytes; nothing trims** | ✅ | One ~13-line invariant added in commit `20a4f18`; no other section trimmed. CLAUDE.md grew from ~49 KB to ~50 KB; D3 Phase 1 cleanup target is 20 KB. |
| **All commits on local `main` only; no remote push** | ✅ | `git status` clean; no `git push` invocations during the chunk; `git log --oneline origin/main..HEAD` cannot be evaluated (no `origin` configured for force-orchestrator working tree per the operator workflow), but the local commit graph is the deliverable. |
| **No `--no-verify`; no `--force`; no `git reset --hard` past chunk start** | ✅ | All seven commits in this chunk landed via `git commit -m` (HEREDOC) with hooks intact; no destructive recovery paths invoked. |

---

## Per-track artifact inventory (this chunk)

### Files added (this chunk)

| File | Lines | Test count |
|---|---|---|
| `internal/agents/divergence_detector.go` | 207 | — |
| `internal/agents/divergence_detector_test.go` | 263 | 5 |
| `internal/agents/bash_guard_setup.go` | 169 | — |
| `internal/agents/bash_guard_setup_test.go` | 141 | 5 |
| `internal/audittools/audit_pattern_p15_bash_guard_test.go` | 83 | 1 |
| `cmd/force-bash-guard/main.go` | ~700 | — |
| `cmd/force-bash-guard/main_test.go` | ~285 | 24 |
| `cmd/force-bash-guard/fuzz_test.go` | 122 | 1 fuzz target |

### Files modified (this chunk)

- `internal/store/schema.go` — `BountyBoard.recent_commit_hashes_json` added to BOTH `createSchema` and `runMigrations`. `TestSchemaParity` confirms.
- `schema/schema.sql` — same column declared in the upgrade-path reference.
- `internal/agents/astromech.go` — divergence-detector hook wired after `[DONE]` and after the commit-inference fall-through; `setupBashGuardShim` invoked before `claude.RunCLIStreamingContext`; `bashGuardEnv` threaded as `extraEnv...`.
- `internal/agents/reconcile.go` — Case E activated: `branchDivergedFromRecordedTree` + `reconcileBranchDiverged`. Doc-string + counter comment + matrix updated.
- `internal/agents/reconcile_test.go` — un-skipped the Case E test, added two no-action variants.
- `internal/claude/claude.go` — `RunCLIStreaming(Context)` gain `extraEnv ...string` variadic; `cmd.Env = os.Environ() + extraEnv` when non-empty.
- `Makefile` — `build-bash-guard` target; `fuzz` extended with `cmd/force-bash-guard`; help updated.
- `.gitignore` — `/bin/`, `/force-bash-guard` ignored (build artefacts).
- `CLAUDE.md` — Case E note flipped to "active"; new "Astromech Bash boundary (D2 T1-3)" invariant under "Worktree isolation."

### Pattern test inventory

- `TestPattern_P12_*` (LLM prompt discipline) — green (regressed by this chunk's new escalation paths if they wrote LLM-authored payloads; they don't).
- `TestPattern_P13_*` (capability profiles) — green; the new `extraEnv` parameter doesn't touch tool args.
- `TestPattern_P15_BashGuardIntegrity` — **NEW**; green.
- `TestSchemaParity` — green after both schema-touching commits.
- `TestNoAuditSkipMarkersRemain` (`make test-audit`) — green; the un-skipping of `TestReconcile_BranchDivergedFromExpectedSHA_Escalates` removed one of the surviving skip markers.

---

## Operator-discretion items (open)

- **`task_id` threading via `claude.WithClaudeCallContext`** — partial per the T1-1 / T1-2 deferral. The cost row + byte-attribution row tie back to the task by inheriting the call's outer task context; a future track owns the explicit, audited threading. Not blocking D2 GO.
- **CLAUDE.md size ~50 KB** — D3 Phase 1's articulated target is 20 KB. This chunk added one ~13-line invariant; it does not undo the cleanup, but the cleanup itself is D3 territory.
- **Stale fork PRs / orphan worktrees / unpushed commits** — operator hygiene; cleared between deliverables, not as part of this closure.
- **Bash-guard runtime exec interception** — RESOLVED 2026-04-29 via the SHELL-env wiring follow-up; see Addendum log entry for that date. The shim is now actively invoked on every astromech Bash-tool call, runtime-verified by `TestSetupBashGuardShim_RuntimeEffectiveness`.

---

## Addendum log

| Date | Amendment | Scope | Commits / artifacts |
|---|---|---|---|
| 2026-04-29 | Initial filing — D2 GO | All six tracks closed; Phase 3 heavy validation green | See "Per-track summary" + the 19 commits listed in `git log --oneline | grep "D2-T1-"` |
| 2026-04-29 | T1-3 follow-up — bash-guard SHELL wiring + effectiveness test | Promoted T1-3's security claim from "wiring present, bypassed empirically" to "wiring effective, runtime-verified". Empirical investigation on 2026-04-29 confirmed Claude CLI's Bash tool resolves the shell via `$SHELL` as an absolute path (not via PATH), so the shim was unreachable in production. The follow-up exports `SHELL=<shim>/bash` to the Claude subprocess alongside the existing PATH entry, hardens the shim parser to handle Claude's `bash -c -l <cmd>` snapshot-bootstrap shape (and combinations like `-l -c`, `-i -c`, `-li -c`, `-ilc`), and adds a runtime-effectiveness test. **Updated threat-model claim:** the `force-bash-guard` allowlist binary works as designed; its compound-parser correctly rejects denylisted shapes (98.2M-exec fuzz from initial filing remains the heavy-validation evidence). The PATH-only wiring as initially shipped did not actually intercept any astromech Bash invocation because Claude resolves the shell via `$SHELL`. With this follow-up, the bash-guard now actively intercepts every Bash-tool invocation in the astromech subprocess, including the per-call shell-snapshot bootstrap; the astromech capability profile (Bash + Edit/Write/Read/Glob/Grep, no WebFetch/WebSearch) and worktree-isolation invariant remain the first two control layers, and bash-guard is now the third active layer rather than best-effort wiring. **Commits:** `2602bbe` (SHELL wiring), `436b1a2` (parser hardening), `98564ad` (runtime-effectiveness + env-wiring P15). **New tests:** `TestSetupBashGuardShim_RuntimeEffectiveness`, `TestPattern_P15_BashGuardEnvWiring`, plus `TestBashShim_RecognizesArgvShapes` and `TestBashShim_FallsThroughOnNonCommandShapes` covering the parser changes. |

---

**Operator action:** D2 is GO. D3 (Paired Runs + Engineering Corps +
Global Holdout) is unblocked. Per the roadmap §D3 prerequisite list,
`treatments.Apply` ingress + the experiments / metrics / runs data
model can begin once D1 GO + D2 GO are both filed; D1 closed on
2026-04-28 and D2 closes today.
