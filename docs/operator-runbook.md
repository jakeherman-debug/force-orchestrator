---
title: Operator runbook
type: operator-doc
last-reviewed: 2026-05-05
audience: operator
scope: Things-go-wrong playbook — daemon crash, stuck convoy, runaway spend, dog failures, schema drift, manual recovery commands.
owner: D13
last_reviewed: 2026-05-05
---

# Operator runbook

When the fleet misbehaves: how to diagnose, how to recover, when to escalate to a real fix. This is the reach-for-when-stuck doc; for first-day setup go to [`onboarding.md`](onboarding.md), and for the architectural picture see [`overview.md`](overview.md).

The general posture: an escalation that surfaces in the dashboard is the fleet asking you to make a judgement call, not a system failure. A pattern of similar escalations IS a system failure — log it as a candidate for a self-healing fix, not a workaround.

## Daily ops

### Inspecting the dashboard

The dashboard is the primary surface — open `./force dashboard` (binds 127.0.0.1:8080) and keep it visible while the fleet runs. The header is permanently visible:

- **Daemon badge** — green `● Daemon PID <N>` when running, red `● Daemon offline` otherwise.
- **E-STOP badge** — appears in red when an emergency stop is active.
- **Total Spend** — cumulative token cost; eyeball this when you spot unusual activity.

The Stats Bar refreshes every 5 s (Running / Pending / Review / Done / Failed / Escalations / Convoys / Unread Mail / Total Spend).

The Tasks tab is the main workspace. Filter to **Failed** when investigating regressions, **Active** when watching a convoy in flight. Click any row for the full task context: meta, broader goal, error log, attempt history, fleet memories used, task mail.

The Escalations tab is what you check first thing each morning. Each card shows severity (LOW / MEDIUM / HIGH), the originating task, and the agent's message. Three actions per card: **Acknowledge**, **Close**, **Close & Requeue**.

CLI equivalents:

```bash
./force list Pending,Active,Failed --limit 50
./force escalations list Open
./force convoy list
./force mail inbox operator
./force stats
./force costs
./force who
```

### Reviewing PRs

Force's default delivery path is a draft GitHub PR. After all sub-PRs merge to the convoy's ask-branch and `ConvoyReview` returns clean, Diplomat opens a draft PR into `main` and emits `[CONVOY REVIEW PASSED]` operator mail.

Inspect:

```bash
./force convoy pr <convoy-id>
```

Shows per-repo ask-branches, draft PR URLs + state, and the sub-PR rollup (open / merged / closed counts, CI state).

Ship:

- **Dashboard:** `Ship It` button on the Convoy card.
- **CLI:** `./force convoy ship <convoy-id> [--merge squash|merge|rebase]`.

The default behavior is to flip the draft PR to ready-for-review and let you merge on GitHub. `--merge` immediately merges using the strategy you pick. There is **no auto-ship** — the human gate is intentional.

If a draft PR sits unshipped, the `ship-it-nag` dog reminds you at 24h / 72h / 1 week.

### Handling escalations

Each open escalation is the fleet asking for a judgement call. Three resolutions:

| Resolution | What to use it for |
|---|---|
| **Acknowledge** | "I've seen it." Doesn't restart the task. Use when you need to think before deciding. |
| **Close** | The task is no longer relevant (the underlying need went away, the work is dropped). |
| **Close & Requeue** | The task is recoverable; reset to Pending and let the fleet retry. |

A pattern to watch for: when 3+ Open HIGH-severity escalations stack up, the dashboard surfaces a persistent banner (AUDIT-064 threshold). That's the fleet telling you a failure mode is recurring — investigate root cause before clearing them.

The Inquisitor auto-bumps unacknowledged escalations every 4 hours via operator mail. If you see the same escalation re-bumped repeatedly, treat that as a self-healing candidate, not a normal-operation event.

## Common interventions

### Re-running a stuck task

A task may stall because the agent hung, the LLM returned junk, or a dependency wasn't ready.

```bash
./force reset <task-id>          # back to Pending; clears error log + retry count + branch
./force retry <task-id>          # alias for reset
./force retry-all-failed         # bulk: every Failed task to Pending
```

