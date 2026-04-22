package agents

import (
	"strings"
	"testing"

	igit "force-orchestrator/internal/git"
	"force-orchestrator/internal/store"
)

// TestAskBranchName_UsesUsernamePrefixWhenSet verifies that
// AskBranchNameForConvoy composes <user>/force/ask-... when a username is
// available.
func TestAskBranchName_UsesUsernamePrefixWhenSet(t *testing.T) {
	restore := igit.SetBranchPrefixOverride("alice-smith/")
	defer restore()
	got := AskBranchNameForConvoy(7, "[7] Add OAuth")
	want := "alice-smith/force/ask-7-add-oauth"
	if got != want {
		t.Errorf("want %q, got %q", want, got)
	}
}

// TestAskBranchName_FallsBackToBareNameWhenNoUsername ensures the enterprise
// prefix doesn't break local/airgapped setups where gh isn't configured.
func TestAskBranchName_FallsBackToBareNameWhenNoUsername(t *testing.T) {
	restore := igit.SetBranchPrefixOverride("")
	defer restore()
	got := AskBranchNameForConvoy(7, "[7] Add OAuth")
	want := "force/ask-7-add-oauth"
	if got != want {
		t.Errorf("want %q, got %q", want, got)
	}
}

// TestBranchAgentName_HandlesPrefixedBranchName verifies the parser still
// extracts the astromech name when the branch has a username prefix.
func TestBranchAgentName_HandlesPrefixedBranchName(t *testing.T) {
	cases := []struct {
		branch, want string
	}{
		// Bare format (legacy pre-prefix behavior, still supported).
		{"agent/R2-D2/task-42", "R2-D2"},
		{"agent/BB-8/task-99", "BB-8"},

		// Username-prefixed format (new).
		{"alice/agent/R2-D2/task-42", "R2-D2"},
		{"wedge-antilles/agent/BB-8/task-99", "BB-8"},

		// Ask-branches are not agent branches — return "".
		{"force/ask-5-test", ""},
		{"alice/force/ask-5-test", ""},

		// Legacy "agent/task-N" (no agent name at all) — must not mis-parse
		// the "task-N" segment as the agent name.
		{"agent/task-42", ""},
		{"alice/agent/task-42", ""},

		// Empty / garbage.
		{"", ""},
		{"main", ""},
		{"feature/some-thing", ""},
	}
	for _, c := range cases {
		if got := BranchAgentName(c.branch); got != c.want {
			t.Errorf("BranchAgentName(%q) = %q, want %q", c.branch, got, c.want)
		}
	}
}

// TestAskBranchNameForConvoy_StripsConvoyBracketPrefix is the pre-existing
// contract but restated here with an explicit username prefix so the combined
// logic is covered.
func TestAskBranchNameForConvoy_StripsConvoyBracketPrefix(t *testing.T) {
	restore := igit.SetBranchPrefixOverride("poe-dameron/")
	defer restore()
	got := AskBranchNameForConvoy(12, "[12] Fix: 🎉 critical bug!!")
	// Emoji and punctuation become dashes in the slug.
	if !strings.HasPrefix(got, "poe-dameron/force/ask-12-fix-") {
		t.Errorf("got %q", got)
	}
	// Must not contain the '[' or ']' or '!'.
	if strings.ContainsAny(got, "[]!:") {
		t.Errorf("unsafe chars leaked: %q", got)
	}
}

// TestAskBranchName_PrefixAppliesBeforeForceNotAfter proves ordering: the
// username comes first, then "force", then the rest. Regression test for
// the obvious reordering mistake.
func TestAskBranchName_PrefixAppliesBeforeForceNotAfter(t *testing.T) {
	restore := igit.SetBranchPrefixOverride("carol/")
	defer restore()
	got := AskBranchNameForConvoy(3, "[3] test")
	if !strings.HasPrefix(got, "carol/force/") {
		t.Errorf("prefix order wrong: got %q", got)
	}
	if strings.HasPrefix(got, "force/carol/") {
		t.Errorf("prefix inserted in wrong place: %q", got)
	}
}

// TestRunCreateAskBranch_UsesPrefixedBranchName wires the prefix through to
// the actual ask-branch creation handler and verifies the stored branch name
// includes the prefix. Integration coverage for the end-to-end path.
func TestRunCreateAskBranch_UsesPrefixedBranchName(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	restore := igit.SetBranchPrefixOverride("ezra-bridger/")
	defer restore()

	wt, _ := makeOriginAndClone(t)
	store.AddRepo(db, "api", wt, "")
	_ = store.SetRepoRemoteInfo(db, "api", "https://github.com/acme/api.git", "main")
	cid, _ := store.CreateConvoy(db, "[4] prefixed")
	_, _ = store.AddConvoyTask(db, 0, "api", "t", cid, 0, "Pending")

	taskID, _ := QueueCreateAskBranch(db, cid)
	b, _ := store.GetBounty(db, taskID)
	runCreateAskBranch(db, b, testLogger{})

	ab := store.GetConvoyAskBranch(db, cid, "api")
	if ab == nil {
		t.Fatal("ask-branch not recorded")
	}
	if !strings.HasPrefix(ab.AskBranch, "ezra-bridger/force/ask-") {
		t.Errorf("expected prefixed branch name, got %q", ab.AskBranch)
	}
	if strings.Contains(ab.AskBranch, "force//") || strings.HasPrefix(ab.AskBranch, "/") {
		t.Errorf("malformed branch name (stray slashes): %q", ab.AskBranch)
	}
}
