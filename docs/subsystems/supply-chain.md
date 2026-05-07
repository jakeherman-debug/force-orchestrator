---
audience: operator
scope: D5 supply-chain hygiene — five SUPPLY rules across PyPI/npm/RubyGems/Maven/Go, license matrix, token-deferral substrate, and recovery dogs.
owner: security
last_reviewed: 2026-05-07
---

# Supply chain hygiene

D5 ships supply-chain hygiene as a manifest-gated rule family on the same ISBReview substrate as `ISB-001..010`. Every rule fires only when a commit's diff touches a recognised manifest file. All five SUPPLY rules ship at `severity=advise` per the universal anti-cheat directive ("no block-default for new rules"); promotion to `block` lands via the standard FleetRules paired-run mechanism, not via this deliverable.

## Overview

Five rule families enforce the spec at commit time:

| Rule | Mechanism | Ecosystems | Source file |
|---|---|---|---|
| **SUPPLY-001** | Hallucinated package detection — CodeArtifact `DescribePackageVersion`; 404 → finding (never cached). | PyPI, npm, RubyGems, Maven (Go skipped — SUPPLY-005 covers Go via osv-scanner) | `internal/isb/rules/supply_001.go` |
| **SUPPLY-002** | Typosquat detection — Damerau-Levenshtein ≤ 2 against per-ecosystem allowlist sourced from `aws codeartifact list-packages` (no hardcoded allowlists). | PyPI, npm, RubyGems, Maven, Go | `internal/isb/rules/supply_002.go` |
| **SUPPLY-003** | Stale-package detection — `PublishedAt < now() − threshold`. Threshold from `SystemConfig.supply_stale_threshold_days` (default 730 ≈ 2 years). | PyPI, npm, RubyGems, Maven (Go silent skip) | `internal/isb/rules/supply_003.go` |
| **SUPPLY-004** | License compatibility — repo SPDX id × dep SPDX id against the static matrix at `internal/isb/rules/license_matrix.yaml`. **No LLM decides licenses.** Pairs absent from matrix → operator review. | PyPI, npm, RubyGems, Maven (Go silent skip) | `internal/isb/rules/supply_004.go` |
| **SUPPLY-005** | Known-CVE blocking — vendored osv-scanner library against the lock file. **Independent of CodeArtifact / AWS auth — no deferral path.** | PyPI, npm, RubyGems, Maven, Go | `internal/isb/rules/supply_005.go` |

All five register through the same `isb.ManifestGatedRule` interface and dispatch via `internal/isb/manifest_gated.go`'s manifest-touch filter. Production wiring lives in a single entry point: `agents.WireSupplyRules(db, caClient, osvClient)` (`internal/agents/supplywire.go`).

## Components

### Rule package — `internal/isb/rules/`

- `supply_001.go` … `supply_005.go` — one file per rule with `Run(ctx, db, input)` returning findings + errors. Each rule body documents its outcome map (200, ErrPackageNotFound, ErrTokenExpired, ErrTransient, ErrUnsupportedEcosystem, structural error) and its caching posture (positive 24h cache; never cache 404s or stale findings — anti-cheat).
- `license_matrix.yaml` (223 lines, embedded via `//go:embed` in `license_matrix.go`) — keyed by repo SPDX id, lists `allow:` and `deny:` SPDX id sets. 13 top-level repo licenses covered. Pairs not declared → advise-mode + operator review. **Never auto-allow, never auto-deny.**
- `wire.go` — package init() that wires `rules.SetFileSet` into `internal/isb`'s indirection seam.

### Manifest scanners — `internal/isb/scanners/manifests/`

Per-ecosystem parsers under `gemfile/`, `pip/`, `npm/`, `maven/`, `gomod/`. Each implements `Parser`:

```go
type Parser interface {
    Detect(path string) bool
    Parse(path string, content []byte) ([]Dependency, error)
    ParseDiff(path string, before, after []byte) (added, removed []Dependency, err error)
}
```

Top-level dispatch in `manifests.go` maps manifest filename → Parser. Lock-file parsing is best-effort regex; malformed input must NOT panic — `(nil, error)` so the deferral path can fire.

### OSV / SPDX scanners

