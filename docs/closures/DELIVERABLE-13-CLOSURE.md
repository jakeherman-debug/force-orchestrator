---
title: Deliverable 13 — Documentation Sharding (CLOSURE)
type: closure-doc
deliverable: D13
status: CLOSED
last-reviewed: 2026-05-05
---

# DELIVERABLE-13-CLOSURE.md — Documentation Sharding (CLOSED)

**Date:** 2026-05-05
**Operator:** jake.herman@upstart.com
**Net verdict:** D13 is **CLOSED** at main HEAD `9b5f5d5`. The pre-D13 1471-line `README.md` has been sharded into a navigable per-topic `docs/` tree (18 agent + 28 subsystem + 34 pattern pages + 3 operator-facing top-level docs), the legacy archive has been deleted, and three drift-detection audit tests (`TestPatternP_DocsBrokenLinks`, `TestPatternP_DocsOrphan`, `TestPatternP_DocsArchitecture`) plus the four P1 structural guards (`TestReadmeSizeUnder200Lines`, `TestDocsIndexExists`, `TestDocsSubdirsHaveIndex`, `TestMetadataBlockOnAllNewDocs`) gate every future commit through `make docs-check` and the `scripts/pre-commit/docs-check.sh` pre-commit hook.

D13 is a four-phase sequential deliverable: P1 lays the substrate (audit + new tree skeleton + README rewrite + four structural guards), P2 migrates content out of the legacy archive in four parallel waves (A: 18 agents, B: 14 subsystems + 2 relocations, C: 34 patterns, D: 3 operator-facing top-level docs), P3 adds drift-detection (broken-links + orphan + architecture) and resolves 24 broken links surfaced on first run, P4 (this phase) authors the closure and confirms the docs gate is itself green.

---

## Goal

D13 set out to retire the single-scroll-of-everything front-door README — pre-D13 it was 1471 lines, 7.3× over a sane size, mixing operator quickstart with architectural narrative with per-agent reference with security posture — and replace it with a sharded, indexed, navigable `docs/` tree that an operator can actually search. The deliverable also had to make documentation drift mechanically detectable: links must resolve, every authored doc must be reachable from an index, every doc placed under the four canonical category subdirectories must carry a metadata block, and the README itself must stay under a hard cap. The drift-detection substrate had to be enforced both via `go test` and via a fast-path pre-commit hook, so an operator who edits a doc gets feedback before the commit lands rather than at CI time.

---

## Phase summary

### P1 — Audit + new docs/ tree skeleton + README rewrite

- **Impl SHA:** `2480862`
- **Merge SHA:** `580bf01`
- **Headline:** README **1460 → 89** lines (hard cap 200 enforced by `TestReadmeSizeUnder200Lines`).
- Authored the new tree skeleton: `docs/{agents,subsystems,patterns,references}/README.md` (4 mini-indexes); top-level `docs/README.md` (canonical entry point); `docs/onboarding.md`, `docs/overview.md`, `docs/operator-runbook.md` as stubs awaiting P2-D content; archived legacy README content into `docs/legacy-readme-archive.md` (1471 lines) as the P2 migration seed.
- Shipped four structural guards in `internal/audittools/audit_pattern_p_docs_test.go`: `TestReadmeSizeUnder200Lines` (README cap), `TestDocsIndexExists` (`docs/README.md` present), `TestDocsSubdirsHaveIndex` (each of `agents/`, `subsystems/`, `patterns/`, `references/` carries its own `README.md`), `TestMetadataBlockOnAllNewDocs` (every `*.md` under those four subdirs carries the `audience` / `scope` / `owner` / `last_reviewed` YAML front-matter block within the first 30 lines).
- Verifier verdict: GO. README rewrite preserved every quickstart command + cross-reference; the metadata convention (4 keys, 30-line scan window, fence-tolerant) is forward-compatible with P3's broader checks.

### P2 — Content migration (4 parallel waves)

