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
	repoRoot := findRepoRoot(t)

	t.Run("AUDIT_045_MaxOpenConns1_and_busy_timeout_DSN", func(t *testing.T) {
		path := filepath.Join(repoRoot, "internal/store/holocron.go")
		src := mustRead(t, path)

		// (a) SetMaxOpenConns(1) is still the global serialization point.
		if !strings.Contains(src, "SetMaxOpenConns(1)") {
			t.Errorf("AUDIT-045: expected SetMaxOpenConns(1) in %s "+
				"(finding predicated on that serialization point still existing)", path)
		}

		// (b) _busy_timeout=5000 lives only in the production DSN query string,
		// so :memory: test DSNs skip it.
		if !strings.Contains(src, "_busy_timeout=5000") {
			t.Errorf("AUDIT-045: expected _busy_timeout=5000 to be baked into the production DSN in %s", path)
		}

		// (c) Pragmas are NOT applied via a post-Open `db.Exec("PRAGMA busy_timeout=...")`
		// loop that every DSN would share. The only post-Open Exec is the journal_mode one.
		// Assert no explicit `busy_timeout` Exec exists.
		if regexp.MustCompile(`(?i)Exec\([^)]*PRAGMA\s+busy_timeout`).MatchString(src) {
			t.Errorf("AUDIT-045: finding says PRAGMA busy_timeout is NOT applied post-Open; "+
				"found an apparent fix in %s — re-check finding", path)
		}

		// (d) Demonstrate empirically that InitHolocronDSN does not PRAGMA-assign
		// busy_timeout for the :memory: DSN path. We open a :memory: DSN, inspect
		// the pragma, and assert we did NOT see an explicit PRAGMA busy_timeout
		// call in the source (the only way a uniform setting would be applied).
		// Note: go-sqlite3 may set its own per-connection default — the finding
		// is about fleet code not normalising it, not about the eventual value.
		db := InitHolocronDSN(":memory:")
		defer db.Close()
		var busyTimeout int
		if err := db.QueryRow("PRAGMA busy_timeout;").Scan(&busyTimeout); err != nil {
			t.Fatalf("AUDIT-045: PRAGMA busy_timeout query failed: %v", err)
		}
		t.Logf("AUDIT-045: :memory: DSN observed busy_timeout=%d (source has no post-Open PRAGMA busy_timeout — value comes from driver default, not fleet code)", busyTimeout)
	})

	t.Run("AUDIT_046_global_mergeMu_not_per_repo", func(t *testing.T) {
		path := filepath.Join(repoRoot, "internal/git/git.go")
		src := mustRead(t, path)

		// Global declaration still present.
		if !regexp.MustCompile(`var\s+mergeMu\s+sync\.Mutex`).MatchString(src) {
			t.Errorf("AUDIT-046: expected global `var mergeMu sync.Mutex` in %s", path)
		}

		// No per-repo sharding construct has been introduced. We look for the
		// typical shapes the fix would take.
		forbidden := []string{
			"sync.Map", // a sync.Map of repo -> mutex
			"map[string]*sync.Mutex",
			"mergeMuByRepo",
			"mergeMus",
		}
		for _, f := range forbidden {
			if strings.Contains(src, f) {
				t.Errorf("AUDIT-046: found %q in %s — looks like per-repo sharding was added; "+
					"finding may be fixed, review", f, path)
			}
		}

		// MergeAndCleanup still Locks the single global mutex.
		if !regexp.MustCompile(`func\s+MergeAndCleanup[^{]*{\s*\n\s*mergeMu\.Lock\(\)`).MatchString(src) {
			t.Errorf("AUDIT-046: expected MergeAndCleanup to still Lock the global mergeMu in %s", path)
		}
	})

	t.Run("AUDIT_047_inquisitor_single_goroutine_blocking_loop", func(t *testing.T) {
		path := filepath.Join(repoRoot, "internal/agents/inquisitor.go")
		src := mustRead(t, path)

		// Per-dog context.WithTimeout — the fix — must still be ABSENT.
		if strings.Contains(src, "context.WithTimeout") {
			t.Errorf("AUDIT-047: found context.WithTimeout in %s — per-dog timeout looks implemented; review finding", path)
		}

		// The Inquisitor loop is still the plain time.Sleep(inquisitorInterval)
		// blocking form rather than a ticker with per-iteration deadline.
		if !strings.Contains(src, "time.Sleep(inquisitorInterval)") {
			t.Errorf("AUDIT-047: expected blocking time.Sleep(inquisitorInterval) in %s", path)
		}

		// No Dogs-table heartbeat row update: scan for an UPDATE/INSERT writing
		// a heartbeat column. The finding says heartbeat is ABSENT.
		if regexp.MustCompile(`(?i)heartbeat`).MatchString(src) {
			t.Errorf("AUDIT-047: found 'heartbeat' in %s — finding may be fixed, review", path)
		}

		// Dogs schema itself lacks a heartbeat column — cross-check schema.go.
		schemaPath := filepath.Join(repoRoot, "internal/store/schema.go")
		schemaSrc := mustRead(t, schemaPath)
		dogsDDL := regexp.MustCompile(`CREATE TABLE IF NOT EXISTS Dogs\s*\([^)]*\)`).FindString(schemaSrc)
		if dogsDDL == "" {
			t.Fatalf("AUDIT-047: could not locate Dogs DDL in %s", schemaPath)
		}
		if regexp.MustCompile(`(?i)heartbeat`).MatchString(dogsDDL) {
			t.Errorf("AUDIT-047: Dogs DDL now has heartbeat column — finding may be fixed, review")
		}
	})

	t.Run("AUDIT_048_pr_flow_tx_with_unindexed_LIKE", func(t *testing.T) {
		path := filepath.Join(repoRoot, "internal/agents/pr_flow.go")
		src := mustRead(t, path)

		// Locate onSubPRCIFailed function body.
		idx := strings.Index(src, "func onSubPRCIFailed")
		if idx == -1 {
			t.Fatalf("AUDIT-048: could not find onSubPRCIFailed in %s", path)
		}
		// Look at the next ~4KB which should span its whole body.
		end := idx + 4096
		if end > len(src) {
			end = len(src)
		}
		body := src[idx:end]

		// The tx is still opened and the dedup SELECT with payload LIKE runs inside it.
		if !strings.Contains(body, "db.Begin()") {
			t.Errorf("AUDIT-048: expected db.Begin() in onSubPRCIFailed in %s", path)
		}
		if !regexp.MustCompile(`tx\.QueryRow\([\s\S]*?payload LIKE`).MatchString(body) {
			t.Errorf("AUDIT-048: expected tx.QueryRow with `payload LIKE` inside onSubPRCIFailed in %s "+
				"(finding is that the unindexed LIKE runs inside the tx and pins the single connection)", path)
		}
	})

	t.Run("AUDIT_092_ExecRunner_no_Kill_backstop", func(t *testing.T) {
		path := filepath.Join(repoRoot, "internal/gh/gh.go")
		src := mustRead(t, path)

		// Locate the timeout select.
		// The pattern is:
		//   case <-time.After(timeout):
		//       _ = cmd.Process.Kill()
		//       <-done          <-- no backstop
		//       return ...
		re := regexp.MustCompile(`case <-time\.After\(timeout\):[\s\S]*?cmd\.Process\.Kill\(\)[\s\S]*?<-done`)
		if !re.MatchString(src) {
			t.Errorf("AUDIT-092: expected raw `<-done` after cmd.Process.Kill() in ExecRunner timeout path in %s", path)
		}

		// Between Kill() and the `<-done` receive, there must NOT be a second
		// time.After / select — that would be the backstop the finding calls for.
		killIdx := strings.Index(src, "cmd.Process.Kill()")
		if killIdx == -1 {
			t.Fatalf("AUDIT-092: could not locate cmd.Process.Kill() in %s", path)
		}
		// Look at the 300 chars immediately after Kill.
		window := src[killIdx:min(killIdx+300, len(src))]
		if strings.Contains(window, "time.After") {
			t.Errorf("AUDIT-092: found time.After near Kill() in %s — backstop may be implemented; review finding", path)
		}
	})

	t.Run("AUDIT_093_claude_RunCLIStreaming_no_WaitDelay", func(t *testing.T) {
		path := filepath.Join(repoRoot, "internal/claude/claude.go")
		src := mustRead(t, path)

		if strings.Contains(src, "WaitDelay") {
			t.Errorf("AUDIT-093: found cmd.WaitDelay in %s — finding may be fixed, review", path)
		}

		// Confirm the streaming function and its exec.CommandContext still exist
		// so the finding is pointing at real code.
		if !strings.Contains(src, "func RunCLIStreaming") {
			t.Errorf("AUDIT-093: expected RunCLIStreaming function in %s", path)
		}
		if !strings.Contains(src, `exec.CommandContext(ctx, "claude"`) {
			t.Errorf("AUDIT-093: expected exec.CommandContext(ctx, \"claude\", ...) in %s", path)
		}
	})

	t.Run("AUDIT_096_rateLimitRetries_non_atomic_and_no_prune", func(t *testing.T) {
		path := filepath.Join(repoRoot, "internal/agents/astromech.go")
		src := mustRead(t, path)

		// sync.Map still the underlying type.
		if !regexp.MustCompile(`var\s+rateLimitRetries\s+sync\.Map`).MatchString(src) {
			t.Errorf("AUDIT-096: expected `var rateLimitRetries sync.Map` in %s", path)
		}

		// Non-atomic Load+Store pair. We look for the .Load(name) followed by
		// an unguarded .Store(name, ...).
		re := regexp.MustCompile(`rateLimitRetries\.Load\(name\)[\s\S]{0,300}?rateLimitRetries\.Store\(name,\s*rlCount\+1\)`)
		if !re.MatchString(src) {
			t.Errorf("AUDIT-096: expected non-atomic Load-then-Store on rateLimitRetries in %s", path)
		}

		// No compare-and-swap / LoadOrStore-based atomic increment in place.
		if strings.Contains(src, "rateLimitRetries.CompareAndSwap") ||
			strings.Contains(src, "rateLimitRetries.LoadOrStore") {
			t.Errorf("AUDIT-096: found atomic update helper on rateLimitRetries in %s — finding may be fixed, review", path)
		}

		// No prune-all step anywhere. A fix to AUDIT-096 would introduce a
		// sync.Map.Range that iterates and deletes retired agent names (driven
		// by the Inquisitor tick). A bare single-key Delete on success (like
		// the existing rateLimitRetries.Delete(name) on successful completion)
		// is NOT a prune — it only evicts the current agent's entry when that
		// agent succeeds, which does nothing for retired / renamed agents.
		if strings.Contains(src, "rateLimitRetries.Range") {
			t.Errorf("AUDIT-096: found rateLimitRetries.Range in %s — prune loop may be implemented; review finding", path)
		}
	})

	t.Run("AUDIT_097_ResetBranchPrefixCache_unsafe_Once_swap", func(t *testing.T) {
		path := filepath.Join(repoRoot, "internal/git/username.go")
		src := mustRead(t, path)

		// The function exists and reassigns sync.Once while only holding usernameMu.
		// sync.Once is not designed to be reassigned under its own guard — reassignment
		// while another goroutine is inside .Do (which doesn't take usernameMu!) is the
		// specific race.
		funcRE := regexp.MustCompile(`func ResetBranchPrefixCache\(\)\s*\{[\s\S]*?\n\}`)
		m := funcRE.FindString(src)
		if m == "" {
			t.Fatalf("AUDIT-097: could not locate ResetBranchPrefixCache in %s", path)
		}
		if !strings.Contains(m, "usernameMu.Lock()") {
			t.Errorf("AUDIT-097: expected usernameMu.Lock() in ResetBranchPrefixCache (finding cites it as insufficient) in %s", path)
		}
		if !strings.Contains(m, "usernameOnce = sync.Once{}") {
			t.Errorf("AUDIT-097: expected `usernameOnce = sync.Once{}` reassignment in ResetBranchPrefixCache in %s", path)
		}

		// Verify the production caller (BranchPrefix) calls usernameOnce.Do()
		// WITHOUT holding usernameMu at the moment of the call. BranchPrefix
		// currently takes the mutex only for the branchPrefixOverride snapshot,
		// then Unlocks before calling Do. That is the race surface: a concurrent
		// ResetBranchPrefixCache holding the mutex can reassign usernameOnce
		// while another goroutine is inside .Do().
		//
		// We assert the preamble before usernameOnce.Do() contains
		// `usernameMu.Unlock()` with no matching Lock after it — meaning the
		// mutex is NOT held at the Do() call.
		doSite := regexp.MustCompile(`usernameOnce\.Do\(`).FindStringIndex(src)
		if doSite == nil {
			t.Fatalf("AUDIT-097: could not locate usernameOnce.Do in %s", path)
		}
		// Look at the 400 chars before the Do call.
		start := doSite[0] - 400
		if start < 0 {
			start = 0
		}
		preamble := src[start:doSite[0]]
		// Count Lock/Unlock pairs: if Lock > Unlock, mutex is held; otherwise not.
		locks := strings.Count(preamble, "usernameMu.Lock()")
		unlocks := strings.Count(preamble, "usernameMu.Unlock()")
		if locks > unlocks {
			t.Errorf("AUDIT-097: usernameOnce.Do() now appears to be called while holding usernameMu in %s "+
				"(locks=%d unlocks=%d in preamble) — finding may be fixed, review", path, locks, unlocks)
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
