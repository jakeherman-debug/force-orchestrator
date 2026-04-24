package store

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestAUDIT_Concurrency is the umbrella test for the AUDIT concurrency batch
// (045, 046, 047, 048, 092, 093, 096, 097). Each sub-test is a mostly-static
// grep against the concrete source files, asserting the smell is present as
// the audit described. When a finding is fixed, the relevant sub-test flips.
//
// These are STATIC / documentation-level tests by design — they pin the
// locations of the defects so future refactors cannot silently "solve"
// (re-open) the finding without flipping a red light in CI.
func TestAUDIT_Concurrency(t *testing.T) {
	// Fix #3 closed AUDIT-048 (outer umbrella skip removed). The remaining
	// sub-tests (045/046/047/092/093/096/097) each skip individually until
	// their respective fixes land (Fix #4 / Fix #8).
	repoRoot := findRepoRoot(t)

	t.Run("AUDIT_045_MaxOpenConns1_and_busy_timeout_DSN", func(t *testing.T) {
		// Without skip, fails with: AUDIT-045: expected post-Open
		// `db.Exec("PRAGMA busy_timeout=...")` uniformly applied across all DSNs
		// in internal/store/holocron.go — none found; fix not landed yet. Also:
		// :memory: DSN observed busy_timeout=0 (driver default, no fleet setting).
		path := filepath.Join(repoRoot, "internal/store/holocron.go")
		src := mustRead(t, path)

		// RGR INVERSION: assert the FIX is present. Today the fix is absent so
		// this sub-test fails without the skip, matching the RGR contract.
		// Fix shape: post-Open `db.Exec("PRAGMA busy_timeout=...")` so every DSN
		// (including :memory: tests) shares the uniform setting.
		if !regexp.MustCompile(`(?i)Exec\([^)]*PRAGMA\s+busy_timeout`).MatchString(src) {
			t.Errorf("AUDIT-045: expected post-Open `db.Exec(\"PRAGMA busy_timeout=...\")` "+
				"uniformly applied across all DSNs in %s — none found; fix not landed yet", path)
		}

		// RGR INVERSION empirical: when the fix lands, a :memory: DSN should
		// observe a fleet-set busy_timeout (> 0). The go-sqlite3 driver default
		// is 0, so today this fails without the skip.
		db := InitHolocronDSN(":memory:")
		defer db.Close()
		var busyTimeout int
		if err := db.QueryRow("PRAGMA busy_timeout;").Scan(&busyTimeout); err != nil {
			t.Fatalf("AUDIT-045: PRAGMA busy_timeout query failed: %v", err)
		}
		if busyTimeout <= 0 {
			t.Errorf("AUDIT-045: :memory: DSN observed busy_timeout=%d; expected "+
				"fleet code to set a positive value post-Open — fix not landed yet", busyTimeout)
		}
	})

	t.Run("AUDIT_046_global_mergeMu_not_per_repo", func(t *testing.T) {
		// Without skip, fails with: AUDIT-046: expected per-repo mergeMu
		// sharding (one of [map[string]*sync.Mutex mergeMuByRepo mergeMus]) in
		// internal/git/git.go — none found; fix not landed yet.
		path := filepath.Join(repoRoot, "internal/git/git.go")
		src := mustRead(t, path)

		// RGR INVERSION: assert per-repo sharding IS present. At least one of
		// these shapes must appear for the fix to be considered landed.
		expected := []string{
			"map[string]*sync.Mutex",
			"mergeMuByRepo",
			"mergeMus",
		}
		found := false
		for _, f := range expected {
			if strings.Contains(src, f) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("AUDIT-046: expected per-repo mergeMu sharding (one of %v) in %s — "+
				"none found; fix not landed yet", expected, path)
		}
	})

	t.Run("AUDIT_047_inquisitor_single_goroutine_blocking_loop", func(t *testing.T) {
		// Without skip, fails with: AUDIT-047: expected per-dog
		// context.WithTimeout in inquisitor.go — not found. Also: expected
		// heartbeat reference in inquisitor.go and Dogs DDL — not found.
		path := filepath.Join(repoRoot, "internal/agents/inquisitor.go")
		src := mustRead(t, path)

		// RGR INVERSION: assert the fix IS present.
		// Fix A: per-dog context.WithTimeout.
		if !strings.Contains(src, "context.WithTimeout") {
			t.Errorf("AUDIT-047: expected per-dog context.WithTimeout in %s — not found; "+
				"fix not landed yet", path)
		}
		// Fix B: inquisitor.go writes a heartbeat column.
		if !regexp.MustCompile(`(?i)heartbeat`).MatchString(src) {
			t.Errorf("AUDIT-047: expected heartbeat reference in %s — not found; "+
				"fix not landed yet", path)
		}
		// Fix C: Dogs DDL has heartbeat column.
		schemaPath := filepath.Join(repoRoot, "internal/store/schema.go")
		schemaSrc := mustRead(t, schemaPath)
		dogsDDL := regexp.MustCompile(`CREATE TABLE IF NOT EXISTS Dogs\s*\([^)]*\)`).FindString(schemaSrc)
		if dogsDDL == "" {
			t.Fatalf("AUDIT-047: could not locate Dogs DDL in %s", schemaPath)
		}
		if !regexp.MustCompile(`(?i)heartbeat`).MatchString(dogsDDL) {
			t.Errorf("AUDIT-047: expected heartbeat column in Dogs DDL — not found; "+
				"fix not landed yet")
		}
	})

	t.Run("AUDIT_048_pr_flow_tx_with_unindexed_LIKE", func(t *testing.T) {
		// Fix #3 closed AUDIT-048: onSubPRCIFailed no longer runs a payload-LIKE
		// dedup query inside the tx. The idempotency key
		// `ci-failure-triage:<sub_pr_row_id>` + idx_bounty_idem (partial UNIQUE)
		// provides atomic dedup via INSERT ... ON CONFLICT DO NOTHING in
		// QueueCIFailureTriageTx instead. Test now asserts the fix shape.
		path := filepath.Join(repoRoot, "internal/agents/pr_flow.go")
		src := mustRead(t, path)

		// Locate onSubPRCIFailed function body.
		idx := strings.Index(src, "func onSubPRCIFailed")
		if idx == -1 {
			t.Fatalf("AUDIT-048: could not find onSubPRCIFailed in %s", path)
		}
		end := idx + 4096
		if end > len(src) {
			end = len(src)
		}
		body := src[idx:end]

		// The payload-LIKE pattern inside a tx was the specific shape the
		// audit flagged — its return would reintroduce the single-connection
		// stall. Forbid it.
		if regexp.MustCompile(`tx\.QueryRow\([\s\S]*?payload LIKE`).MatchString(body) {
			t.Errorf("AUDIT-048 regression: found tx.QueryRow with `payload "+
				"LIKE` in onSubPRCIFailed in %s — idempotency-key dedup "+
				"(via QueueCIFailureTriageTx + idx_bounty_idem) should "+
				"replace it", path)
		}
	})

	t.Run("AUDIT_092_ExecRunner_no_Kill_backstop", func(t *testing.T) {
		// Without skip, fails with: AUDIT-092: expected time.After backstop
		// within 300 chars after cmd.Process.Kill() in internal/gh/gh.go —
		// not found; fix not landed yet.
		path := filepath.Join(repoRoot, "internal/gh/gh.go")
		src := mustRead(t, path)

		// RGR INVERSION: assert a Kill+drain backstop IS present — there must
		// be a time.After between Kill() and the final receive.
		killIdx := strings.Index(src, "cmd.Process.Kill()")
		if killIdx == -1 {
			t.Fatalf("AUDIT-092: could not locate cmd.Process.Kill() in %s", path)
		}
		window := src[killIdx:min(killIdx+300, len(src))]
		if !strings.Contains(window, "time.After") {
			t.Errorf("AUDIT-092: expected time.After backstop within 300 chars after "+
				"cmd.Process.Kill() in %s — not found; fix not landed yet", path)
		}
	})

	t.Run("AUDIT_093_claude_RunCLIStreaming_no_WaitDelay", func(t *testing.T) {
		// Without skip, fails with: AUDIT-093: expected cmd.WaitDelay in
		// RunCLIStreaming in internal/claude/claude.go — not found; fix not landed yet.
		path := filepath.Join(repoRoot, "internal/claude/claude.go")
		src := mustRead(t, path)

		// RGR INVERSION: assert cmd.WaitDelay IS set (fix landed).
		if !strings.Contains(src, "WaitDelay") {
			t.Errorf("AUDIT-093: expected cmd.WaitDelay in RunCLIStreaming in %s — "+
				"not found; fix not landed yet", path)
		}
	})

	t.Run("AUDIT_096_rateLimitRetries_non_atomic_and_no_prune", func(t *testing.T) {
		// Without skip, fails with: AUDIT-096: expected atomic update helper
		// (CompareAndSwap or LoadOrStore) on rateLimitRetries — not found.
		// Also: expected rateLimitRetries.Range-driven prune — not found.
		path := filepath.Join(repoRoot, "internal/agents/astromech.go")
		src := mustRead(t, path)

		// RGR INVERSION: assert atomic update + prune loop are present.
		hasAtomic := strings.Contains(src, "rateLimitRetries.CompareAndSwap") ||
			strings.Contains(src, "rateLimitRetries.LoadOrStore")
		if !hasAtomic {
			t.Errorf("AUDIT-096: expected atomic update helper (CompareAndSwap or "+
				"LoadOrStore) on rateLimitRetries in %s — not found; fix not landed yet", path)
		}
		if !strings.Contains(src, "rateLimitRetries.Range") {
			t.Errorf("AUDIT-096: expected rateLimitRetries.Range-driven prune in %s — "+
				"not found; fix not landed yet", path)
		}
	})

	t.Run("AUDIT_097_ResetBranchPrefixCache_unsafe_Once_swap", func(t *testing.T) {
		// Without skip, fails with: AUDIT-097: ResetBranchPrefixCache still
		// reassigns `usernameOnce = sync.Once{}` in internal/git/username.go —
		// fix not landed yet (sync.Once is not safe to reassign under its own guard).
		path := filepath.Join(repoRoot, "internal/git/username.go")
		src := mustRead(t, path)

		// RGR INVERSION: assert the unsafe sync.Once reassignment pattern has
		// been removed (fixed) — either the whole function is gone, or it no
		// longer reassigns usernameOnce.
		funcRE := regexp.MustCompile(`func ResetBranchPrefixCache\(\)\s*\{[\s\S]*?\n\}`)
		m := funcRE.FindString(src)
		if m != "" && strings.Contains(m, "usernameOnce = sync.Once{}") {
			t.Errorf("AUDIT-097: ResetBranchPrefixCache still reassigns `usernameOnce = "+
				"sync.Once{}` in %s — fix not landed yet (sync.Once is not safe to "+
				"reassign under its own guard)", path)
		}
	})
}

// ── helpers ────────────────────────────────────────────────────────────────

func findRepoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	// Walk upward until we find a directory containing go.mod.
	dir := wd
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("could not locate repo root (go.mod) upward from %s", wd)
	return ""
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