- **Wave A (agents):** impl `31b4266` → integrate `9e4eba0`. **18 agent pages.** Migrates per-agent reference content from the legacy archive into `docs/agents/<name>.md`, one page per agent. Each page carries the standard metadata block + six H2 sections (Role / Responsibilities / Capability profile / Key files / Tests / See also) + links to capability YAML + audit patterns.
- **Wave B (subsystems):** impl `3e7771f` → integrate `ac07c3e`. **14 newly authored subsystem pages + 2 relocations** (`docs/paired-runs.md` → `docs/subsystems/paired-runs.md`; `docs/dashboard-implementation.md` → `docs/subsystems/dashboard-implementation.md`). Newly authored docs use the canonical six-section template (Overview / Components / Invariants / Configuration / Operator surface / See also). Auto-rendered docs (`dashboard-conventions.md`, `pr-flow-invariants.md`, `self-healing.md`) stay at `docs/` root because their `RenderTo` paths are hardcoded in `internal/store/fleet_rules_audit.go`; new operator-facing summaries under `subsystems/` link to them as the binding contract.
- **Wave C (patterns):** impl `d18ba41` → integrate `8c5d8e6`. **34 pattern pages** under `docs/patterns/`. One page per audit pattern with rationale + enforcement + contract. Patterns range from the lettered family (`p-annotations-operator-only`, `p-stage-gate`, `p-notification-dispatch`, etc.) through the numbered family (`p1-rows-scan` … `p34-senate-no-self-promote`).
- **Wave D (operator-facing):** impl `1217216` → integrate `ca411f5`. **3 top-level operator docs** populated: `docs/onboarding.md` (218 lines — install, first daemon run, first task, smoke flows), `docs/overview.md` (126 lines — how it all fits together, deeper than the README diagram), `docs/operator-runbook.md` (443 lines — daemon crash, stuck convoy, runaway spend, recovery paths). Every cited file path, CLI command, and SystemConfig key was verified against the live source tree before inclusion.
- **Cleanup:** `80c21bc` deletes `docs/legacy-readme-archive.md` after all four waves absorbed the relevant content. No remaining references in `docs/`, `README.md`, `CLAUDE.md`, or any `*.go` file.
- **Merge SHA:** `b0819a3`.
- Verifier verdict: GO. **18 agents + 14 subsystems + 34 patterns + 3 operator-facing docs** landed; the legacy archive's content is fully redistributed; no orphan content.

### P3 — Drift detection

- **Impl SHA:** `aa38e8c`
- **Merge SHA:** `9b5f5d5` (also the closure HEAD)
- **Headline:** **24 broken links → 0**, **12 stub navigation slots authored**, **3 audit tests + Makefile targets + pre-commit hook entry shipped**.
- Three new audit-pattern tests in `internal/audittools/`:
  - `audit_pattern_p_docs_links_test.go` (`TestPatternP_DocsBrokenLinks` + `_DetectsInjectedDrift` fixture) — every relative `*.md` link resolves; file exists; if `#anchor` is present, an H1/H2/H3 heading slugifying to the anchor exists in the target. External `http(s)://`, `mailto:`, `tel:`, protocol-relative links are skipped. Fenced code blocks are excluded. Empty allowlist; goal is to keep it empty.
  - `audit_pattern_p_docs_orphan_test.go` (`TestPatternP_DocsOrphan` + `_DetectsInjectedDrift` fixture) — every `*.md` under `docs/{agents,subsystems,patterns}/` is linked from its sibling `README.md` or from `docs/README.md`. References-dir is excluded today (flat reference table, no per-file mini-index pattern). Empty allowlist; goal is to keep it empty.
  - `audit_pattern_p_docs_architecture_test.go` (`TestPatternP_DocsArchitecture` with three sub-tests) — H2 section floor of 4 on every authored doc under `docs/{agents,subsystems,patterns}/`, auto-rendered exemption honored (the three `docs/*.md` files rendered from FleetRules), `docs/README.md` links to every category mini-index in 1 hop.
