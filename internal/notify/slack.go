// internal/notify/slack.go — D11 Phase 1.
//
// Defines the Slack-side side effect (the `notify-after` shell-out)
// behind the SlackNotifyFn package var so tests can inject a stub.
// Production routes through the same shell-out that
// internal/agents/dogs_supply_token_recheck.go's realNotifyAfter
// previously owned — moved here so internal/notify owns the entire
// dispatch + side-effect surface (per Pattern P-NotificationDispatch).
//
// The agents-package realNotifyAfter still exists as a thin wrapper
// (internal/agents/dogs_supply_token_recheck.go) so the test seam
// notifyAfterFn keeps working for the migration window — it now calls
// notify.SlackNotify under the hood. Pattern P-NotificationDispatch
// rejects any new callsite that uses notifyAfterFn directly outside
// this package + the existing seam.

package notify

import (
	"context"
	"fmt"
	"os/exec"
	"sync"
	"testing"
	"time"
)

// SlackNotifier is the side-effect interface for the notify-after Slack
// shell-out. The concrete production implementation is realSlackNotify
// (below); tests install a counting stub via SetSlackNotifierForTest.
type SlackNotifier func(ctx context.Context, label string) error

var (
	slackNotifyMu sync.RWMutex
	slackNotify   SlackNotifier = realSlackNotify
)

// SetSlackNotifierForTest swaps the package-level slack notifier and
// returns a restore closure. Tests use this to count Slack pings or
// intercept labels. Mirrors the existing
// agents.SetStageTransitionNotifyForTest shape.
func SetSlackNotifierForTest(fn SlackNotifier) (restore func()) {
	slackNotifyMu.Lock()
	prev := slackNotify
	slackNotify = fn
	slackNotifyMu.Unlock()
	return func() {
		slackNotifyMu.Lock()
		slackNotify = prev
		slackNotifyMu.Unlock()
	}
}

// SlackNotify is the package-level entry point used by Dispatch. It
// reads the current notifier under a lock so tests can swap in a stub
// concurrently with normal traffic without racing.
func SlackNotify(ctx context.Context, label string) error {
	slackNotifyMu.RLock()
	fn := slackNotify
	slackNotifyMu.RUnlock()
	return fn(ctx, label)
}

// realSlackNotify shells `notify-after "<label>" -- true`. Best-effort:
// if the binary isn't on PATH (e.g. on CI without the plugin), the
// call is a no-op and returns nil — webhook delivery is a UX bonus.
//
// Test-mode short-circuit: when the binary was built by `go test`
// (testing.Testing() == true, Go 1.21+), realSlackNotify returns nil
// immediately without shelling out. This guard exists because
// integration-style tests that exercised notify-after fired the real
// long-running-notifier helper, flooding the operator's Slack channel
// every test run. Tests that explicitly want to assert on the call
// path install a stub via SetSlackNotifierForTest — that override
// takes precedence because it replaces the function pointer entirely;
// this guard only fires when no override is installed.
func realSlackNotify(ctx context.Context, label string) error {
	if testing.Testing() {
		return nil
	}
	bin, err := exec.LookPath("notify-after")
	if err != nil {
		// Helper not installed — silently no-op. The dispatcher logs
		// the resolution decision so operators tracking Slack-routed
		// categories will see "resolved=slack" without a confirming
		// webhook hit on the channel side.
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, bin, label, "--", "true")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("notify-after: %w", err)
	}
	return nil
}
