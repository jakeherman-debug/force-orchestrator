---
audience: both
scope: Design sketch for next-gen agents (Senate, ISB, BoS, Engineering Corps); D4 + D3 substrate framing.
owner: D13
last_reviewed: 2026-05-05
---

# Next-Generation Agents

Design sketch for four agents not yet built. Three are review layers at distinct lifecycle points; the fourth orchestrates experimentation across the whole fleet:

| Agent | Layer | Scope | Trigger |
|---|---|---|---|
| **Senate** | Plan-time | Domain-aware (per-repo / per-team) | Chancellor invokes during plan review |
| **Imperial Security Bureau (ISB)** | Commit-time | Security concerns in the diff | After astromech commit, before Captain |
| **Bureau of Standards (BoS)** | Commit-time | CLAUDE.md invariant enforcement | After astromech commit, before Captain |
| **Engineering Corps** | Experimentation-axis | Authors / monitors / promotes experiments | Librarian hypothesis arrival + dog cadence |

Each is motivated by a concrete failure class surfaced in the Code Red audit (docs/operator-archives/AUDIT.md) and its fix campaign. The pipeline position, scope, and cost model of each is chosen so they're additive to existing review gates, not duplicative.

**Companion doc.** Engineering Corps and the mechanisms it operates (experiments, holdout, metric/rule registries, three-layer versioning) are specified in detail in [paired-runs.md](subsystems/paired-runs.md). This doc covers the agent's role and boundaries; paired-runs covers the full experimentation primitive.

**Prerequisite.** Senate and Engineering Corps both depend on the Librarian evolution outlined at the bottom — Senators can't function without curated per-repo memory, and Engineering Corps can't function without a hypothesis stream from Librarian. Raw FleetMemory is too noisy to serve either purpose.

**Rule registry.** Rules owned by ISB, BoS, and Senate (and CLAUDE.md itself) live in the `FleetRules` DB table as versioned rows, not as hand-edited markdown files. The markdown files (`CLAUDE.md`, `SENATE.md`, `bos/rules/*.yaml`, `isb/finders/*.yaml`) are auto-rendered exports maintained by a `rule-renderer` dog. Operators change rules through the promotion pipeline (evidence-gated) or the direct-write escape hatch (operator-authored); manual edits to rendered files are rejected by a pre-commit hook. See paired-runs.md for the full rule-registry design.

---

## Pipeline position

```
Operator queues Feature
  │
  ▼
Commander decomposes → TaskPlan
  │
  ▼
┌─────────────────────────────┐
│  Senate router              │  ← NEW (plan-time)
│  identify affected Senators │
│  parallel Senator reviews   │
│  aggregate: concur/amend/   │
│  dissent                    │
└─────────────────────────────┘
  │
  ▼
Chancellor approves plan (with or without Senate amendments)
  │
  ▼
Convoy Active — astromechs claim tasks
  │
  ▼
Astromech commits
  │
  ▼
┌─────────────────────────────┐
│  ISB scan ── BoS scan       │  ← NEW (commit-time, parallel)
│  both must approve          │
└─────────────────────────────┘
  │
  ▼
Captain review (scope / coherence)
  │
  ▼
Jedi Council review (code quality)
  │
  ▼
sub-PR CI → auto-merge
  │
  ▼
ConvoyReview (completeness)
  │
  ▼
Operator Ship It
```

Five review layers total (Senate / ISB+BoS / Captain / Council / ConvoyReview). Each covers a different concern; overlap between layers is the exception, not the rule.

Engineering Corps operates on an orthogonal axis — not inline with any call in the pipeline above, but running alongside as the experimentation orchestrator. It authors experiments from Librarian hypotheses, monitors running experiments' posteriors and budgets, declares winners/nulls, and assembles promotion proposals. Its effects reach every agent in the pipeline through the `treatments.Apply` ingress that wraps LLM calls. See [Engineering Corps](#engineering-corps) and [paired-runs.md](subsystems/paired-runs.md).

---

## Imperial Security Bureau (ISB)

**Role.** Deterministic-first security scanner that runs after an astromech commits but before the commit reaches Captain. Blocks on hard security violations; annotates on soft ones. Orthogonal to Captain (which owns scope) and Council (which owns code quality).

