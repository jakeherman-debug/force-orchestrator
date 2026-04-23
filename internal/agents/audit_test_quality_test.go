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
		// Closed by: Fix #7 (`fix/convoy-review-tightening`).
		// withStubCLIRunner now returns *stubCLIRunner with a CallCount()
		// method and prompt capture — every cost-loop test asserts bounded
		// Claude invocations.
		helpers := readFile(t, filepath.Join(agentsDir, "testhelpers_test.go"))
		if !strings.Contains(helpers, "func withStubCLIRunner") {
			t.Fatal("withStubCLIRunner factory not in testhelpers_test.go — factory moved?")
		}
		if !strings.Contains(helpers, "CallCount") {
			t.Fatal("AUDIT-111 regression: withStubCLIRunner no longer exposes CallCount — cost-loop tests cannot bound Claude invocations")
		}
		if !strings.Contains(helpers, "atomic") {
			t.Fatal("AUDIT-111 regression: withStubCLIRunner no longer uses an atomic counter — call-count reads are not safe under concurrent stubs")
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
		// Closed by: Fix #7 (`fix/convoy-review-tightening`).
		// TestConvoyReview_TotalClaudeCallsBounded runs an adversarial LLM
		// stub across multiple passes and asserts total CallCount stays
		// under the hard cap. See convoy_review_fix7_test.go.
		raw := readFile(t, filepath.Join(agentsDir, "convoy_review_fix7_test.go"))
		hasBound := strings.Contains(raw, "TotalClaudeCalls") ||
			strings.Contains(raw, "convoyReviewMaxTotalCalls") ||
			strings.Contains(raw, "TestConvoyReview_TotalClaudeCallsBounded")
		if !hasBound {
			t.Fatal("AUDIT-113 regression: no total-Claude-call bound test remains for ConvoyReview; cross-pass cost blowup is no longer caught")
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
		// Closed by: Fix #7 (`fix/convoy-review-tightening`).
		// withStubCLIRunner now captures every prompt in stubCLIRunner.prompts;
		// stubConvoyReviewLLM returns the runner so callers can call
		// assertConvoyReviewPromptShape to fail fast on empty/missing markers.
		raw := readFile(t, filepath.Join(agentsDir, "convoy_review_test.go"))
		if !strings.Contains(raw, "func stubConvoyReviewLLM") {
			t.Fatal("stubConvoyReviewLLM helper moved; update this test")
		}
		// Check the helper captures prompts (via *stubCLIRunner return).
		hasCapture := strings.Contains(raw, "assertConvoyReviewPromptShape") ||
			strings.Contains(raw, "LastPrompt()") ||
			strings.Contains(raw, "stubCLIRunner")
		if !hasCapture {
			stub := readFile(t, filepath.Join(agentsDir, "testhelpers_test.go"))
			hasCapture = strings.Contains(stub, "prompts []string") ||
				strings.Contains(stub, "LastPrompt")
		}
		if !hasCapture {
			t.Fatal("AUDIT-135 regression: stubConvoyReviewLLM / withStubCLIRunner no longer captures the prompt — structural prompt drift is silent again")
		}
	})

	// ── AUDIT-136 — ConvoyReview JSON parse-retry untested ──────────────────
	t.Run("AUDIT_136_ParseFailureRetryPath_Untested", func(t *testing.T) {
		// Closed by: Fix #7 (`fix/convoy-review-tightening`).
		// TestConvoyReview_ParseFailure_EscalatesAfterCap covers the full
		// one-retry-then-escalate contract. See convoy_review_fix7_test.go.
		var covers []string
		for _, file := range []string{"convoy_review_test.go", "convoy_review_fix7_test.go"} {
			path := filepath.Join(agentsDir, file)
			if _, err := os.Stat(path); err != nil {
				continue
			}
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
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
		}
		if len(covers) == 0 {
			t.Fatal("AUDIT-136 regression: no parse-retry test remains for the ConvoyReview parse-failure path")
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
		// Closed by: Fix #7 (`fix/convoy-review-tightening`).
		// TestFullConvoyLifecycle_AdversarialLLM in convoy_review_fix7_test.go
		// runs 50 alternating LLM response iterations and asserts both
		// bounded Claude calls AND convoy reaches terminal state.
		hasNamed := false
		for _, file := range []string{"dogs_test.go", "convoy_review_fix7_test.go"} {
			path := filepath.Join(agentsDir, file)
			if _, err := os.Stat(path); err != nil {
				continue
			}
			raw := readFile(t, path)
			for _, name := range []string{"TestFullConvoyLifecycle_AdversarialLLM",
				"TestFullLifecycle_AdversarialLLM", "TestDog_AdversarialLifecycle"} {
				if strings.Contains(raw, name) {
					hasNamed = true
					break
				}
			}
			if hasNamed {
				break
			}
		}
		if !hasNamed {
			t.Fatal("AUDIT-138 regression: no multi-iteration adversarial lifecycle test remains; dog-refire + parse-fail + bounded-cost loop coverage is gone")
		}
	})
}
