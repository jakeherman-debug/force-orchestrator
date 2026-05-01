// Package dashboard — D3 polish-pass iteration 2 test harness.
//
// TestMain pins LIVE_HAIKU_DISABLED=1 so dashboard handler tests that
// invoke renderer agents (Ask, retro, briefing, learning panel,
// transcript-archive) take the deterministic synthesise* path instead
// of routing through claude.CallWithTranscript. Without this pin the
// handlers would attempt real Haiku calls during unit tests — slow,
// expensive, and at-the-mercy of the LLM CLI's availability.

package dashboard

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	if err := os.Setenv("LIVE_HAIKU_DISABLED", "1"); err != nil {
		panic("dashboard tests: failed to pin LIVE_HAIKU_DISABLED: " + err.Error())
	}
	os.Exit(m.Run())
}