**Rationale.** The Code Red audit identified 30+ security-class findings across six patterns (P8 dashboard exposure, P9 outbound exfil, P10 shell injection, P12 prompt injection, plus hardcoded-secrets and path-traversal classes). None of these were caught by the existing review gates because Captain/Council are LLM reasoners, not pattern-matchers. Deterministic security patterns need a deterministic gate.

### Trigger

After `Astromech` successfully commits and calls `store.UpdateBountyStatus(db, id, "AwaitingCaptainReview")`. ISB claims the task, runs its scan, and either:

- Approves — task forwards to Captain unchanged.
- Rejects — task returns to Pending with a `[SECURITY GUARD]` block prepended to the payload, incrementing `retry_count`.
- Annotates — task forwards to Captain with advisory notes in the payload.

Runs in parallel with BoS. Both must approve for the task to proceed to Captain.

### Operation

**Deterministic base layer.** Most ISB rules are regex or AST patterns that fire on a `git diff`. Wrap `gosec`, `semgrep`, and `gitleaks` as shell-out tools; parse their JSON output; map findings to ISB rules. 90%+ of rules don't need the LLM.

**LLM layer for context-sensitive cases.** Reserve Claude for rules that require understanding the wrapping code: "is this new HTTP handler authenticated," "does this new function that reads from BountyBoard get called from the PR-flow path that already validates," "could this error message leak a secret." Budget: one LLM call per commit maximum; abort early if deterministic rules already blocked.

**Rule library.** Lives in the `FleetRules` DB table (category `isb`) as versioned rows; `isb/finders/*.yaml` is the auto-rendered export maintained by the `rule-renderer` dog. Rule changes go through the promotion pipeline (evidence-gated when possible) or the operator direct-write escape hatch. Each rule's rendered YAML:

```yaml
id: ISB-042
name: outbound_url_without_validator
severity: block
category: exfil
detection:
  type: ast
  matches:
    - "http.Client.Do"
    - "http.Post"
    - "http.PostForm"
  requires_preceding_call: "store.ValidateOutboundURL"
remediation: |
  Every new outbound HTTP call must route through store.ValidateOutboundURL
  at config-write AND before every request. See CLAUDE.md "Outbound-channel
  hardening (Fix #10)".
claude_md_anchor: "Outbound-channel hardening"
```

Rules are versioned by `FleetRules.version` per `rule_key`, testable (each ships with positive/negative fixture code), and individually mute-able per repo via `SystemConfig.isb_muted_rules`. Experiments can A/B-test proposed rules before promotion (see paired-runs.md §"Rule Registry").

### Rules at launch (minimum set)

Each is a direct extraction from an audit Critical or High:

