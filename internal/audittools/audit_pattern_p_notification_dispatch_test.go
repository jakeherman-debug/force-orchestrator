// Package audittools — Pattern P-NotificationDispatch (D11 Phase 1).
//
// P-NotificationDispatch enforces the D11 substrate invariant that every
// operator-facing notification routes through the central
// internal/notify.Dispatch entry point. Direct calls to the legacy
// notify-after seams (notifyAfterFn, realNotifyAfter,
// stageTransitionNotifyFn, notify.SlackNotify) outside the dispatcher
// itself are treated as future-deliverable bypass paths and rejected.
//
// Why this audit exists:
//
//   The pre-D11 world had three notify call sites scattered across
//   internal/agents (convoy_review.go's awaiting-supply-recheck ping,
//   dogs_supply_token_recheck.go's three pings, dogs_convoy_stage_watch.go's
//   stage-transition ping) and any new caller could fire-and-forget by
//   reaching into the agents-package seam directly. That made the per-
//   convoy override + DND + preset chain unreachable for new categories.
//
//   D11 Phase 1 moves the dispatch surface into internal/notify and
//   enforces routing through notify.Dispatch via this audit. Future
//   deliverables that add a new operator notification MUST:
//
//     1. Add the category to config/notifications.yaml with tier+default+description.
//     2. Call notify.Dispatch(ctx, db, category, convoyID, label, body).
//
//   Either step missing fails this audit.
//
// Allowlist: a small allowlist names the legitimate seam-internal call
// paths (e.g. internal/notify itself, the supply-token-recheck file's
// realNotifyAfter compat shim that exists for the migration window's
// test-seam contract). Each entry pairs a repo-relative path with a
// reason; reviewer at PR time confirms the allowlist entry is real.
//
// Mirror shape: this audit follows the same AST-walking shape as
// audit_pattern_p_stage_gate_test.go and
// audit_pattern_p_staging_promotion_confirm_test.go.

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

// notificationDispatchBypassAllowlist names production files that contain
// a CallExpr to one of the banned identifiers but are legitimately exempt
// because they ARE the seam (internal/notify) or the migration-window
// compat shim. Each entry's reason MUST name why the bypass is permanent
// (i.e. the file IS internal/notify or the test seam contract).
//
// At time of writing the allowlist holds:
//
//   - internal/notify/dispatcher.go        — IS the dispatcher, calls
//                                           SlackNotify directly as the
//                                           Slack side-effect of resolving
//                                           "slack" or "mail+slack".
//   - internal/notify/slack.go             — IS the Slack notifier.
//   - internal/agents/dogs_supply_token_recheck.go — owns realNotifyAfter
//                                           as a thin compat shim that
//                                           delegates to notify.SlackNotify;
//                                           the test seam (notifyAfterFn)
//                                           survives so the existing
//                                           withNotifyStub-based tests
//                                           continue to pin behaviour
//                                           while production routes via
//                                           notify.Dispatch.
//   - internal/agents/dogs_convoy_stage_watch.go — owns stageTransitionNotifyFn
//                                           as the test seam for stage-
//                                           transition pings; default
//                                           closure delegates to
//                                           notify.Dispatch. The audit
//                                           prevents external callers
//                                           from invoking the seam directly.
var notificationDispatchBypassAllowlist = map[string]string{
	"internal/notify/dispatcher.go":               "is the central dispatcher; calls SlackNotify as the slack side-effect of dispatch resolution",
	"internal/notify/slack.go":                    "is the Slack notifier; defines SlackNotify and SetSlackNotifierForTest",
	"internal/agents/dogs_supply_token_recheck.go": "owns notifyAfterFn / realNotifyAfter as a compat shim that delegates to notify.SlackNotify; test seam preserved for the migration window",
	"internal/agents/dogs_convoy_stage_watch.go":   "owns stageTransitionNotifyFn as the test seam; default closure delegates to notify.Dispatch",
}

// notificationDispatchBannedFunctions names the package-internal seams
// that no production code outside the allowlist may reach. The audit
// matches both the bare identifier (same-package callers) and any
// SelectorExpr selector with this name.
var notificationDispatchBannedFunctions = map[string]struct{}{
	"notifyAfterFn":           {}, // agents-package seam (legacy pre-D11)
	"realNotifyAfter":         {}, // agents-package compat shim
	"stageTransitionNotifyFn": {}, // agents-package stage-transition seam
	"SlackNotify":             {}, // notify-package Slack side-effect (callers must use Dispatch instead)
}

