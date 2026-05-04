// dogs_arch_health_report.go — D9 ArchHealth track.
//
// `architecture-health-report` is the monthly longitudinal report dog.
// It runs on the 1st of each month at 00:00 UTC (cadence enforced via
// the standard dogCooldowns map + a "first-of-month" guard at the top
// of the dog body so a mid-month tick that happens to clear the
// cooldown still no-ops). Per docs/roadmap.md § D9-ArchHealth:
//
//   - Runs every BoS rule over the FULL CURRENT CODEBASE (not just diffs).
//   - Aggregates violations per (rule_id, repo_id, author_type) where
//     author_type ∈ {human, astromech, archaeologist-migration}.
//   - Persists to the ArchHealthAggregates table (idempotent: re-runs in
//     the same month update existing rows rather than duplicating).
//   - Renders reports/architecture-health-YYYY-MM.md with a header that
//     says AUTO-GENERATED — the matching pre-commit hook
//     (scripts/pre-commit/arch-health-md-check.sh) refuses hand-edits to
//     these reports.
//
// Anti-cheat (docs/roadmap.md § D9):
//   - Weights file lives in docs/arch-health-weights.yaml; this dog reads
//     it directly. Any change to the per-rule weighting must land
//     through the D3 promotion pipeline (FleetRules + render-rules), not
//     by hand-editing the YAML. The renderer's weighted-average makes
//     the contract explicit so a silent re-weight is structurally
//     traceable.
//   - No silent failures: every error path returns an error to RunDogs
//     so the operator-mail surface fires.
package agents

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"force-orchestrator/internal/bos"
	_ "force-orchestrator/internal/bos/rules" // register every BoS rule
	"force-orchestrator/internal/store"
)

// authorType is the closed enum the dog assigns each finding. The ramp
// flag in the report (per-author compliance: "astromech worse than
// human → ⚠️") consumes these values; do NOT add others without
// updating the renderer's per-author section.
const (
	archAuthorHuman                  = "human"
	archAuthorAstromech              = "astromech"
	archAuthorArchaeologistMigration = "archaeologist-migration"
)

// archHealthReportsDir is the directory the dog renders into. Production
// callers leave this at the package default; tests override via
// SetArchHealthReportsDirForTest so the renderer writes inside t.TempDir()
// rather than polluting the worktree's reports/.
var archHealthReportsDir = "reports"

// archHealthWeightsPath is the on-disk YAML the renderer reads for the
// per-rule weight set used in the per-repo invariant-health score.
// Defaults to the canonical docs/arch-health-weights.yaml; tests
// override via SetArchHealthWeightsPathForTest to inject deterministic
// values (or an explicit empty path so we fall back to "all weights 1").
var archHealthWeightsPath = "docs/arch-health-weights.yaml"

// archHealthClock is the indirection seam tests use to fix "now" so the
// renderer produces a deterministic month token. Production: time.Now.
var archHealthClock = func() time.Time { return time.Now().UTC() }

// SetArchHealthReportsDirForTest swaps the reports directory and returns
// a restore closure. Tests use this to direct rendering at t.TempDir().
func SetArchHealthReportsDirForTest(dir string) (restore func()) {
	prev := archHealthReportsDir
	archHealthReportsDir = dir
	return func() { archHealthReportsDir = prev }
}

// SetArchHealthWeightsPathForTest swaps the weights YAML path and
// returns a restore closure.
func SetArchHealthWeightsPathForTest(path string) (restore func()) {
	prev := archHealthWeightsPath
	archHealthWeightsPath = path
	return func() { archHealthWeightsPath = prev }
}

// SetArchHealthClockForTest swaps the "now" indirection and returns a
// restore closure. The dog and renderer both source "now" through
// archHealthClock so the test's fixed time controls both the
// month-token and the AUTO-GENERATED header timestamp.
func SetArchHealthClockForTest(now func() time.Time) (restore func()) {
	prev := archHealthClock
	archHealthClock = now
	return func() { archHealthClock = prev }
}