| Rule ID | Detects | Severity | Source |
|---|---|---|---|
| ISB-001 | Hardcoded secret patterns (`ghp_`, `gho_`, basic-auth URL) | block | AUDIT-055 |
| ISB-002 | `exec.Command` with positional ref not preceded by `--` | block | AUDIT-018, P10 |
| ISB-003 | Concatenated SQL (not parameterized) | block | P3 |
| ISB-004 | New outbound HTTP call without `ValidateOutboundURL` | block | AUDIT-016, P9 |
| ISB-005 | New mutating HTTP handler not wrapped by `securityMiddleware` | block | AUDIT-001, P8 |
| ISB-006 | `os.Create` / `os.MkdirAll` with mode > 0700 in sensitive paths | block | AUDIT-100 |
| ISB-007 | `os.Remove` / `git clean -fdx` without symlink + containment check | block | AUDIT-019 |
| ISB-008 | New LLM prompt that concatenates external content without `<user_content>` tags | block | P12 (Fix #8.5) |
| ISB-009 | Unbounded `bytes.Buffer` / `io.ReadAll` on external input | advise | AUDIT-057 |
| ISB-010 | New `json.Unmarshal` of LLM response without `DisallowUnknownFields` | advise | P12 (Fix #8.5) |

### Storage

New table:

```sql
CREATE TABLE SecurityFindings (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id     INTEGER NOT NULL,
    bureau      TEXT NOT NULL,            -- 'ISB' | 'BoS'
    rule_id     TEXT NOT NULL,
    severity    TEXT NOT NULL,            -- 'block' | 'advise'
    file        TEXT,
    line        INTEGER,
    excerpt     TEXT,
    disposition TEXT,                     -- 'resolved' | 'suppressed' | 'overridden'
    created_at  TEXT DEFAULT (datetime('now'))
);
CREATE INDEX idx_sec_findings_task ON SecurityFindings(task_id);
CREATE INDEX idx_sec_findings_rule ON SecurityFindings(rule_id, created_at);
```

Shared with BoS (`bureau` column). Rule precision/recall is computable from this table.

### Cost model

- Deterministic rules: ~free (shell-out to existing tools).
- LLM layer: one call per commit, ~2K tokens each, ~$0.05.
- Per-task hard cap: $0.50. Exceeding the cap hard-blocks with "diff too large for security review."
- Respects fleet-wide `SpendCapExceeded(db)` gate (Fix #1).

### Risks

- **False positive spiral.** Every noisy rule burns tokens and breaks trust. Each rule must hit 70% precision over its first 30 firings or get auto-muted until tuned.
- **Bypass temptation.** Operator frustration leads to disabling. The escape hatch is an explicit `// ISB-BYPASS: AUDIT-NNN approved` inline comment required per-file; bypasses land in `SecurityFindings` with `disposition='overridden'` for audit.
- **Captain/ISB overlap.** Captain owns "is this in-scope for the task," ISB owns "does this change create a security concern regardless of scope." A diff can fail one and pass the other.

---

## Bureau of Standards (BoS)

**Role.** Same pipeline position as ISB, different rule set. Enforces CLAUDE.md invariants mechanically so they stop drifting between "documented" and "actually followed."

**Rationale.** The audit's investigation Domain 25 ("documentation drift") found several CLAUDE.md invariants that the code silently contradicted. Worse: Pattern P1 ("no silent failures") was violated at 30+ call sites despite being CLAUDE.md's headline rule. Documentation-as-policy fails without enforcement; BoS is the enforcement.

### Trigger

Identical to ISB — post-astromech-commit, parallel to ISB, both must approve.

### Operation

**AST-first, not regex-first.** Most invariants are structural, not lexical. "Function returns void AND body contains `db.Exec`" is an AST query. "Every new `Spawn*` has an e-stop guard" is an AST pattern. Regex can't reliably express either. Use `go/parser` + `go/ast` — a thin rule-authoring framework makes AST queries ergonomic.

**Rules are extracted from CLAUDE.md, not hand-written separately.** Every invariant in CLAUDE.md corresponds to at least one BoS rule. When a new Fix lands and CLAUDE.md gets a new invariant section, the same promotion lands the matching BoS rule(s). Both live in `FleetRules` (categories `claude-md` and `bos`), so the promotion of a new invariant and its enforcement rule is a single DB transaction rendered to the two rendered files atomically.

**Rule library.** Check bodies live at `internal/bos/rules/` as Go files implementing a small interface; their metadata, severity, and activation state live in `FleetRules` (category `bos`). The Go check body is keyed by the rule's `rule_key` so a promotion/demotion toggles enforcement without a redeploy:

```go
type Rule interface {
    ID() string
    CLAUDEMDAnchor() string
    Severity() Severity
    Check(*ast.File, *types.Info) []Finding
}
```

Each rule ships with tests — red (example violating code that fails the check) and green (example compliant code that passes). Makes the rule library itself testable.

### Rules at launch (from CLAUDE.md post-Code-Red)

| Rule ID | Detects | Severity | CLAUDE.md Anchor |
|---|---|---|---|
| BOS-001 | Void-returning new store mutator | block | No silent failures → Fix #8 |
| BOS-002 | New `_ = store.Foo(...)` without `// TODO(Fix #8b):` marker | block | Fix #8a |
| BOS-003 | Multi-write function without `db.Begin()` / `tx.Commit()` | block | AUDIT-069 |
| BOS-004 | New `Spawn*` without `ctx` + `IsEstopped` + `SpendCapExceeded` sequence | block | Fix #1 |
| BOS-005 | New destructive git op without `AssertNotDefaultBranch` call | block | Fix #0 |
| BOS-006 | New DB write to a ref-bearing column without validator on the path in | block | Fix #9 |
| BOS-007 | New `payload LIKE '%"convoy_id"...'` pattern | block | P3, AUDIT-011 |
| BOS-008 | New table missing index on columns referenced in WHERE elsewhere | advise | P4 |
| BOS-009 | Raw `time.Sleep(backoff)` inside loop that checks `IsEstopped` | block | Fix #1 |
| BOS-010 | New outbound content without `RedactSecrets` wrapping | block | Fix #10 |

### Storage

Shares `SecurityFindings` table with ISB (`bureau='BoS'`).

### Cost model

- Nearly all rules are AST checks — deterministic, zero LLM cost.
- LLM fallback reserved for "does the surrounding control flow honor the invariant" cases; rare by construction.
- Per-task hard cap: $0.10 (most checks never hit the LLM).

### Risks

- **Over-strict BoS blocks all work.** Severity tiers are critical; default everything to `advise` until precision confirmed, promote to `block` only after 30 clean firings.
- **Rule authoring burden.** The whole point is keeping CLAUDE.md and code in lockstep. If adding a BoS rule for every new CLAUDE.md invariant is painful, rules lag, drift returns. Framework ergonomics matter more than initial rule count.
- **Override auditability.** Same pattern as ISB — explicit `// BOS-BYPASS: <reason>` comment required, override lands in `SecurityFindings` with `disposition='overridden'`.

### Distinction from ISB

| Dimension | ISB | BoS |
|---|---|---|
| Concerns | Security (attacker-actionable) | Quality / invariant (drift-actionable) |
| Trigger source | External threat models + audit security findings | CLAUDE.md invariants |
| Detection mix | Regex + static analysis tools + LLM | Primarily AST |
| Rule velocity | Slow (new threat classes) | Fast (new invariants each Fix) |
| Failure mode if wrong | Data breach / code exec | Silent drift, future audit findings |

Both exist. Neither subsumes the other. A commit can fail one and pass the other.

---

## Senate

**Role.** Domain-aware advisors consulted by Chancellor during plan review — BEFORE a proposed convoy becomes an active one. Each Senator has deep context on one domain (repo or team) and reviews plans touching that domain for architectural concerns, consumer impact, and repo-specific invariants.

**Rationale.** The audit's investigation Domain 21 flagged the Chancellor as fail-OPEN on Claude errors (AUDIT-030/116) — any LLM flake auto-approved the plan. The deeper issue Chancellor can't solve alone: it has no domain memory. A plan that mentions "modify internal/dashboard/handlers.go" gets the same generic review whether dashboard is a simple CRUD layer or a critical auth gate. Senators encode that domain knowledge so plan-time review is substantive.

### Trigger

`Chancellor` finishes decomposing a Feature into a TaskPlan. Senate router identifies affected Senators:

```
affected_senators(plan) = {senator ∈ Senate : plan.touches(senator.repo)}
                        ∪ {team_senator : plan.cross_repo(team_senator.team)}
```

If no Senator is affected (e.g., plan touches a fresh repo with no Senator yet), Senate is skipped. Zero cost, zero delay.

### Per-Senator context

Each Senator persists:

- **Static**: repo's README, ARCHITECTURE.md, CONTRIBUTING.md, design docs.
- **`FleetRules` rows** with `category='senate'` and `agent_scope='senate:<repo>'` — domain invariants in the same shape as CLAUDE.md but repo-scoped. `SENATE.md` at the repo root is the auto-rendered export; promotions land via the standard pipeline; experiments can A/B-test proposed senate rules before promotion.
- **Public API surface**: exported interfaces, HTTP handlers, CLI commands, their consumers.
- **Historical signal**: task-outcome patterns for the repo — which task types historically succeed/fail, common rejection reasons, areas of fragility. Fed from curated Librarian memories.
- **Dependency surface**: what depends on this repo, what this repo depends on.
- **Recent commits**: last 90 days summarized (not full diffs).

### Operation

Parallel Senator reviews, one LLM call per affected Senator:

```
Chancellor's proposed plan
      │
      ▼
Senate router → identify affected Senators
      │
      ▼
For each affected Senator, in parallel:
  Load Senator's persistent context (from Librarian-curated memory)
  Receive plan + specific tasks that touch this Senator's domain
  LLM call: review for domain concerns
      │
      ▼
Aggregate Senator verdicts → {concur, amend, dissent}
      │
      ▼
Chancellor decides:
  all concur       → auto-approve
  amendments only  → apply amendments, re-review if material
  any dissent      → escalate to operator with Senator positions attached
```

### Senator verdict shape

```json
{
  "senator": "force-orchestrator",
  "position": "concur | dissent | amend",
  "rationale": "one paragraph",
  "concerns": [
    {
      "task_id": 3,
      "concern": "this modifies the Chancellor claim query that gated the last token-burn incident — needs a UNIQUE constraint first",
      "severity": "block | warn"
    }
  ],
  "amendments": [
    {"task_id": 3, "new_task": "add UNIQUE constraint migration BEFORE the query change"}
  ]
}
```

### Bootstrap + refresh

**New repo added** (`force add-repo`): queue a `SenatorOnboarding` task. The responsible agent reads the repo's README, walks the public API surface, samples recent commits, and produces initial `FleetRules` rows for the Senator (category `senate`, agent_scope `senate:<repo>`) plus seeds `SenateMemory`. Operator ratifies the proposed rules before the Senator is activated — same ratification flow as any other rule promotion.

**Periodic refresh**: `senate-refresh` dog runs weekly (or after N commits to the repo). Librarian produces an updated memory digest; Senator's prompt context incorporates new commits, new rejections, new escalations in its domain. Senator's own rules are NOT auto-edited — Librarian emits candidate `PromotionProposals` that Engineering Corps turns into experiments; operator ratifies winning promotions.

### Storage

```sql
CREATE TABLE SenateChambers (
    senator_name     TEXT PRIMARY KEY,    -- 'force-orchestrator' | 'billing' | ...
    scope            TEXT NOT NULL,        -- 'repo:<name>' | 'team:<name>'
    senate_md_path   TEXT NOT NULL,        -- path to SENATE.md in the repo
    last_refreshed_at TEXT,
    status           TEXT DEFAULT 'active' -- 'active' | 'suspended'
);

CREATE TABLE SenateMemory (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    senator    TEXT NOT NULL,
    topic      TEXT,
    summary    TEXT NOT NULL,
    source     TEXT,                       -- 'rejection' | 'commit' | 'escalation' | 'manual'
    weight     REAL DEFAULT 1.0,           -- curated by Librarian
    created_at TEXT DEFAULT (datetime('now'))
);
CREATE INDEX idx_senate_memory_senator ON SenateMemory(senator, weight DESC);

CREATE TABLE SenateReview (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    feature_id  INTEGER NOT NULL,           -- references BountyBoard.id (Feature type)
    senator     TEXT NOT NULL,
    position    TEXT NOT NULL,              -- concur | amend | dissent
    concerns    TEXT,                       -- JSON
    amendments  TEXT,                       -- JSON
    created_at  TEXT DEFAULT (datetime('now'))
);
CREATE INDEX idx_senate_review_feature ON SenateReview(feature_id);
```

### Cost model

- Per-Senator review: ~$0.10–$0.30 (plan + Senator context + task details).
- Per-Feature consultation hard cap: $2.00 (10× a baseline Chancellor pass).
- If more than 5 Senators are affected, escalate to operator with "too many domain touches for autonomous review" rather than fanning out further.
- `SenatorOnboarding` is the expensive operation: reading a 100k-line repo, producing initial SENATE.md. One-time $5–$20 per repo; amortizes over many future plan reviews.

### Teeth

- **Dissent + concern.severity=block** → Chancellor cannot approve; escalates to operator with Senator positions.
- **Dissent + concern.severity=warn** → Chancellor can approve but records the override in convoy metadata (`convoy_overrides` table). Operator can see post-facto which convoys overrode which Senators.
- **Concur with amendments** → Chancellor applies amendments. Material changes trigger re-review.
- **All concur** → auto-approve, skip operator review. This is the fast-throughput path.

### Risks

- **Senators hallucinate concerns.** Concern-precision metrics per Senator; Senators whose concerns are historically wrong get muted. Track `concern_precision = (real concerns) / (total concerns)` computed by post-merge retrospectives (did the raised concern materialize?).
- **Senator memory drifts.** Weekly refresh is the minimum. A Senator with stale memory gives worse advice than no Senator at all — that's worse than the Chancellor alone.
- **Circular blocking.** Senator X says "don't touch repo A" when the Feature requires touching repo A. Escape hatch: operator override with forced convoy advancement. Override lands in `convoy_overrides`.
- **Bootstrap cost.** First read of a large repo is expensive. Budget for it; don't silently run onboarding on every `force add-repo` without operator confirmation.
- **Senate as a bottleneck.** Serial Senator reviews would stall every plan. Parallel reviews with per-Senator time budgets (30s max per Senator) keep wall-clock bounded.

---

## Engineering Corps

**Role.** The experimentation orchestrator. Authors experiments from Librarian hypotheses, monitors running experiments, declares winners and nulls, assembles promotion and demotion proposals, and manages the long-term global holdout. Never adjudicates specific code changes; its subject is the configurations of the other agents, not their outputs.

**Rationale.** Without Engineering Corps, the promotion pipeline has no mechanism to turn Librarian's "this pattern keeps recurring" signal into "this rule is measurably better than its absence." Every promotion to CLAUDE.md / SENATE.md / ISB / BoS would land on operator intuition alone. Engineering Corps closes the loop: Librarian proposes, Engineering Corps tests, operator ratifies on evidence.

### Scheduling

Its own multi-task claim-loop agent (Diplomat-style pattern — separate goroutine, separate queue, claims multiple task types). Claimable task types:

| Type | Purpose |
|---|---|
| `ExperimentAuthor` | Turn a Librarian candidate `PromotionProposal` into a complete experiment YAML |
| `ExperimentMonitor` | Watch posterior, budget, duration; declare winner/null/inconclusive; emergency-stop on degradation |
| `PromotionAuthor` | On winner + confirm, assemble `PromotionProposal` with evidence trail |
| `DemotionAuthor` | On retention-report signal, assemble demotion proposal |
| `MetricAuthor` | Generate metric SQL + test + manifest when a hypothesis needs an unregistered metric |
| `HoldoutMonitor` | Manage holdout refresh lifecycle; detect model-deprecation threats |

### Operator boundary

Engineering Corps can **propose but never promote**. Every terminal output (promotion proposal, demotion proposal, new metric, experiment YAML) requires operator ratification before it becomes fleet state. The dashboard one-click flow is designed so operator ratification is the highest-leverage action the operator takes — experiments, metrics, and rules all land through the same button with clear evidence summaries.

Engineering Corps CAN unilaterally: pull the plug on a running experiment that's showing active harm (emergency stop), archive expired unratified proposals, mint successor global holdouts on the annual refresh cadence. Those are safety actions, not promotion actions.

### Storage

Fully specified in [paired-runs.md](subsystems/paired-runs.md). Tables introduced: `Experiments`, `ExperimentTreatments`, `ExperimentMetrics`, `ExperimentRuns`, `ExperimentOutcomes`, `TreatmentSpecs`, `MetricVersions`, `AnalysisFrameworks`, `FleetStateSnapshots`, `FleetRules`, `PromotionProposals`, `GlobalHoldouts`, `ModelAvailability`.

### Cost model

- Authoring (ExperimentAuthor, MetricAuthor, PromotionAuthor): ~$0.20 per call (YAML generation, evidence summarization). One call per proposal.
- Monitoring (ExperimentMonitor, HoldoutMonitor): nearly free — SQL queries + posterior recomputation.
- Demotion authoring: similar to promotion.
- Daily throttle: `engineering_corps_daily_proposal_cap` (default 3). Queues overflow to next day.

The experiments themselves are where spend lives; Engineering Corps overhead is a rounding error by comparison.

### Risks

- **Runaway proposal volume.** Daily cap + critic-note retry loop on YAML authoring.
- **LLM-authored YAML that semantically measures the wrong thing.** Operator pre-approval gate before any experiment enters `running`; metric ratification separate from experiment ratification so scoring correctness is reviewed before it can taint results.
- **Declared-winner thrash across fleet state changes.** DB history (`ExperimentOutcomes`) with `fleet_state_hash` comparison drives re-experiment decisions — no fixed cooldown; prior-null hypotheses are re-testable when fleet state has meaningfully changed.
- **Emergency-stop false positives.** Minimum-runs gate (default 20) + threshold on "worse by practical effect" not "worse by anything" prevents small-sample thrash.

---

## Librarian evolution (prerequisite for Senate and Engineering Corps)

Not a new agent — the existing `internal/agents/librarian.go` needs to grow from "WriteMemory handler + RAG-tag producer" to "curator + synthesizer + hypothesis emitter." The evolved scope:

- **Dedup + merge** near-identical memories into canonical entries.
- **Quality-score** every memory: freshness decay, relevance scoring per retrieval, validation scoring based on whether memories that were injected led to successful outcomes.
- **Conflict detection** — two memories saying opposite things produces a ticket, not silent both-inject.
- **Hypothesis emission** — memories consulted N times with high validation become candidate `PromotionProposals` (kind `candidate`, origin `librarian`). Engineering Corps picks these up via its `ExperimentAuthor` task, turns them into experiments, and the evidence produced by running experiments is what drives operator ratification. Librarian no longer "promotes" directly — it emits candidates for experimental validation.
- **CLAUDE.md drift detection** — weekly pass diffs the rendered CLAUDE.md against observable code, emits candidate proposals when invariants appear violated in committed code.
- **Senator bootstrap + refresh** — produces initial Senator rule candidates (promoted through the standard pipeline), weekly refreshes Senator memories.
- **Injection pipeline** — every agent prompt that wants context queries Librarian (weighted, deduplicated, scope-filtered) rather than raw FleetMemory.

**Why this is a Senate + Engineering Corps prerequisite.** Senators can't review against stale or noisy repo knowledge. The value of a Senator is bounded by the quality of their memory. Engineering Corps can't run experiments it has no hypotheses for; its input queue is fed entirely by Librarian's curated hypothesis stream. Raw FleetMemory grows faster than it decays, accumulates contradictions, and has no scope mechanism — unusable as either substrate.

Detailed design lives outside this doc (Librarian is existing, this is a scope-expansion discussion). Key insight: the experiment-gated promotion pipeline is the mechanism by which the fleet graduates from "remembers things" to "learns things." Librarian remembers; Engineering Corps tests; operator ratifies.

---

## Composition — the feedback loop

```
  ┌─ Senate concern      ─┐
  ├─ ISB finding          │
  ├─ BoS violation        │
  ├─ Captain rejection    ├─▶ Librarian (curator)
  ├─ Council rejection    │        │   dedup / score / synthesize
  ├─ ConvoyReview finding │        ▼
  └─ Task outcome        ─┘   candidate PromotionProposal (origin=librarian)
                                    │
                                    ▼
                        Engineering Corps / ExperimentAuthor
                                    │   writes YAML → operator pre-approves
                                    ▼
                             Experiment runs
                        (via treatments.Apply ingress)
                                    │   Bayesian posterior + confirm phase
                                    ▼
                        Engineering Corps / PromotionAuthor
                                    │   assembles evidence trail
                                    ▼
                          ratified PromotionProposal
                                    │   operator ratifies
                                    ▼
              FleetRules insert + rule-renderer auto-commit
                                    │
       ┌────────────┬───────────────┼───────────────┬─────────────┐
       ▼            ▼               ▼               ▼             ▼
  SenateMemory   SENATE.md      ISB rules       BoS rules     CLAUDE.md
  (per repo)     (rendered)     (rendered)      (rendered)    (rendered)
```

Every agent emits signals. Librarian curates them into memory and emits hypotheses. Engineering Corps tests those hypotheses against the live fleet. Evidence-backed winners become ratified proposals; operator ratification writes `FleetRules` rows, which render back to the markdown files the fleet and the IDE see.

Long-term global holdout (managed by Engineering Corps / HoldoutMonitor) runs 2% of traffic against a frozen reference state, providing the only honest signal on whether the cumulative effect of all these promotions is actually improving the fleet.

Without the loop: agents forget, rules don't emerge, drift wins. Without experimental gating: rules promote on intuition, fleet state overfits to whichever rules the operator most recently found annoying, and drift returns in a different shape.

---

## Implementation order

1. **FleetRules + rule-renderer** — shared substrate for BoS, ISB, Senate, and CLAUDE.md. Bootstrap migration parses the current CLAUDE.md into `FleetRules` rows; pre-commit hook rejects hand-edits to rendered files. Everything downstream assumes DB-as-SOT. Full design in [paired-runs.md](subsystems/paired-runs.md).

2. **BoS** — smallest new-surface-area for enforcement code; rule bodies already exist in CLAUDE.md form. Extract invariants into `FleetRules` rows + AST check bodies; render BoS's YAML files from the DB. Gives the fleet enforcement of what it already says it does.

3. **ISB** — reuse `gosec`/`semgrep`/`gitleaks` as deterministic base; add thin LLM layer for context-sensitive rules. Most value-per-effort of the three review-layer agents.

4. **Paired-runs foundations** — `treatments.Apply` ingress, Experiments / TreatmentSpecs / MetricVersions / AnalysisFrameworks / FleetStateSnapshots / GlobalHoldouts tables, holdout minted on day 1 so longitudinal fleet-progress signal begins accumulating immediately. Log-only mode for `treatments.Apply` while data model stabilizes. Full phased plan in paired-runs.md §"Rollout Plan."

5. **Librarian evolution** — before Senate and Engineering Corps. Quality of both is bounded by Librarian's curation quality. Build curation + hypothesis emission first.

6. **Engineering Corps** — activates the experiment pipeline. With Librarian producing candidate hypotheses and foundations in place, EC can start authoring experiments, ratifying metrics, and proposing promotions.

7. **Senate** — largest architectural change, most prerequisites, highest ROI once working. Sequence last so the rules it enforces (BoS/ISB) and the memory it consults (Librarian) are in place, and so its own rules can ride the experimental-promotion pipeline (Engineering Corps) from day 1 rather than being bolted on after.

---

## Open questions

Things not settled in this sketch; worth resolving before implementation:

- **Cost governance across review layers.** ISB + BoS + Senate + Captain + Council + ConvoyReview is six LLM-touching layers per convoy. A per-Feature review budget (e.g., 15% of Commander's estimated cost) that all layers share would bound total spend regardless of how any one layer behaves. The experiment-budget framework (paired-runs.md §"Budget enforcement") solves this for *experiments* on these layers; the per-Feature production budget is still open and orthogonal.

- **Override telemetry.** ISB bypass comments, BoS bypass comments, Senator override flags — each needs a durable audit trail. A unified `review_overrides` table shared by all three agents is probably the right shape; bikeshed TBD.

- **Rule-authoring workflow.** For BoS specifically: when a new Fix lands CLAUDE.md invariants, adding the BoS rule should be a small step, not a separate project. A `rule-starter-kit` scaffolding CLI that generates the AST-check template + test fixtures + the corresponding `FleetRules` promotion YAML would make this routine.

- **Senator scope granularity.** Per-repo vs per-team vs per-subdirectory. First cut: per-repo. Per-team Senators are additive and optional; only worth building once per-repo Senators prove their value.

- **How Senate interacts with ProposedConvoys.** Today Chancellor's approval flow writes to ProposedConvoys then transitions to AwaitingChancellorReview. Senate review probably sits between ProposedConvoys write and Chancellor's decision — but the exact state-machine needs thinking.

*Resolved elsewhere:*

- ~~**Who authors the first SENATE.md for existing repos.**~~ Engineering Corps authors initial Senator rules as candidate `PromotionProposals` during the `SenatorOnboarding` task; operator ratifies through the standard flow. Rendered `SENATE.md` follows automatically from `FleetRules`. See [paired-runs.md](subsystems/paired-runs.md) §"Engineering Corps" and §"Rule Registry."
