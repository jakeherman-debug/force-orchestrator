// Package audittools — Pattern P-SupplyDeferral (D5 Phase 1, slice γ).
//
// P-SupplyDeferral enforces the "No silent token-expired passthroughs"
// anti-cheat directive from docs/roadmap.md § Deliverable 5 (line
// 1619). Every CodeArtifact registry-call site in
// `internal/isb/rules/supply_*.go` that catches an auth-class error
// (errors.Is(err, codeartifact.ErrTokenExpired)) MUST emit a
// SecurityFindings deferral row via supplydeferral.RecordDeferral —
// either directly on that branch, or via a same-file helper that the
// branch invokes.
//
// The pattern is two complementary AST checks:
//
//  1. TestPattern_PSupplyDeferral_DeferralOnTokenExpired walks each
//     `internal/isb/rules/supply_*.go` file. If the file calls any
//     codeartifact.Client interface method (DescribePackageVersion,
//     ListPackages, Health) AND the file references
//     `codeartifact.ErrTokenExpired`, then the file MUST also reference
//     `supplydeferral.RecordDeferral`. Files that don't call CodeArtifact
//     at all are exempt (their auth-error path is N/A — SUPPLY-002 is
//     such a no-network rule that builds its allowlist from
//     SystemConfig).
//
//  2. TestPattern_PSupplyDeferral_NoSilentReturnNilOnAuth walks every
//     `if errors.Is(err, codeartifact.ErrTokenExpired) { ... }` block in
//     `supply_*.go` and rejects the silent-return shape:
//     `return nil, nil` (or `return nil`) without any prior call to
//     supplydeferral.RecordDeferral on that branch. The SUPPLY-001
//     production code uses `r.recordDeferral(...)` — a helper method
//     on the same struct that ultimately calls supplydeferral.
//     RecordDeferral. The check handles both forms: a direct call and
//     a method call whose name is `recordDeferral` (or contains the
//     deferral substring).
//
// Both tests carry synthesized "bad" source fixtures inline (parsed via
// parser.ParseFile with a string src argument) so the failure modes are
// regression-tested without polluting the real rule directory.
//
// The pattern walks production source only — *_test.go files under
// internal/isb/rules/ are skipped (test code may legitimately fabricate
// auth errors without recording deferrals as part of its harness).
package audittools

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// pSupplyDeferralCAClientMethods is the set of method names on
// codeartifact.Client that constitute a registry-hit. Kept tight on
// purpose: the interface shape is small and stable (see
// internal/clients/codeartifact/client.go). New methods landing on the
// interface MUST be added here so the audit catches new sites.
var pSupplyDeferralCAClientMethods = map[string]struct{}{
	"DescribePackageVersion": {},
	"ListPackages":           {},
	"Health":                 {},
}