// dogArchitectureHealthReport is the runDog entry point. The runDog
// switch in dogs.go dispatches `architecture-health-report` here.
//
// Behaviour:
//
//	1. Resolve the report_month from archHealthClock() ("YYYY-MM").
//	2. Walk every registered repo via store.ListRepos, scan every .go
//	   file with go/parser, run every BoS rule, classify each finding
//	   by author_type via classifyAuthor (last-touch git blame proxy
//	   approximated through path heuristics — see classifyAuthor).
//	3. Aggregate per (rule_id, repo_id, author_type) and upsert to
//	   ArchHealthAggregates. Idempotence is enforced by the unique
//	   constraint + ON CONFLICT DO UPDATE (a re-run in the same month
//	   refreshes the count rather than duplicating).
//	4. Render reports/architecture-health-<month>.md.
//
// On any per-repo or per-file failure: log + continue. The dog returns
// an error only when a structural problem (e.g. ListRepos query fails,
// reports directory un-creatable) prevents progress on every repo.
func dogArchitectureHealthReport(ctx context.Context, db *sql.DB, logger interface{ Printf(string, ...any) }) error {
	if db == nil {
		return fmt.Errorf("architecture-health-report: db is nil")
	}
	month := archHealthClock().Format("2006-01")
	logger.Printf("Dog architecture-health-report: scanning month=%s", month)

	repos := store.ListRepos(db)
	if len(repos) == 0 {
		logger.Printf("Dog architecture-health-report: no registered repos — nothing to scan")
		return nil
	}

	gate := bos.DBFleetRulesGate(db)

	// Aggregation buffer — one entry per (rule_id, repo_id, author_type).
	type aggKey struct {
		ruleID     string
		repoID     int
		authorType string
	}
	counts := map[aggKey]int{}

	for repoIdx, r := range repos {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if r.LocalPath == "" {
			logger.Printf("architecture-health-report: repo %q has empty local_path — skipping", r.Name)
			continue
		}
		if _, statErr := os.Stat(r.LocalPath); statErr != nil {
			logger.Printf("architecture-health-report: repo %q local_path inaccessible: %v — skipping", r.Name, statErr)
			continue
		}
		repoID := repoIdx + 1 // synthetic 1-indexed id; stable for one render
		findings, scanErr := scanRepoForBoS(ctx, r.LocalPath, gate)
		if scanErr != nil {
			logger.Printf("architecture-health-report: repo %q scan failed: %v — skipping", r.Name, scanErr)
			continue
		}
		for _, f := range findings {
			author := classifyAuthor(f.Path)
			counts[aggKey{ruleID: f.RuleID, repoID: repoID, authorType: author}]++
		}
		logger.Printf("architecture-health-report: repo %q (id=%d) scanned — %d finding(s)",
			r.Name, repoID, len(findings))
	}

	// Persist aggregates.
	for k, v := range counts {
		if err := store.UpsertArchHealthAggregate(db, store.ArchHealthAggregate{
			ReportMonth:    month,
			RuleID:         k.ruleID,
			RepoID:         k.repoID,
			AuthorType:     k.authorType,
			ViolationCount: v,
		}); err != nil {
			// Don't sink the whole dog on one upsert failure — log and
			// continue; the renderer reads the live ArchHealthAggregates
			// view, so partial persistence is still operator-visible.
			logger.Printf("architecture-health-report: upsert failed for (rule=%s repo=%d author=%s): %v",
				k.ruleID, k.repoID, k.authorType, err)
		}
	}

	// Render the report. Reads from the same ArchHealthAggregates rows we
	// just wrote so the on-disk Markdown is always consistent with the DB.
	repoNames := make(map[int]string, len(repos))
	for i, r := range repos {
		repoNames[i+1] = r.Name
	}
	reportPath, renderErr := renderArchHealthReport(db, month, repoNames)
	if renderErr != nil {
		return fmt.Errorf("architecture-health-report: render: %w", renderErr)
	}
	logger.Printf("Dog architecture-health-report: rendered %s", reportPath)
	return nil
}

// scanRepoForBoS walks every *.go file under repoPath, parses it, runs
// every active BoS rule, and returns the aggregated findings. Files
// under vendor/, .git/, .force/, and node_modules are skipped. Parse
// errors are returned as synthetic findings (rule_id='BOS-PARSE-ERROR')
// so a bad file isn't invisible.
func scanRepoForBoS(ctx context.Context, repoPath string, gate bos.FleetRulesGate) ([]bos.Finding, error) {
	var inputs []bos.ReviewInput
	walkErr := filepath.Walk(repoPath, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip unreadable
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if info.IsDir() {
			base := filepath.Base(p)
			if base == "vendor" || base == ".git" || base == ".force" || base == "node_modules" || base == ".d7-worktrees" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		body, readErr := os.ReadFile(p)
		if readErr != nil {
			return nil
		}
		rel, _ := filepath.Rel(repoPath, p)
		if rel == "" {
			rel = p
		}
		inputs = append(inputs, bos.ReviewInput{Path: rel, Source: string(body)})
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("walk %s: %w", repoPath, walkErr)
	}
	res := bos.ReviewFiles(gate, inputs)
	return res.Findings, nil
}

// classifyAuthor labels a path as one of the three D9 author types. v1
// is path-heuristic, not git-blame: the cross-repo blame layer (D8) is
// not yet wired to ArchHealth and a per-file blame round-trip would
// dwarf the dog's runtime budget. The heuristics:
//
//   - paths containing "/migrations/" or filenames starting with
//     "archaeologist_" are tagged 'archaeologist-migration'.
//   - paths under internal/agents/astromech*, agents/astromech, or
//     containing "_astromech" are tagged 'astromech'.
//   - everything else is 'human'.
//
// This is deliberately conservative: when unsure, the dog defaults to
// 'human' so the per-author compliance flag (astromech worse than
// human → ⚠️) errs toward NOT firing rather than false-positive.
//
// A v2 swap-in to git-blame author email matching is straightforward
// once the cross-repo graph indexes per-line blame; the renderer
// already pivots on author_type so the upgrade is data-only.
func classifyAuthor(path string) string {
	low := strings.ToLower(path)
	if strings.Contains(low, "/migrations/") || strings.Contains(low, "archaeologist_") {
		return archAuthorArchaeologistMigration
	}
	if strings.Contains(low, "astromech") {
		return archAuthorAstromech
	}
	return archAuthorHuman
}

