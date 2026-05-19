# DELIVERABLE-14-CLOSURE.md — Senate Knowledge-First Advisor + Tag-Driven Rule Scoping

**Closed:** 2026-05-18
**Branch:** migration/initial-import
**Commits:**

```
43496b4 fix(audittools): add P3 dispatcher commands to P_CLIFlagParsing allowlist
da5858e feat(d14-p5): PromotionProposal migration — LLM classifier, knowledge absorption, rule surfacing
8ae1cfe feat(d14-p4): dashboard Rules tab, tag registry UI, tag-suggestion banner, Pulse tag-suggestion card
9a9d241 feat(d14-p3): CLI surface — force repos tag/untag/tags, force tags, force tag-suggestions, force rules
cc6cd90 feat(d14-p2): senate-onboarding rewrite — 3 LLM outputs, auto-active, senate-refresh dog
d015acf Merge branch 'd14/p1-schema' — D14 P1: Tags/RepoTags/TagSuggestions schema + ResolveRulesForRepo
27e8d6f feat(d14-p1): Tags/RepoTags/TagSuggestions schema, ResolveRulesForRepo, SenateChamber auto-activate
```

---

## What shipped

### P1 — Schema + store foundation

**Three new tables** added to both `createSchema` and `runMigrations` (3-place parity: schema.go + schema/schema.sql):

- `Tags` — operator-defined label registry (name PK, description, created_at, created_by)
- `RepoTags` — many-to-many join between repos and tags, FK `tag REFERENCES Tags(name)`, composite PK `(repo_name, tag)`, `source ∈ {operator, librarian-suggestion, dog-refresh}`; index `idx_repotags_tag`
- `TagSuggestions` — LLM-proposed tags awaiting operator review; `status ∈ {pending, accepted, dismissed}`; index `idx_tag_suggestions_status(status, suggested_at)`

**Two new columns** on `PromotionProposals` (P5, but added in `runMigrations` alongside P1 tables):

- `classification_status TEXT DEFAULT ''` — `'' | absorbed_as_knowledge | awaiting_scope_review`
- `suggested_scope TEXT DEFAULT ''` — LLM-recommended scope for enforceable-rule candidates
- Supporting index: `idx_promotion_proposals_classification WHERE classification_status != ''`

**Store helpers shipped** (`internal/store/tags.go`):

- Tags CRUD: `CreateTag`, `GetTag`, `ListTags`, `DeleteTag`
- RepoTags: `AddRepoTag` (idempotent via INSERT OR IGNORE), `RemoveRepoTag`, `ListTagsForRepo`, `ListReposForTag`
- TagSuggestions: `CreateTagSuggestion`, `ListTagSuggestions` (status-filtered), `ResolveTagSuggestion`
- `ResolveRulesForRepo(db, repoName)` — returns the union of `senate:*` + `senate:<repo>` + all `senate:tag:<t>` for tags the repo carries; ordered by `rule_key`
- `FleetRulesRow` struct added to `store` package (previously only existed in the promotions layer)

**PromotionProposals store helpers** (`store.ListPendingPromotionProposals`, `store.SetProposalClassification`) added to support P5.

**SenateChamber auto-activate** in `runMigrations`: one-time UPDATE flips `onboarding` chambers to `active` when they have at least one `SenateMemory` row. Idempotent (re-run matches zero rows).

**Pattern tests** (all in `internal/audittools/audit_pattern_p_d14_tags_test.go`):

| Test | What it asserts |
|---|---|
| `TestPattern_P_RuleScopeSyntaxValid` | Canonical regex `^senate:(\*\|tag:[a-z0-9_-]+\|[a-zA-Z0-9_/-]+)$` correctly accepts/rejects scope values |
| `TestPattern_P_SenateNoRepoTagsWrites` | AST walk of `internal/senate/*.go` — no raw `INSERT INTO RepoTags` SQL in Senate package |
| `TestPattern_P_TagRegistryEnforced` | In-memory DB: FK constraint fires when inserting a RepoTags row with a tag absent from Tags |
| `TestPattern_P_ResolveRulesForRepoComplete` | In-memory DB: 4 matching + 1 non-matching rule → `ResolveRulesForRepo` returns exactly 4 |
| `TestPattern_P_D14_TagCRUD` | Full CRUD lifecycle for Tags, RepoTags, TagSuggestions including FK failure modes |

---

### P2 — Senate-onboarding rewrite

**`runSenatorOnboardingTask`** rewritten in `internal/agents/senate.go` to produce three LLM outputs in a single prompt call:

1. `knowledge_digest` — factual observations about the repo (persisted as `SenateMemory` rows with `type="knowledge_digest"`)
2. `rule_suggestions` — enforceable coding/process standards (persisted via `lib.EmitCandidate` into `PromotionProposals`)
3. `tag_suggestions` — tag labels the repo likely belongs to (persisted as `TagSuggestions` rows; missing Tags created first)

**Auto-active transition**: after the three outputs are persisted, the handler unconditionally calls `store.SetSenateChamberStatus(db, repoID, "active")`. No ratification gate. A senator that completes onboarding is immediately operational.

