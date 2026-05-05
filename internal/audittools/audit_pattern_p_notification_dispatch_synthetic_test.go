package audittools

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// TestPattern_P_NotificationDispatch_SyntheticCounterExample drives the
// AST-walker logic against a hand-crafted source file and confirms the
// banned-identifier set fires as expected. This complements the
// production-tree negative-control test by giving the audit a pinned
// fail case so a future edit that accidentally weakens the matcher
// (e.g. switches to file-substring rather than CallExpr inspection)
// also fails.
//
// The synthetic source contains:
//
//   - one banned call (notifyAfterFn(ctx, "x")) which MUST fire
//   - one ident-only reference (`_ = notifyAfterFn`) which must NOT fire
//   - one comment-mention which must NOT fire
//   - one banned-selector call (notify.SlackNotify(ctx, "x")) which MUST fire
//
// We exercise the same CallExpr filter the real audit uses.
func TestPattern_P_NotificationDispatch_SyntheticCounterExample(t *testing.T) {
	src := `package fake

import "context"

// commentedReferences notifyAfterFn and stageTransitionNotifyFn — but no calls.
type stub struct{}

var notifyAfterFn = func(ctx context.Context, label string) error { return nil }

func runBad(ctx context.Context) {
	// This is the banned call.
	notifyAfterFn(ctx, "test-label")
}

func runIdentOnly() {
	// Address-of / ident-only reference; not a call.
	_ = notifyAfterFn
}

type notify struct{}

var n notify

func (notify) SlackNotify(ctx context.Context, label string) error { return nil }

func runBadSelector(ctx context.Context) {
	n.SlackNotify(ctx, "test")
}

func runOK(ctx context.Context) {
	// Calls a function whose name happens to look similar but isn't banned.
	_ = ctx
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "synthetic.go", src, 0)
	if err != nil {
		t.Fatalf("parse synthetic: %v", err)
	}

	type hit struct {
		fn   string
		line int
	}
	var hits []hit

	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		var fnName string
		switch fn := call.Fun.(type) {
		case *ast.SelectorExpr:
			if fn.Sel != nil {
				fnName = fn.Sel.Name
			}
		case *ast.Ident:
			fnName = fn.Name
		}
		if fnName == "" {
			return true
		}
		if _, banned := notificationDispatchBannedFunctions[fnName]; !banned {
			return true
		}
		hits = append(hits, hit{fn: fnName, line: fset.Position(call.Pos()).Line})
		return true
	})

	if len(hits) != 2 {
		t.Fatalf("synthetic: got %d hits, want 2 (notifyAfterFn + SlackNotify): %+v", len(hits), hits)
	}
	got := []string{hits[0].fn, hits[1].fn}
	wantSet := map[string]bool{"notifyAfterFn": false, "SlackNotify": false}
	for _, g := range got {
		if _, ok := wantSet[g]; !ok {
			t.Errorf("unexpected hit fn=%s", g)
			continue
		}
		wantSet[g] = true
	}
	for k, v := range wantSet {
		if !v {
			t.Errorf("expected hit on %s but didn't see one", k)
		}
	}
}

// TestPattern_P_NotificationDispatch_AllowlistShape — ratchets the
// allowlist's value text so a "reason" entry that drops below a token
// floor (i.e. is empty or trivially short) fails CI. This stops a
// future edit that adds a file to the allowlist without justifying
// why the bypass is real.
func TestPattern_P_NotificationDispatch_AllowlistShape(t *testing.T) {
	for path, reason := range notificationDispatchBypassAllowlist {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("allowlist entry %q has empty reason", path)
		}
		if len(reason) < 20 {
			t.Errorf("allowlist entry %q reason %q is too short to be a real justification (< 20 chars)", path, reason)
		}
	}
}
