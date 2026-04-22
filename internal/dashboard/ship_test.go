package dashboard

import (
	"testing"
)

func TestDeriveGHRepoForDashboard(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"git@github.com:acme/widgets.git", "acme/widgets"},
		{"https://github.com/acme/widgets.git", "acme/widgets"},
		{"https://github.com/acme/widgets", "acme/widgets"},
		{"file:///tmp/repo", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := deriveGHRepoForDashboard(c.in); got != c.want {
			t.Errorf("%q → %q, want %q", c.in, got, c.want)
		}
	}
}