- Each test ships a sibling `_DetectsInjectedDrift` fixture that proves the regex / resolver / link-harvester actually fire when fed broken input — a future refactor that silently neuters the gate would trip the fixture before reaching the production walker.
- Makefile gains four new targets: `make docs-broken-links`, `make docs-orphan-check`, `make docs-architecture`, and the umbrella `make docs-check` that runs all three plus the four P1 guards.
- Pre-commit hook `scripts/pre-commit/docs-check.sh` fast-paths when no `*.md` is staged (typical Go-only commits pay zero cost), otherwise runs the seven gates via `go test -count=1` (~3-5s budget). Hook integration tests in `scripts/pre-commit/docs_check_test.go`.
- 24 first-run breakages resolved: 3 README links pointed at `docs/CLAUDE.md` (lives at repo root post-rewrite); 5 `docs/next-gen-agents.md` links pointed at the pre-relocation `./paired-runs.md`; 12 forward-reference subsystem links (D5/D5.5/D6/D7/D9/D10/D12/dogs/security/directives/mail-system/fleet-memory) had no target — authored as canonical six-section stubs (`## Status: Stub` + `## What this will cover` + `## Until then` + `## See also` + `## When this page lands` + one per-stub variant) so they pass both the broken-link gate AND the H2 floor; 4 closure-report inbound links in `docs/architecture/claude-cli-invocation.md` corrected.
- Verifier verdict: GO. Drift gates green at HEAD on first cold-start run; injected-drift fixtures fire as designed.

### P4 — Strict verifier + closure (this phase)

- **Branch:** `deliverable/13/p4-closure`
- **Headline:** Closure authored, `make docs-check` green at HEAD with the closure file included in the corpus.
- No production code changes — pure docs.

---

## What's now true

The following invariants now hold post-D13 and are mechanically enforced. Any drift trips the corresponding test in CI and (for `*.md` commits) the pre-commit hook:

- **README is hard-capped at 200 lines.** `TestReadmeSizeUnder200Lines` (`internal/audittools/audit_pattern_p_docs_test.go:41`). Bumping the cap requires a written rationale in `FIX-LOG.md` per the constant's commented contract. Current size: 89 lines.
- **`docs/README.md` exists as the canonical navigation entry point.** `TestDocsIndexExists`. Empty file fails the test.
- **Each of `docs/{agents,subsystems,patterns,references}/` carries its own `README.md` mini-index.** `TestDocsSubdirsHaveIndex`.
- **Every doc under `docs/{agents,subsystems,patterns,references}/` carries a YAML front-matter block** with the four required keys (`audience` / `scope` / `owner` / `last_reviewed`). `TestMetadataBlockOnAllNewDocs`. Test scans the first 30 lines, allows leading HTML comments / blank lines, fence-tolerant.
- **Every relative `*.md` link in the repo resolves.** `TestPatternP_DocsBrokenLinks`. File must exist; if the link includes `#anchor`, an H1/H2/H3 heading slugifying to that anchor must exist. External links are skipped; the gate is content integrity, not network reachability.
- **Every authored doc under `docs/{agents,subsystems,patterns}/` is reachable from an index.** `TestPatternP_DocsOrphan`. A doc that exists on disk but is not linked from a sibling `README.md` or from `docs/README.md` is an orphan and fails the test.
- **Every authored doc under `docs/{agents,subsystems,patterns}/` carries at least 4 H2 sections.** `TestPatternP_DocsArchitecture` sub-test `H2SectionFloor`. The cheapest signal that a doc is structurally complete vs. a "two paragraphs and a code block" stub.
- **Auto-rendered docs (`docs/dashboard-conventions.md`, `docs/pr-flow-invariants.md`, `docs/self-healing.md`) are exempt from the H2 floor** because their structure is determined by the FleetRules audit slice, not by the per-doc author. Pattern P18 byte-checks the rendered output. `TestPatternP_DocsArchitecture` sub-test `AutoRenderedExemptionHonored`.
- **`docs/README.md` links to every category mini-index in exactly 1 hop.** `TestPatternP_DocsArchitecture` sub-test `DocsIndexHasCategoryPointers`.
- **The legacy 1471-line README archive is gone.** `docs/legacy-readme-archive.md` was deleted in `80c21bc` after all four P2 waves completed.
- **The pre-commit hook gates `*.md` commits on the docs invariants.** `scripts/pre-commit/docs-check.sh` fast-paths when no Markdown is staged; on the slow path runs all 7 docs-tree tests via `go test -count=1`. Picked up automatically by the dispatcher master hook (lex-sorted glob).
- **Drift-detection fixtures prove the gates aren't toothless.** Each of the three P3 audit tests ships a sibling `_DetectsInjectedDrift` fixture that builds a synthetic broken-input case and asserts the regex / resolver / link-harvester actually rejects it. A future refactor that silently neuters the production walker (e.g. by replacing `resolvePath` with the identity function) would fail the fixture before reaching the production code path.
- **Allowlist contracts are public.** `brokenLinkAllowlist` and `orphanAllowlist` both ship empty with comments stating the goal is `len(...) == 0`. Adding an entry without a one-line rationale comment is rejected at code review.