- `internal/isb/scanners/osv/osv.go` — wraps the vendored `github.com/google/osv-scanner` Go library as a P16-compliant `Client` interface. Used by SUPPLY-005 only. Calls OSV.dev's public API; no AWS dependency.
- `internal/isb/scanners/spdx/detect.go` — deterministic SPDX detector for ~10 common licenses. Used by D5 P0 to backfill `Repositories.license` at AddRepo time. Returns `Unknown` on no match (never guesses).

### CodeArtifact client — `internal/clients/codeartifact/`

Pattern-P16 service interface (`Client` interface, `NewInProcess(...)` factory; AWS SDK v2 default credential chain — picks up SSO / env / instance-profile creds). Operations the rules consume: `DescribePackageVersion` (SUPPLY-001/003/004), `ListPackages` (allowlist-refresh dog), `DescribeDomain` (token-recheck health probe). All auth-class errors map to `ErrTokenExpired` so the deferral path fires uniformly.

### Deferral substrate — `internal/isb/supplydeferral/`

- `deferral.go` — `RecordDeferral(db, taskID, payload)` writes a `SecurityFindings` row with `disposition='token_expired'` and a stable JSON payload (`DeferralPayload{RuleKey, ManifestPath, DepsAdded, Branch, CommitSHA, DeferredAt}`). Used by SUPPLY-001/003/004 only — SUPPLY-002 reads from cached allowlists and SUPPLY-005 talks to OSV, neither needs it.
- `replay.go` — `ReplayPendingDeferrals` re-runs the original rule against the dep set and flips the original row to one of `resolved_late`, `superseded` (with a fresh `block` row), or `branch_gone`.

### Bypass parser — `internal/isb/supply_bypass.go`

Multi-language `SUPPLY-BYPASS` regex that accepts the marker in any of `//`, `/*`, `#`, `<!--`, `--` comment styles so a Gemfile, requirements.txt, package.json, pom.xml, or build.gradle can carry the same shape. Requires `AUDIT-NNN` + reason ≥ 10 chars; malformed markers produce no match (the underlying finding still surfaces).

### Production wiring — `internal/agents/supplywire.go`

`WireSupplyRules(db, caClient, osvClient)` — single entry called once at daemon startup from `cmd/force/fleet_cmds.go`. Constructs all 5 rule objects via the Pattern-P16 client interfaces, registers them with `isb.DBFleetRulesGate`, and stamps `SupplyRecheckDeps` (per-rule `ReplayAdapter` map) so the recovery dog has the per-rule replay surface. Returns `error` on nil osvClient (closed-fail per CLAUDE.md "no silent failures"); a nil codeartifact client is tolerated (CI / non-AWS dev — SUPPLY-002 / SUPPLY-005 still function).

### Recovery dogs — `internal/agents/dogs.go` + `dogs_supply_token_recheck.go`

| Dog | Cooldown | Behavior |
|---|---|---|
| `supply-allowlist-refresh` | 24h | Walks per-ecosystem CodeArtifact via `ListPackages`, writes the result into `SystemConfig.supply_allowlist_<eco>` and the timestamp into `..._last_refresh`. SUPPLY-002 reads from these keys. |
| `supply-token-recheck` | 30m | Probes CodeArtifact via `DescribeDomain`. On 401: sets `supply_token_expired_notified='1'` + fires one debounced Slack ping. On 200 with the flag set: clears flag + fires "token recovered" ping + replays every `disposition='token_expired'` SecurityFinding via `supplydeferral.ReplayPendingDeferrals`. |

The dogs are ordered: allowlist-refresh runs first so its output is fresh when token-recheck's replay sweep fires.

### Convoy review gate — `internal/agents/convoy_review.go`

`evaluateSupplyRecheckGate(ctx, db, convoyID, logger)` counts `disposition='token_expired'` rows for the convoy's ask-branches and, if non-zero, holds the convoy in `ConvoyStatusAwaitingSupplyRecheck` until either (a) CodeArtifact comes back healthy and inline replay clears the deferrals, or (b) the operator manually resolves them. The convoy never silently ships with un-replayed deferrals.

## Invariants

