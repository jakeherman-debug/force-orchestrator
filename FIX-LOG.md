# Fix Log

Operator narrative for each audit-fix PR. Written as each fix merges to main.
Each entry answers: what broke, what shipped, how it was proved, what to
watch for next.

## Fix #0 — Protected-branch guard

**AUDIT IDs closed:** AUDIT-102, AUDIT-103, AUDIT-104, AUDIT-121, AUDIT-122, AUDIT-124

**Branch:** `fix-0-protected-branch-guard`

**What broke.** Every destructive git op in `internal/git` consumed its
`branch` argument without checking whether it named the repo's default
branch. A single DB-corrupt `ConvoyAskBranches.ask_branch = "main"` row
(from a manual edit or a migration bug) would flow through
`completeAskBranchResolution` and become `git push --force-with-lease origin
main`. In parallel, `pilot_rebase.go:77` hardcoded `defaultBranch = "main"`
as a fallback — so any master-default repo with an empty
`repos.default_branch` looped forever trying to rebase onto a nonexistent
ref, and `pr_flow.go:709` fell back to `branch := pr.Repo` when the parent
task's `branch_name` was empty — a short repo name could collide with the
default branch name and trigger the CI-rerun empty-commit push on origin/main.

**What shipped.**

- New helper in `internal/git/protected.go`:
  - `AssertNotDefaultBranch(repoPath, branch string) error` — three layers:
    empty branch rejected, hard denylist (main/master/develop/trunk/
    production/prod/HEAD, case- and ref-prefix-insensitive), and a repo-aware
    `GetDefaultBranch(repoPath)` check when the path is provided.
  - `IsValidAskBranch(branch string) bool` — checks the
    `<prefix>force/ask-<digits>-<slug>` shape.
  - `IsProtectedBranchName(branch string) bool` — exported subset for store
    ingress validators that can't import `internal/git`.
  - `ErrProtectedBranch` sentinel wrap target.
- Guard installed at the top of `ForcePushBranch`, `TriggerCIRerun`,
  `DeleteAskBranch`, `MergeAndCleanup`, and
  `completeAskBranchResolution`. Every one refuses the op before shelling
  out to git.
- `completeAskBranchResolution` additionally checks
  `IsValidAskBranch(ab.AskBranch)` — a well-formed DB row with a
  default-branch name IS still rejected.
- `pilot_rebase.go:77` replaced its `"main"` literal fallback with
  `igit.GetDefaultBranch(repo.LocalPath)` — master-default repos stop
  looping.
- `pr_flow.go:709` dropped the `branch := pr.Repo` fallback. When the
  parent task's `branch_name` is empty, we escalate instead of pushing to a
  guessed branch.
- Store ingress: `UpsertConvoyAskBranch` now rejects protected branch names
  at write time via a local `isProtectedAskBranchName` helper (duplicated
  denylist to keep the `store → git` layering intact). A corrupt or
  mis-migrated DB cannot admit a "main" row.

**How it was proved.**

- `TestAUDIT_102_103_104_121_122_124_ProtectedBranchGuardsMissing` — 7
  subtests in `internal/git/audit_protected_branch_test.go`. Red-phase
  skips removed; post-Fix assertions inverted so the test now acts as
  permanent regression protection. Also fixed a latent bug in the test's
  `extractFuncBody` helper that mis-reported function bodies when the
  signature contained an inline interface (`logger interface{ Printf... }`).
- `TestAssertNotDefaultBranch_HardDenylist` — 14 cases, table-driven
  unit coverage of the validator (canonicalisation, case-insensitivity,
  ref-prefix stripping, empty input).
- `TestAssertNotDefaultBranch_AllowsAskBranches` — 8 positive cases so
  the denylist doesn't over-broaden.
- `TestAssertNotDefaultBranch_HonoursRepoDefault` — integration; makes a
  real temp repo and confirms the discovered default is rejected.
- `TestForcePushBranch_RefusesProtectedBeforeShellout` — integration;
  calls against a non-existent repo path to prove the guard fires BEFORE
  `git -C` would ever run.