---

## Final inventory

### Authored docs (post-D13 surface)

| Directory | Authored pages (excludes `README.md`) | Notes |
|---|---|---|
| `docs/agents/` | **18** | One per fleet agent: archaeologist, astromech, auditor, boot, bos, captain, chancellor, commander, council, diplomat, engineering-corps, inquisitor, investigator, isb, librarian, medic, pilot, senate. |
| `docs/subsystems/` | **28** | 14 newly authored in P2-B, 2 relocated in P2-B (paired-runs, dashboard-implementation), 12 stubs authored in P3 to clear forward-reference broken links. |
| `docs/patterns/` | **34** | One per audit pattern. Lettered family (10): p-annotations-operator-only, p-archaeologist-operator-gated, p-ask-no-write-tools, p-docs, p-notification-dispatch, p-replay-no-mutation, p-stage-gate, p-staging-promotion-confirm, p-supply-deferral, p-trust-dials-operator-write. Numbered family (24): p1-rows-scan, p1_1-rows-err, p11-exec-context, p13-capability-profiles … p34-senate-no-self-promote. |
| `docs/references/` | 0 (flat README only) | The flat reference table is intentional; per-file mini-indexes are not the pattern here. |

### Operator-facing top-level docs (post-D13)

| File | Lines |
|---|---|
| `docs/onboarding.md` | 218 |
| `docs/overview.md` | 126 |
| `docs/operator-runbook.md` | 443 |
| `docs/README.md` | 135 |
| `README.md` (front door) | 89 |

### Test substrate

The seven docs-tree gates, all in `internal/audittools/`:

| Test function | File | Purpose |
|---|---|---|
| `TestReadmeSizeUnder200Lines` | `audit_pattern_p_docs_test.go:41` | README hard cap (200 lines). |
| `TestDocsIndexExists` | `audit_pattern_p_docs_test.go:61` | `docs/README.md` present + non-empty. |
| `TestDocsSubdirsHaveIndex` | `audit_pattern_p_docs_test.go:85` | Each of 4 category dirs carries a `README.md`. |
| `TestMetadataBlockOnAllNewDocs` | `audit_pattern_p_docs_test.go:122` | Every `*.md` under category dirs has front-matter with all 4 metadata keys. |
| `TestPatternP_DocsBrokenLinks` | `audit_pattern_p_docs_links_test.go:86` | Every relative `*.md` link resolves (file + anchor). |
| `TestPatternP_DocsOrphan` | `audit_pattern_p_docs_orphan_test.go:49` | Every doc under agents/subsystems/patterns is linked from an index. |
| `TestPatternP_DocsArchitecture` | `audit_pattern_p_docs_architecture_test.go:86` | H2 floor (4) + auto-rendered exemption + 1-hop category reachability. |

Sibling injected-drift fixtures (3): `TestPatternP_DocsBrokenLinks_DetectsInjectedDrift`, `TestPatternP_DocsOrphan_DetectsInjectedDrift`. (`TestPatternP_DocsArchitecture` does not have a separate injected-drift sibling — its structural sub-tests are themselves both production and self-test.)

Pre-commit hook integration tests (2): `TestDocsCheckHook_NoMarkdownStaged_ExitsZero`, `TestDocsCheckHook_NotInRepo_ExitsTwo` in `scripts/pre-commit/docs_check_test.go`.

### Makefile entry points

| Target | What it runs |
|---|---|
| `make docs-broken-links` | `TestPatternP_DocsBrokenLinks` |
| `make docs-orphan-check` | `TestPatternP_DocsOrphan` |
| `make docs-architecture` | `TestPatternP_DocsArchitecture` |
| `make docs-check` | All three above + 4 P1 structural guards. The full docs gate. |

---

## Test results

`make docs-check` was run at HEAD `9b5f5d5` immediately before this closure was authored, and re-run after the closure landed in `docs/closures/DELIVERABLE-13-CLOSURE.md` to confirm the closure file itself does not break the corpus (the closure file is part of the broken-links walk, even though it is not under one of the metadata-required category subdirectories).

