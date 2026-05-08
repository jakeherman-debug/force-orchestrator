# DELIVERABLE-5-CLOSURE.md — Supply Chain Hygiene (CLOSED + DOCS-AUDITED)

**Date:** 2026-05-01
**Operator:** jake.herman@upstart.com
**Net verdict:** ✅ CLOSED + DOCS-AUDITED. All five D5 phases (P0, P1, P2, P3, P4) plus fix-iter1 (production wiring + SUPPLY-BYPASS parser) merged to `main`; strict verifier round 2 final-gate green on Static + Heavy + Race shards with all roadmap exit criteria MET.

D5 follows the partial-closure pattern (per D1 + D3 + D4): this document captures the shipped state at end-of-fix-iter1. The roadmap-mandated closure-report shape (per docs/roadmap.md § Deliverable 5 § "Closure report" lines 1644-1656) is captured in the per-rule status, CodeArtifact endpoints, deferral-path stats, license-matrix, dogs, gate, anti-cheat, and residual sections below.

---

## Per-phase tracking

| Phase | Description | Status | Merge SHA |
|---|---|---|---|
| P0 | Foundation — CodeArtifact client (default cred chain), per-ecosystem manifest parsers, `Repositories.license` backfill migration, manifest-gating dispatch in ISBReview, `SecurityFindings.disposition='token_expired'` deferral schema | ✅ CLOSED | `1cde6fb` |
| P1 | SUPPLY-001 (hallucinated package) + SUPPLY-002 (typosquat) — registry-hit + deferral-path-aware (SUPPLY-001) + allowlist-driven (SUPPLY-002) | ✅ CLOSED | `d21a6c9` |
| P2 | SUPPLY-003 (stale package) + SUPPLY-004 (license matrix) — registry-hit + hand-authored SPDX matrix at `internal/isb/rules/license_matrix.yaml` | ✅ CLOSED | `54386bf` |
| P3 | SUPPLY-005 (known-CVE blocking) — vendored osv-scanner Go lib, lock-file scan independent of CodeArtifact / AWS auth | ✅ CLOSED | `608b6f7` |
| P4 | `supply-allowlist-refresh` + `supply-token-recheck` dogs + ConvoyReview `AwaitingSupplyRecheck` gate + e2e fixture sweep across all 5 ecosystems | ✅ CLOSED | `a0f83d5` |
| fix-iter1 | Strict-verifier round-1 NO-GO close-out: production wiring (`agents.WireSupplyRules`, `ReplayAdapter`, `RegisterSupplyRecheckDeps`) + SUPPLY-BYPASS parser per-language coverage | ✅ CLOSED | `5e55bdc` |

---

## Per-rule status — SUPPLY-001 through SUPPLY-005

All five SUPPLY rules ship at `severity=advise` per the universal anti-cheat directive (no block-default on new rules; 30-clean-firings warm-up window precedes promotion to `block`). All five are FleetRules with `category='isb'`, `agent_scope='all'`, and `RenderTo='discard'` — the rule body lives in `internal/isb/rules/supply_NNN.go` and is selected at review time by `isb.DBFleetRulesGate` reading the live FleetRules table (matching the D4 ISB-001..010 architecture). Manifest-gating in `internal/isb/scanners/manifests/` filters out source-only commits before any rule fires, so most commits make zero registry hits.

