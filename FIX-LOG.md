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

## Fix #2 — Dashboard hardening

**AUDIT IDs closed:** AUDIT-001, AUDIT-002, AUDIT-003, AUDIT-053, AUDIT-054, AUDIT-064

**Branch:** `fix/dashboard-hardening`

**What broke.** The dashboard was a localhost-shaped service on the public
internet. `http.ListenAndServe(":PORT", …)` bound every interface while the
banner misleadingly printed `http://localhost`. Every response set
`Access-Control-Allow-Origin: *`. There was no auth, no Origin/Referer
check, no CSRF token, no body size cap, and no CSP. `marked.min.js` was
loaded unpinned from `cdn.jsdelivr.net` and `marked.parse(m.body)` was
assigned directly to the mail modal's `innerHTML` — mail bodies are
written by every agent + every GitHub comment author + operator paste, so
a crafted review comment was stored XSS. Together, a drive-by page the
operator visited could `fetch('http://<operator-ip>:8080/api/control/estop')`
or `/api/tasks/.../approve` and own the fleet. Even without exploitation,
any origin could EventSource `/api/fleet-log` and exfil gh-auth stderr
(with `ghp_…` token prefixes) plus Claude env-echo output. And when
self-healing genuinely gave up — three or more HIGH-severity escalations
open — the operator had no top-of-page signal.

**What shipped.**