- **No silent token-expired passthroughs.** Pattern **P-SupplyDeferral** (`docs/patterns/p-supply-deferral.md`) AST-walks every `internal/isb/rules/supply_*.go` file. Files that call CodeArtifact and reference `ErrTokenExpired` MUST also call `supplydeferral.RecordDeferral`; branch-level scan also rejects any `if errors.Is(err, codeartifact.ErrTokenExpired) {...}` block whose body lacks a `RecordDeferral` (or same-file `recordDeferral` helper) call.
- **No hardcoded allowlists for popular packages.** SUPPLY-002's allowlist source is *only* CodeArtifact `ListPackages`, refreshed daily by the dog. Zero baked-in package names ship in `supply_002.go`.
- **No LLM decides licenses.** SUPPLY-004 imports zero LLM packages — verified in the D5 closure: `supply_004.go` and `license_matrix.go` import only stdlib + in-house clients. Pairs absent from `license_matrix.yaml` land in advise-mode for human review.
- **No negative cache.** A 404 today might exist tomorrow; a stale package today might receive a new release tomorrow; an empty license today might be populated tomorrow. The 24h positive cache is keyed on `(ecosystem, name, version)`; negative outcomes re-evaluate every Run.
- **SUPPLY-005 is offline-capable.** Independent of CodeArtifact / AWS auth — the rule keeps running through token-expiry windows. Pattern P-SupplyDeferral exempts files that don't import the codeartifact client.
- **Bypass requires AUDIT-NNN + reason ≥ 10 chars.** Malformed `SUPPLY-BYPASS` markers produce no match; the finding still surfaces.

## Configuration

- **YAML.** `internal/isb/rules/license_matrix.yaml` — embedded at build time. PR-reviewable when it changes; D5 closure tracks the 13 covered repo licenses.
- **SystemConfig keys.**
  - `supply_allowlist_<ecosystem>` — newline-joined package name set; populated daily by `supply-allowlist-refresh`.
  - `supply_allowlist_<ecosystem>_last_refresh` — RFC3339 timestamp.
  - `supply_typosquat_preapproved` — operator-managed pre-approved set.
  - `supply_stale_threshold_days` — SUPPLY-003 threshold (default 730).
  - `supply_token_expired_notified` — `'1'` while we're waiting on the operator to run `umt artifacts`; cleared on the next successful Health probe.
- **AWS credentials.** No tokens cached; AWS SDK v2 default chain (`~/.aws/credentials`, `~/.aws/sso/cache/`, env vars) is read on every call. Domain `code-artifacts-prod`, account `801997600626`, region `us-east-1`. Per-ecosystem repos: `pypi-prod`, `npm-prod`, `rubygems-prod`, `maven-prod`. The `grpc-generic-prod` repo is excluded by design.

## Operator surface

### Dashboard

The ISB findings tab surfaces SUPPLY findings alongside ISB-* findings; deferred rows are flagged with their `disposition='token_expired'` state so the operator can see why a convoy is in `AwaitingSupplyRecheck`.

### CLI

- `force supply preapprove <ecosystem> <name>` — adds a name to the typosquat pre-approved set with durable audit (operator-rationale required). The dog will not flag this name on subsequent runs.

### Mail / Slack

Three D11 categories own the operator-facing surface:

- `supply_token_expired` (Tier-1, `mail+slack`) — debounced one-per-session ping when CodeArtifact returns 401; cleared on recovery.
- `supply_token_recovered` (Tier-3, `off`) — opt-in trace ping when the recovery dog replays deferrals successfully.
- `awaiting_supply_recheck` (Tier-2, `mail`) — fires when ConvoyReview holds a convoy in `AwaitingSupplyRecheck`.

The recovery flow is "operator runs `umt artifacts` to refresh AWS SSO → next 30-min tick of `supply-token-recheck` sees a healthy CodeArtifact → flag clears, deferrals replay, per-branch summary ping fires (Tier-3, opt-in)."

## See also

- [`closures/DELIVERABLE-5-CLOSURE.md`](../closures/DELIVERABLE-5-CLOSURE.md) — full per-rule status + CodeArtifact endpoints + license matrix audit.
- [`patterns/p-supply-deferral.md`](../patterns/p-supply-deferral.md) — token-expired non-passthrough invariant (P-SupplyDeferral).
- [`subsystems/notification-routing.md`](notification-routing.md) — D11 routing applied to the three supply-chain categories.
- [`subsystems/convoy-lifecycle.md`](convoy-lifecycle.md) — `AwaitingSupplyRecheck` convoy status and where it sits in the lifecycle.