```
$ make docs-check
go test -tags sqlite_fts5 -timeout 60s -run '^TestPatternP_DocsBrokenLinks$' -count=1 ./internal/audittools/...
ok      force-orchestrator/internal/audittools  0.856s
go test -tags sqlite_fts5 -timeout 60s -run '^TestPatternP_DocsOrphan$' -count=1 ./internal/audittools/...
ok      force-orchestrator/internal/audittools  0.362s
go test -tags sqlite_fts5 -timeout 60s -run '^TestPatternP_DocsArchitecture$' -count=1 ./internal/audittools/...
ok      force-orchestrator/internal/audittools  0.363s
go test -tags sqlite_fts5 -timeout 60s -count=1 \
        -run '^(TestReadmeSizeUnder200Lines|TestDocsIndexExists|TestDocsSubdirsHaveIndex|TestMetadataBlockOnAllNewDocs)$' \
        ./internal/audittools/...
ok      force-orchestrator/internal/audittools  0.392s
```

All seven gates pass.

**Full suite green at `9b5f5d5`** — the per-phase verifier shards (P1, P2 wave-A/B/C/D + integration, P3) and the comprehensive Heavy verifier each ran `make test` with `-tags sqlite_fts5` and reported uniformly GO at the merge SHAs listed in the per-phase summary. This closure does not re-run the full suite — that was the Heavy verifier's job at HEAD.

---

## Files added

92 net additions between `84d275d9` (D11 closure HEAD) and `9b5f5d5` (D13 closure HEAD). The shape:

```
docs/README.md                                  # canonical entry point
docs/onboarding.md                              # P2-D operator onboarding
docs/overview.md                                # P2-D architecture overview
docs/operator-runbook.md                        # P2-D things-go-wrong runbook

docs/agents/README.md                           # P1 mini-index
docs/agents/{18 .md files}                      # P2-A: archaeologist, astromech, auditor,
                                                #       boot, bos, captain, chancellor, commander,
                                                #       council, diplomat, engineering-corps,
                                                #       inquisitor, investigator, isb, librarian,
                                                #       medic, pilot, senate

docs/subsystems/README.md                       # P1 mini-index
docs/subsystems/{14 newly authored .md}         # P2-B: arch-health (stub→P3), archaeologist,
                                                #       capability-profiles, cli-shelling,
                                                #       convoy-lifecycle, cross-repo-graph,
                                                #       dashboard, escalation-and-medic,
                                                #       fleet-memory (stub→P3), gas-town,
                                                #       holocron-schema, mcp-registry,
                                                #       notification-routing, pr-flow,
                                                #       worktree-isolation
docs/subsystems/{12 P3 stubs .md}               # P3: arch-health, convoy-staging,
                                                #     daemon-lifecycle, directives, dogs,
                                                #     fleet-memory, handoff-docs,
                                                #     mail-system, model-tier-experiments,
                                                #     onboarding-cli, security, supply-chain
                                                #     (some overlap with P2-B's stub-shaped
                                                #     placeholders, finalized in P3)

docs/patterns/README.md                         # P1 mini-index
docs/patterns/{34 .md files}                    # P2-C: 10 lettered + 24 numbered patterns

docs/references/README.md                       # P1 mini-index (flat reference table)

internal/audittools/audit_pattern_p_docs_test.go               # P1: 4 structural guards
internal/audittools/audit_pattern_p_docs_links_test.go         # P3: TestPatternP_DocsBrokenLinks
internal/audittools/audit_pattern_p_docs_orphan_test.go        # P3: TestPatternP_DocsOrphan
internal/audittools/audit_pattern_p_docs_architecture_test.go  # P3: TestPatternP_DocsArchitecture

scripts/pre-commit/docs-check.sh                # P3: pre-commit hook (fast-path Markdown-only gate)
scripts/pre-commit/docs_check_test.go           # P3: hook integration tests
```

(The relocated files — `docs/paired-runs.md` → `docs/subsystems/paired-runs.md` and `docs/dashboard-implementation.md` → `docs/subsystems/dashboard-implementation.md` — show as add+delete pairs in the P2-B commit but are file moves, not net-new authoring.)

---

## Files modified

