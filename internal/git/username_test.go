package git

import (
	"strings"
	"testing"
)

// TestSanitizeUsername covers the slug rules: lowercase, alphanumerics +
// underscore/dash preserved, everything else collapses to '-', runs of '-'
// collapsed, trimmed, capped at 40 chars.
func TestSanitizeUsername(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"alice", "alice"},
		{"Alice", "alice"},
		{"alice-smith", "alice-smith"},
		{"alice_smith", "alice_smith"},
		{"Alice O'Brien", "alice-o-brien"},
		{"alice@example.com", "alice-example-com"},
		{"  leading-space  ", "leading-space"},
		{"--dashes--", "dashes"},
		{"multi   spaces", "multi-spaces"},
		{"", ""},
		{"@@@", ""},
		{"this-is-a-really-really-really-really-long-username-that-exceeds-40-chars",
			"this-is-a-really-really-really-really-lo"},
	}
	for _, c := range cases {
		if got := sanitizeUsername(c.in); got != c.want {
			t.Errorf("sanitizeUsername(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestBranchPrefix_OverrideWinsOverDiscovery proves tests can force a fixed
// prefix regardless of the developer's gh/git config.
func TestBranchPrefix_OverrideWinsOverDiscovery(t *testing.T) {
	restore := SetBranchPrefixOverride("alice/")
	defer restore()
	if got := BranchPrefix(); got != "alice/" {
		t.Errorf("expected override 'alice/', got %q", got)
	}
	restore()
	// Reinstalling TestMain's default override leaves us at "".
	// (TestMain's restore fires at end of m.Run, but within-test restores
	// fall back to the "" override from TestMain.)
	if got := BranchPrefix(); got != "" {
		t.Errorf("after restore expected TestMain override '', got %q", got)
	}
}

// TestBranchPrefix_EmptyOverrideMeansNoPrefix verifies the "can't discover"
// case — callers append BranchPrefix() directly, so "" must compose correctly.
func TestBranchPrefix_EmptyOverrideMeansNoPrefix(t *testing.T) {
	restore := SetBranchPrefixOverride("")
	defer restore()
	if got := BranchPrefix(); got != "" {
		t.Errorf("empty override must produce empty prefix, got %q", got)
	}
}

// TestBranchPrefix_ComposesIntoValidBranchName verifies downstream formatting
// does the right thing with both empty and populated prefixes.
func TestBranchPrefix_ComposesIntoValidBranchName(t *testing.T) {
	restore := SetBranchPrefixOverride("bob/")
	defer restore()
	name := BranchPrefix() + "force/ask-1-test"
	if name != "bob/force/ask-1-test" {
		t.Errorf("composed branch: %q", name)
	}

	restore()
	restore2 := SetBranchPrefixOverride("")
	defer restore2()
	bareName := BranchPrefix() + "force/ask-1-test"
	if bareName != "force/ask-1-test" {
		t.Errorf("bare composed branch: %q", bareName)
	}
}

// TestBranchPrefix_TrailingSlashOnlyWhenNonEmpty guards against accidental
// double-slashes. A populated prefix MUST include the trailing slash; an
// empty prefix MUST NOT.
func TestBranchPrefix_TrailingSlashOnlyWhenNonEmpty(t *testing.T) {
	restore := SetBranchPrefixOverride("wedge-antilles/")
	defer restore()
	got := BranchPrefix()
	if !strings.HasSuffix(got, "/") {
		t.Errorf("populated prefix must end with '/': %q", got)
	}
	restore()
	restore2 := SetBranchPrefixOverride("")
	defer restore2()
	got = BranchPrefix()
	if got != "" {
		t.Errorf("empty prefix must be exactly empty string, got %q", got)
	}
}