- `TestTriggerCIRerun_RefusesProtectedBeforeShellout` — ditto for the
  CI-rerun path.
- `TestAddRepo_ProtectedBranchFlow` — acceptance; drives the real
  `cmdAddRepo` CLI helper against a live git repo, then proves post-
  registration the store still rejects `ask_branch = "main"`.

**Stats.**

- 14 new unit sub-cases + 8 allow-case sub-cases in
  `protected_test.go` (all t.Parallel).
- 1 repo-aware unit test + 2 integration tests in same file.
- 1 acceptance test in `cmd/force/fix0_addrepo_protected_test.go`.
- 7 audit-test subtests flipped from Red to Green in
  `audit_protected_branch_test.go`.

**Known pre-existing issue surfaced during Fix #0 verification.**

`TestEmitEvent_WithOTLPEndpoint` in `internal/telemetry/telemetry_test.go`
races under `go test -race -count=1` (reproduced against bare main before
any Fix #0 change). The test launches an async HTTP POST goroutine and
resets `otlpEndpoint` / `otlpHTTPClient` in a deferred cleanup without
waiting for the goroutine. This is unrelated to the protected-branch
guard — noted here because the original fix prompt asked for `-race`
cleanliness. The project's canonical `make test` runs without `-race`,
and the full suite is green there. The race belongs in the Fix #10
outbound-channels scope (same file owns OTLP export).

**Watch for.**

- If a future pair of agents needs to rewrite a protected branch for a
  legitimate reason (e.g. repository-init flow creating the default branch
  as a first commit), they'll need to bypass the guard explicitly. That
  bypass must go through a new entry point, not a loosening of
  `AssertNotDefaultBranch` — adding an explicit opt-in argument is
  preferable to relaxing the denylist.
- The store-ingress duplicated denylist
  (`store.isProtectedAskBranchName`) drifts if anyone updates
  `git.protectedBranchNames` without updating `store.protectedAskBranchNames`.
  A cross-package CLAUDE.md directive should probably be added if more
  names land on either side.

## Fix #10 — Outbound-channel hardening

**AUDIT IDs closed:** AUDIT-016, AUDIT-017, AUDIT-055, AUDIT-056, AUDIT-057 (plus P9 pattern)

**Branch:** `fix/redact-and-outbound`

**What broke.** Three outbound surfaces each had their own exfil hole,
and all three shared the same shape of defect: no destination allow-list
and no content redaction. (a) `FireWebhook` POSTed the first 500 chars
of `BountyBoard.payload` verbatim to whatever URL lived in
`SystemConfig.webhook_url` — operator-pasted tokens, Claude stdout
echoing a GitHub PAT, or a PR-review-comment body would leave the
daemon whenever any task hit Completed/Failed/Escalated. The
`http.Client` had no `CheckRedirect` policy, so a permitted first-hop
host could 302 us to `169.254.169.254` (AWS/GCP instance metadata).
(b) `FORCE_OTEL_LOGS_URL` was taken verbatim from the environment and
passed straight to `http.Post`; an operator with env access (or an
attacker who could set one) could redirect every `task_claimed`
payload preview to an arbitrary HTTP endpoint. (c) `internal/gh/gh.go`
wrapped every non-zero `gh` exit's stderr into a returned error via
`fmt.Errorf("...: %w: %s", err, stderr)`, and those errors landed in
`BountyBoard.error_log`, `Escalations.message`, and `Fleet_Mail.body` —
all visible on the (currently unauth) dashboard. A `gh` auth-failure
stderr can contain token prefixes (`ghp_`, `gho_`, `ghu_`, `ghs_`,
`github_pat_`) and URL-embedded basic auth. (d) Separately, the
`ExecRunner` captured stdout into an unbounded `bytes.Buffer`; a
`gh api --paginate repos/.../comments` against a PR with tens of
thousands of comments would OOM the daemon.

**What shipped.**

- One chokepoint in `internal/store/redact.go`:
  - `RedactSecrets(string) string` — six regex classes (`ghp_`, `gho_`,
    `ghu_`, `ghs_`, `ghr_`, `github_pat_`), Bearer tokens (preserves the
    `Bearer` keyword), and URL-embedded basic auth (preserves scheme
    and host). Replacement token is `[REDACTED]` so redaction is
    visible in logs.
  - `RedactSecretsBytes([]byte) []byte` — []byte wrapper so captured
    gh stderr can be scrubbed without string conversion at every call
    site.
  - Allocation-free fast path: a cheap substring scan skips regex
    work when no anchor prefix is present.
- One allow-list in `internal/store/webhook.go`:
  - `ValidateOutboundURL(string) error` — scheme in `{http, https}`,
    host non-empty, every resolved A/AAAA record rejected if loopback,
    link-local, private RFC1918, multicast, or unspecified. A DNS name
    whose records mix public and private addresses is rejected in
    full — first-hop routing must not be order-dependent.
  - `lookupHostFn` is indirected so tests can pin resolutions
    without depending on the host's DNS.
  - `SetAllowLoopbackForTest(bool) func()` is a deliberately awkward
    escape hatch — httptest servers bind to 127.0.0.1, and existing
    webhook tests need to hit them. Grep-visible.
- Webhook hardening in `FireWebhook`:
  - Pre-validate `webhook_url` via `ValidateOutboundURL` on every
    call (defense in depth — `holocron.db` may have been edited by
    hand).
  - `http.Client.CheckRedirect` re-validates the target host on every
    hop, capped at 5 redirects. SSRF-via-302 closed.
  - Payload fed through `RedactSecrets` BEFORE truncation, so a PAT
    that straddles the 500-char cutoff is still scrubbed.
- Config-write gate in `cmd/force/config.go`:
  - `force config set webhook_url <url>` runs `ValidateOutboundURL`
    before writing. Operators see `Error: webhook_url failed
    validation: ...` instead of having the webhook silently drop at
    runtime.
- Telemetry hardening in `internal/telemetry/telemetry.go`:
  - `InitTelemetry` validates `FORCE_OTEL_LOGS_URL` via the shared
    allow-list before enabling OTLP export. A rejected URL logs a
    warning and leaves the export disabled.
  - The OTLP HTTP client gets the same `CheckRedirect` policy as the
    webhook client.
  - Event payloads pass through `redactEventPayload` (walks the
    `Payload` map and scrubs string values + `[]string` values).
  - OTLP log-record body also scrubs the raw event JSON before
    marshaling.
- `gh` hardening in `internal/gh/gh.go`:
  - New `redactGHError(prefix, err, stderr)` helper — every existing
    `fmt.Errorf("gh ...: %w: %s", err, stderr)` site rewritten to
    route through it. 12 wrap sites consolidated.
  - `capWriter` bounds the stdout buffer at `maxGHStdoutBytes`
    (64 MiB). Overflow returns `ErrOverflow`, surfaced via the
    command's error. `ClassifyError` maps "gh output exceeded" to
    `ErrClassPermanent` so the fleet escalates instead of retrying
    into the same OOM.

**Pre-existing telemetry race — fixed here.** The original Fix #0 log
noted that `TestEmitEvent_WithOTLPEndpoint` races under `-race` because
the async POST goroutine reads `otlpEndpoint` / `otlpHTTPClient` while
the deferred cleanup resets them. Fix #10 owned telemetry anyway, so
the fix landed here: `EmitEvent` captures the endpoint + client under
`telemetryMu` and passes them to `sendOTLPLog` as function arguments,
and a new `otlpInFlight sync.WaitGroup` tracks launched goroutines.
Tests call `WaitForOTLPDrain()` in their teardown before nulling the
globals. `sendOTLPLog`'s signature changed from
`(event, rawEvent)` to `(event, rawEvent, endpoint, client)` — all
callers updated.

Equivalent pattern applied to the new `SetAllowLoopbackForTest` /
`SetLookupHostForTest` globals on the webhook side: `webhookInFlight
sync.WaitGroup` tracks fired webhook goroutines; `WaitForWebhookDrain`
is the teardown helper. `lookupHostFn` + `allowLoopbackForTests` are
protected by an RWMutex so the async webhook goroutine's read is
serialised against a test cleanup's write.

**Known out-of-scope race.** `cmd/force/testhelpers_test.go:captureOutput`
hot-swaps `os.Stdout` without synchronisation; `TestRunCommandCenter_WithTasks`
and `TestRunCommandCenter_WithEscalations` can run concurrently and race on
the global. Reproduced on main at `1cceef6` (pre-dates Fix #10) and NOT
introduced by any Fix #10 change. Leaving for a follow-up fix focused
on the `cmd/force` test harness.

**How it was proved.**

- Un-skipped P9 pattern tests
  (`TestPattern_P9_SecretLeaksInOutboundChannels/A_*,B_*,C_*`) now
  assert the post-fix contract directly.
- Un-skipped AUDIT-017 and AUDIT-057 sub-tests in
  `audit_misc_security_test.go`.
- 4 new unit tests in `redact_test.go`, one per regex class
  (ghp_/Bearer/url-basic-auth/github_pat_), plus benign-input and
  `[]byte` wrapper coverage.
- `FuzzRedactSecrets` (seeded) — 10s run, no crashes, no token
  survives redaction when the input contained a matchable prefix.
- `outbound_url_test.go` — table-driven
  `TestValidateOutboundURL_AllowList` (14 rows covering scheme,
  empty host, loopback literal, loopback via DNS, link-local
  literal, link-local via DNS, private RFC1918 in three classes,
  unspecified, mixed-DNS-result rejection).
- `TestFireWebhook_AllowListRejectsMetadataHost` — behavioural
  integration test using a pinned `lookupHostFn`.
- `TestFireWebhook_CheckRedirect_BlocksInternal` — stands up a
  loopback redirector that 302s to a DNS-pinned link-local target;
  asserts the metadata stand-in never receives the POST.
- `TestFireWebhook_RedactsEmbeddedToken` — end-to-end acceptance:
  seed a `BountyBoard` row containing a fake PAT, fire the webhook,
  confirm the POST body has `[REDACTED]` and not the token.
- `TestRedactGHError_StrippsPATFromStderr` and
  `TestAuthFailureErrorLogRedacted` — acceptance tests simulating a
  gh auth failure whose stderr contains a PAT + Bearer + URL basic
  auth; asserts all three are scrubbed while the prefix / exit-code
  stay intact.
- `TestClassifyError_OverflowMapsToPermanent` — wires the
  `ErrOverflow` → `ErrClassPermanent` routing so a 64MiB cap hit
  escalates immediately.
- `TestCapWriter_EnforcesLimit` — direct unit test on the cap
  wrapper.
- Full suite green under `go test -tags sqlite_fts5 -race` including
  the previously-racy `TestEmitEvent_WithOTLPEndpoint`.

**Watch for.**

- If a new outbound channel is added (Slack webhook, PagerDuty alert,
  etc.), it must route through both `ValidateOutboundURL` (destination)
  and `RedactSecrets` (content). The CLAUDE.md invariant was added to
  catch this in code review.
- Fine-grained PAT format (`github_pat_<opaque>`) requires ≥ 20 opaque
  characters for the regex to match — GitHub's documented format has
  72 chars of opaque, so the 20-char floor is well below realistic
  tokens but above the "looks like a literal in docs" false-positive
  threshold.
- The 64 MiB stdout cap is generous for paginated comment fetches
  (every GitHub PR we've seen fits under 10 MiB) but not infinite. If
  a repo legitimately needs more — e.g., a release-notes dump —
  escalate to the operator and consider bumping `maxGHStdoutBytes`
  rather than removing the cap.
- `SetAllowLoopbackForTest` is the one sanctioned way to bypass the
  loopback rejection. Greppable; anyone who adds a new production
  path that calls it is visible on PR review.
