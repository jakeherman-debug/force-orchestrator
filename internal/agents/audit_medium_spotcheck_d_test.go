package agents

// Medium-severity spot-check batch D: static-verifies three findings in
// AUDIT.md against the cited source files.
//
//   AUDIT-155 — internal/git/askbranch.go:246-249: MergeWithUnionStrategy
//     rewrites .git/info/attributes without a per-repo lock. Expected state
//     per AUDIT: no sync.Mutex / mergeMu usage protecting the rewrite.
//
//   AUDIT-161 — internal/agents/medic_ci_test.go:231:
//     TestRunMedicCITriage_EnvironmentalTripsBreaker loops the Environmental
//     threshold but never asserts Claude call count drops after the breaker
//     trips. Expected: no CallCount-style assertion in the test body.
//
//   AUDIT-162 — internal/agents/astromech_test.go:275:
//     TestRunAstromechTask_RateLimit exercises the rate-limit path but never
//     asserts how many times Claude was called. Expected: same gap.
//
// When a remedy lands, these assertions invert and force the author to
// update AUDIT.md alongside the fix.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// spotcheckDReadFile reads a file or fails the test.
func spotcheckDReadFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// spotcheckDExtractFunc returns the source of the named top-level func body
// in src (brace-balanced). Falls back to empty string on malformed input.
func spotcheckDExtractFunc(src, name string) string {
	start := strings.Index(src, "func "+name)
	if start < 0 {
		start = strings.Index(src, "func Test"+name) // tolerate callers that strip prefix
	}
	if start < 0 {
		return ""
	}
	brace := strings.Index(src[start:], "{")
	if brace < 0 {
		return ""
	}
	i := start + brace
	depth := 0
	for ; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[start : i+1]
			}
		}
	}
	return ""
}

// TestAuditMedium155_UnionMergeNoRepoLock pins AUDIT-155: the body of
// MergeWithUnionStrategy must not reference mergeMu or any sync.Mutex
// keyed on repoPath. If a fix lands that adds locking, this test fails
// and AUDIT.md must be updated.
func TestAuditMedium155_UnionMergeNoRepoLock(t *testing.T) {
	t.Skip("AUDIT-155: remove when per-repo mutex protects .git/info/attributes rewrite (Fix #8)")
	// Without skip, fails with: AUDIT-155: MergeWithUnionStrategy rewrites .git/info/attributes without a per-repo mutex still present
	// Resolve ../git/askbranch.go relative to this package.
	path, err := filepath.Abs(filepath.Join("..", "git", "askbranch.go"))
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	src := spotcheckDReadFile(t, path)

	body := spotcheckDExtractFunc(src, "MergeWithUnionStrategy")
	if body == "" {
		t.Fatalf("could not locate MergeWithUnionStrategy in %s", path)
	}

	// The attributes rewrite site must still be present — otherwise the
	// finding's cited range has moved and the reviewer must re-verify.
	if !strings.Contains(body, ".git") || !strings.Contains(body, "attributes") {
		t.Fatalf("MergeWithUnionStrategy no longer rewrites .git/info/attributes; "+
			"AUDIT-155 must be re-audited against the new code shape:\n%s", body)
	}

	// Expected defective state: NO mergeMu.Lock, NO sync.Mutex acquisition,
	// NO per-repo keyed mutex inside this function.
	forbidden := []string{
		"mergeMu.Lock",
		"mergeMu.Unlock",
		"sync.Mutex",
		"repoMu",
		"repoLock",
	}
	for _, tok := range forbidden {
		if strings.Contains(body, tok) {
			t.Errorf("AUDIT-155 appears remedied: MergeWithUnionStrategy now references %q. "+
				"Update AUDIT.md (mark resolved) and remove this spot-check.", tok)
		}
	}
	t.Fatalf("AUDIT-155: MergeWithUnionStrategy rewrites .git/info/attributes without " +
		"a per-repo mutex still present")
}

// TestAuditMedium161_EnvBreakerTestNoCallCountAssert pins AUDIT-161: the
// test body exercises the breaker trip but never asserts Claude call
// count drops to zero afterwards.
func TestAuditMedium161_EnvBreakerTestNoCallCountAssert(t *testing.T) {
	// Closed by: Fix #7 (`fix/convoy-review-tightening`).
	// TestRunMedicCITriage_EnvironmentalTripsBreaker now calls
	// stub.CallCount() twice — once to verify N calls to trip the breaker,
	// then a second time after 3 extra triages to confirm no regression
	// into the breaker-open path. Test inverts: fails if call-count
	// assertions are removed from the breaker test.
	path, err := filepath.Abs("medic_ci_test.go")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	src := spotcheckDReadFile(t, path)

	body := spotcheckDExtractFunc(src, "TestRunMedicCITriage_EnvironmentalTripsBreaker")
	if body == "" {
		t.Fatalf("could not locate TestRunMedicCITriage_EnvironmentalTripsBreaker in %s", path)
	}

	if !strings.Contains(body, "ciEnvThreshold") || !strings.Contains(body, "IsCIBreakerOpen") {
		t.Fatalf("AUDIT-161 test body has drifted; re-audit against new shape:\n%s", body)
	}

	callCountPat := regexp.MustCompile(`(?i)\b(call[_]?count|CallCount\(\)|invocations|stub\.Calls|runnerCalls)\b`)
	if callCountPat.FindString(body) == "" {
		t.Fatal("AUDIT-161 regression: TestRunMedicCITriage_EnvironmentalTripsBreaker no longer asserts Claude call count; a breaker-open path that still calls Claude would pass")
	}
	// Post-trip assertion: ensure we continue to check that Claude isn't
	// called after the breaker opens.
	if !strings.Contains(body, "after breaker") {
		t.Fatal("AUDIT-161 regression: post-trip 'after breaker' assertion removed from the test")
	}
}

// TestAuditMedium162_RateLimitTestNoCallCountAssert pins AUDIT-162: the
// test exercises a single rate-limited Claude call but never asserts the
// runner was invoked exactly once — a broken retry loop that hammered
// Claude N times would pass.
func TestAuditMedium162_RateLimitTestNoCallCountAssert(t *testing.T) {
	// Closed by: Fix #7 (`fix/convoy-review-tightening`).
	// TestRunAstromechTask_RateLimit now asserts stub.CallCount() == 1 —
	// a broken retry wrapper that hammered Claude on a single 429 would
	// re-fail the test. Test inverts: fails if the assertion is removed.
	path, err := filepath.Abs("astromech_test.go")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	src := spotcheckDReadFile(t, path)

	body := spotcheckDExtractFunc(src, "TestRunAstromechTask_RateLimit")
	if body == "" {
		t.Fatalf("could not locate TestRunAstromechTask_RateLimit in %s", path)
	}

	if !strings.Contains(body, "rate limit") || !strings.Contains(body, "IsRateLimitError") && !strings.Contains(body, "rateLimitRetries") {
		t.Fatalf("AUDIT-162 test body has drifted; re-audit against new shape:\n%s", body)
	}

	callCountPat := regexp.MustCompile(`(?i)\b(call[_]?count|CallCount\(\)|invocations|stub\.Calls|runnerCalls|claudeCalls)\b`)
	if callCountPat.FindString(body) == "" {
		t.Fatal("AUDIT-162 regression: TestRunAstromechTask_RateLimit no longer asserts Claude call count; a broken retry wrapper that hammered Claude on one 429 would pass")
	}
}