- **New file `internal/dashboard/security.go`** — the single source of
  truth for the dashboard's security posture:
  - `loopbackBindAddr(port)` — returns `127.0.0.1:PORT`. Replaces the
    all-interfaces `fmt.Sprintf(":%d", port)` in `RunDashboard`. The banner
    now prints the actual bind address (`http://127.0.0.1:PORT`).
  - `originAllowed(origin, port)` / `refererAllowed(referer, port)` —
    same-origin allow-list. Accepts only `http://127.0.0.1:PORT` and
    `http://localhost:PORT`. Rejects `null` (file:// / about:blank), wrong
    port, wrong scheme, and every foreign host.
  - `securityMiddleware(port, next)` — outer handler gate. Stamps
    `Content-Security-Policy: default-src 'self'; …`, `X-Content-Type-Options: nosniff`,
    `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer` on every
    response. For mutating methods (POST / PUT / PATCH / DELETE), enforces
    the Origin allow-list (Referer fallback) BEFORE the handler runs and
    wraps `r.Body` in `http.MaxBytesReader(w, r.Body, 256<<10)`.
  - `writeBodyReadError(w, err)` — translates `*http.MaxBytesError` to
    413 Request Entity Too Large; anything else to 400 Bad Request.
- `jsonCORS` in `handlers.go` no longer writes wildcard
  `Access-Control-Allow-Origin`. Same-origin requests don't need CORS.
  The function name is preserved for the P8 audit test's regex.
- SSE handlers (`handleHolonetStream`, `handleFleetLogStream`) no longer
  emit the wildcard CORS header either — `AUDIT-053`'s exfiltration path
  is gone.
- `handleAdd`, the task `reject`/`cancel` sub-routes, and the PR-comment
  post-reply handler now translate `*http.MaxBytesError` into 413.
- **Static assets.**
  - `index.html` gains a `<meta http-equiv="Content-Security-Policy" …>`
    belt-and-suspenders tag (duplicated as a response header by the
    middleware). The `<script src="https://cdn.jsdelivr.net/…/marked.min.js">`
    tag is removed entirely.
  - `app.js` — the mail-modal render site switched from
    `innerHTML = marked.parse(m.body)` to `textContent = m.body`. No HTML
    parse, no script execution, no URL auto-run. DOMPurify would have been
    acceptable but textContent is safer-by-default and drops a whole class
    of dependencies.
- **High-escalation banner (AUDIT-064).** A red `#high-esc-banner` element
  lives above the existing ship-ready banner. It appears from every tab
  when `status.high_escalations >= 3` and links to the Escalations tab.
  CSS styled in `style.css` with a red gradient (parallel to the ship
  banner's blue).

**How it was proved.**

- `TestPattern_P8_DashboardBindsAllInterfaces_ServesWildcardCORS` —
  skip removed. Static-checks the five sources of the defect (bind line,
  `jsonCORS` body, marked CDN tag, `marked.parse` call-site, CSP meta
  tag) and dynamically exercises `/api/status` to confirm no wildcard
  CORS header.
- New acceptance tests (`internal/dashboard/security_test.go`):
  - `TestFix2_OriginAllowlist_RejectsForeignOrigin` — httptest.NewServer
    round-trips a POST with `Origin: http://evil.example`, expects 403.
  - `TestFix2_CSPHeader_PresentOnEveryResponse` — table-driven across
    GET /healthz, GET /api/status, same-origin POST, foreign-origin POST.
    Every response must carry the CSP + supporting headers, INCLUDING
    the 403 rejection.
  - `TestFix2_CSRFAttackerForm_Blocked` — classic `<form>` POST with a
    foreign Referer and no Origin. Middleware must reject.
  - `TestFix2_RequestSizeLimit_Returns413` — 512 KB payload against
    `/api/add` (same-origin), expects 413.
  - `TestFix2_LoopbackBind_AddressPrefix` — `net.Listen` on
    `loopbackBindAddr(0)` and asserts the bound host is `127.0.0.1`.
  - `TestFix2_MailBody_RendersAsText_NotHTML` — static check that the
    mail-modal-body render site uses `textContent`, not
    `innerHTML = marked.parse(...)`.
  - `TestFix2_Sanitizer_HandlesClassicXSSPayloads` — threat-model
    coverage for `<script>`, `<img onerror>`, `javascript:` URLs, SVG
    onload, quote-break. These payloads cannot reach an innerHTML sink
    because the render path is textContent.
  - `TestFix2_Healthz_ServesQuickly` — httptest server replies 200 to
    `/healthz` in under 1 s.
  - `TestFix2_OriginAllowedMatrix` / `TestFix2_RefererAllowedMatrix` —
    table-driven unit coverage of the allow-list (same-port same-origin
    good; wrong-port, wrong-scheme, foreign-host, `null`, empty all bad).
  - `TestFix2_HighEscalationBanner_Present` — static check that app.js
    reads `s.high_escalations`, toggles the `high-esc-banner` element,
    and gates on the 3-escalation threshold.
- Three pre-existing CORS-wildcard tests
  (`TestHandleStatus_CORS`, `TestHandleTasks_CORS`,
  `TestHandleHolonetStream_SSEHeaders`) were inverted: now they assert
  the wildcard header is ABSENT.

**Stats.**

- 1 new source file (`security.go`, ~155 LOC).
- 1 new test file (`security_test.go`, 11 test functions, ~340 LOC).
- 3 existing tests inverted (now assert the SAFE posture).
- 1 audit-pattern test (P8) flipped from Red to Green.

**Known follow-ups (not in scope for Fix #2).**

- No auth. The dashboard is still single-user + loopback. A session cookie
  + CSRF token is the right long-term move if the tool ever grows
  multi-user or needs remote access — for now, SSH tunneling is the
  supported path.
- `style-src 'unsafe-inline'` is kept in the CSP because a handful of
  existing markup nodes use inline `style=` attributes. If those ever get
  cleaned up, tighten to `style-src 'self'`.
- Redaction of gh-auth stderr (AUDIT-055) is a separate fix (Fix #10) —
  even with same-origin gating, SSE log streams should not be printing
  `ghp_…` tokens in the first place.

**Watch for.**

- A new mutating endpoint that forgets to invoke the middleware —
  shouldn't be possible because the middleware wraps the mux, but any
  future refactor that bypasses the wrap (e.g. a raw
  `http.ListenAndServe(addr, someOtherHandler)`) would re-open the
  allow-list and size-cap gaps. The P8 audit test will catch the CORS
  regression but not the size cap; consider adding a P8-adjacent test if
  a new server entry point is introduced.
- If marked.js is ever re-added for rich rendering, the P8 test requires
  the tag to be bundled locally AND carry an `integrity=` SRI hash. Any
  tag missing either constraint fails the test.
- The CLAUDE.md "Dashboard invariants" block captures the four
  load-bearing properties (loopback bind, Origin allow-list,
  MaxBytesReader, textContent for attacker-writable strings, HIGH
  banner threshold). Read it before touching the dashboard package.
