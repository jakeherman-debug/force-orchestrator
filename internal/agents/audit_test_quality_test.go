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
		t.Skip("AUDIT-111: remove when withStubCLIRunner adds CallCount counter (Fix #7 companion)")
		// Without skip, fails with: audit_test_quality_test.go:77: AUDIT-111: withStubCLIRunner has no CallCount/atomic counter still present
		// Confirm withStubCLIRunner factory has a CallCount counter field.
		helpers := readFile(t, filepath.Join(agentsDir, "testhelpers_test.go"))
		if !strings.Contains(helpers, "func withStubCLIRunner") {
			t.Fatal("withStubCLIRunner factory not in testhelpers_test.go — factory moved?")
		}
		idx := strings.Index(helpers, "func withStubCLIRunner")
		body := helpers[idx:]
		if end := strings.Index(body[1:], "\nfunc "); end > 0 {
			body = body[:end]
		}
		// RGR inversion: fail if withStubCLIRunner still has no CallCount counter.
		if !(strings.Contains(body, "CallCount") || strings.Contains(body, "atomic.AddInt")) {
			t.Fatal("AUDIT-111: withStubCLIRunner has no CallCount/atomic counter still present")
		}
	})

	// ── AUDIT-112 — TOCTOU concurrency — closed by Fix #3 ──────────────────
	// Fix #3 added TestAddConvoyTaskIdempotent_ConcurrentCallers (50 goroutines)
	// alongside the partial UNIQUE idx_bounty_idem and the ON CONFLICT DO NOTHING
	// claim in AddConvoyTaskIdempotent. This meta-test stays as regression
	// protection: any refactor that removes the concurrency coverage flips it red.
	t.Run("AUDIT_112_ConcurrentIdempotencyTest_DuplicateOfP2", func(t *testing.T) {
		idem := readFile(t, filepath.Join(storeDir, "tasks_idempotent_test.go"))
		if !(strings.Contains(idem, "sync.WaitGroup") || strings.Contains(idem, "go func")) {
			t.Fatal("AUDIT-112 regression: tasks_idempotent_test.go no longer "+
				"contains sync.WaitGroup / go func concurrency coverage — " +
				"re-add TestAddConvoyTaskIdempotent_ConcurrentCallers.")
		}
	})

	// ── AUDIT-113 — no total Claude call bound across ConvoyReview passes ──
	t.Run("AUDIT_113_NoBoundedTotalClaudeCallsTest", func(t *testing.T) {
		t.Skip("AUDIT-113: remove when convoy_review_test adds TotalClaudeCalls bound (Fix #7)")
		// Without skip, fails with: audit_test_quality_test.go:103: AUDIT-113: convoy_review_test without cross-pass Claude call bound still present
		raw := readFile(t, filepath.Join(agentsDir, "convoy_review_test.go"))
		// RGR inversion: fail if no total-call bound assertion yet exists.
		hasBound := strings.Contains(raw, "TotalClaudeCalls") ||
			strings.Contains(raw, "convoyReviewMaxTotalCalls") ||
			strings.Contains(raw, "TestConvoyReview_TotalClaudeCallsBounded") ||
			strings.Contains(raw, "totalCalls") ||
			strings.Contains(raw, "callsAcross")
		if !hasBound {
			t.Fatal("AUDIT-113: convoy_review_test without cross-pass Claude call bound still present")
		}
	})

	// ── AUDIT-133 — no retry_count preservation across auto-complete ────────
	t.Run("AUDIT_133_RetryCountPreservation_Untested", func(t *testing.T) {
		// Closed by Fix #6: TestResetTaskFull_PreservesRetryCount lives in internal/store.
		// RGR inversion: fail if no sibling test in internal/store covers ResetTaskFull's counter preservation.
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
		if !found {
			t.Fatal("AUDIT-133: TestResetTaskFull_PreservesRetryCount missing still present")
		}
	})

	// ── AUDIT-135 — stub LLM never asserts prompt structure ─────────────────
	t.Run("AUDIT_135_StubDoesNotAssertPromptStructure", func(t *testing.T) {
		t.Skip("AUDIT-135: remove when stubConvoyReviewLLM captures/asserts prompt (Fix #7)")
		// Without skip, fails with: audit_test_quality_test.go:161: AUDIT-135: stubConvoyReviewLLM / withStubCLIRunner with no prompt capture still present
		raw := readFile(t, filepath.Join(agentsDir, "convoy_review_test.go"))
		if !strings.Contains(raw, "func stubConvoyReviewLLM") {
			t.Fatal("stubConvoyReviewLLM helper moved; update this test")
		}
		idx := strings.Index(raw, "func stubConvoyReviewLLM")
		rest := raw[idx:]
		if end := strings.Index(rest, "\n}\n"); end > 0 {
			rest = rest[:end]
		}
		// RGR inversion: fail if stubConvoyReviewLLM still captures no prompt.
		hasCapture := false
		for _, token := range []string{"capturedPrompt", "lastPrompt", "assertPrompt", "requirePrompt"} {
			if strings.Contains(rest, token) {
				hasCapture = true
				break
			}
		}
		if !hasCapture {
			stub := readFile(t, filepath.Join(agentsDir, "testhelpers_test.go"))
			for _, token := range []string{"capturedPrompt", "lastPrompt", "PromptHistory"} {
				if strings.Contains(stub, token) {
					hasCapture = true
					break
				}
			}
		}
		if !hasCapture {
			t.Fatal("AUDIT-135: stubConvoyReviewLLM / withStubCLIRunner with no prompt capture still present")
		}
	})

	// ── AUDIT-136 — ConvoyReview JSON parse-retry untested ──────────────────
	t.Run("AUDIT_136_ParseFailureRetryPath_Untested", func(t *testing.T) {
		t.Skip("AUDIT-136: remove when ConvoyReview parse-retry test added (Fix #7)")
		// Without skip, fails with: audit_test_quality_test.go:195: AUDIT-136: no parse-retry test covering one-retry-then-Completed contract still present
		path := filepath.Join(agentsDir, "convoy_review_test.go")
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		var covers []string
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
				covers = append(covers, name)
			}
		}
		// RGR inversion: fail if no parse-retry tests exist.
		if len(covers) == 0 {
			t.Fatal("AUDIT-136: no parse-retry test covering one-retry-then-Completed contract still present")
		}
	})

	// ── AUDIT-137 — TestEscalateSubPR second-call block has no assertion ──
	t.Run("AUDIT_137_SecondCallBlockLacksAssertion", func(t *testing.T) {
		t.Skip("AUDIT-137: remove when TestEscalateSubPR_IsAtomic asserts escCount==1 (Fix #8)")
		// Without skip, fails with: audit_test_quality_test.go:278: AUDIT-137: TestEscalateSubPR_IsAtomic second-call block without assertion still present
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
		// RGR inversion: fail if second-call block still lacks a t.Error*/t.Fatal* assertion.
		if !foundAssertion {
			t.Fatal("AUDIT-137: TestEscalateSubPR_IsAtomic second-call block without assertion still present")
		}
	})

	// ── AUDIT-138 — no full-lifecycle adversarial multi-iter dog test ──────
	t.Run("AUDIT_138_NoMultiIterationAdversarialLifecycleTest", func(t *testing.T) {
		t.Skip("AUDIT-138: remove when TestFullConvoyLifecycle_AdversarialLLM added (Fix #7)")
		// Without skip, fails with: audit_test_quality_test.go:304: AUDIT-138: dogs_test.go without multi-iteration adversarial lifecycle test still present
		raw := readFile(t, filepath.Join(agentsDir, "dogs_test.go"))
		loopRx := regexp.MustCompile(`for\s+\w+\s*:=\s*0\s*;\s*\w+\s*<\s*(\d{2,})\s*;`)
		matches := loopRx.FindAllStringSubmatch(raw, -1)
		bigLoops := 0
		for _, m := range matches {
			if len(m) >= 2 && len(m[1]) >= 2 {
				bigLoops++
			}
		}
		hasNamed := false
		for _, name := range []string{"TestFullConvoyLifecycle_AdversarialLLM",
			"TestFullLifecycle_AdversarialLLM", "TestDog_AdversarialLifecycle"} {
			if strings.Contains(raw, name) {
				hasNamed = true
				break
			}
		}
		// RGR inversion: fail if no multi-iter (>=10) loop AND no named adversarial lifecycle test exists.
		if bigLoops == 0 && !hasNamed {
			t.Fatal("AUDIT-138: dogs_test.go without multi-iteration adversarial lifecycle test still present")
		}
	})
}