**`runSenatorRefreshTask`** added: re-runs the same three-output LLM pass for an already-active senator on cadence. Does not change chamber status (stays `active`); bumps `last_refreshed_at`.

**`dogSenateRefresh`** added in `internal/agents/dogs.go`: cadence `7 * 24h`; iterates every active `SenateChamber` and queues a `SenatorRefresh` task for each one that does not already have a `Pending` or `Locked` task of that type (dedup check).

**Tests** (`internal/agents/senate_d14_p2_test.go`):

- `TestSenatorOnboarding_D14P2_TransitionsToActive` — LIVE_HAIKU_DISABLED stub completes and transitions chamber to `active`
- `TestSenatorOnboarding_D14P2_NewTagCreated` — tag suggestions with an unknown tag cause the tag to be auto-created in Tags
- `TestSenatorOnboarding_D14P2_EmptyMemory` — empty LLM outputs complete gracefully
- `TestDogSenateRefresh_D14P2_SkipsDuplicate` — dedup gate fires
- `TestDogSenateRefresh_D14P2_QueuesRefreshTasks` — queues one task per active senator
- `TestSenatorRefreshTask_D14P2_RoundTrip` — completes and leaves status as `active`

---

### P3 — CLI surface

All 12 commands shipped in `cmd/force/`:

| Command | File |
|---|---|
| `force repos tag <repo> <tag> [--added-by]` | `tags_cmds.go` |
| `force repos untag <repo> <tag>` | `tags_cmds.go` |
| `force repos tags [--repo X] [--tag Y]` | `tags_cmds.go` |
| `force tags list` | `tags_cmds.go` |
| `force tags create <name> [--description "..."]` | `tags_cmds.go` |
| `force tags remove <name> [-y]` | `tags_cmds.go` |
| `force tag-suggestions list [--repo X] [--status S]` | `tag_suggestions_cmds.go` |
| `force tag-suggestions accept <id>` | `tag_suggestions_cmds.go` |
| `force tag-suggestions dismiss <id> [-y]` | `tag_suggestions_cmds.go` |
| `force ec promote <id> --scope <global\|tag:X\|repo:Y>` | `ec.go` (extended) |
| `force rules list [--scope S] [--repo R]` | `rules_cmds.go` |
| `force rules upgrade <rule-key> --to-scope <spec>` | `rules_cmds.go` |

`force repos tag` auto-creates the tag in the Tags registry if it does not already exist (silent UNIQUE-constraint ignore on race). `force tag-suggestions accept` atomically creates the Tag if absent, inserts the RepoTag, then marks the suggestion accepted. A P_CLIFlagParsing allowlist update (`43496b4`) added the three new dispatcher groups to the audittools AST pattern test.

---

### P4 — Dashboard surfaces

Added in `internal/dashboard/handlers_tags.go` and wired via `dashboard.go`:

**REST API routes** (`/api/tags`, `/api/tags/`, `/api/tag-suggestions`, `/api/tag-suggestions/`, `/api/rules`, `/api/rules/`):

- `GET/POST /api/tags` — list all tags / create a tag
- `DELETE /api/tags/{name}` — remove a tag
- `GET /api/repos/{name}/tags` / `POST /api/repos/{name}/tags` / `DELETE /api/repos/{name}/tags/{tag}`
- `GET /api/tag-suggestions?status=…` — list suggestions (status filter)
- `POST /api/tag-suggestions/{id}/accept` — accept: creates RepoTag + marks accepted
- `POST /api/tag-suggestions/{id}/dismiss` — dismiss
- `GET /api/rules?repo={name}` — resolved rules via `ResolveRulesForRepo` or all active
- `POST /api/rules/{key}/upgrade-scope` — updates `agent_scope` with scope validation

**SPA surfaces** (`internal/dashboard/static/app.js`):

- **Senate "Rules" sub-tab** (`loadRulesTab`): repo selector, per-repo or global rule list, inline scope-upgrade modal (`confirmUpgradeScope` → `POST /api/rules/{key}/upgrade-scope`)
- **Tag registry view** (`loadTagRegistry`): list all tags with create/remove actions
- **Tag-suggestion banner in Briefing** (`fetchAndShowTagSuggestionBanner`): appears when there are pending tag suggestions, links to the tag-suggestions review surface
- **Pulse vital-sign card** (`renderPulseTagSuggestions`): "Pending tag suggestions" count card on the Pulse surface; refreshed with each `loadPulse` call

---

### P5 — PromotionProposal migration

**`RunMigrationClassifyProposals`** in `internal/agents/migration_classify_proposals.go`:

