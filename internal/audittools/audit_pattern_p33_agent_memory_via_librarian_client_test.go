// Package audittools — Pattern P33 (D4 Phase 0).
//
// P33 enforces that production agent code (internal/agents/*.go,
// non-_test) retrieves FleetMemory rows for prompt injection through
// the Librarian Client surface — never via direct
// store.GetFleetMemories / store.GetFleetMemoriesByIDs /
// store.ListAllFleetMemories calls.
//
// Allowlist:
//
//   - librarian_dogs.go      — the Librarian's own maintenance dogs
//                              (drift watcher, hypothesis emitter, etc.)
//                              call store helpers directly because they
//                              ARE the Librarian backend; the Client
//                              surface is the inverse interface.
//   - librarian_ingress.go   — the canonical seam: this file routes
//                              every other agent's memory read through
//                              Client.GetWeightedMemories.
//   - memory_rerank.go       — the LLM re-ranker is part of the
//                              Librarian retrieval pipeline; it
//                              consumes the candidate slice but does
//                              not itself read from the store.
//
// Files outside the allowlist that import store and reference any of
// the forbidden function names are flagged. The check is AST-based
// (so a comment that mentions "store.GetFleetMemories" is fine) and
// scoped to selector expressions of the form `store.<func>`.
package audittools

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// p33ForbiddenStoreFuncs is the set of direct-store FleetMemory
// readers that production agent code MUST NOT call.
var p33ForbiddenStoreFuncs = map[string]struct{}{
	"GetFleetMemories":       {},
	"ListAllFleetMemories":   {},
	"GetFleetMemoriesByIDs":  {},
}

// p33Allowlist names files exempted from the Pattern P33 check. Each
// entry MUST carry a one-line truthful rationale.
var p33Allowlist = map[string]string{
	"internal/agents/librarian_dogs.go":     "Librarian's own maintenance dogs — they ARE the curator backend; routing through the Client would be self-referential and would obscure the dogs' direct intent",
	"internal/agents/librarian_ingress.go":  "the canonical Librarian-Client ingress seam (Pattern P33's intended replacement). Calls store.RecordRetrieval but does NOT call any forbidden FleetMemory reader",
	"internal/agents/memory_rerank.go":      "LLM re-ranker — part of the Librarian retrieval pipeline. Consumes the candidate slice, does not read from the store directly",
	"internal/agents/librarian.go":          "the Librarian agent itself — writes memory via store.StoreFleetMemory; never calls a forbidden reader",
}

// TestPattern_P33_AgentMemoryInjectionViaLibrarianClient walks
// internal/agents/*.go (non-test) and reports any file (outside the
// allowlist) that calls one of the forbidden direct-store readers.
func TestPattern_P33_AgentMemoryInjectionViaLibrarianClient(t *testing.T) {
	root := moduleRoot(t)
	agentDir := filepath.Join(root, "internal", "agents")

	type offence struct {
		File string
		Line int
		Func string
	}
	var offences []offence

	walkErr := filepath.WalkDir(agentDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel := relForP33(root, path)
		if _, allowed := p33Allowlist[rel]; allowed {
			return nil
		}

		fset := token.NewFileSet()
		f, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return nil
		}

		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "store" {
				return true
			}
			if _, forbidden := p33ForbiddenStoreFuncs[sel.Sel.Name]; !forbidden {
				return true
			}
			pos := fset.Position(call.Pos())
			offences = append(offences, offence{
				File: rel,
				Line: pos.Line,
				Func: sel.Sel.Name,
			})
			return true
		})
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk %s: %v", agentDir, walkErr)
	}

	if len(offences) == 0 {
		return
	}
	sort.Slice(offences, func(i, j int) bool {
		if offences[i].File != offences[j].File {
			return offences[i].File < offences[j].File
		}
		return offences[i].Line < offences[j].Line
	})
	t.Errorf("Pattern P33 (D4-P0): %d agent file(s) call a forbidden direct-store FleetMemory reader. Route the call through the Librarian Client (use getMemoriesForPromptInjection or Client.GetWeightedMemories) — agents must depend on the Librarian Client surface, not on store internals:", len(offences))
	for _, o := range offences {
		t.Errorf("  %s:%d — store.%s(...)", o.File, o.Line, o.Func)
	}
}

// TestPattern_P33_AllowlistReasonsTruthful asserts every allowlist
// entry carries a non-empty rationale (mirrors P25's truth-check).
func TestPattern_P33_AllowlistReasonsTruthful(t *testing.T) {
	for path, reason := range p33Allowlist {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("p33Allowlist[%q] empty rationale — every allowlist entry MUST carry a one-line truthful reason", path)
		}
	}
}

// relForP33 returns a path relative to root, with forward-slash
// separators so test output is stable on Windows.
func relForP33(root, path string) string {
	r, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(r)
}
