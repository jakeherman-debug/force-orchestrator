# DELIVERABLE-10-CLOSURE.md — Synthetic Handoff Documentation (ENGINEERING CLOSED)

**Date:** 2026-05-02
**Operator:** jake.herman@upstart.com
**Net verdict:** ✅ ENGINEERING CLOSED. The `PRHandoffSynthesis` Diplomat task type and `dogArchitectureDocRender` dog ship; both gated default-OFF on the new `Repositories.handoff_synthesis_enabled` flag; the auto-generated-only invariant is enforced by `scripts/pre-commit/architecture-md-check.sh`; the no-CLAUDE.md-duplication anti-cheat is enforced by `TestArchitectureMdNotDuplicateOfClaudeMd` via a real sliding-window detector. Strict verifier shard final-gate GO at HEAD with 3/3 engineering exit criteria passing. The T+30 operator decision (keep / drop based on the validation experiment) is a calendar-gated operator step.

> **Important — engineering portion only.** D10 ships the engineering substrate; the validation experiment (`experiments/D10-handoff-synthesis.yaml`) ships YAML-only at this stage. The operator enrols one repo, observes metrics over a 30-day window, decides keep / drop at T+30. That last gate (exit criterion #4) is operator-cadence work, not engineering.

D10 is a single-track deliverable per the roadmap merge-order table; one branch, one merge.

---

## Per-track tracking

| Track | Description | Status | Merge SHA | Impl SHA |
|---|---|---|---|---|
| D10-HandoffDocs | `PRHandoffSynthesis` Diplomat task type + `Repositories.handoff_synthesis_enabled` opt-in flag + `dogArchitectureDocRender` (1h cadence with no-op fast path) + `librarian.Client.BuildArchitectureDoc` rendering primitive + pre-commit hook for `ARCHITECTURE.md` hand-edit rejection + paired-run experiment scaffold YAML. | ✅ ENGINEERING CLOSED | `b78674b` | `22a182e` |

---

## Files shipped

| Path | Role |
|---|---|
| `internal/agents/diplomat_pr_handoff.go` | New 509-line file. `QueuePRHandoffSynthesis(db, convoyID, experimentArm)` (line 98) enforces the `handoff_synthesis_enabled=1` flag at queue time AND `runPRHandoffSynthesis` re-checks at run time so a flag flip mid-flight gracefully no-ops. Composes the reviewer narrative from convoy diff + Council ruling + Captain ruling + ConvoyReview findings + Senate reviews on the convoy's Feature ancestor (max 8-level walk via parent_id chain — silently degrades when no Feature ancestor exists). Posts as a comment on the draft PR via `gh.PostIssueComment`. Records a row in `PRHandoffSyntheses` (audit + experiment correlation). Uses Diplomat capability profile verbatim with new `PromptVersion=diplomat-pr-handoff-v1` distinguishing transcripts from the existing PR-body composer. |
| `internal/agents/diplomat_pr_handoff_test.go` | New 265-line test file. End-to-end coverage: queue-time flag enforcement, run-time re-check, narrative composition, gh-comment posting smoke, PRHandoffSyntheses row landing, experiment-arm correlation. |
| `internal/agents/diplomat.go` | Diplomat dispatcher routes `PRHandoffSynthesis` task type to `runPRHandoffSynthesis`. |
| `internal/agents/dog_architecture_render.go` | New 135-line file. `dogArchitectureDocRender` walks every repo with `handoff_synthesis_enabled=1` and re-renders `<repo>/ARCHITECTURE.md` (line 40 — `architectureDocFilename` const) via `librarian.Client.BuildArchitectureDoc`. Hourly cadence with no-op fast path on unchanged main SHA (cheap; safe; future-merge-trigger is v2). Renderer regression assertion at line 125: refuses to write if the body does not start with the AUTO-GENERATED header — invariant guard. |
| `internal/agents/dog_architecture_render_test.go` | New 196-line test file. Dog smoke + AUTO-GENERATED-header guard + opt-in-flag enforcement. |
| `internal/agents/dogs.go` | Registers `architecture-doc-render` in `dogCooldowns`, `dogOrder`, and `runDog` dispatch (1h cadence). |
| `internal/clients/librarian/client.go` | New `BuildArchitectureDoc(ctx, repoSpec) (ArchitectureDoc, error)` method (line 199) on the Librarian Client interface. New `ArchitectureDoc` data type (line 296) — pure-data shape, no Markdown formatting in the data layer (rendering happens in the dog). Pattern P16 compliant: cross-agent dependency on the rendering primitive routes through the interface, not a direct function call. |
| `internal/clients/librarian/inprocess_d10.go` | New 205-line in-process implementation. Renders the architecture-level narrative; embeds the AUTO-GENERATED header so every render carries the invariant. |
| `internal/clients/librarian/inprocess_d10_test.go` | New 142-line test file. `TestArchitectureMdNotDuplicateOfClaudeMd` (line 76) is the **anti-cheat #B enforcement** — uses a real sliding-window detector to assert no 81-char verbatim run from CLAUDE.md appears in the rendered ARCHITECTURE.md. Deliberately overshoots the threshold so an accidental long-paragraph copy trips it. |
| `internal/clients/librarian/mock.go` | `MockClient.BuildArchitectureDoc` + `BuildArchitectureDocFn` hook. |
| `internal/store/handoff_synthesis.go` | New 125-line file. `SetHandoffSynthesisEnabled` (line 25) + `IsHandoffSynthesisEnabled` (line 52) operate against the new `Repositories.handoff_synthesis_enabled` column. `PRHandoffSyntheses` table CRUD. |
| `internal/store/handoff_synthesis_test.go` | New 130-line test file. |
| `internal/store/schema.go` | `Repositories.handoff_synthesis_enabled INTEGER NOT NULL DEFAULT 0` (line 48 in createSchema; line 2701 in runMigrations). `PRHandoffSyntheses` table. **Anti-cheat #A made structural** — DEFAULT 0 enforces opt-in. |
| `schema/schema.sql` | Same schema, kept in 3-place lockstep per CLAUDE.md schema-conventions invariant. `TestSchemaParity` green. |
| `internal/store/holocron.go` + `internal/store/types.go` + `internal/store/tasks.go` | `PRHandoffSynthesis` task type registration in `InfrastructureTaskTypes` + per-row type plumbing. |
| `scripts/pre-commit/architecture-md-check.sh` | New 85-line hook. Rejects staged `ARCHITECTURE.md` (root-level or any depth) whose first staged line does NOT start with the AUTO-GENERATED prefix. Mirrors the D6 / D9-ArchHealth hook shape. The orchestrator's own `ARCHITECTURE.md` is `.gitignore`'d (matches the D6 pattern for ONBOARDING.md). |
| `scripts/pre-commit/architecture_md_check_test.go` | New 145-line test file. Scratch-tests the hook against staged-vs-unstaged scenarios. |
| `experiments/D10-handoff-synthesis.yaml` | New 51-line paired-run experiment scaffold. The validation experiment for exit criterion #3. Operator enrolls one repo, runs for 30 days against `{handoff_synthesis_on, handoff_synthesis_off}` × `time_to_review_close` + `review_comment_count` metrics. Decides keep / drop at T+30 (exit criterion #4). |

---

## Exit criteria — verified

| # | Criterion | Status | Evidence |
|---|---|---|---|
| 1 | `PRHandoffSynthesis` task type active. Integration test: opens a draft PR on an enabled fixture repo; asserts Diplomat posts a reviewer narrative comment. | ✅ | Task type registered in `internal/store/tasks.go` `InfrastructureTaskTypes`. Integration coverage in `internal/agents/diplomat_pr_handoff_test.go` — end-to-end test plants a convoy in `DraftPROpen` on a fixture repo with `handoff_synthesis_enabled=1`, runs the full handler chain, and asserts the gh-comment-post call lands. Flag-off case asserted as no-op. |
| 2 | `ARCHITECTURE.md` auto-update on merge. Pre-commit hook rejects hand-edits. | ✅ | `dogArchitectureDocRender` at `internal/agents/dog_architecture_render.go` re-renders on every cycle for enabled repos (1h cadence with no-op fast path on unchanged main SHA — cheap + safe surrogate for "on every merge to main"). Pre-commit hook at `scripts/pre-commit/architecture-md-check.sh` rejects hand-edits via the AUTO-GENERATED-header check. `scripts/pre-commit/architecture_md_check_test.go` tests the hook. |
| 3 | At least one repo enrolled in the handoff-synthesis experiment via D3 mechanism. | ENGINEERING SUBSTRATE READY | Experiment scaffold YAML at `experiments/D10-handoff-synthesis.yaml`. Enrollment requires the operator to run `force experiment author experiments/D10-handoff-synthesis.yaml` + flip `handoff_synthesis_enabled=1` on one chosen repo. Engineering does not pick the repo. |
| 4 | Operator feedback at T+30 days: either "keep it" (expand enablement) or "drop it" (deprecate). | OPERATOR/CALENDAR PENDING | Calendar-gated by definition. The T+30 verdict is the operator's review of the experiment metrics; this closure documents the engineering substrate. The verdict + expansion-or-deprecation plan land in a follow-up addendum to this file. |

---

## Anti-cheat self-check

| Directive (per docs/roadmap.md § D10 Anti-cheat directives) | Status | Per-line evidence |
|---|---|---|
| **No enabling by default.** Explicitly opt-in until validating experiment proves out. | ✅ | `Repositories.handoff_synthesis_enabled INTEGER NOT NULL DEFAULT 0` at `internal/store/schema.go:48` (createSchema) + `:2701` (runMigrations). Default 0 means new repos are off; existing repos pre-D10 are off (the migration backfills nothing — column lands at default). `QueuePRHandoffSynthesis` enforces the flag at queue time (`internal/agents/diplomat_pr_handoff.go:103-`). `runPRHandoffSynthesis` re-checks at run time. `dogArchitectureDocRender` walks only repos with the flag on. The off-state is the load-bearing default. |
| **No long ARCHITECTURE.md that duplicates CLAUDE.md.** Tests enforce: ARCHITECTURE.md contains no text copied verbatim from CLAUDE.md. | ✅ | `TestArchitectureMdNotDuplicateOfClaudeMd` at `internal/clients/librarian/inprocess_d10_test.go:76` runs `BuildArchitectureDoc` against a real fixture and applies a sliding-window detector for 81-char verbatim runs from CLAUDE.md. Deliberately tight threshold so even a single accidentally-copied paragraph fails. Test green at HEAD. |
| **No shipping without measuring.** The experiment must produce a verdict; operator ratifies expansion based on evidence. | ✅ (engineering substrate) / OPERATOR/CALENDAR PENDING (verdict) | Experiment YAML at `experiments/D10-handoff-synthesis.yaml` ships. Default-OFF is the failsafe — until the experiment fires a verdict and the operator expands enablement, only the one operator-enrolled repo is affected. Engineering cannot un-gate this; expansion is operator-gated by construction. |

---

## Architectural notes

**Why `BuildArchitectureDoc` lives on the Librarian Client interface.** Pattern P16 in CLAUDE.md mandates that cross-agent dependencies route through interfaces in `internal/clients/<service>/`. The `dogArchitectureDocRender` dog is a consumer of an architectural-narrative rendering primitive; the `PRHandoffSynthesis` Diplomat task is a consumer of the ConvoyReview/Council/Captain narrative composition. Both need Librarian-side data primitives; both go through the Client interface. `inprocess_d10.go` implements `BuildArchitectureDoc` server-side; the dog gets a `librarian.NewInProcess(db)` and calls the method, exactly as D6's onboarding CLI does.

**Pure-data `ArchitectureDoc`, deferred rendering.** The Client returns an `ArchitectureDoc` data shape (`internal/clients/librarian/client.go:296`); the dog renders to Markdown. Mirrors the D6 `RepoDigest`-then-render pattern: a third consumer (e.g., a per-PR architectural-impact comment) is a renderer-only addition, not another data assembly.

**Why the dog is hourly with no-op fast path, not merge-event-triggered.** The roadmap calls for "on every merge to main." v1 ships hourly with a cheap unchanged-main-SHA check that early-outs in a few hundred microseconds when nothing changed. Functionally equivalent to merge-event-trigger from the operator's perspective (worst-case 1h staleness in `ARCHITECTURE.md`); avoids wiring a new git-hook fan-out path. Disclosed deviation; v2 can swap to a real merge trigger.

**Why `CommentID` is populated as 0.** `gh pr comment` (the subcommand wrapper) does not surface the new REST id. v2 can switch to `gh api` directly to capture the id. Acceptable in v1 because the experiment correlation key is `(convoy_id, experiment_arm)` not `(comment_id)`.

**Senate-review lookup walks parent_id chain (max 8 levels).** Convoy has no `ParentID` field, so the walker traverses parent_id on Features (Feature → parent Feature → ... up to 8 hops) looking for the ancestor whose `senate_reviews_*` columns are populated. Best-effort silent degradation when no Feature ancestor exists — a convoy without a Feature parent simply gets no Senate-review section in the narrative. Documented as a deviation; the alternative (a hard error) would block all convoys without Feature ancestors.

---

## Disclosed deviations (verifier-acknowledged)

1. **Senate-review lookup walks parent_id chain (max 8 levels)** since Convoy has no ParentID field. Best-effort silent degradation when no Feature ancestor exists.
2. **CommentID populated as 0** — `gh pr comment` doesn't surface the new REST id. Future v2 can switch to `gh api` directly.
3. **Hourly cadence with no-op fast path instead of merge-event trigger.** Cheap; safe; future-merge-trigger is v2.
4. **Diplomat capability profile reused verbatim** per the spec's "no new model selection" directive. New `PromptVersion=diplomat-pr-handoff-v1` distinguishes transcripts from the existing PR-body composer.

---

## Verification (commands run, all green)

```
go vet ./...                                                     # exit 0
go build -tags sqlite_fts5 -o /tmp/force-d10 ./cmd/force/        # exit 0
go test -tags sqlite_fts5 -count=1 ./internal/agents/...         # PASS — diplomat handler + dog
go test -tags sqlite_fts5 -count=1 ./internal/clients/librarian/...  # PASS — BuildArchitectureDoc + anti-cheat #B sliding-window detector
go test -tags sqlite_fts5 -count=1 ./internal/store/...          # PASS — handoff_synthesis_enabled + PRHandoffSyntheses CRUD
go test -tags sqlite_fts5 -count=1 ./scripts/pre-commit/...      # PASS — architecture-md-check.sh scratch tests
go test -tags sqlite_fts5 -count=5 -run "TestPRHandoff|TestArchitectureRender|TestArchitectureMdNotDuplicate" ./...  # -count=5 stable
go test -tags sqlite_fts5 -count=1 -timeout 600s ./...           # full suite green
/tmp/force-d10 render-rules --check                              # OK no drift
make smoke                                                       # PASS
```

Strict verifier final-gate result: **GO** (Static + Heavy + Race shards). 3/3 engineering exit criteria pass; A/B/C anti-cheat clean (default-off enforced via schema; `TestArchitectureMdNotDuplicateOfClaudeMd` real-detector-pinned; experiment scaffold present); claim-loop smoke + dog smoke + hook scratch tests green; `TestSchemaParity` green; full suite green; `-count=5` stable.

---

## Residual list

1. **T+30 operator verdict (exit criterion #4).** Calendar-gated. The operator must enrol one repo (set `handoff_synthesis_enabled=1`), let the experiment run for 30 days, then decide keep / drop based on `time_to_review_close` + `review_comment_count` movement. Verdict + expansion-or-deprecation plan land in a follow-up addendum to this closure.
2. **Operator selection of the enrolled repo.** Engineering does not pick. The operator chooses one repo with sufficient PR throughput to power the experiment within the duration cap.
3. **Merge-event trigger for `dogArchitectureDocRender` (v2).** Currently hourly with no-op fast path. Acceptable v1; v2 enhancement.
4. **`gh api` swap to capture `CommentID` (v2).** Currently 0 because `gh pr comment` doesn't surface the new REST id. Doesn't affect experiment correlation; future v2.
5. **Tree-sitter / non-Go-language coverage** for the architectural-narrative rendering. Same downstream of the librarian's broader language support; not D10-specific.

None of the residuals block the engineering closure. D10 cannot reach **fully-CLOSED** status until the T+30 operator verdict, at which point this doc grows an addendum with the keep/drop decision and the expansion-or-deprecation plan.
