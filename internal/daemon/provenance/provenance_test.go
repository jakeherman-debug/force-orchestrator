package provenance

import (
	"strings"
	"testing"
)

func TestGet_Defaults(t *testing.T) {
	// In test mode, the linker did not inject ldflags — so the values
	// should remain at their compile-time defaults ("unknown"). We don't
	// assert exact equality (a future test or other test in this package
	// may have called Set), just that Get() returns a non-empty struct.
	i := Get()
	if i.GoVersion == "" {
		t.Errorf("GoVersion empty")
	}
}

func TestSet_OverridesDefaults(t *testing.T) {
	Set("abc123", "2026-05-05T00:00:00Z", "deliverable/12/p1")
	i := Get()
	if i.GitSHA != "abc123" {
		t.Errorf("GitSHA = %q", i.GitSHA)
	}
	if i.BuildTime != "2026-05-05T00:00:00Z" {
		t.Errorf("BuildTime = %q", i.BuildTime)
	}
	if i.GitBranch != "deliverable/12/p1" {
		t.Errorf("GitBranch = %q", i.GitBranch)
	}
}

func TestSet_EmptyPreservesPrior(t *testing.T) {
	Set("aaa", "ttt", "bbb")
	Set("", "", "") // should be a no-op
	i := Get()
	if i.GitSHA != "aaa" || i.BuildTime != "ttt" || i.GitBranch != "bbb" {
		t.Errorf("empty Set should not overwrite, got %+v", i)
	}
}

func TestString_RendersAllFields(t *testing.T) {
	Set("sha1", "tt", "br")
	s := Get().String()
	for _, want := range []string{"sha1", "tt", "br", "go="} {
		if !strings.Contains(s, want) {
			t.Errorf("%q missing %q", s, want)
		}
	}
}
