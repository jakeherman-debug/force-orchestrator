package agents

// Test-quality meta-findings from AUDIT.md — AUDIT-111, -112, -113, -133,
// -135, -136, -137, -138.
//
// These are *static* findings: the suite lacks assertions of a particular
// kind. We can't dynamically "trigger" them — instead we AST/grep across the
// existing test corpus and assert the absence (or presence, where P2 has
// since closed the gap) of particular tokens. When a remedy lands, these
// assertions invert and force the author to delete or update this file.
//
// The tests here deliberately do NOT mutate source. They scan files under
// internal/agents/ and internal/store/ for specific patterns and fail if
// the defective state is still present.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// readFile reads a file or fails the test. Keeps the test focused on
// pattern-matching, not IO plumbing.
func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// walkTestFiles returns every *_test.go file under dir (non-recursive for
// internal/agents which is flat). Used by the CallCount sweep.
func walkTestFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir %s: %v", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), "_test.go") {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}

func TestAuditTestQualityMetaFindings(t *testing.T) {
	agentsDir := "."
	storeDir := filepath.Join("..", "store")

	// ── AUDIT-111 — no CallCount / invocations counter ──────────────────────
	t.Run("AUDIT_111_NoClaudeCallCountAsserted", func(t *testing.T) {
		files := walkTestFiles(t, agentsDir)
		if len(files) == 0 {
			t.Fatal("no test files found under internal/agents")
		}
		// Ignore this self-referential file — it contains the literal
		// substrings we're searching for.
		self, _ := filepath.Abs("audit_test_quality_test.go")

		// Look for *real* counter usage, not coincidental substrings in log
		// messages. A genuine CallCount counter shows up as one of:
		//   stub.CallCount, .CallCount(), CallCount ==, CallCount !=,
		//   atomic.AddInt... involving a CallCount/invocations name.
		// Bare "invocations" appearing inside a t.Logf is not a remedy.
		patterns := []*regexp.Regexp{
			regexp.MustCompile(`\bstub\.CallCount\b`),
			regexp.MustCompile(`\.CallCount\s*\(`),
			regexp.MustCompile(`\bCallCount\s*[=!<>]=`),
			regexp.MustCompile(`\bCallCount\s*\+\+`),
			regexp.MustCompile(`atomic\.Add\w*\([^)]*[Cc]allCount`),
			regexp.MustCompile(`atomic\.Add\w*\([^)]*[Ii]nvocations`),
			regexp.MustCompile(`\binvocations\s*\+\+`),
			regexp.MustCompile(`\binvocations\s*[=!<>]=`),
		}
		var hits []string
		for _, f := range files {
			abs, _ := filepath.Abs(f)
			if abs == self {
				continue
			}
			body := readFile(t, f)
			for _, rx := range patterns {
				if rx.MatchString(body) {
					hits = append(hits, f+" ("+rx.String()+")")
					break
				}
			}
		}
		if len(hits) > 0 {
			t.Errorf("AUDIT-111 REMEDY DETECTED: real CallCount/invocations counter in %v — "+
				"update this test to expect the counter instead of its absence", hits)
		}

		// Confirm withStubCLIRunner factory has no CallCount counter field.
		helpers := readFile(t, filepath.Join(agentsDir, "testhelpers_test.go"))
		if !strings.Contains(helpers, "func withStubCLIRunner") {
			t.Fatal("withStubCLIRunner factory not in testhelpers_test.go — factory moved?")
		}
		// Extract just the function body (up to the next top-level func).
		idx := strings.Index(helpers, "func withStubCLIRunner")
		body := helpers[idx:]
		if end := strings.Index(body[1:], "\nfunc "); end > 0 {
			body = body[:end]
		}
		if strings.Contains(body, "CallCount") || strings.Contains(body, "atomic.AddInt") {
			t.Errorf("AUDIT-111 REMEDY: withStubCLIRunner now has a counter; update assertion")
		}
		t.Logf("AUDIT-111 REPRODUCED: no CallCount/invocations counter anywhere in %d test files; "+
			"withStubCLIRunner stub returns canned output with no counter. Runaway Claude loops pass silently.",
			len(files))
	})

	// ── AUDIT-112 — TOCTOU concurrency — now pattern-covered by P2 ──────────
	t.Run("AUDIT_112_ConcurrentIdempotencyTest_DuplicateOfP2", func(t *testing.T) {
		idem := readFile(t, filepath.Join(storeDir, "tasks_idempotent_test.go"))
		if strings.Contains(idem, "sync.WaitGroup") || strings.Contains(idem, "go func") {
			t.Log("AUDIT-112: tasks_idempotent_test.go now has concurrency coverage locally")
		} else {
			t.Log("AUDIT-112 REPRODUCED-STATIC: tasks_idempotent_test.go has no sync.WaitGroup or `go func` block")
		}
		// But P2's audit_pattern_p2_test.go covers the same helper concurrently.
		p2Path := filepath.Join(storeDir, "audit_pattern_p2_test.go")
		if _, err := os.Stat(p2Path); err != nil {
			t.Fatalf("AUDIT-112 expected pattern-cover at %s, file missing: %v", p2Path, err)
		}
		p2 := readFile(t, p2Path)
		needs := []string{"sync.WaitGroup", "AddConvoyTaskIdempotent", "goroutines", "TestPattern_P2_IdempotencyKeyRace"}
		for _, n := range needs {
			if !strings.Contains(p2, n) {
				t.Errorf("AUDIT-112 DUPLICATE-OF-P2 expected %q in audit_pattern_p2_test.go, missing", n)
			}
		}
		t.Log("AUDIT-112 DUPLICATE-OF-P2: 50-goroutine race on AddConvoyTaskIdempotent covered by " +
			"TestPattern_P2_IdempotencyKeyRace. Finding is pattern-covered; canonical location is the P2 test.")
	})

	// ── AUDIT-113 — no total Claude call bound across ConvoyReview passes ──
	t.Run("AUDIT_113_NoBoundedTotalClaudeCallsTest", func(t *testing.T) {
		raw := readFile(t, filepath.Join(agentsDir, "convoy_review_test.go"))
		if strings.Contains(raw, "TotalClaudeCalls") ||
			strings.Contains(raw, "convoyReviewMaxTotalCalls") ||
			strings.Contains(raw, "TestConvoyReview_TotalClaudeCallsBounded") {
			t.Errorf("AUDIT-113 REMEDY DETECTED: total-call bound assertion exists; update this test")
		}
		// Also: current tests do not sum CallCount across passes because
		// CallCount does not exist. Belt-and-suspenders: grep for any sum
		// counter tokens.
		if strings.Contains(raw, "totalCalls") || strings.Contains(raw, "callsAcross") {
			t.Errorf("AUDIT-113 REMEDY DETECTED: running total counter present")
		}
		t.Log("AUDIT-113 REPRODUCED: convoy_review_test.go has no cross-pass Claude call bound. " +
			"5 passes × 5 findings × N retries product is unverified.")
	})

	// ── AUDIT-133 — no retry_count preservation across auto-complete ────────
	t.Run("AUDIT_133_RetryCountPreservation_Untested", func(t *testing.T) {
		raw := readFile(t, filepath.Join(agentsDir, "medic_recovery_test.go"))
		// Assert the auto-complete test (line ~54) does NOT touch retry_count.
		if !strings.Contains(raw, "TestAutoCompletedMedicTask_BranchHasNoDiff") {
			t.Fatal("expected TestAutoCompletedMedicTask_BranchHasNoDiff in medic_recovery_test.go")
		}
		if strings.Contains(raw, "retry_count") {
			t.Errorf("AUDIT-133 REMEDY DETECTED: retry_count referenced in medic_recovery_test.go — " +
				"update this test to inspect pre/post assertions")
		}
		// Also confirm no sibling test in internal/store covers ResetTaskFull's
		// counter preservation.
		storeFiles, err := os.ReadDir(storeDir)
		if err != nil {
			t.Fatalf("readdir store: %v", err)
		}
		found := false
		for _, e := range storeFiles {
			if !strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			body := readFile(t, filepath.Join(storeDir, e.Name()))
			if strings.Contains(body, "TestResetTaskFull_PreservesRetryCount") {
				found = true
				break
			}
		}
		if found {
			t.Errorf("AUDIT-133 REMEDY DETECTED: TestResetTaskFull_PreservesRetryCount exists")
		}
		t.Log("AUDIT-133 REPRODUCED: neither medic_recovery_test.go nor internal/store/*_test.go " +
			"seeds retry_count=N then asserts preservation. AUDIT-005 regression is uncovered.")
	})

	// ── AUDIT-135 — stub LLM never asserts prompt structure ─────────────────
	t.Run("AUDIT_135_StubDoesNotAssertPromptStructure", func(t *testing.T) {
		// stubConvoyReviewLLM is defined in convoy_review_test.go near line 19.
		raw := readFile(t, filepath.Join(agentsDir, "convoy_review_test.go"))
		if !strings.Contains(raw, "func stubConvoyReviewLLM") {
			t.Fatal("stubConvoyReviewLLM helper moved; update this test")
		}
		// Body of the helper — between the signature and the next blank
		// closing brace at indent 0.
		idx := strings.Index(raw, "func stubConvoyReviewLLM")
		rest := raw[idx:]
		if end := strings.Index(rest, "\n}\n"); end > 0 {
			rest = rest[:end]
		}
		// It must only forward to withStubCLIRunner — no prompt capture.
		for _, forbidden := range []string{"capturedPrompt", "lastPrompt", "assertPrompt", "requirePrompt"} {
			if strings.Contains(rest, forbidden) {
				t.Errorf("AUDIT-135 REMEDY DETECTED: stubConvoyReviewLLM has %q — remedy landed",
					forbidden)
			}
		}
		// And: the testhelpers stub is a 3-line forward with no capture.
		stub := readFile(t, filepath.Join(agentsDir, "testhelpers_test.go"))
		for _, forbidden := range []string{"capturedPrompt", "lastPrompt", "PromptHistory"} {
			if strings.Contains(stub, forbidden) {
				t.Errorf("AUDIT-135 REMEDY DETECTED: withStubCLIRunner captures prompt (%q)", forbidden)
			}
		}
		t.Log("AUDIT-135 REPRODUCED: stubConvoyReviewLLM is a 3-line json.Marshal→withStubCLIRunner " +
			"forward; neither helper captures or inspects the prompt. summarizeConvoyTasks returning " +
			"\"\" would still pass every test.")
	})

	// ── AUDIT-136 — ConvoyReview JSON parse-retry untested ──────────────────
	t.Run("AUDIT_136_ParseFailureRetryPath_Untested", func(t *testing.T) {
		path := filepath.Join(agentsDir, "convoy_review_test.go")
		// Parse via go/ast so we inspect actual test function names, not
		// comments mentioning the phrase.
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		var offenders []string
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			name := fn.Name.Name
			if !strings.HasPrefix(name, "Test") {
				continue
			}
			lower := strings.ToLower(name)
			if strings.Contains(lower, "parsefail") ||
				strings.Contains(lower, "parse_failure") ||
				strings.Contains(lower, "retryonce") ||
				strings.Contains(lower, "parseretry") ||
				strings.Contains(lower, "malformedjson") {
				offenders = append(offenders, name)
			}
		}
		if len(offenders) > 0 {
			t.Errorf("AUDIT-136 REMEDY DETECTED: parse-retry tests exist: %v — update this assertion", offenders)
		}
		t.Log("AUDIT-136 REPRODUCED: no test covers CLAUDE.md's \"one retry with critic note, " +
			"second failure → mark Completed\" parse-retry contract for ConvoyReview.")
	})

	// ── AUDIT-137 — TestEscalateSubPR second-call block has no assertion ──
	t.Run("AUDIT_137_SecondCallBlockLacksAssertion", func(t *testing.T) {
		path := filepath.Join(agentsDir, "pr_flow_test.go")
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		var escFn *ast.FuncDecl
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if ok && fn.Name.Name == "TestEscalateSubPR_IsAtomic" {
				escFn = fn
				break
			}
		}
		if escFn == nil {
			t.Fatal("TestEscalateSubPR_IsAtomic missing; AUDIT-137 may have been refactored")
		}
		// Find the second `_ = escalateSubPR(...)` call inside the function.
		var secondCallPos token.Pos
		seen := 0
		ast.Inspect(escFn.Body, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for _, rhs := range as.Rhs {
				call, ok := rhs.(*ast.CallExpr)
				if !ok {
					continue
				}
				id, ok := call.Fun.(*ast.Ident)
				if !ok || id.Name != "escalateSubPR" {
					continue
				}
				seen++
				if seen == 2 {
					secondCallPos = as.Pos()
				}
			}
			return true
		})
		if secondCallPos == 0 {
			t.Fatal("second escalateSubPR call not found; AUDIT-137 already remedied?")
		}
		// Walk the statements after the second call. Any *ast.IfStmt whose
		// body calls t.Errorf/t.Fatalf would be a real assertion. An IfStmt
		// with an empty body + a comment is the defect.
		foundAssertion := false
		for _, stmt := range escFn.Body.List {
			if stmt.Pos() < secondCallPos {
				continue
			}
			ifs, ok := stmt.(*ast.IfStmt)
			if !ok {
				continue
			}
			if ifs.Body == nil || len(ifs.Body.List) == 0 {
				continue // the defective empty-body if
			}
			ast.Inspect(ifs.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
					if sel.Sel.Name == "Errorf" || sel.Sel.Name == "Fatalf" ||
						sel.Sel.Name == "Error" || sel.Sel.Name == "Fatal" {
						foundAssertion = true
						return false
					}
				}
				return true
			})
		}
		if foundAssertion {
			t.Errorf("AUDIT-137 REMEDY DETECTED: second-call block now contains t.Error*/t.Fatal* — update this test")
		}
		t.Log("AUDIT-137 REPRODUCED: TestEscalateSubPR_IsAtomic second escalateSubPR call is followed " +
			"by an `if escCount != 2 { /* comment only */ }` with no assertion body. " +
			"Idempotency gate is untested.")
	})

	// ── AUDIT-138 — no full-lifecycle adversarial multi-iter dog test ──────
	t.Run("AUDIT_138_NoMultiIterationAdversarialLifecycleTest", func(t *testing.T) {
		raw := readFile(t, filepath.Join(agentsDir, "dogs_test.go"))
		// Look for a loop with bound ≥ 10 (the audit suggests 50). This is
		// the structural shape of a full-lifecycle adversarial test.
		loopRx := regexp.MustCompile(`for\s+\w+\s*:=\s*0\s*;\s*\w+\s*<\s*(\d{2,})\s*;`)
		matches := loopRx.FindAllStringSubmatch(raw, -1)
		bigLoops := 0
		for _, m := range matches {
			// Any ≥ 10-iteration numeric for loop counts as suspicious.
			if len(m) >= 2 && len(m[1]) >= 2 {
				bigLoops++
			}
		}
		if bigLoops > 0 {
			t.Errorf("AUDIT-138 POSSIBLE REMEDY: dogs_test.go has %d loop(s) of bound ≥10; "+
				"check if it's a full-lifecycle test and update this assertion", bigLoops)
		}
		// And: no test with the suggested name exists.
		for _, name := range []string{"TestFullConvoyLifecycle_AdversarialLLM",
			"TestFullLifecycle_AdversarialLLM", "TestDog_AdversarialLifecycle"} {
			if strings.Contains(raw, name) {
				t.Errorf("AUDIT-138 REMEDY DETECTED: %q exists in dogs_test.go", name)
			}
		}
		t.Log("AUDIT-138 REPRODUCED: dogs_test.go has no multi-iteration (≥10) loop-driven dog test. " +
			"The $300 burn's feedback loop (dog → parse fail → Completed → refire) is structurally unexercised.")
	})
}