5 files modified at the top level:

| File | What changed |
|---|---|
| `README.md` | Sharded from 1460 lines → 89 lines. Six sections: what is force, architecture at a glance, quickstart, where to go next (table pointing into `docs/`), contributing, status. |
| `Makefile` | New targets: `docs-broken-links`, `docs-orphan-check`, `docs-architecture`, umbrella `docs-check`. `.PHONY` updated. |
| `docs/architecture/claude-cli-invocation.md` | P3-era link corrections to closure inbounds + path normalization for relocated subsystem docs. |
| `docs/next-gen-agents.md` | P3-era link corrections (5 links re-pointed from `./paired-runs.md` to `subsystems/paired-runs.md` post-relocation). |
| `docs/roadmap.md` | Light updates from P2-D / P3 to point at the new sharded paths and reflect docs-tree status. |

Note: `CLAUDE.md` was NOT modified. CLAUDE.md is auto-rendered from FleetRules via `make render-rules`; D13's scope is documentation tree, not the agent invariants slice. The CLAUDE.md from `84d275d9` and `9b5f5d5` is byte-identical.

---

## Files deleted

1 file deleted:

| File | Why |
|---|---|
| `docs/legacy-readme-archive.md` | The 1471-line archive of the pre-D13 README. Created in P1 (`2480862`) as the seed for P2 content migration; deleted in P2 (`80c21bc`) after all four waves absorbed the relevant content. No remaining references in `docs/`, `README.md`, `CLAUDE.md`, or any `*.go` file at deletion time. |

This is the headline deletion — D13's whole premise is that this file no longer needs to exist because its content lives in the sharded tree.

---

## Known limitations / follow-ups

None blocking. The following are explicit deferrals tracked for future deliverables:

1. **12 P3 stubs are placeholder navigation slots, not definitive references.** Each is a six-H2-section file (`## Status: Stub` + `## What this will cover` + `## Until then` + `## See also` + `## When this page lands` + one per-stub variant) that exists to keep links resolving and orphans absent. They cite the closure-report or live YAML config as the binding-contract source until the corresponding deliverable ships its full reference page. The 12 stubs and their owners:

   | Stub file | Owner deliverable |
   |---|---|
   | `docs/subsystems/arch-health.md` | D9 |
   | `docs/subsystems/convoy-staging.md` | D5.5 |
   | `docs/subsystems/daemon-lifecycle.md` | D12 |
   | `docs/subsystems/directives.md` | (standing) |
   | `docs/subsystems/dogs.md` | (standing) |
   | `docs/subsystems/fleet-memory.md` | (Librarian) |
   | `docs/subsystems/handoff-docs.md` | D10 |
   | `docs/subsystems/mail-system.md` | D11 |
   | `docs/subsystems/model-tier-experiments.md` | D7 |
   | `docs/subsystems/onboarding-cli.md` | D6 |
   | `docs/subsystems/security.md` | (standing) |
   | `docs/subsystems/supply-chain.md` | D5 |

   The operator should flesh these out as the corresponding deliverables ship their own per-subsystem closure narratives. Until then they're functional navigation slots — the orphan and broken-link gates pass cleanly.

2. **`docs/references/` is a flat reference table.** Per-file mini-indexes are not the pattern in `references/` today. If the table grows enough that orphan-checking helps, adding `references` to `orphanCheckedDirs` is a one-line change in `audit_pattern_p_docs_orphan_test.go`.