// TestPattern_PSupplyDeferral_DeferralOnTokenExpired is the slice γ
// regression. For every production supply_*.go rule body, if the file
// touches the CodeArtifact registry AND checks for the token-expired
// sentinel, it MUST also wire in supplydeferral.RecordDeferral. The
// directive is anti-cheat-load-bearing: a rule that catches auth
// errors without leaving a row behind is the worst failure mode (the
// recovery dog has nothing to replay when the operator runs `umt
// artifacts`).
func TestPattern_PSupplyDeferral_DeferralOnTokenExpired(t *testing.T) {
	root := moduleRoot(t)
	rulesDir := filepath.Join(root, "internal", "isb", "rules")

	matches, err := filepath.Glob(filepath.Join(rulesDir, "supply_*.go"))
	if err != nil {
		t.Fatalf("glob supply_*.go: %v", err)
	}
	if len(matches) == 0 {
		t.Fatalf("no supply_*.go files under %s — D5 P1 supply rules expected", rulesDir)
	}

	type offence struct {
		File   string
		Reason string
	}
	var offences []offence

	for _, path := range matches {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			t.Fatalf("read %s: %v", path, rerr)
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, body, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", path, perr)
		}

		callsCA, callsTokenExpired, callsRecordDeferral := pSupplyDeferralScanFile(t, f)

		// Allowlist: rules that don't touch CodeArtifact at all are
		// exempt — their auth-error path is structurally N/A.
		if !callsCA {
			continue
		}

		// If the file checks ErrTokenExpired but never calls
		// supplydeferral.RecordDeferral, that's the silent-passthrough
		// shape the directive forbids.
		if callsTokenExpired && !callsRecordDeferral {
			offences = append(offences, offence{
				File:   relForP(root, path),
				Reason: "calls CodeArtifact + checks ErrTokenExpired but never calls supplydeferral.RecordDeferral — silent token-expired passthrough",
			})
			continue
		}

		// If the file calls CodeArtifact but never checks
		// ErrTokenExpired, the auth-error path is unhandled. That's
		// also a silent passthrough (the SDK's auth error would just
		// flow up unwrapped, never landing in SecurityFindings).
		if !callsTokenExpired {
			offences = append(offences, offence{
				File:   relForP(root, path),
				Reason: "calls CodeArtifact but never checks errors.Is(err, codeartifact.ErrTokenExpired) — auth-error path unhandled",
			})
		}
	}

	// Synthetic bad-source fixture: a rule that calls DescribePackageVersion,
	// checks ErrTokenExpired, but returns nil without recording a deferral.
	// The audit's per-file logic should flag this. We assert against a
	// synthesized AST rather than a testdata file so the fixture stays
	// inline.
	badSrc := `package rules

import (
	"context"
	"database/sql"
	"errors"

	"force-orchestrator/internal/clients/codeartifact"
	"force-orchestrator/internal/isb"
)

type bad struct{ client codeartifact.Client }

func (b *bad) Run(ctx context.Context, db *sql.DB, in isb.ManifestGatedInput) ([]isb.Finding, error) {
	_, err := b.client.DescribePackageVersion(ctx, codeartifact.EcosystemPyPI, "x", "1")
	if errors.Is(err, codeartifact.ErrTokenExpired) {
		return nil, nil
	}
	return nil, err
}
`
	badFset := token.NewFileSet()
	badAST, parseErr := parser.ParseFile(badFset, "synthetic_bad.go", badSrc, 0)
	if parseErr != nil {
		t.Fatalf("parse synthetic bad fixture: %v", parseErr)
	}
	bca, btoken, bdeferral := pSupplyDeferralScanFile(t, badAST)
	if !bca {
		t.Errorf("synthetic bad fixture: scanner failed to detect CodeArtifact call")
	}
	if !btoken {
		t.Errorf("synthetic bad fixture: scanner failed to detect ErrTokenExpired")
	}
	if bdeferral {
		t.Errorf("synthetic bad fixture: scanner falsely detected supplydeferral.RecordDeferral")
	}

	if len(offences) == 0 {
		return
	}
	sort.Slice(offences, func(i, j int) bool {
		return offences[i].File < offences[j].File
	})
	t.Errorf("Pattern P-SupplyDeferral (D5-P1): %d supply rule file(s) silently swallow token-expired errors. Per docs/roadmap.md § D5 anti-cheat \"No silent token-expired passthroughs\", every auth-error path must call supplydeferral.RecordDeferral:", len(offences))
	for _, o := range offences {
		t.Errorf("  %s — %s", o.File, o.Reason)
	}
	t.Errorf("\nFix: on the `errors.Is(err, codeartifact.ErrTokenExpired)` branch, call supplydeferral.RecordDeferral(db, taskID, payload) before returning. The recovery dog (D5 P4) replays these rows when the operator runs `umt artifacts`.")
}