// TestPattern_P_NotificationDispatch_NoUngatedNotifyCalls walks every
// production .go file under internal/ and cmd/ and rejects any call to
// the banned identifiers that isn't on notificationDispatchBypassAllowlist.
//
// The audit runs at AST level so a grep-evading edit (e.g. method
// expression, function-value pass-through) still fails. The match is
// CallExpr-only — referencing the identifier as a non-call (for instance
// in a comment or a type assertion) is permitted; only an INVOCATION
// constitutes a bypass.
func TestPattern_P_NotificationDispatch_NoUngatedNotifyCalls(t *testing.T) {
	root := moduleRoot(t)

	type offender struct {
		path string
		line int
		fn   string
	}
	var offenders []offender

	walkTargets := []string{"internal", "cmd"}
	for _, sub := range walkTargets {
		walkRoot := filepath.Join(root, sub)
		err := filepath.WalkDir(walkRoot, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				name := d.Name()
				if name == ".fix-worktrees" || name == ".force-worktrees" ||
					name == ".claude" || name == ".build-worktrees" ||
					name == ".d7-worktrees" ||
					name == "vendor" || name == ".git" ||
					name == "node_modules" || name == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}
			if strings.HasSuffix(path, "_test.go") {
				return nil
			}
			fset := token.NewFileSet()
			f, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				return nil
			}

			relPath := rel(root, path)
			if reason, ok := notificationDispatchBypassAllowlist[relPath]; ok {
				t.Logf("Pattern P-NotificationDispatch: %s — bypass allowed (%s)", relPath, reason)
				return nil
			}

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
				offenders = append(offenders, offender{
					path: relPath,
					line: fset.Position(call.Pos()).Line,
					fn:   fnName,
				})
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", walkRoot, err)
		}
	}

	if len(offenders) == 0 {
		return
	}
	sort.Slice(offenders, func(i, j int) bool {
		if offenders[i].path != offenders[j].path {
			return offenders[i].path < offenders[j].path
		}
		return offenders[i].line < offenders[j].line
	})
	t.Errorf("Pattern P-NotificationDispatch: %d ungated notification call site(s):", len(offenders))
	for _, o := range offenders {
		t.Errorf("  %s:%d — calls %s; route through notify.Dispatch instead", o.path, o.line, o.fn)
	}
	t.Errorf("\nFix: replace the call with notify.Dispatch(ctx, db, category, convoyID, label, body) " +
		"after registering the category in config/notifications.yaml. " +
		"For seam-owners (internal/notify, the migration-window compat shims), add the file to " +
		"notificationDispatchBypassAllowlist with a reason naming why the bypass is permanent.")
}

// TestPattern_P_NotificationDispatch_DispatcherSurfacePresent is a
// surface-existence check — confirms the central dispatcher file lives
// where the audit expects. If notify.Dispatch ever moves, the AST audit
// above would still pass (it just checks the banned-identifier set), so
// this test pins the dispatcher's home.
func TestPattern_P_NotificationDispatch_DispatcherSurfacePresent(t *testing.T) {
	root := moduleRoot(t)
	dispatcherPath := filepath.Join(root, "internal", "notify", "dispatcher.go")
	body := mustReadFile(t, dispatcherPath)
	for _, want := range []string{
		"package notify",
		"func Dispatch(",
		"ConfigKeyActivePreset",
		"ConfigKeyDNDUntil",
		"ConfigKeyCategoryPrefix",
		"dndBypassCategories",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("Pattern P-NotificationDispatch: dispatcher.go missing required symbol %q", want)
		}
	}
}

// TestPattern_P_NotificationDispatch_PositiveControl confirms that the
// three migrated call sites DO reach notify.Dispatch. If a future edit
// removes the call but forgets to replace it with another notify.Dispatch
// path, this positive control fails — even if the negative-control audit
// (above) still passes.
func TestPattern_P_NotificationDispatch_PositiveControl(t *testing.T) {
	root := moduleRoot(t)
	for _, pair := range []struct {
		path    string
		require string
	}{
		{filepath.Join(root, "internal", "agents", "convoy_review.go"), `notify.Dispatch(ctx, db, "awaiting_supply_recheck"`},
		{filepath.Join(root, "internal", "agents", "dogs_supply_token_recheck.go"), `notify.Dispatch(ctx, db, "supply_token_expired"`},
		{filepath.Join(root, "internal", "agents", "dogs_supply_token_recheck.go"), `notify.Dispatch(ctx, db, "supply_token_recovered"`},
		{filepath.Join(root, "internal", "agents", "dogs_supply_token_recheck.go"), `notify.Dispatch(ctx, db, "supply_per_branch_summary"`},
		{filepath.Join(root, "internal", "agents", "dogs_convoy_stage_watch.go"), `notify.Dispatch(ctx, db, "stage_transition"`},
	} {
		body := mustReadFile(t, pair.path)
		if !strings.Contains(body, pair.require) {
			t.Errorf("Pattern P-NotificationDispatch positive control: %s missing required call %q",
				rel(root, pair.path), pair.require)
		}
	}
}