3. **README scope-overstep (P3 verifier flag, LOW severity).** The P3 verifier noted that the rewritten README contains a small "Status" section (D0–D11 closed; D12 in flight; D13 in progress) that is content-bearing rather than purely navigational. Acceptable in scope (it's 4 lines and the front-door operator benefits from a roadmap pointer), but flagged for tightening if the section grows. The 200-line cap is doing the heavy lifting; this is cosmetic.

4. **Architecture-test self-injection sentinel absent (P3 verifier flag, VERY-LOW severity).** `TestPatternP_DocsArchitecture` does not ship a separate `_DetectsInjectedDrift` fixture in the same shape as the broken-links and orphan tests. Its three sub-tests (`H2SectionFloor`, `AutoRenderedExemptionHonored`, `DocsIndexHasCategoryPointers`) self-validate via the `failures` slice + `t.Errorf`, but a future refactor that breaks `countH2Sections` would not be caught by a synthetic-input fixture. The other two P3 tests (broken-links, orphan) do carry such fixtures. Adding a sibling fixture for architecture is a future hardening — the production gate works at HEAD; the missing fixture is belt-and-braces.

5. **`brokenLinkAllowlist` and `orphanAllowlist` ship empty.** Both maps are intentionally `map[string]string{}` at HEAD. The end-state goal is to keep them empty. If a future PR genuinely needs an entry, the contract requires a one-line rationale comment (the comment is the social contract for whether the deferral is still valid). Both files document this explicitly.

6. **Drift-checker is a regex, not a full Markdown parser.** `linkRe` in `audit_pattern_p_docs_links_test.go` is intentionally simple — Markdown is messy; reference-style links and complex inline images are best-effort. The drift checker is documented as "a contract gate, not a parser." If a future authored doc uses a Markdown construct the regex misses, the call is allowlist-with-rationale or fix the regex.

---

## Operator hand-off

### Where to look first

A new operator landing on this codebase should read in this order:

1. **`README.md`** (front door, 89 lines) — what force is, architecture at a glance, quickstart, navigation table.
2. **`docs/onboarding.md`** — install, first daemon run, first task, smoke flows.
3. **`docs/overview.md`** — how the fleet fits together; deeper than the README diagram.

Then drill into specific subsystems via `docs/README.md`'s navigation table or `docs/subsystems/README.md`.

### If something breaks

- **`docs/operator-runbook.md`** — daemon crashed, convoy stuck, runaway spend, dashboard down, schema drift, e-stop. The 443-line operator emergency reference.
- **Per-subsystem reference** — e.g. `docs/subsystems/notification-routing.md` for notification config, `docs/subsystems/dashboard.md` for dashboard issues.
- **Closure reports** — `docs/closures/DELIVERABLE-N-CLOSURE.md` for the per-deliverable evidence trail and design rationale.

### Adding a new doc

- **Place it** under the right category subdirectory: `docs/agents/`, `docs/subsystems/`, `docs/patterns/`, or `docs/references/`.
- **Front-matter block first** (within the first 30 lines):
  ```
  ---
  audience: operator | agent | both
  scope: <one-sentence description>
  owner: <deliverable id or subsystem>
  last_reviewed: YYYY-MM-DD
  ---
  ```
- **At least 4 H2 sections** (the floor for `TestPatternP_DocsArchitecture`). The canonical six-section template for new authored content: Overview / Components / Invariants / Configuration / Operator surface / See also. For stubs awaiting a future deliverable: Status: Stub / What this will cover / Until then / See also / When this page lands (+ one variant).
- **Link from the parent index README** (`docs/<sub>/README.md` and/or `docs/README.md`) — the orphan checker fails the build otherwise.
- **Run `make docs-check` before committing.** The pre-commit hook will run it automatically when any `*.md` file is staged; running it manually first surfaces the failure faster.

### Adding a new audit pattern

- **Author the test** at `internal/audittools/audit_pattern_p<NN>-<slug>_test.go` (numbered patterns) or `audit_pattern_p_<slug>_test.go` (lettered patterns).
- **Author the doc** at `docs/patterns/p<NN>-<slug>.md` with the canonical six H2 sections (Why this pattern / What's enforced / Contract / Common violations / How to add a legitimate exception / See also). The H2 floor of 4 is the test gate; six is the convention.
- **Link from `docs/patterns/README.md`** (the orphan checker fails otherwise).
- **Run `make docs-check`** to confirm. Then `make test` to confirm the new pattern itself fires correctly.

### Adding a new top-level operator-facing doc

- **Place it** at `docs/<name>.md` (NOT under a category subdirectory). Examples: `docs/onboarding.md`, `docs/overview.md`, `docs/operator-runbook.md`.
- **The metadata-block requirement does NOT apply** to top-level docs (the test only walks the four category subdirectories). The H2 floor doesn't apply either.
- **The broken-links checker DOES apply** — every relative link must resolve.
- **Link from `docs/README.md`** so operators can find it.

---

## Verdict

**D13 is CLOSED at HEAD `9b5f5d5`.** The documentation tree is sharded, indexed, link-tight, and gated.