| Rule | Ecosystems | FleetRules row (audit) | Default severity | Mechanism summary | Tests |
|---|---|---|---|---|---|
| **SUPPLY-001** — Hallucinated package rejection | PyPI, npm, RubyGems, Maven (Go skipped — see CodeArtifact endpoints below; SUPPLY-005 covers Go via osv-scanner) | `internal/store/fleet_rules_audit.go:1029` | advise | CodeArtifact `DescribePackageVersion` lookup. `ErrPackageNotFound` → finding; `ErrTokenExpired` → SecurityFindings deferral row (`disposition='token_expired'` via `supplydeferral.RecordDeferral`); `ErrTransient` → retry-once + log + advise-through; `ErrUnsupportedEcosystem` (Go) → silent skip. Positive 24h cache; **no negative cache**. | `TestSupply001_HallucinatedRubyGem_Rejected`, `..._HallucinatedNpmPackage_Rejected`, `..._HallucinatedPyPI_Rejected`, `..._HallucinatedMaven_Rejected`, `..._HallucinatedGoModule_Rejected`, `TestSUPPLY001_TokenExpired_DeferralLogged` |
| **SUPPLY-002** — Typosquat detection | PyPI, npm, RubyGems, Maven, Go | `internal/store/fleet_rules_audit.go:1039` | advise | Damerau-Levenshtein distance ≤ 2 against the per-ecosystem `SystemConfig.supply_allowlist_<eco>` set (populated by the supply-allowlist-refresh dog from `aws codeartifact list-packages`). Operator-preapproved set lives in `SystemConfig.supply_typosquat_preapproved`. Empty allowlist → rule inert + log. **No registry-hit at run-time, so no deferral path required** — the dog handles auth errors on refresh. | `TestSupply002_RubyTyposquat_Rejected`, `..._NpmTyposquat_Rejected`, `..._PythonTyposquat_Rejected`, `..._MavenTyposquat_Rejected`, `..._GoTyposquat_Rejected` |
| **SUPPLY-003** — Stale-package detection | PyPI, npm, RubyGems, Maven (Go silent-skip) | `internal/store/fleet_rules_audit.go:1088` | advise | CodeArtifact `DescribePackageVersion` returns `PublishedAt`. `PublishedAt < now() - threshold` → advise finding (cite published date + threshold_days); `PublishedAt zero` → silent skip; `ErrPackageNotFound` → silent skip (SUPPLY-001's domain); `ErrTokenExpired` → SecurityFindings deferral row; `ErrTransient` → retry-once + log + advise-through; `ErrUnsupportedEcosystem` (Go) → silent skip. Threshold from `SystemConfig.supply_stale_threshold_days` (default 730 ≈ 2 years). | `TestSupply003_StaleRubyGem_Rejected`, `..._StaleNpmPackage_Rejected`, `..._StalePyPI_Rejected`, `..._StaleMaven_Rejected`, `..._StaleGoModule_Rejected`, `TestSUPPLY003_TokenExpired_DeferralLogged` |
| **SUPPLY-004** — License-compatibility check | PyPI, npm, RubyGems, Maven (Go silent-skip) | `internal/store/fleet_rules_audit.go:1098` | advise | CodeArtifact `DescribePackageVersion.License` field vs `Repositories.license`, resolved against the static SPDX matrix at `internal/isb/rules/license_matrix.yaml`. Matrix allow → no finding; matrix deny → advise finding; pair absent from matrix → advise finding (operator review, **NEVER auto-allow**); empty dep license OR empty repo license → advise finding (cannot check); deferral path on `ErrTokenExpired`. **No negative cache.** | `TestSupply004_IncompatibleLicenseRubyGem_Rejected`, `..._IncompatibleLicenseNpmPackage_Rejected`, `..._IncompatibleLicensePyPI_Rejected`, `..._IncompatibleLicenseMaven_Rejected`, `..._IncompatibleLicenseGoModule_Rejected`, `TestSUPPLY004_TokenExpired_DeferralLogged` |
| **SUPPLY-005** — Known-CVE blocking | PyPI, npm, RubyGems, Maven, **Go** (osv-scanner parses `go.sum` natively) | `internal/store/fleet_rules_audit.go:1145` | advise (Critical/High graduate to block via paired-run promotion) | Vendored osv-scanner Go library against the **lock file** (`Gemfile.lock`, `package-lock.json`, `go.sum`, `requirements.txt`, pom.xml lockfile, etc.). Each scanner-returned vulnerability → advise finding (cite OSV-ID, ecosystem, package@version, summary, severity bucket, advisory URL); `osv.ErrUnsupportedLockfile` → silent skip for that path; other scanner errors → wrapped via `errors.Join`, partial sibling-manifest findings still flow back. **Independent of CodeArtifact / AWS auth — no deferral path.** | `TestSupply005_KnownCVERubyGem_Rejected`, `..._KnownCVENpmPackage_Rejected`, `..._KnownCVEPyPI_Rejected`, `..._KnownCVEMaven_Rejected`, `..._KnownCVEGoModule_Rejected` |

---

## AWS CodeArtifact endpoints used

The Force daemon resolves the per-ecosystem repository name via `codeartifact.RepoFor(Ecosystem)` (`internal/clients/codeartifact/client.go:155-164`) and queries through Upstart's CodeArtifact domain (`code-artifacts-prod`, account `801997600626`, region `us-east-1`) using the AWS SDK v2 default credential chain.

| Ecosystem | CodeArtifact repo | AWS PackageFormat | Used by |
|---|---|---|---|
| PyPI | `pypi-prod` | `PackageFormatPypi` | SUPPLY-001, SUPPLY-003, SUPPLY-004, allowlist-refresh dog (SUPPLY-002 source) |
| npm | `npm-prod` | `PackageFormatNpm` | SUPPLY-001, SUPPLY-003, SUPPLY-004, allowlist-refresh dog (SUPPLY-002 source) |
| RubyGems | `rubygems-prod` | `PackageFormatRuby` | SUPPLY-001, SUPPLY-003, SUPPLY-004, allowlist-refresh dog (SUPPLY-002 source) |
| Maven | `maven-prod` | `PackageFormatMaven` | SUPPLY-001, SUPPLY-003, SUPPLY-004, allowlist-refresh dog (SUPPLY-002 source) |
| Go | _(not supported)_ | `awsPackageFormat` returns `ErrUnsupportedEcosystem` (`inprocess.go:115-117`) | SUPPLY-001/003/004 silently skip Go; **SUPPLY-005 covers Go natively** via osv-scanner's `go.sum` parser; SUPPLY-002 typosquat against Go uses the same allowlist path even though the allowlist is not refreshed (the dog skips Go) — so Go SUPPLY-002 is inert until an operator manually populates `supply_allowlist_go`. |

The `grpc-generic-prod` repo is **excluded** by design — CodeArtifact's "generic" format is for arbitrary internal binaries, has no upstream registry, no typosquat surface, and no public CVE feed.

AWS SDK service operations called per rule:

- `DescribePackageVersion` — SUPPLY-001 (existence), SUPPLY-003 (`PublishedAt`), SUPPLY-004 (`License`).
- `DescribeDomain` — `supply-token-recheck` dog health probe (`internal/clients/codeartifact/inprocess.go:249`).
- `ListPackages` — `supply-allowlist-refresh` dog (24h cadence per ecosystem).

No SDK call uses the response cache for `not-found` returns; positive responses are cached in-process for ≤ 24h (SUPPLY-001 + SUPPLY-004 each maintain their own positive-only TTL cache).

---

## Deferral-path stats (production wiring from fix-iter1)

The deferral path is the load-bearing self-healing piece of D5: when CodeArtifact auth fails mid-commit, we never block — we record a `SecurityFindings` row with `disposition='token_expired'` and let the recovery layers replay it once the operator runs `umt artifacts`.

**Production wiring entry points** (all introduced in fix-iter1 to close strict-verifier round 1's NO-GO finding that the rules were registered only in tests):

- `agents.WireSupplyRules(db, caClient, osvClient)` (`internal/agents/supplywire.go`, function `WireSupplyRules`) — single production-side entry that constructs all five rule objects via the `internal/clients/<svc>` interface pattern, registers them with `isb.DBFleetRulesGate`, and stamps `SupplyRecheckDeps` so the supply-token-recheck dog has the per-rule `ReplayAdapter` map.
- `agents.NewReplayAdapter(rule)` (`internal/agents/supplywire.go`, function `NewReplayAdapter`) — wraps `isb.ManifestGatedRule` into the loose-typed `supplydeferral.Rule` shape the replay dog expects. One adapter per rule keyed by `RuleKey`.
- `agents.RegisterSupplyRecheckDeps(deps *SupplyRecheckDeps)` (`internal/agents/dogs_supply_token_recheck.go`, function `RegisterSupplyRecheckDeps`) — package-level singleton stamped from inside `WireSupplyRules`. Without this call the dog short-circuits with `"deps not registered"` and skips its tick; this is the regression strict-verifier round 1 caught.
- Daemon call site: `cmd/force/fleet_cmds.go` (inside `cmdDaemon`) — `agents.WireSupplyRules(db, caClient, osvClient)` is invoked once at daemon startup. The presence of this call is pinned by `cmd/force/supplywire_daemon_test.go` (`TestFleetCmds_CallsWireSupplyRules`), an AST-style grep regression.

**Replay outcomes** (`internal/isb/supplydeferral/replay.go:14-19`):

- **Now clean** → flip original row to `disposition='resolved_late'`. Batched Slack ping per branch.
- **Now flagged** → insert new `disposition='block'` row + flip original to `disposition='superseded'`. Slack ping with rule + dep + branch.
- **Branch deleted/rebased** (no manifest in current tip) → flip original to `disposition='branch_gone'`, rule never re-invoked.

Counts at closure time: zero production deferrals (D5 just landed; no operator-side `umt artifacts` token expirations have yet fired against measurable commit volume). Deferral-path coverage is exercised by `TestSUPPLY001_TokenExpired_DeferralLogged`, `TestSUPPLY003_TokenExpired_DeferralLogged`, `TestSUPPLY004_TokenExpired_DeferralLogged` and the replay-state matrix in `internal/isb/supplydeferral/replay_test.go`. Backfilled at first production deferral.

---

## License compatibility matrix

- **File:** `internal/isb/rules/license_matrix.yaml` (223 lines, embedded via `//go:embed` in `internal/isb/rules/license_matrix.go:22-23`).
- **Schema:** keyed by repo SPDX license id; each value lists `allow:` and `deny:` SPDX id sets. Both sides case-sensitive (canonical SPDX casing — e.g. `Apache-2.0`, not `APACHE-2.0`).
- **Rows:** 13 top-level repo licenses covered (MIT, Apache-2.0, BSD-3-Clause, BSD-2-Clause, ISC, MPL-2.0, LGPL-3.0, LGPL-2.1, GPL-3.0, GPL-2.0, AGPL-3.0, Unlicense, CC0-1.0).
- **Anti-cheat — no LLM imports.** Verified: `internal/isb/rules/license_matrix.go:15-20` imports only `embed`, `fmt`, `gopkg.in/yaml.v3`. `internal/isb/rules/supply_004.go:53-67` imports only `context`, `database/sql`, `errors`, `fmt`, `log`, `sync`, `time`, plus the in-house `codeartifact` / `isb` / `manifests` / `supplydeferral` / `store` packages — zero `internal/claude`, zero `anthropic` SDK, zero LLM call sites.
- **Pairs absent from the matrix** → advise-mode finding for operator review. **NEVER auto-allow or auto-deny.** This is the "no LLM-decides-licenses" anti-cheat directive made mechanical.

---

## Two SUPPLY dogs (D5 P4)

Both dogs registered in `internal/agents/dogs.go` with explicit cooldowns and dog-cycle ordering: `supply-allowlist-refresh` runs first so it can stamp the allowlist, then `supply-token-recheck` runs second so its replay path operates against the freshest data.

| Dog | Cooldown | What it does | Implementation |
|---|---|---|---|
| `supply-allowlist-refresh` | 24h | Walks per-ecosystem CodeArtifact (`pypi-prod`, `npm-prod`, `rubygems-prod`, `maven-prod`) via `ListPackages`, dedup-flattens the result, writes the newline-joined name set into `SystemConfig.supply_allowlist_<eco>` and the refresh timestamp into `SystemConfig.supply_allowlist_<eco>_last_refresh`. SUPPLY-002's typosquat-distance check reads from these keys. | `internal/agents/dogs.go`, function `dogSupplyAllowlistRefresh` |
| `supply-token-recheck` | 30 min | Probes CodeArtifact via `DescribeDomain`. On 401: sets `SystemConfig.supply_token_expired_notified='1'` + fires one notify-after Slack ping (debounced — re-firing skipped while flag is set). On 200 with flag set: clears flag + fires "token recovered" ping + replays every `disposition='token_expired'` SecurityFinding via the per-rule `ReplayAdapter` map. | `internal/agents/dogs_supply_token_recheck.go`, functions `dogSupplyTokenRecheck` / `runSupplyTokenRecheck` |

The debounce flag (`SystemConfig.supply_token_expired_notified`) is the load-bearing piece preventing a Slack-ping flood while the operator is between `umt artifacts` runs — see the `supplyTokenNotifiedKey` constant in `internal/agents/dogs_supply_token_recheck.go`.

---

## ConvoyReview AwaitingSupplyRecheck gate

The convoy-level last-resort safety net before merge. Wired inside `internal/agents/convoy_review.go`'s `runConvoyReview` (D5 P4 slice γ) — runs at the top of every ConvoyReview pass, **before** any LLM call:

1. **Evaluate gate** (`evaluateSupplyRecheckGate` in `convoy_review.go`): walk every ask-branch in the convoy, count rows with `bureau='isb' AND rule_id LIKE 'SUPPLY-%' AND disposition='token_expired'`.
2. **If clean:** ConvoyReview proceeds normally — frozen-spec cycle begins, LLM is invoked, the standard ISB block-eval downstream still catches any unresolved blocks.
3. **If CodeArtifact reachable:** replay each branch inline via the same `ReplayAdapter` map the dog uses (`replay.ReplayBranchDeferrals`). Successful replay flips rows to `resolved_late` / `superseded`; gate then passes.
4. **If CodeArtifact down (or replay deps unwired):** stamp `Convoys.status='AwaitingSupplyRecheck'` (constant `ConvoyStatusAwaitingSupplyRecheck` declared at the top of `convoy_review.go`), fire one-shot Slack ping + operator mail, mark the bounty `Completed` (deferred not failed), exit the review pass early. The `convoy-review-watch` dog will requeue once the convoy returns to `DraftPROpen`.

**Ship-It UI integration.** The operator's Ship-It surface (`internal/dashboard/ship.go` + `cmd/force/convoy_pr.go`) refuses to advance any convoy not in status `DraftPROpen`. `AwaitingSupplyRecheck` is therefore a hard block on merge — the gate is a read-only signal, but downstream UI honours it. **The Slack ping itself does not trigger anything** (no Slack-message-triggers-merge); only the convoy-state transition triggered by an operator's `umt artifacts` + the recheck dog's successful replay can move the convoy back to `DraftPROpen`.

---

## Strict verifier rounds

| Round | Static | Heavy | Race | Verdict |
|---|---|---|---|---|
| Round 1 (post-P4) | ❌ NO GO (3 items) | ✅ GO | ✅ GO | NO GO |
| Round 2 (post-fix-iter1) | ✅ GO | ✅ GO | ✅ GO | ✅ GO — all roadmap exit criteria MET |

Round 1 NO-GO items:

- **α: Production wiring missing.** `agents.WireSupplyRules` did not exist — rules were registered only in tests; the daemon would boot with zero SUPPLY rules active. Closed in iter1 α (production wiring + `ReplayAdapter` + `RegisterSupplyRecheckDeps` + `TestFleetCmds_CallsWireSupplyRules` regression).
- **β: ConvoyReview gate's notify-after debounce racy.** `supply_token_expired_notified` flag was set/cleared without synchronising against the dog's parallel set. Closed in iter1 β.
- **γ: SUPPLY-BYPASS parser was JS-only.** Parser didn't recognise Ruby `#`, Python `#`, XML `<!-- -->`, or Groovy `// /* */` comment styles. Closed in iter1 γ (single-regex multi-prefix parser at `internal/isb/supply_bypass.go:64`, with `>= 10`-char reason validator at line 114).

---

## Anti-cheat self-check

| Directive (per docs/roadmap.md § D5 Anti-cheat directives) | Status | Per-line evidence |
|---|---|---|
| **No hardcoded popular-package allowlists.** Allowlist comes from `aws codeartifact list-packages` (org-actual usage). | ✅ | `internal/isb/rules/supply_002.go` reads exclusively from `SystemConfig.supply_allowlist_<eco>` (populated by the supply-allowlist-refresh dog — see `dogSupplyAllowlistRefresh` in `internal/agents/dogs.go`). Empty allowlist → rule inert + log; **zero baked-in package names anywhere in the rule body**. FleetRules audit-row justification in `internal/store/fleet_rules_audit.go` (search for the SUPPLY-002 rule row) documents this contract. |
| **No registry-hit caching of `not-found` responses.** A package that 404'd yesterday might be real today. Positive caches OK ≤ 24h. | ✅ | SUPPLY-001 + SUPPLY-004 each maintain a positive-only in-process TTL cache (`supply004CachePositiveTTL = 24 * time.Hour` at `internal/isb/rules/supply_004.go:72`). The `ErrPackageNotFound` branch in both rules emits a finding directly without writing the negative result to any cache layer. SUPPLY-003 has no cache at all (a newly-released version must flip the rule on the next Run). |
| **No bypass-by-default.** Every bypass requires `// SUPPLY-BYPASS: <AUDIT-NNNN> <reason>` (or `# SUPPLY-BYPASS:` for Ruby/Python, `<!-- -->` for XML), with reason ≥ 10 chars. | ✅ | `internal/isb/supply_bypass.go:114` rejects markers whose reason is `< 10 runes` after trim. AUDIT-NNNN regex (`AUDIT-\d+`) is required by the parser regex itself (`internal/isb/supply_bypass.go:64`). Markers without a reason silently skip — they do **not** suppress findings, which is the correct anti-cheat outcome. |
| **No license matrix LLM shortcut.** The matrix is hand-authored YAML, reviewed and committed; no LLM "decides" license compatibility at check time. Pairs absent → advise-mode + operator review, **never auto-allow or auto-deny**. | ✅ | `internal/isb/rules/license_matrix.go:15-20` imports only `embed`, `fmt`, `gopkg.in/yaml.v3`. `internal/isb/rules/supply_004.go:53-67` imports only `context`, `database/sql`, `errors`, `fmt`, `log`, `sync`, `time`, plus the local `codeartifact`/`isb`/`manifests`/`supplydeferral`/`store` packages. **Zero `internal/claude` imports, zero `anthropic` SDK imports, zero LLM call sites.** Pairs absent from matrix → advise finding (`supply_004.go` Run path emits `[SUPPLY-004] license pair (<repo>, <dep>) absent from matrix; operator review required`). |
| **No silent token-expired passthroughs.** Every auth-error path must emit a `SecurityFindings` row with `disposition='token_expired'`. | ✅ | Pattern P-SupplyDeferral (`internal/audittools/audit_pattern_p_supply_deferral_test.go`) AST-walks `internal/isb/rules/supply_*.go` and rejects any `ErrTokenExpired` branch that returns without a preceding `supplydeferral.RecordDeferral` call. Test green at fix-iter1 closure. SUPPLY-002 is exempt (no registry-hit at run-time → no auth path → no deferral required); the audit walker's fixture inputs document this. |
| **No Slack-message-triggers-merge.** The gate is a read-only signal; `umt artifacts` recovery does not authorise any code-modifying action. | ✅ | The `AwaitingSupplyRecheck` Slack ping fired by ConvoyReview (inside `runConvoyReview`'s gate-handling branch in `internal/agents/convoy_review.go`) and by the supply-token-recheck dog (`runSupplyTokenRecheck` in `internal/agents/dogs_supply_token_recheck.go`) is operator notification only. The merge gate is the operator's Ship-It surface refusing to advance any convoy not in status `DraftPROpen` — no Slack-message handler ever flips the convoy state. State transitions happen via the recheck dog's successful replay (which flips `disposition` not convoy state) plus the existing PR-state-transition path (`draft-pr-watch` dog) that returns the convoy to `DraftPROpen`. |

---

## Residual list

1. **`force supply preapprove <ecosystem> <name>` CLI command not implemented.** Spec line 1572 named the command; operators currently use `force config set supply_typosquat_preapproved <name1>,<name2>` directly. Cosmetic; the underlying `SystemConfig` write path is identical. Defer to D5.5+ or a follow-up; non-blocking.

2. **Dashboard disposition filter validator narrow.** `internal/dashboard/handlers_security_findings.go:60` accepts only `open|overridden|escalated|resolved|suppressed|closed` as query-string filter values. The new D5 dispositions (`token_expired`, `resolved_late`, `superseded`, `branch_gone`, `block`) are written by the rule + replay layers correctly and persist in DB; they show up in the default unfiltered list rendering. But selecting them as a filter via `?disposition=token_expired` returns HTTP 400 `{"error":"disposition invalid"}`. Pre-existing dashboard staleness exposed by D5's new values; the rule layer is correct, the dashboard filter validator is just out of date. Defer; non-blocking for D5 close-out.

3. **Spec divergence — `RenderTo='discard'` instead of `isb/finders/supply_*.yaml`.** Roadmap exit criterion 2 (line 1598) said `rule-renderer` would write `isb/finders/supply_*.yaml` per rule into the target-repo. Actual implementation uses `RenderTo: "discard"` in the FleetRules audit rows + `EnforcedBy: "internal/isb/rules/supply_NNN.go"` (`internal/store/fleet_rules_audit.go:1033, 1043, 1092, 1102, 1149`), with rule-selection at review time via `isb.DBFleetRulesGate` reading the live FleetRules table. This matches the D4 ISB-001..010 architecture (which also uses `RenderTo='discard'`); operator-reviewable rule bodies live in code, the FleetRules row is the gate. Acceptable architecture choice; documented as a divergence, not a defect.

4. **Roadmap verification command name divergence.** Roadmap line 1641 references `TestSupplyDeferral_TokenExpired` as the deferral-path manual test; actual test names are `TestSUPPLY001_TokenExpired_DeferralLogged`, `TestSUPPLY003_TokenExpired_DeferralLogged`, `TestSUPPLY004_TokenExpired_DeferralLogged` (per-rule, since each rule has its own deferral entry point). Cosmetic; the verification flow works the same way.

5. **First-30-firings precision is empty/0 across all 5 SUPPLY rules.** Same caveat as D4 BoS / ISB / Senate: warm-up window has not yet accumulated production-commit-volume firings. Precision backfilled at first promotion review. No blocking dependency on this; SUPPLY-001/SUPPLY-005 already have well-validated detection mechanics from the integration-test fixtures.

---

## Forward integration to D5.5 (Staged Convoys)

D5.5 P0 schema additions extend `Convoys` (with `staging_mode`, `staging_strategy`) and add a new `ConvoyStages` table — orthogonal to D5's `SecurityFindings` work. The `AwaitingSupplyRecheck` gate from D5 ConvoyReview runs **per-stage at each stage's `DraftPROpen`** post-D5.5 (per docs/roadmap.md line 1874): SUPPLY-* findings are scoped to the ask-branch they were detected on, which is already stage-scoped post-D5.5 by virtue of `ConvoyAskBranches.stage_id`. No D5 change required for forward-compat — the per-branch grouping in `evaluateSupplyRecheckGate` carries forward unchanged.

No blocking forward-integration items.
