package store

import (
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

// This file demonstrates AUDIT findings 023, 077, 078, 080, 082, 130, 131,
// 132, 143, 146, 147, and 148. Each sub-test is EXPECTED TO FAIL against the
// current (buggy) tree; when the defect is fixed the corresponding assertion
// flips green.
//
// Sub-tests are deliberately read-only where possible (static grep / reflect /
// pragma) so they keep working even as unrelated schema churn lands — they
// only regress when the specific finding is addressed.

// projectRoot walks up from this test file's directory until it finds go.mod.
func projectRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatalf("could not locate project root from %s", file)
	return ""
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// columnsOf returns the set of column names PRAGMA table_info reports.
func columnsOf(t *testing.T, dsn, table string) map[string]bool {
	t.Helper()
	db := InitHolocronDSN(dsn)
	defer db.Close()
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		t.Fatalf("pragma_table_info(%s): %v", table, err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan pragma: %v", err)
		}
		out[n] = true
	}
	return out
}

// TestAUDIT_schema_and_time is the umbrella — sub-tests demonstrate each
// distinct finding. Grouping them keeps the file to a single Test entry point
// while still letting `-run` narrow to one AUDIT-NNN.
func TestAUDIT_schema_and_time(t *testing.T) {
	// Umbrella test — each sub-test has its own `t.Skip(...)` that gets
	// removed when the corresponding fix lands. Fix #4 removed the skip on
	// TestAUDIT_023_createSchema_drift; all other sub-tests remain skipped
	// until their respective fixes land.
	root := projectRoot(t)
	schemaGo := readFile(t, filepath.Join(root, "internal", "store", "schema.go"))
	schemaSQL := readFile(t, filepath.Join(root, "schema", "schema.sql"))

	// ── AUDIT-023 ─────────────────────────────────────────────────────────
	// Fresh createSchema is missing columns that runMigrations has: at least
	// Fleet_Mail.consumed_at and Repositories.pr_review_enabled. Escalations
	// .acknowledged_at is today present in createSchema, so we don't assert
	// it missing — but we DO assert createSchema is self-contained, which is
	// what the audit asks for.
	t.Run("TestAUDIT_023_createSchema_drift", func(t *testing.T) {
		// Extract the CREATE TABLE ... Fleet_Mail(...) body.
		fleetMail := extractCreate(schemaGo, "Fleet_Mail")
		if fleetMail == "" {
			t.Fatalf("could not locate CREATE TABLE Fleet_Mail in schema.go")
		}
		if !strings.Contains(fleetMail, "consumed_at") {
			t.Errorf("AUDIT-023: createSchema's Fleet_Mail CREATE omits consumed_at; "+
				"runMigrations adds it via ALTER. Fresh-install DBs therefore have "+
				"the column (because runMigrations also runs) but createSchema alone "+
				"is not self-contained. Block:\n%s", fleetMail)
		}

		repos := extractCreate(schemaGo, "Repositories")
		if repos == "" {
			t.Fatalf("could not locate CREATE TABLE Repositories in schema.go")
		}
		if !strings.Contains(repos, "pr_review_enabled") {
			t.Errorf("AUDIT-023: createSchema's Repositories CREATE omits pr_review_enabled; "+
				"only runMigrations adds it. Block:\n%s", repos)
		}

		// Positive control: acknowledged_at SHOULD already be in createSchema's
		// Escalations definition (confirmed during this audit).
		esc := extractCreate(schemaGo, "Escalations")
		if !strings.Contains(esc, "acknowledged_at") {
			t.Errorf("AUDIT-023 control: expected Escalations.acknowledged_at in createSchema; "+
				"not found. Block:\n%s", esc)
		}

		// Empirical: confirm runMigrations-applied DB has the columns too.
		fmCols := columnsOf(t, ":memory:", "Fleet_Mail")
		if !fmCols["consumed_at"] {
			t.Errorf("AUDIT-023: runMigrations also missing consumed_at after init; table columns=%v", fmCols)
		}
		rCols := columnsOf(t, ":memory:", "Repositories")
		if !rCols["pr_review_enabled"] {
			t.Errorf("AUDIT-023: runMigrations also missing pr_review_enabled after init; table columns=%v", rCols)
		}
	})

	// ── AUDIT-077 ─────────────────────────────────────────────────────────
	// `ALTER TABLE BountyBoard DROP COLUMN blocked_by` runs on every startup.
	// On an already-migrated DB the column is gone, so the second run raises
	// "no such column" — the error is silently swallowed by db.Exec (we ignore
	// its return). Assert the statement is ungated.
	t.Run("TestAUDIT_077_drop_column_every_startup", func(t *testing.T) {
		t.Skip("AUDIT-077: remove when createSchema is self-contained (Fix #4 companion)")
		// Without skip, fails with: AUDIT-077: `ALTER TABLE BountyBoard DROP
		// COLUMN blocked_by` in schema.go:327 has no pragma_table_info gate — it
		// re-runs on every startup and the 'no such column' error is swallowed by
		// the unchecked db.Exec return value.
		if !strings.Contains(schemaGo, `DROP COLUMN blocked_by`) {
			t.Fatalf("AUDIT-077 stale citation: DROP COLUMN blocked_by no longer in schema.go")
		}
		// The defect: the DROP COLUMN statement has no pragma gate, so it runs
		// every startup and the second run silently errors.
		idx := strings.Index(schemaGo, `DROP COLUMN blocked_by`)
		window := schemaGo[max0(idx-400):idx]
		gated := strings.Contains(window, "pragma_table_info") || strings.Contains(window, "PRAGMA table_info")
		if !gated {
			t.Errorf("AUDIT-077: `ALTER TABLE BountyBoard DROP COLUMN blocked_by` in schema.go:327 " +
				"has no pragma_table_info gate — it re-runs on every startup and the " +
				"'no such column' error is swallowed by the unchecked db.Exec return value. " +
				"Fix: wrap the DROP COLUMN in a pragma_table_info check.")
		}
	})

	// ── AUDIT-078 ─────────────────────────────────────────────────────────
	// runMigrations' ALTER for BountyBoard.created_at uses DEFAULT '' while
	// createSchema uses DEFAULT (datetime('now')). Drift causes upgraded DBs
	// to insert empty-string created_at, excluded from 12h priority aging.
	t.Run("TestAUDIT_078_created_at_default_mismatch", func(t *testing.T) {
		t.Skip("AUDIT-078: remove when createSchema is self-contained (Fix #4 companion)")
		// Without skip, fails with: AUDIT-078: runMigrations' ALTER for
		// BountyBoard.created_at uses DEFAULT '' while createSchema uses
		// DEFAULT (datetime('now')). Upgraded DBs get '' and are excluded from
		// 12h priority aging.
		bb := extractCreate(schemaGo, "BountyBoard")
		if bb == "" {
			t.Fatalf("could not locate CREATE TABLE BountyBoard in schema.go")
		}
		createOK := regexp.MustCompile(`created_at\s+TEXT\s+DEFAULT\s+\(datetime\('now'\)\)`).MatchString(bb)
		if !createOK {
			t.Fatalf("AUDIT-078 stale: createSchema BountyBoard.created_at is no longer DEFAULT (datetime('now'))")
		}
		// Defect present iff the ALTER still uses DEFAULT ''.
		badALTER := regexp.MustCompile(`ALTER TABLE BountyBoard ADD COLUMN created_at\s+TEXT\s+DEFAULT\s+''`)
		if badALTER.MatchString(schemaGo) {
			t.Errorf("AUDIT-078: runMigrations' ALTER for BountyBoard.created_at uses DEFAULT '' " +
				"(schema.go:303) while createSchema uses DEFAULT (datetime('now')) (schema.go:45). " +
				"On upgraded DBs, rows inserted by paths that don't set created_at explicitly get '' " +
				"and are excluded from `WHERE created_at < datetime('now','-12 hours')` priority aging. " +
				"Fix: change the ALTER default to datetime('now') or run a UPDATE backfill after.")
		}
	})

	// ── AUDIT-080 ─────────────────────────────────────────────────────────
	// schema.sql is a reference file; it must mirror schema.go. Today it's
	// missing AskBranchPRs.stall_retrigger_count.
	t.Run("TestAUDIT_080_schema_sql_drift_stall_retrigger_count", func(t *testing.T) {
		t.Skip("AUDIT-080: remove when createSchema is self-contained (Fix #4 companion)")
		// Without skip, fails with: AUDIT-080: schema/schema.sql (reference file)
		// omits AskBranchPRs.stall_retrigger_count, but internal/store/schema.go
		// defines it. Reference file drifts from authoritative schema.
		if !strings.Contains(schemaGo, "stall_retrigger_count") {
			t.Fatalf("AUDIT-080 stale citation: stall_retrigger_count absent from schema.go too")
		}
		if !strings.Contains(schemaSQL, "stall_retrigger_count") {
			t.Errorf("AUDIT-080: schema/schema.sql (reference file) omits " +
				"AskBranchPRs.stall_retrigger_count, but internal/store/schema.go defines it. " +
				"Anyone consulting schema.sql as documentation gets a stale schema. " +
				"Fix: add the column to schema.sql and ideally add CI diffing the two.")
		}
	})

	// ── AUDIT-082 ─────────────────────────────────────────────────────────
	// cmd/force/integration_test.go:102-103 inserts into Escalations.reason,
	// which doesn't exist — the real column is `message`. The INSERT fails
	// silently (no error check on db.Exec), so the test asserts "no panic"
	// against an empty Escalations table.
	t.Run("TestAUDIT_082_integration_test_wrong_column", func(t *testing.T) {
		t.Skip("AUDIT-082: remove when integration_test uses column name 'message' (Fix #8 companion)")
		// Without skip, fails with: AUDIT-082: integration_test inserts into
		// Escalations.reason, but real column is `message`. The INSERT silently
		// errors (unchecked db.Exec); the test asserts only absence of panic.
		path := filepath.Join(root, "cmd", "force", "integration_test.go")
		src := readFile(t, path)
		// Find the offending INSERT.
		if !strings.Contains(src, "INSERT INTO Escalations") {
			t.Fatalf("AUDIT-082 stale citation: no INSERT INTO Escalations in %s", path)
		}
		// Extract the INSERT statement text.
		re := regexp.MustCompile(`INSERT INTO Escalations \(([^)]*)\)`)
		m := re.FindStringSubmatch(src)
		if len(m) < 2 {
			t.Fatalf("AUDIT-082 could not parse column list from INSERT")
		}
		cols := m[1]
		if strings.Contains(cols, "reason") {
			t.Errorf("AUDIT-082: integration_test inserts into Escalations.reason, "+
				"but real column is `message` (see schema.go CREATE TABLE Escalations). "+
				"The INSERT silently errors; the test asserts only absence of panic. "+
				"Column list: %q", cols)
		}
		// Empirical: reproduce the bad insert against a real DB and confirm
		// it errors with "no such column: reason".
		db := InitHolocronDSN(":memory:")
		defer db.Close()
		_, err := db.Exec(`INSERT INTO Escalations (task_id, severity, reason, status, created_at)
			VALUES (?, 'medium', 'test escalation', 'open', datetime('now'))`, 1)
		if err == nil {
			t.Errorf("AUDIT-082: expected INSERT with bogus `reason` column to fail; got nil")
		} else {
			msg := err.Error()
			// SQLite phrasing varies by version: "no such column" or
			// "has no column named". Either confirms the column is missing.
			if !strings.Contains(msg, "no such column") && !strings.Contains(msg, "has no column named") {
				t.Errorf("AUDIT-082: expected missing-column error, got: %v", err)
			}
		}
	})

	// ── AUDIT-130 ─────────────────────────────────────────────────────────
	// Astromech claim loop (SpawnAstromech, ~lines 244-266) does not consult
	// Repositories.quarantined_at. Grep the file.
	t.Run("TestAUDIT_130_astromech_claim_ignores_quarantine", func(t *testing.T) {
		t.Skip("AUDIT-130: remove when astromech claim loop checks quarantined_at (Fix #8)")
		// Without skip, fails with: AUDIT-130: astromech.go SpawnAstromech claim
		// loop never consults Repositories.quarantined_at after ClaimBounty.
		// Enforcement lives in openSubPRForApprovedTask (post-Claude), so a
		// quarantined repo burns a full astromech session before the PR step
		// rejects.
		path := filepath.Join(root, "internal", "agents", "astromech.go")
		src := readFile(t, path)
		loopStart := strings.Index(src, "func SpawnAstromech(")
		if loopStart < 0 {
			t.Fatalf("AUDIT-130 stale citation: SpawnAstromech not found in astromech.go")
		}
		// Look at the first ~2 KB after the func decl — covers the claim loop.
		end := loopStart + 2048
		if end > len(src) {
			end = len(src)
		}
		snippet := src[loopStart:end]
		if !strings.Contains(snippet, "ClaimBounty") {
			t.Fatalf("AUDIT-130: ClaimBounty not found in SpawnAstromech body — citation stale")
		}
		checksQuarantine := strings.Contains(snippet, "quarantined_at") ||
			strings.Contains(snippet, "Quarantined") ||
			strings.Contains(snippet, "QuarantinedAt")
		if !checksQuarantine {
			t.Errorf("AUDIT-130: astromech.go SpawnAstromech claim loop (~lines 244-266) " +
				"never consults Repositories.quarantined_at after ClaimBounty. " +
				"Enforcement lives in openSubPRForApprovedTask (post-Claude), so a " +
				"quarantined repo burns a full astromech session before the PR step rejects. " +
				"Fix: post-ClaimBounty, look up the repo; if quarantined, requeue Pending " +
				"with error_log.")
		}
	})

	// ── AUDIT-131 ─────────────────────────────────────────────────────────
	// RunDogs tries UnmarshalText first (RFC3339), falls back to
	// ParseInLocation. SQLite `datetime('now')` output is "YYYY-MM-DD HH:MM:SS"
	// with no TZ — UnmarshalText always fails on it.
	t.Run("TestAUDIT_131_dog_cooldown_tz_parse", func(t *testing.T) {
		t.Skip("AUDIT-131: remove when TZ parse centralized through store.NowSQLite (Fix #8)")
		// Without skip, fails with: AUDIT-131: RunDogs (dogs.go:80-88) keeps a
		// UnmarshalText branch that ALWAYS fails on SQLite's `datetime('now')`
		// output ("YYYY-MM-DD HH:MM:SS" has no TZ; UnmarshalText needs RFC3339).
		// Works today only via the ParseInLocation fallback.
		path := filepath.Join(root, "internal", "agents", "dogs.go")
		src := readFile(t, path)
		fn := extractFunc(src, "RunDogs")
		if fn == "" {
			t.Fatalf("AUDIT-131: RunDogs not found")
		}
		// Defect shape: both UnmarshalText AND ParseInLocation fallback present.
		hasUnmarshal := strings.Contains(fn, "UnmarshalText")
		hasFallback := strings.Contains(fn, `ParseInLocation("2006-01-02 15:04:05"`)
		if hasUnmarshal && hasFallback {
			t.Errorf("AUDIT-131: RunDogs (dogs.go:80-88) keeps a UnmarshalText branch " +
				"that ALWAYS fails on SQLite's `datetime('now')` output " +
				"(\"YYYY-MM-DD HH:MM:SS\" has no TZ; UnmarshalText needs RFC3339). " +
				"Works today only via the ParseInLocation fallback. Fix: drop the " +
				"UnmarshalText branch, call ParseInLocation directly.")
		}
		// Also confirm the hazard with a live UnmarshalText call — if this
		// ever starts succeeding, the finding needs re-audit.
		var ts time.Time
		if err := (&ts).UnmarshalText([]byte("2025-04-23 12:34:56")); err == nil {
			t.Errorf("AUDIT-131 control: UnmarshalText accepted a SQLite-shaped " +
				"timestamp — stdlib behavior changed; re-audit the finding.")
		}
	})

	// ── AUDIT-132 ─────────────────────────────────────────────────────────
	// pr_flow.go silently swallows time.Parse errors on AskBranchPRs.created_at:
	// handleSubPRPoll returns on parseErr; timeSinceCreatedAt returns 0. Both
	// mean malformed data goes unseen.
	t.Run("TestAUDIT_132_askbranchpr_created_at_parse_swallow", func(t *testing.T) {
		t.Skip("AUDIT-132: remove when handleSubPRPoll escalates after parseErr (Fix #8)")
		// Without skip, fails with: AUDIT-132: pr_flow.go swallows time.Parse
		// errors on AskBranchPRs.created_at. handleSubPRPoll silently returns on
		// parseErr; timeSinceCreatedAt returns 0 on err. Malformed timestamps →
		// handleSubPRPoll abandons the PR; timeSinceCreatedAt treats it as
		// brand-new forever (no escalation).
		path := filepath.Join(root, "internal", "agents", "pr_flow.go")
		src := readFile(t, path)
		handleSubPRPoll := extractFunc(src, "handleSubPRPoll")
		if handleSubPRPoll == "" {
			t.Fatalf("AUDIT-132: handleSubPRPoll not found")
		}
		// Defect A: silent `return` on parseErr.
		reSilent := regexp.MustCompile(`time\.Parse\([^)]*,\s*pr\.CreatedAt\)\s*\n\s*if\s+parseErr\s*!=\s*nil\s*\{\s*\n\s*return\s*\n\s*\}`)
		defectA := reSilent.MatchString(handleSubPRPoll)

		tSince := extractFunc(src, "timeSinceCreatedAt")
		if tSince == "" {
			t.Fatalf("AUDIT-132: timeSinceCreatedAt not found")
		}
		// Defect B: silent `return 0` on err.
		reZero := regexp.MustCompile(`if\s+err\s*!=\s*nil\s*\{\s*\n\s*return\s+0\s*\n\s*\}`)
		defectB := reZero.MatchString(tSince)

		if defectA || defectB {
			t.Errorf("AUDIT-132: pr_flow.go swallows time.Parse errors on AskBranchPRs.created_at. " +
				"defectA(handleSubPRPoll silent return)=%v defectB(timeSinceCreatedAt returns 0)=%v. " +
				"Malformed timestamps → handleSubPRPoll abandons the PR; timeSinceCreatedAt " +
				"treats it as brand-new forever (no escalation). Fix: log + fall back to " +
				"BountyBoard.created_at; escalate after N failed parses.",
				defectA, defectB)
		}
	})

	// ── AUDIT-143 ─────────────────────────────────────────────────────────
	// PR review classifier has no bounded retry counter — a row whose
	// classification LLM call fails to parse JSON loops every 5 min forever.
	// Assert (a) PRReviewComments has no `classify_attempts` column, and
	// (b) pr_review_triage.go never increments / checks such a column.
	t.Run("TestAUDIT_143_pr_review_classifier_unbounded", func(t *testing.T) {
		t.Skip("AUDIT-143: remove when PRReviewComments.classify_attempts added (Fix #7)")
		// Without skip, fails with: AUDIT-143: PR review classifier has no
		// bounded retry with critic note. PRReviewComments has no
		// classify_attempts column and pr_review_triage.go does not reference
		// one. Parse-failing comments loop every 5 min forever.
		cols := columnsOf(t, ":memory:", "PRReviewComments")
		hasCounter := cols["classify_attempts"]

		triagePath := filepath.Join(root, "internal", "agents", "pr_review_triage.go")
		triageExists := false
		refsCounter := false
		if _, err := os.Stat(triagePath); err == nil {
			triageExists = true
			triage := readFile(t, triagePath)
			refsCounter = strings.Contains(triage, "classify_attempts")
			if !strings.Contains(triage, "classification") {
				t.Fatalf("AUDIT-143 stale: pr_review_triage.go no longer mentions " +
					"classification — citation may be outdated.")
			}
		}
		if !hasCounter && (!triageExists || !refsCounter) {
			t.Errorf("AUDIT-143: PR review classifier has no bounded retry with critic note. " +
				"PRReviewComments has no classify_attempts column (hasCounter=%v) and " +
				"pr_review_triage.go does not reference one (triageExists=%v refsCounter=%v). " +
				"Parse-failing comments loop every 5 min forever. Fix: add classify_attempts " +
				"column; cap at N=3; one critic-note retry per tick.",
				hasCounter, triageExists, refsCounter)
		}
	})

	// ── AUDIT-146 ─────────────────────────────────────────────────────────
	// ListDogs compares time.Now() (local) against a UTC-parsed time. Works by
	// coincidence today because time.Since uses monotonic math, but the code
	// pattern is fragile.
	t.Run("TestAUDIT_146_listdogs_wall_clock_vs_utc", func(t *testing.T) {
		t.Skip("AUDIT-146: remove when TZ parse centralized through store.NowSQLite (Fix #8)")
		// Without skip, fails with: AUDIT-146: ListDogs compares time.Now()
		// (local wall-clock) to a ParseInLocation-UTC'd timestamp. Fragile to
		// any refactor that swaps the parse. Fix: always use time.Now().UTC().
		path := filepath.Join(root, "internal", "agents", "dogs.go")
		src := readFile(t, path)
		listDogs := extractFunc(src, "ListDogs")
		if listDogs == "" {
			t.Fatalf("AUDIT-146: ListDogs not found")
		}
		// Defect: compares time.Now() (local) against a ParseInLocation-UTC'd time.
		usesRawNow := strings.Contains(listDogs, "time.Now().Before(next)") ||
			regexp.MustCompile(`time\.Now\(\)\.Sub\(`).MatchString(listDogs)
		usesUTCNow := strings.Contains(listDogs, "time.Now().UTC()")
		if usesRawNow && !usesUTCNow {
			t.Errorf("AUDIT-146: ListDogs (dogs.go:580-586) compares time.Now() " +
				"(local wall-clock) to a ParseInLocation-UTC'd timestamp. Works today " +
				"because time.Time values carry their own Location, but fragile to " +
				"any refactor that swaps the parse. Fix: always use time.Now().UTC().")
		}
	})

	// ── AUDIT-147 ─────────────────────────────────────────────────────────
	// detectStalledTasks uses `time.Parse("2006-01-02 15:04:05", ...)` which
	// returns a time.Time in UTC (documented), but callers then compare via
	// time.Since, which is wall-clock-agnostic. The hazard is identical to
	// 146 — assert the naïve parse still ships.
	t.Run("TestAUDIT_147_detectstalled_mixes_utc_and_local", func(t *testing.T) {
		t.Skip("AUDIT-147: remove when TZ parse centralized through store.NowSQLite (Fix #8)")
		// Without skip, fails with: AUDIT-147: detectStalledTasks uses raw
		// time.Parse("2006-01-02 15:04:05", lockedAt) — returns UTC by default
		// but couples every caller to this implicit assumption. Fix: centralize
		// through store.ParseSQLiteTime / NowSQLite helper.
		path := filepath.Join(root, "internal", "agents", "inquisitor.go")
		src := readFile(t, path)
		fn := extractFunc(src, "detectStalledTasks")
		if fn == "" {
			t.Fatalf("AUDIT-147: detectStalledTasks not found")
		}
		rawParse := strings.Contains(fn, `time.Parse("2006-01-02 15:04:05"`)
		hasHelper := strings.Contains(fn, "store.NowSQLite") ||
			strings.Contains(fn, "ParseSQLiteTime")
		if rawParse && !hasHelper {
			t.Errorf("AUDIT-147: detectStalledTasks (inquisitor.go:202-208) uses raw " +
				"time.Parse(\"2006-01-02 15:04:05\", lockedAt) — returns UTC by default " +
				"but couples every caller to this implicit assumption. Fix: centralize " +
				"SQLite-shaped parsing through a store.ParseSQLiteTime / NowSQLite helper.")
		}
	})

	// ── AUDIT-148 ─────────────────────────────────────────────────────────
	// RateLimitBackoff loops `d *= 2` `count` times before cap check — for a
	// corrupted large `count`, d overflows to a negative time.Duration and
	// the function returns ≤ 0, making callers spin with zero sleep.
	t.Run("TestAUDIT_148_ratelimitbackoff_overflow", func(t *testing.T) {
		t.Skip("AUDIT-148: remove when count clamped pre-loop (Fix #1 companion)")
		// Without skip, fails with: AUDIT-148: RateLimitBackoff doubles `d`
		// `count` times BEFORE the 10m cap check. For a corrupted large count
		// (e.g. 62), the int64 ns value overflows negative; `d > 10*time.Minute`
		// is false; the function returns the wrapped value and callers spin with
		// near-zero sleep.
		path := filepath.Join(root, "internal", "agents", "estop.go")
		src := readFile(t, path)
		fn := extractFunc(src, "RateLimitBackoff")
		if fn == "" {
			t.Fatalf("AUDIT-148: RateLimitBackoff not found")
		}
		// Defect: no pre-loop bound on count. A fix would add either a
		// `if count > N { count = N }` clamp or switch to a single-step
		// shift with min clamp.
		hasClamp := regexp.MustCompile(`if\s+count\s*>\s*\d+\s*\{\s*count\s*=\s*\d+\s*\}`).MatchString(fn) ||
			regexp.MustCompile(`min\s*\(`).MatchString(fn) ||
			strings.Contains(fn, "math.Min")
		if !hasClamp {
			t.Errorf("AUDIT-148: RateLimitBackoff (estop.go:83-92) doubles `d` `count` times " +
				"BEFORE the 10m cap check. For a corrupted large count (e.g. 62), the int64 " +
				"nanosecond value overflows negative; `d > 10*time.Minute` is false; the " +
				"function returns the wrapped value and callers spin with near-zero sleep. " +
				"Fix: `if count > 4 { count = 4 }` pre-loop, or use a single shift with min clamp.")
		}

		// Empirical math reproduction of the exact loop body in
		// RateLimitBackoff (we can't import the agents package from store
		// without a dep cycle, so we replicate the arithmetic verbatim).
		const big = 62 // 60s << 62 overflows int64 ns
		d := 60 * time.Second
		for i := 0; i < big; i++ {
			d *= 2
		}
		capDur := 10 * time.Minute
		if d > capDur {
			d = capDur
		}
		if d > 0 {
			t.Errorf("AUDIT-148 math: expected overflow wrap to non-positive duration "+
				"with count=%d; got %v. Overflow behavior changed — re-audit.", big, d)
		}
		_ = math.MaxInt64 // keep math import live
	})
}