// TestPattern_PSupplyDeferral_NoSilentReturnNilOnAuth scans every
// `if errors.Is(err, codeartifact.ErrTokenExpired) { ... }` block in
// production supply rule files and rejects the silent-return shape
// where the branch immediately returns nil/empty without any
// supplydeferral.RecordDeferral (or same-file helper named
// recordDeferral) call.
//
// SUPPLY-001's branch:
//
//	if errors.Is(err, codeartifact.ErrTokenExpired) {
//	    if defErr := r.recordDeferral(...); defErr != nil { ... }
//	    log.Printf(...)
//	}
//
// PASSES — recordDeferral is the same-file helper that calls
// supplydeferral.RecordDeferral. A synthetic shape that returns nil,
// nil from the branch FAILS.
func TestPattern_PSupplyDeferral_NoSilentReturnNilOnAuth(t *testing.T) {
	root := moduleRoot(t)
	rulesDir := filepath.Join(root, "internal", "isb", "rules")

	matches, err := filepath.Glob(filepath.Join(rulesDir, "supply_*.go"))
	if err != nil {
		t.Fatalf("glob supply_*.go: %v", err)
	}

	type offence struct {
		File string
		Line int
	}
	var offences []offence

	for _, path := range matches {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		hits := pSupplyDeferralScanForSilentReturn(t, path)
		for _, h := range hits {
			offences = append(offences, offence{File: relForP(root, path), Line: h})
		}
	}

	// Synthetic bad fixture — the silent return shape.
	badSrc := `package rules

import (
	"errors"

	"force-orchestrator/internal/clients/codeartifact"
)

func RunBad(err error) ([]int, error) {
	if errors.Is(err, codeartifact.ErrTokenExpired) {
		return nil, nil
	}
	return nil, err
}
`
	badPath := "synthetic_bad_silent.go"
	badFset := token.NewFileSet()
	badAST, perr := parser.ParseFile(badFset, badPath, badSrc, 0)
	if perr != nil {
		t.Fatalf("parse synthetic bad: %v", perr)
	}
	badHits := pSupplyDeferralWalkAST(badAST, badFset)
	if len(badHits) == 0 {
		t.Errorf("synthetic bad fixture: scanner failed to detect silent `return nil, nil` on ErrTokenExpired branch")
	}

	// Synthetic good fixture — direct supplydeferral.RecordDeferral
	// call on the branch.
	goodSrc := `package rules

import (
	"database/sql"
	"errors"

	"force-orchestrator/internal/clients/codeartifact"
	"force-orchestrator/internal/isb/supplydeferral"
)

func RunGood(db *sql.DB, taskID int, err error) error {
	if errors.Is(err, codeartifact.ErrTokenExpired) {
		_, derr := supplydeferral.RecordDeferral(db, taskID, supplydeferral.DeferralPayload{})
		return derr
	}
	return err
}
`
	goodFset := token.NewFileSet()
	goodAST, perr := parser.ParseFile(goodFset, "synthetic_good.go", goodSrc, 0)
	if perr != nil {
		t.Fatalf("parse synthetic good: %v", perr)
	}
	goodHits := pSupplyDeferralWalkAST(goodAST, goodFset)
	if len(goodHits) > 0 {
		t.Errorf("synthetic good fixture: scanner false-positive at lines %v — direct supplydeferral.RecordDeferral on branch should pass", goodHits)
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
	t.Errorf("Pattern P-SupplyDeferral (D5-P1): %d ErrTokenExpired branch(es) silently return without calling supplydeferral.RecordDeferral:", len(offences))
	for _, o := range offences {
		t.Errorf("  %s:%d — silent `return nil[, nil]` on token-expired branch", o.File, o.Line)
	}
	t.Errorf("\nFix: call supplydeferral.RecordDeferral(db, taskID, payload) on the ErrTokenExpired branch (or invoke a same-file helper named recordDeferral that does).")
}

// pSupplyDeferralScanFile inspects an AST and returns three booleans:
//   - callsCA          — the file calls a codeartifact.Client method
//   - callsTokenExpired — the file references codeartifact.ErrTokenExpired
//   - callsRecordDeferral — the file references supplydeferral.RecordDeferral
//
// All three are file-level checks (not function-scoped) — that's the
// invariant: a rule body that wires CodeArtifact at all must also
// import the deferral helper somewhere in the same file.
func pSupplyDeferralScanFile(t *testing.T, f *ast.File) (callsCA, callsTokenExpired, callsRecordDeferral bool) {
	t.Helper()
	ast.Inspect(f, func(n ast.Node) bool {
		switch e := n.(type) {
		case *ast.CallExpr:
			if sel, ok := e.Fun.(*ast.SelectorExpr); ok {
				name := sel.Sel.Name
				if _, hit := pSupplyDeferralCAClientMethods[name]; hit {
					callsCA = true
				}
				// supplydeferral.RecordDeferral
				if name == "RecordDeferral" {
					if id, ok := sel.X.(*ast.Ident); ok && id.Name == "supplydeferral" {
						callsRecordDeferral = true
					}
				}
			}
		case *ast.SelectorExpr:
			// codeartifact.ErrTokenExpired reference (not necessarily a call)
			if e.Sel.Name == "ErrTokenExpired" {
				if id, ok := e.X.(*ast.Ident); ok && id.Name == "codeartifact" {
					callsTokenExpired = true
				}
			}
		}
		return true
	})
	return callsCA, callsTokenExpired, callsRecordDeferral
}

// pSupplyDeferralScanForSilentReturn walks the production source at
// path and returns the line numbers of `if errors.Is(err,
// codeartifact.ErrTokenExpired) { ... }` blocks whose body matches the
// silent-return shape (no recordDeferral call before a `return nil[, nil]`).
func pSupplyDeferralScanForSilentReturn(t *testing.T, path string) []int {
	t.Helper()
	body, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("read %s: %v", path, rerr)
	}
	fset := token.NewFileSet()
	f, perr := parser.ParseFile(fset, path, body, 0)
	if perr != nil {
		t.Fatalf("parse %s: %v", path, perr)
	}
	return pSupplyDeferralWalkAST(f, fset)
}