- Loads all unclassified pending `PromotionProposals` (those with `classification_status = ''`)
- Classifies in batches of 20 using a dedicated LLM call (profile: `migration-classifier`)
- `knowledge_observation` branch: calls `absorbProposalAsKnowledge` — inserts a `SenateMemory` row for the inferred senator, marks proposal `classification_status = 'absorbed_as_knowledge'`
- `enforceable_rule` branch: writes `classification_status = 'awaiting_scope_review'` and stores `suggested_scope` (defaults to `senate:*` if LLM returns empty)
- Fully idempotent: `SetProposalClassification` guards on `WHERE classification_status = ''`
- Sends a fleet-mail summary to `operator` listing absorbed count, rule count, and per-rule scope suggestions
- `--dry-run` mode: prints what would happen without writing to the DB

Operator flow (`force migrate classify-proposals`):

```
force migrate classify-proposals --dry-run   # preview
force migrate classify-proposals             # execute (idempotent, safe to re-run)
force rules list                             # review surfaces enforceable rules with suggested scopes
force rules upgrade <key> --to-scope senate:*   # promote a rule to desired scope
```

---

## How to run the P5 migration

1. `force migrate classify-proposals --dry-run` — preview what would be classified; no writes
2. Review the output — knowledge observations vs. enforceable rule candidates
3. `force migrate classify-proposals` — execute (idempotent; re-running is a no-op for already-classified rows)
4. `force rules list` — review surfaced enforceable rules with `awaiting_scope_review` status and LLM-suggested scopes
5. For any rule you want to promote: `force rules upgrade <key> --to-scope senate:*` (or `senate:tag:<tag>` or `senate:<repo>`)
6. Fleet-mail summary is automatically sent to `operator` after a real run (not dry-run)

---

## What was fixed

- **17 senators stuck in `onboarding` status** — auto-activated by the `runMigrations` UPDATE on next daemon start, provided they have at least one `SenateMemory` row
- **78 pending PromotionProposals** unclassified — `force migrate classify-proposals` buckets them as knowledge (auto-absorbed) or enforceable rules (surfaced for operator scope review)
- **Senate no longer requires operator ratification** to become operational — `runSenatorOnboardingTask` now transitions directly to `active` upon completing the knowledge digest, removing the blocking gate that kept every chamber in `onboarding`

---

## Invariants introduced

- `senate:*` — global FleetRules scope (new); applies to all repos
- `senate:tag:<tag>` — tag-scoped FleetRules (new); applies to repos carrying that tag
- `senate:<repo>` — single-repo scope (unchanged from D4, still valid)
- Tags are **flat** (no hierarchy) in D14; tag-of-tags deferred
- `P_SenateNoRepoTagsWrites` enforces that the Senate package (`internal/senate/`) never writes directly to `RepoTags` — only `store.AddRepoTag` may do so
- `P_TagRegistryEnforced` — the FK constraint `RepoTags(tag) REFERENCES Tags(name)` is enforced at the DB level (PRAGMA foreign_keys=ON)
- `TagSuggestions` FK requires the tag to exist in `Tags` before a suggestion can be accepted (the accept handler creates the Tag first if absent)
- `ResolveRulesForRepo` is the single canonical function for resolving all applicable rules for a repo — both CLI (`force rules list --repo`) and dashboard (`GET /api/rules?repo=`) route through it

---

## Out of scope (deferred)

- **Auto-tagging without operator confirmation** — librarian's `tag_suggestions` surface covers suggestions; auto-apply without operator review is a future deliverable
- **Tag-of-tags hierarchy** — tags are flat strings in D14
- **Conflict resolution between tag rules and repo rules** — both fire as separate Senate verdicts; operator decides; no precedence ordering in D14
- **Per-rule expiry / sunset dates** — existing `FleetRules.active_until` covers manual sunset; automated sunset scheduling is deferred
- **D15 → D14 hook** (richer tag signals from API surface graph) — D14 tag suggestions derive from README parsing; the richer file-detection signals (`has .proto` → `grpc-server`, `config/routes.rb` → `has-rest-api`) are a follow-up patch once D15 lands

---

## Spec deviations

**`suggested_by_senator` status vs. `EmitCandidate` routing.** The roadmap spec (§ "Phases" P2 description) uses the language `status="suggested_by_senator"` when describing how `rule_suggestions` land in `PromotionProposals`. The actual implementation routes these through `lib.EmitCandidate`, which inserts with `kind='candidate'` and `authored_by='librarian'` — the same shape as any other librarian-emitted candidate.

This is correct. `EmitCandidate` is the single, audited, pattern-P34-compliant ingress for PromotionProposals. The spec phrase `suggested_by_senator` was a documentation shorthand for "this proposal originated from a Senator's analysis", not a new `status` value. Using `EmitCandidate` preserves:

1. Pattern P34 (`audit_pattern_p34_senate_no_self_promote_test.go`) — Senate cannot write FleetRules directly; all promotion goes through the PromotionProposal pipeline
2. The existing EC `ExperimentAuthor` consumer, which reads `kind='candidate'` rows authored by `'librarian'`
3. Idempotency — `EmitCandidate` is the established, tested path; a new `status` field would have required a parallel insertion path with no additional value

The downstream P5 classifier identifies senator-originated knowledge vs. rules by examining `rule_key` structure and `authored_by` field, not by a special status value.