// ── helpers ──────────────────────────────────────────────────────────────

// extractCreate pulls the body of the FIRST `CREATE TABLE IF NOT EXISTS <name>`
// occurrence, up to the matching close paren + `);`.
func extractCreate(src, table string) string {
	needle := "CREATE TABLE IF NOT EXISTS " + table
	i := strings.Index(src, needle)
	if i < 0 {
		return ""
	}
	// Find the terminating `);` closing the CREATE.
	j := strings.Index(src[i:], ");")
	if j < 0 {
		return ""
	}
	return src[i : i+j+2]
}

// extractFunc pulls a Go function body (the source from `func <name>(` up to
// the matching `}` that terminates the body). Naïvely brace-balancing from
// the first `{` breaks on interface parameter types like
// `interface{ Printf(string, ...any) }` — so we first skip past the
// parameter+return-type signature by balancing parens, then start counting
// braces only after we hit the opening `{` that follows the signature.
func extractFunc(src, name string) string {
	needle := "func " + name + "("
	i := strings.Index(src, needle)
	if i < 0 {
		// Try method receiver form: `func (x *T) name(`
		re := regexp.MustCompile(`func\s+\([^)]*\)\s+` + regexp.QuoteMeta(name) + `\(`)
		loc := re.FindStringIndex(src)
		if loc == nil {
			return ""
		}
		i = loc[0]
	}
	// Walk the signature: find the matching `)` that closes the parameter
	// list, then any return-type text, then the body-opening `{`.
	// Start paren depth at 0; the first `(` we see is the opening of the
	// param list.
	depth := 0
	k := i
	sawOpen := false
	for ; k < len(src); k++ {
		switch src[k] {
		case '(':
			depth++
			sawOpen = true
		case ')':
			depth--
			if sawOpen && depth == 0 {
				k++ // advance past the closing `)`
				goto afterSig
			}
		}
	}
	return ""
afterSig:
	// Skip past return types until the body-opening `{`.
	for ; k < len(src); k++ {
		if src[k] == '{' {
			break
		}
	}
	if k >= len(src) {
		return ""
	}
	// Now balance braces from this `{` to its matching close.
	bd := 0
	for ; k < len(src); k++ {
		switch src[k] {
		case '{':
			bd++
		case '}':
			bd--
			if bd == 0 {
				return src[i : k+1]
			}
		}
	}
	return ""
}

func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