// pSupplyDeferralWalkAST is the AST-walk core. Returns line numbers of
// offending if-blocks. Shared by file-based scanner and synthetic
// fixture asserts.
func pSupplyDeferralWalkAST(f *ast.File, fset *token.FileSet) []int {
	var hits []int
	ast.Inspect(f, func(n ast.Node) bool {
		ifStmt, ok := n.(*ast.IfStmt)
		if !ok {
			return true
		}
		if !pSupplyDeferralCondMatchesTokenExpired(ifStmt.Cond) {
			return true
		}
		if ifStmt.Body == nil {
			return true
		}
		if !pSupplyDeferralBodyHasDeferralCall(ifStmt.Body) {
			// Body lacks any deferral-emitting call — the silent shape.
			pos := fset.Position(ifStmt.Pos())
			hits = append(hits, pos.Line)
		}
		return true
	})
	return hits
}

// pSupplyDeferralCondMatchesTokenExpired returns true when the
// expression is `errors.Is(<anything>, codeartifact.ErrTokenExpired)`.
func pSupplyDeferralCondMatchesTokenExpired(expr ast.Expr) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if sel.Sel.Name != "Is" {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok || id.Name != "errors" {
		return false
	}
	if len(call.Args) != 2 {
		return false
	}
	target, ok := call.Args[1].(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if target.Sel.Name != "ErrTokenExpired" {
		return false
	}
	tid, ok := target.X.(*ast.Ident)
	if !ok || tid.Name != "codeartifact" {
		return false
	}
	return true
}

// pSupplyDeferralBodyHasDeferralCall returns true when the body
// contains any call to:
//
//   - supplydeferral.RecordDeferral(...)
//   - <recv>.recordDeferral(...) — the SUPPLY-001 same-file helper shape
//
// Either form satisfies the "no silent passthrough" directive: the
// helper method ultimately calls supplydeferral.RecordDeferral itself,
// and the audit's per-file scanner (Test 1) catches the case where it
// doesn't.
func pSupplyDeferralBodyHasDeferralCall(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if found {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch sel.Sel.Name {
		case "RecordDeferral":
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == "supplydeferral" {
				found = true
			}
		case "recordDeferral":
			// Same-file helper method (e.g. r.recordDeferral on the
			// SUPPLY-001 receiver). The Test-1 file-level scanner ensures
			// the helper itself routes to supplydeferral.RecordDeferral.
			found = true
		}
		return true
	})
	return found
}

// relForP normalises a path to module-relative slashes for stable
// error messages. Mirrors relForP34 / rel from sibling tests.
func relForP(root, path string) string {
	r, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(r)
}
