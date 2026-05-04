// dogs_arch_health_report_test.go — D9 ArchHealth dog tests.
//
// Smoke + idempotence + per-author flag + 6-month trend coverage.
// Tests construct the BoS rule fixtures inline and run the dog against
// a tiny tree under t.TempDir() so the production filesystem is never
// touched.
//
// Per CLAUDE.md "Testing rules":
//   - real SQLite (in-memory)
//   - real BoS reviewer (no mocks)
//   - real filesystem (t.TempDir)
//   - real renderer output (read back from disk for assertions)

package agents

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"force-orchestrator/internal/store"
)

// archHealthTestLogger captures dog log output for assertions.
type archHealthTestLogger struct{ msgs []string }

func (l *archHealthTestLogger) Printf(format string, a ...any) {
	l.msgs = append(l.msgs, fmt.Sprintf(format, a...))
}

// setupArchHealthEnv seeds two repos × two violating Go files and pins
// the dog's reports dir + clock so the test is deterministic.
//
// Returns (db, reportsDir, restoreFns) — caller defers restoreFns.
func setupArchHealthEnv(t *testing.T, fixedNow time.Time, perRepoFiles map[string][]archHealthFixture) (*sql.DB, string, func()) {
	t.Helper()
	db, _ := seedDB(t)
	// Drop the seeded "demo" repo — we re-create per-test repos below
	// so the path layout is fully under the test's control.
	if _, err := db.Exec(`DELETE FROM Repositories WHERE name = 'demo'`); err != nil {
		t.Fatalf("clear demo repo: %v", err)
	}
	for repoName, files := range perRepoFiles {
		repoDir := t.TempDir()
		for _, f := range files {
			full := filepath.Join(repoDir, f.path)
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(f.source), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := db.Exec(`INSERT INTO Repositories (name, local_path, mode) VALUES (?, ?, 'write')`, repoName, repoDir); err != nil {
			t.Fatalf("seed repo %s: %v", repoName, err)
		}
	}

	reportsDir := t.TempDir()
	restoreDir := SetArchHealthReportsDirForTest(reportsDir)
	restoreClock := SetArchHealthClockForTest(func() time.Time { return fixedNow })
	restoreWeights := SetArchHealthWeightsPathForTest("") // force fall-through to default 1.0/rule

	restore := func() {
		restoreDir()
		restoreClock()
		restoreWeights()
	}
	return db, reportsDir, restore
}

// archHealthFixture is a single file laid down inside a test repo.
type archHealthFixture struct{ path, source string }

// TestDogArchHealth_Smoke — the spec-required smoke shape:
// 2 repos × 2 BoS rules × 5 violations → run dog → ArchHealthAggregates
// populated + report rendered with the expected sections.
func TestDogArchHealth_Smoke(t *testing.T) {
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

	// Five copies of a BOS-001 (mutator returns no error) fixture per repo.
	violation := func(i int) archHealthFixture {
		return archHealthFixture{
			path: fmt.Sprintf("internal/store/fix%d.go", i),
			source: fmt.Sprintf(`
package store
import "database/sql"
func UpdateThing%d(db *sql.DB, id int) {
	db.Exec("UPDATE T SET x=1 WHERE id=?", id)
}
`, i),
		}
	}
	// Add a BOS-007 (payload LIKE '%"convoy_id"...') violation per repo
	// to satisfy the "2 BoS rules" spec line. BOS-007 fires on a query
	// embedded inside internal/store/.
	bos007 := func(i int) archHealthFixture {
		return archHealthFixture{
			path: fmt.Sprintf("internal/store/like%d.go", i),
			source: fmt.Sprintf(`
package store
import "database/sql"
func ListByConvoy%d(db *sql.DB) error {
	_, err := db.Exec(%cSELECT * FROM BountyBoard WHERE payload LIKE '%%"convoy_id":1,%%'%c)
	return err
}
`, i, '`', '`'),
		}
	}

	per := map[string][]archHealthFixture{
		"alpha": {
			violation(1), violation(2), violation(3), violation(4), violation(5),
			bos007(1),
		},
		"beta": {
			violation(6), violation(7), violation(8), violation(9), violation(10),
			bos007(2),
		},
	}

	db, reportsDir, restore := setupArchHealthEnv(t, now, per)
	defer restore()

	logger := &archHealthTestLogger{}
	if err := dogArchitectureHealthReport(context.Background(), db, logger); err != nil {
		t.Fatalf("dogArchitectureHealthReport: %v", err)
	}

	// ArchHealthAggregates populated.
	month := now.Format("2006-01")
	rows, err := store.ListArchHealthAggregatesForMonth(db, month)
	if err != nil {
		t.Fatalf("ListArchHealthAggregatesForMonth: %v", err)
	}
	if len(rows) == 0 {
		t.Fatalf("expected ArchHealthAggregates rows; got 0; logs:\n%s", strings.Join(logger.msgs, "\n"))
	}
	// Expect at least BOS-001 + BOS-009 firings.
	gotRules := map[string]int{}
	for _, r := range rows {
		gotRules[r.RuleID] += r.ViolationCount
	}
	if gotRules["BOS-001"] < 10 {
		t.Errorf("expected ≥10 BOS-001 violations across both repos (5 each); got %d", gotRules["BOS-001"])
	}
	if gotRules["BOS-007"] < 2 {
		t.Errorf("expected ≥2 BOS-007 violations (one per repo); got %d (rule counts: %v)", gotRules["BOS-007"], gotRules)
	}

	// Report rendered with all six sections.
	reportPath := filepath.Join(reportsDir, fmt.Sprintf("architecture-health-%s.md", month))
	body, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	bodyStr := string(body)
	for _, section := range []string{
		"AUTO-GENERATED",
		"## 1. Executive Summary",
		"## 2. Per-Invariant",
		"## 3. Per-Repo",
		"## 4. Per-Author",
		"## 5. 6-Month Trend",
		"## 6. Methodology",
	} {
		if !strings.Contains(bodyStr, section) {
			t.Errorf("report missing section %q; full body:\n%s", section, bodyStr)
		}
	}
}

// TestDogArchHealth_Idempotence — running the dog twice in the same
// month produces the same number of rows (no duplicates), per the
// UNIQUE(report_month, rule_id, repo_id, author_type) clause.
func TestDogArchHealth_Idempotence(t *testing.T) {
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

	per := map[string][]archHealthFixture{
		"solo": {
			{
				path: "internal/store/x.go",
				source: `
package store
import "database/sql"
func UpdateOnly(db *sql.DB) {
	db.Exec("UPDATE T SET x=1")
}
`,
			},
		},
	}

	db, _, restore := setupArchHealthEnv(t, now, per)
	defer restore()

	logger := &archHealthTestLogger{}
	if err := dogArchitectureHealthReport(context.Background(), db, logger); err != nil {
		t.Fatalf("first run: %v", err)
	}
	first, _ := store.ListArchHealthAggregatesForMonth(db, now.Format("2006-01"))
	if err := dogArchitectureHealthReport(context.Background(), db, logger); err != nil {
		t.Fatalf("second run: %v", err)
	}
	second, _ := store.ListArchHealthAggregatesForMonth(db, now.Format("2006-01"))

	if len(first) != len(second) {
		t.Errorf("idempotence: first=%d rows, second=%d rows; expected equal", len(first), len(second))
	}
}

// TestArchHealthReport_PerAuthorFlag — when astromech violations exceed
// human violations, the rendered report MUST include the ⚠️ flag.
func TestArchHealthReport_PerAuthorFlag(t *testing.T) {
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	db, reportsDir, restore := setupArchHealthEnv(t, now, map[string][]archHealthFixture{
		"a": {
			{path: "human-only.go", source: "package main\n"},
		},
	})
	defer restore()

	// Hand-seed the aggregates so we don't depend on classifyAuthor for
	// this assertion: astromech 10 violations, human 2 violations.
	month := now.Format("2006-01")
	if err := store.UpsertArchHealthAggregate(db, store.ArchHealthAggregate{
		ReportMonth: month, RuleID: "BOS-001", RepoID: 1,
		AuthorType: archAuthorAstromech, ViolationCount: 10,
	}); err != nil {
		t.Fatalf("seed astromech: %v", err)
	}
	if err := store.UpsertArchHealthAggregate(db, store.ArchHealthAggregate{
		ReportMonth: month, RuleID: "BOS-001", RepoID: 1,
		AuthorType: archAuthorHuman, ViolationCount: 2,
	}); err != nil {
		t.Fatalf("seed human: %v", err)
	}

	repoNames := map[int]string{1: "a"}
	if _, err := renderArchHealthReport(db, month, repoNames); err != nil {
		t.Fatalf("render: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(reportsDir, fmt.Sprintf("architecture-health-%s.md", month)))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(body), "⚠️") {
		t.Errorf("expected ⚠️ flag (astromech > human); body:\n%s", string(body))
	}
	if !strings.Contains(string(body), "astromech violations exceed human") {
		t.Error("expected per-author flag wording in report")
	}
}

// TestArchHealthReport_SixMonthTrend — pre-seed 6 months of aggregates
// and assert the 6-Month Trend section renders all six month columns.
func TestArchHealthReport_SixMonthTrend(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	db, reportsDir, restore := setupArchHealthEnv(t, now, map[string][]archHealthFixture{
		"r": {{path: "noop.go", source: "package main\n"}},
	})
	defer restore()

	// Seed 6 months trailing through 2026-06.
	monthsToSeed := []string{"2026-01", "2026-02", "2026-03", "2026-04", "2026-05", "2026-06"}
	for i, m := range monthsToSeed {
		if err := store.UpsertArchHealthAggregate(db, store.ArchHealthAggregate{
			ReportMonth: m, RuleID: "BOS-001", RepoID: 1,
			AuthorType: archAuthorHuman, ViolationCount: i + 1,
		}); err != nil {
			t.Fatalf("seed %s: %v", m, err)
		}
	}

	repoNames := map[int]string{1: "r"}
	if _, err := renderArchHealthReport(db, "2026-06", repoNames); err != nil {
		t.Fatalf("render: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(reportsDir, "architecture-health-2026-06.md"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	bs := string(body)
	for _, m := range monthsToSeed {
		if !strings.Contains(bs, m) {
			t.Errorf("expected month %q in trend section; body:\n%s", m, bs)
		}
	}
	// Sparkline characters present (▁▂▃▄▅▆▇█).
	hasSparkRune := false
	for _, r := range "▁▂▃▄▅▆▇█" {
		if strings.ContainsRune(bs, r) {
			hasSparkRune = true
			break
		}
	}
	if !hasSparkRune {
		t.Errorf("expected sparkline rune in report; body:\n%s", bs)
	}
}

// TestArchHealthReport_AutoGeneratedHeader — the renderer MUST stamp the
// AUTO-GENERATED prefix the pre-commit hook expects.
func TestArchHealthReport_AutoGeneratedHeader(t *testing.T) {
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	db, reportsDir, restore := setupArchHealthEnv(t, now, map[string][]archHealthFixture{
		"r": {{path: "noop.go", source: "package main\n"}},
	})
	defer restore()

	month := now.Format("2006-01")
	if _, err := renderArchHealthReport(db, month, map[int]string{1: "r"}); err != nil {
		t.Fatalf("render: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(reportsDir, fmt.Sprintf("architecture-health-%s.md", month)))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	first := strings.SplitN(string(body), "\n", 2)[0]
	if !strings.HasPrefix(first, archHealthAutoGeneratedPrefix) {
		t.Errorf("expected first line to start with %q; got %q", archHealthAutoGeneratedPrefix, first)
	}
}

// TestArchHealthReport_NoReposNoOp — dog returns nil and writes no
// report when there are zero registered repos. Per CLAUDE.md "no
// silent failures": this is a legitimate empty-state, not a failure;
// we assert the dog logs the empty-state and writes nothing.
func TestArchHealthReport_NoReposNoOp(t *testing.T) {
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	db, reportsDir, restore := setupArchHealthEnv(t, now, map[string][]archHealthFixture{})
	defer restore()

	logger := &archHealthTestLogger{}
	if err := dogArchitectureHealthReport(context.Background(), db, logger); err != nil {
		t.Fatalf("dogArchitectureHealthReport: %v", err)
	}

	month := now.Format("2006-01")
	path := filepath.Join(reportsDir, fmt.Sprintf("architecture-health-%s.md", month))
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected no report when no repos registered; got %v", err)
	}
	if !strings.Contains(strings.Join(logger.msgs, "\n"), "no registered repos") {
		t.Errorf("expected log line about no registered repos; got logs:\n%s", strings.Join(logger.msgs, "\n"))
	}
}

// TestClassifyAuthor — covers the path-heuristic enum.
func TestClassifyAuthor(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"internal/agents/dogs.go", archAuthorHuman},
		{"internal/agents/astromech.go", archAuthorAstromech},
		{"internal/store/migrations/001_init.go", archAuthorArchaeologistMigration},
		{"internal/foo/archaeologist_old.go", archAuthorArchaeologistMigration},
		{"some/path/AstroMech_thing.go", archAuthorAstromech},
		{"unknown/file.go", archAuthorHuman},
	}
	for _, c := range cases {
		got := classifyAuthor(c.path)
		if got != c.want {
			t.Errorf("classifyAuthor(%q) = %q; want %q", c.path, got, c.want)
		}
	}
}