Dashboard: Tasks tab → click the task → **Reset to Pending** (or **Retry** if Failed/Escalated).

If a task keeps coming back rejected with the same feedback, send a directive before resetting it:

```bash
./force mail send astromech --task <id> --type directive \
  "Always use the internal logging package — never fmt.Println."
./force reset <id>
```

### Force-completing a convoy

There is no "force complete" — convoys auto-close when every constituent task reaches `Completed` (or terminal failure). The mechanism is the Inquisitor's `staleConvoys` check.

If you want to abandon a convoy:

```bash
./force convoy reject <convoy-id> "Replan needed — direction changed"
```

This cancels un-started tasks, mails Commander with the feedback, and requeues the parent Feature for replan. In-flight tasks finish (or fail) on their own — there is no in-flight cancel.

To cancel one task in a convoy:

```bash
./force cancel <task-id>
```

Or use the dashboard's per-task **Cancel** button (which lets you optionally re-queue as a different task type).

### Retrying a failed Captain or Council review

If Captain or Council rejected and the task is now in `Failed` after exhausting retries, the Medic should already have triaged it. If Medic decided `escalate`, you'll see the escalation. To override:

```bash
./force retry <task-id>          # back to Pending; new attempt
./force approve <task-id>        # skip review, merge the existing branch
./force reject <task-id> "<reason>"  # re-reject with new feedback
```

`force approve` bypasses Captain AND Council — use with care; it is the operator's manual override.

### Killing a stuck astromech

Astromechs are goroutines, not separate processes — you can't `kill <pid>` one. The mechanisms:

- **Stale-lock reset.** The Inquisitor auto-resets any task `Locked` longer than `staleLockTimeout` (45 min) — see [`internal/agents/inquisitor.go`](../internal/agents/inquisitor.go).
- **E-stop.** `./force estop` (or the dashboard's E-Stop button) halts every claim loop AND cancels in-flight Claude subprocesses (long sessions poll `IsEstopped` via heartbeat goroutines, target ≤ 3 s wall-clock response per Pattern P11). Resume with `./force resume`.
- **Daemon restart.** If a goroutine is genuinely wedged (rare): `kill $(cat fleet.pid)`, wait for the 30 s drain, restart with `./force daemon`. Reconcile-on-startup will repair any state divergence.

If a specific Bash command inside an astromech is the culprit, the bash-guard log records every command:

```bash
tail -F .d7-worktrees/<agent>/<repo>/bash.log
```

### Quarantining a runaway repo

If a repo is producing too many failures and you want the fleet to stop touching it:

- **Dashboard:** Repos section → **Quarantine** button.
- **HTTP API:** `POST /api/repos/<name>/quarantine`.

The `quarantined-repo-watch` dog will keep alerting the operator until quarantine is lifted. Restore via the dashboard's **Restore** button (or `POST /api/repos/<name>/restore` — sets `mode='read_only'`; promote to `write` separately when ready).

## Debugging

### Where the logs live

All daemon-wide files live under `~/.force/` (overridable via `FORCE_DIR`). See [`docs/subsystems/state-files.md`](subsystems/state-files.md) for the resolver contract.

| File | Path | Format | Content |
|---|---|---|---|
| `fleet.log` | `~/.force/fleet.log` | Plain text, timestamped | Human-readable agent log lines (one process-wide writer) |
| `holonet.jsonl` | `~/.force/holonet.jsonl` | NDJSON | Structured telemetry events; rotates at 50 MB via `holonet-rotate` dog |
| `bash.log` (per worktree) | inside each worktree | TSV | Every bash-guard invocation (allow + deny); rotates at `bash_guard_log_max_bytes` (default 10 MiB) |
| `force.pid` | `~/.force/force.pid` | One line | PID of the running daemon (D12 singleton) |
| `holocron.db` (+`-wal`/`-shm`) | `~/.force/holocron.db` | SQLite | The Gas Town source of truth |

Tail with:

```bash
./force logs-fleet                            # follow fleet.log
./force logs-fleet --filter "[RECONCILE]"     # grep for a subject
./force logs-fleet --agent <name>             # filter by agent
./force logs-fleet --task <id>                # filter by task
./force holonet                               # follow holonet.jsonl
./force holonet --filter <event_type>
./force tail                                  # combined live tail of both
```

The dashboard's Logs tab does the same via Server-Sent Events with a 1000-line cap, both auto-reconnect on disconnect.

### Reading the holocron

`holocron.db` is just SQLite. Keep a `sqlite3 ~/.force/holocron.db` shell open while debugging (or `sqlite3 "$FORCE_DIR/holocron.db"` if you override the state dir):

```sql
-- Anything in flight?
SELECT id, status, type, owner, repo, locked_at, retry_count
  FROM BountyBoard
  WHERE status IN ('Locked','UnderReview','UnderCaptainReview','AwaitingCaptainReview','AwaitingCouncilReview')
  ORDER BY locked_at;

-- Open escalations
SELECT id, task_id, severity, message, status, created_at FROM Escalations WHERE status='Open';

-- Per-task spend
SELECT task_id, ts, window_seconds, spend_usd FROM TaskSpendWatch ORDER BY ts DESC LIMIT 20;

-- What's the spend cap actually set to?
SELECT key, value FROM SystemConfig WHERE key LIKE '%spend%';

-- Convoy progress
SELECT c.id, c.name, c.status,
       SUM(CASE WHEN b.status='Completed' THEN 1 ELSE 0 END) AS done,
       COUNT(b.id) AS total
  FROM Convoys c LEFT JOIN BountyBoard b ON b.convoy_id = c.id
  WHERE c.status='Active' GROUP BY c.id;

-- AuditLog of last 50 operator/agent actions
SELECT ts, actor, action, target FROM AuditLog ORDER BY ts DESC LIMIT 50;

-- What dogs ran when?
SELECT name, last_run_at, next_run_at, last_outcome FROM Dogs ORDER BY last_run_at DESC;
```

The full schema lives in [`schema/schema.sql`](../schema/schema.sql) (kept in parity with `createSchema` + `runMigrations` by `TestSchemaParity`).

### Seeing what an agent thinks the world looks like

Per-task drill-down:

```bash
./force task <id>                 # event timeline + LLM transcripts + git ops + cost rollup
./force history <id>              # every Claude attempt — agent name, outcome, tokens, timestamp
./force history --full <id>       # full Claude output, not truncated
./force diff <id>                 # git diff for the task's branch
./force bounty <id>               # raw BountyBoard row (stable JSON for scripting)
```

Per-task fleet memories used (the BM25-ranked `FleetMemory` rows that landed in the prompt):

```bash
./force memories <repo>
./force memories search <repo> <query>
```

Or in the dashboard: Tasks → click the row → the slide-in panel includes a **Fleet Memories** section listing the top-10 retrieved for this task.

LLM-call transcripts: every `claude -p` invocation routes through `claude.CallWithTranscript*` (Pattern P31), writing to `LLMCallTranscripts`. `force task <id>` surfaces them.

## Recovering from incidents

### Daemon crash mid-run

Restart with `./force daemon`. On boot the daemon runs a two-step crash-recovery sequence **before any agent spawns**:

1. `store.ReleaseInFlightTasks` resets `Locked` / `UnderReview` / `UnderCaptainReview` rows to `Pending`.
2. `agents.ReconcileOnStartup(ctx, db)` walks every non-terminal `BountyBoard` row and reconciles against actual disk + git state.

The five reconcile divergence cases (and their recovery actions):

| Case | Recovery |
|---|---|
| Clean | proceed |
| Branch missing pre-Captain | auto-recover as a re-pend |
| Branch missing post-Captain | escalate (downstream may have already merged) |
| Worktree missing-or-dirty | queue an idempotent `WorktreeReset` infra task |
| Branch SHA-diverged | escalate |

A non-nil reconcile return is **fatal** — the daemon refuses to start with an unreliable view. You will see `[RECONCILE FATAL]` in `fleet.log` if this happens; investigate the row(s) in question (the log line names task IDs).

Sleep / wake survival is D12; today, an OS sleep that crosses the 45-min stale-lock timeout will trigger Inquisitor resets after the daemon resumes.

### Daemon won't start

Common causes, in roughly decreasing order of frequency:

- **`gh auth status` failed.** The PR-based delivery flow needs it. Run `gh auth login`, then retry.
- **Reconcile-on-startup escalated a row.** Look for `[RECONCILE]` lines in `fleet.log`. Resolve the underlying state issue (manual git surgery on the branch / worktree the row points to), then retry.
- **`holocron.db` ACL or permission issue.** If you ran `make protect-db` and now need to remove the file (rare), run `make unprotect-db` first.
- **Stale `fleet.pid`.** If the previous daemon was killed `-9`, the PID file may point at a dead process. The daemon detects this and prints a stale-PID notice; if it doesn't, `rm fleet.pid` and retry.

### Partial worktree state

If an astromech worktree ends up dirty and the reconciler queues a `WorktreeReset` you can also act manually. From the agent worktree directory:

```bash
git worktree list                # see what's registered
cd /path/to/<agent>/worktree
git status                       # diagnose
git reset --hard <fork-point>    # nuke local changes
```

Or use the bulk cleanup:

```bash
./force cleanup
```

This removes stale worktrees from disk AND from the `Agents` table.

### Schema drift

`TestSchemaParity` is the gate: it asserts `createSchema` (used for fresh DBs), `runMigrations` (used for existing DBs), and `schema/schema.sql` (the documented schema) all agree on column names. Drift means CI is red.

When adding a column you must update **all three** in the same commit. The `IFNULL(col, '')` pattern in SELECTs handles rows written before the column existed.

If a deployed `holocron.db` somehow drifts off `schema.sql` at runtime (e.g. a half-applied migration after a power loss), the recovery is:

1. `./force estop` — pause the fleet.
2. Inspect `PRAGMA table_info(<table>)` for the affected table.
3. Either backfill the missing column with the documented default (preserve data) or restore from `~/.force/backups/` (lose any state since the snapshot).
4. `./force resume`.

`runMigrations` has no transaction boundary today (called out in the audit-fix campaign, [`docs/operator-archives/FINAL-STATUS.md`](operator-archives/FINAL-STATUS.md) § "Additional dangerous patterns"). A power loss mid-migration can leave the DB half-applied — restore from snapshot is the safe path.

### Audit allowlist drift

`make test-audit` enforces the AUDIT-skip ratchet — every `t.Skip("AUDIT-NNN: ...")` marker must be on the allowlist in `internal/audittools/audittools_test.go`. Adding a new skip without allowlisting it fails CI; removing one from the allowlist requires the matching `t.Skip` also be removed (or the test explicitly re-added with a successor fix name).

If `make test-audit` fails after a merge:

1. `git diff` the test files for new `t.Skip("AUDIT-...` markers.
2. Either fix the underlying issue (preferred) or add the marker to the allowlist with a justifying fix name.

Zero is still the goal — every skip is a known-broken regression awaiting a fix.

### Holocron corrupted

```bash
./force estop              # pause fleet
ls -lt ~/.force/backups/   # find a recent snapshot
make unprotect-db          # remove the macOS ACL guard (resolves $FORCE_DIR or ~/.force)
cp ~/.force/backups/holocron.db.<timestamp> ~/.force/holocron.db
make protect-db            # reapply the ACL
./force resume
```

You will lose any state changes since the snapshot — escalations created, in-flight work, fleet memory written. The mechanism is conscious data-loss to recover; it is the right tradeoff when the alternative is a corrupt DB.

If snapshots aren't installed, run `make install-snapshots` immediately so future incidents have a recovery path. Hourly `sqlite3 .backup` snapshots into `~/.force/backups/`, with a daily 04:00 cleanup that prunes snapshots older than 30 days.

`make db-status` is the read-only diagnostic — shows the current ACL state, snapshot crontab entries, and the most recent snapshots.

## Operational levers

### SystemConfig knobs worth knowing

Set with `./force config set <key> <value>`; read with `./force config get <key>`; list everything with `./force config list`.

| Key | Default | What it controls |
|---|---|---|
| `num_astromechs` | `2` | Worker-agent count. Hot-add via `force scale -astromechs <N>` (SIGUSR1); scale-down only on restart. |
| `num_council` | `1` | Reviewer count. Restart to take effect. |
| `num_commanders` | `3` | Planner count. Restart to take effect. |
| `num_captain` | `1` | Captain count. Restart to take effect. |
| `num_librarians` / `num_medics` / `num_investigators` / `num_auditors` / `num_pilots` / `num_diplomats` / `num_bos` / `num_isb` / `num_senate` | `1` | Per-agent counts. Restart to take effect. |
| `max_concurrent` | `0` (unlimited) | Fleet-wide simultaneous task ceiling. |
| `spawn_delay_ms` | `0` | Smooth-out delay between agent claims. |
| `batch_size` | `0` (unlimited) | Fleet-wide max claims per 60 s window. |
| `max_turns` | `40` | Per-task Claude CLI turn cap. |
| `hourly_spend_cap_usd` | `25` | **Soft** trailing-hour spend cap. Claim loops sleep until trailing-hour drops below. |
| `hourly_spend_estop_usd` | `200` | **Hard** trailing-hour ceiling. `spend-burn-watch` dog auto-flips e-stop when crossed. |
| `per_task_spend_alert_usd` | `5` | Per-task trailing-10-min spend alert threshold; mails operator. |
| `per_task_spend_escalate_usd` | `15` | Per-task escalate threshold; sets `BountyBoard.spend_suspended=1` so claim queries skip the row. |
| `agent_max_prompt_bytes_default` | `200000` | Per-agent prompt-byte cap. Override per-agent via `agent_max_prompt_bytes_<agent>`. Overflow invokes `librarian.SummarizeForContextOverflow`; if summary still exceeds, returns `ErrContextOverflow` to `handleInfraFailure`. |
| `bash_guard_curl_hosts` | _empty_ | Comma-separated allowlist of hosts the astromech bash-guard permits for `curl` / `wget`. Default empty — operator must populate before astromechs can fetch over the network. |
| `supply_stale_threshold_days` | `730` | SUPPLY-003 staleness threshold. |
| `treatments_apply_mode` | `live` | Single-write rollback for D3 paired-runs experiments. Set to `log-only` to immediately stop enrollment + descriptor rewrite; `TreatmentApplyLog` row still lands either way. |

`force config list` will also surface auto-managed keys like `rl_hits_<agent>` (rate-limit backoff state) and `supply_allowlist_<eco>_last_refresh` (per-ecosystem CodeArtifact refresh timestamps).

### FleetRules

`FleetRules` is the rule database that `make render-rules` (or `./force render-rules`) compiles into `CLAUDE.md`, `FIX-LOG.md`, and selected `docs/*.md` files. Every architectural invariant lives here as a row, with a `render_to` field controlling which artifact it lands in.

To add a universal-load rule: insert a `FleetRules` row with `render_to='claude-md-file'` AND a justification comment in [`internal/store/fleet_rules_audit.go`](../internal/store/fleet_rules_audit.go), then run `make render-rules`. Drift is detected by `make render-rules-check` (runs in pre-commit).

The `category='senate'` slice carries Senate-promoted rules. The Senate package contains **no direct `INSERT INTO FleetRules`** — Senator rules promote ONLY via the operator-ratified pipeline (Librarian emits a candidate, Engineering Corps runs a paired-run experiment, the operator ratifies a `PromotionProposals` row, and the materialization step inserts the row). This is enforced by Pattern P34.

### Capability profile changes

Per-agent YAML profiles live under [`agents/capabilities/`](../agents/capabilities/). To grant a tool to an agent: edit the profile, ensure the tool is on `REGISTRY.yaml`, and verify it is **not** on `.forceblocklist.yaml` (Slack-write namespace, Confluence-write tools, destructive Jira ops, destructive Sonar ops are blocked). The loader fails closed: a missing YAML, an unknown tool reference, or a profile granting a blocklisted tool all return errors and the agent does not start.

Removing an entry from the blocklist requires explicit operator action with an audit trail — Pattern P13 walks every Claude call site and rejects any hardcoded tool literal.

## When to escalate to yourself

A few heuristics for distinguishing "the fleet is asking for help" from "this needs a real fix":

- **Same escalation, third occurrence in 24h** → not a one-off; treat as a recurring failure mode. Investigate the agent prompt, the directives, the relevant FleetMemory entry, or whether a new pattern test belongs in `internal/audittools/`.
- **Spend cap tripped on a normal-shape feature** → the prompt is bloating. Check `PromptByteAttribution` for the offending source (`librarian_memory`, `task_payload`, `file_read`, `claude_md`, `fleet_rules`, …). Often the fix is pruning low-value memories or tightening a directive.
- **A manual operator action that you do twice in a week** → that's a CLI gap (Pattern P25 enforces CLI parity for every mutating dashboard handler — but new dashboard mutations sometimes ship before their CLI sibling). File a candidate.
- **A diff Council approves that BoS or ISB later flags** → the gates have a coverage gap. Look at which rule should have caught it; promote it from `advise` to `block` once it has the 30-clean-firings warm-up.
- **Two convoys conflict at merge time** → Chancellor missed the dependency. Look at the `ProposedConvoys` plans both submitted; if they touch the same files the SEQUENCE / MERGE rulings should have caught it.

Workarounds you take should always be paired with a self-healing candidate. The system gets better when escalations turn into pattern tests, FleetRules entries, or BoS/ISB rules — not when you keep dismissing them by hand.

## Manual recovery commands

The destructive set, in order from "common" to "absolutely-stop-and-think":

```bash
# Reset / requeue
./force reset <task-id>                    # one task back to Pending
./force retry-all-failed                   # bulk reset everything Failed
./force convoy reset <convoy-id>           # all failed/escalated tasks in a convoy back to Pending
./force convoy reject <id> "<feedback>"    # cancel un-started; replan parent Feature

# Fleet control
./force estop                              # pause everything
./force resume                              # lift the e-stop
./force scale -astromechs <N>              # hot-add astromechs (SIGUSR1); other flags: -council, -captain, -commanders, -investigators, -auditors, -librarians
./force cleanup                            # remove stale worktrees from disk + Agents table

# Worktree surgery (manual, when cleanup isn't enough)
git worktree list
git worktree remove <path>
git worktree prune

# Repo mode (via dashboard buttons or HTTP API; no CLI today)
curl -X POST -H 'Origin: http://localhost:8080' \
  http://localhost:8080/api/repos/<name>/quarantine
curl -X POST -H 'Origin: http://localhost:8080' \
  http://localhost:8080/api/repos/<name>/restore
curl -X POST -H 'Origin: http://localhost:8080' \
  http://localhost:8080/api/repos/<name>/promote-to-write

# Dangerous — these wipe state
./force purge --confirm                    # delete fleet.log, holonet.jsonl, all worktrees + agent branches; DB untouched
./force hard-reset --confirm               # wipe ALL state: tasks, history, memories, mail, escalations, audit log, worktrees, branches
./force hard-reset --purge-repos --confirm # also drops registered repos
./force migrate pr-flow --rollback --confirm  # restore holocron.db from the most recent pre-PR-flow snapshot (loses subsequent state)
```

Without `--confirm`, the dangerous ones print the full destruction list and require interactive `DELETE` confirmation. With `--confirm`, the warning still prints but the prompt is skipped.

A few D12 commands are **planned, not yet shipped**: `force daemon update`, `force daemon update --rollback`, `force daemon foreground`. The current daemon is `./force daemon` only.

## Recovery surfaces summary

When in doubt, these are the four places to look:

| Surface | What it tells you |
|---|---|
| **Dashboard** (`http://localhost:8080`) | Live fleet state; primary surface |
| **`fleet.log`** | Human-readable timestamped agent log |
| **`holonet.jsonl`** | Structured telemetry events; replay-friendly |
| **`AuditLog` table** | Every operator + dashboard-initiated state transition |

Operator-mail subjects are **stable** so you can filter on them: `[RECONCILE]`, `[RECONCILE FATAL]`, `[TASK SPEND ANOMALY]`, `[TASK SPEND ESCALATE]`, `[INBOUND REDACT]`, `[CIRCULAR COMMITS]`, `[CHANCELLOR FAIL-CLOSED]`, `[CONTEXT OVERFLOW]`, `[CONVOY REVIEW PASSED]`, `[APPROVED]`. Pipe `./force mail inbox operator | grep "<prefix>"` for any of them.

Operator mail is rate-limited per source/channel via `respectNotificationBudget` (Pattern P27) so a runaway alert source can't flood the inbox.
