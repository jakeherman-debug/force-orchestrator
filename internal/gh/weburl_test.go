package gh

import "testing"

func TestWebRepoURL(t *testing.T) {
	cases := []struct {
		name   string
		remote string
		want   string
	}{
		{"ssh github with .git", "git@github.com:acme/api.git", "https://github.com/acme/api"},
		{"ssh github no suffix", "git@github.com:acme/api", "https://github.com/acme/api"},
		{"ssh enterprise host", "git@ghe.corp.internal:team/widgets.git", "https://ghe.corp.internal/team/widgets"},
		{"https with .git", "https://github.com/acme/api.git", "https://github.com/acme/api"},
		{"https no suffix", "https://github.com/acme/api", "https://github.com/acme/api"},
		{"http scheme", "http://localhost/acme/api.git", "http://localhost/acme/api"},
		{"empty", "", ""},
		{"file scheme", "file:///tmp/origin", ""},
		{"garbage", "not-a-url", ""},
		{"ssh without colon", "git@github.com", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := WebRepoURL(c.remote); got != c.want {
				t.Errorf("WebRepoURL(%q) = %q, want %q", c.remote, got, c.want)
			}
		})
	}
}

func TestWebBranchURL(t *testing.T) {
	cases := []struct {
		name   string
		remote string
		branch string
		want   string
	}{
		{"happy path ssh", "git@github.com:acme/api.git", "force/ask-1-feature",
			"https://github.com/acme/api/tree/force/ask-1-feature"},
		{"happy path https", "https://github.com/acme/api", "main",
			"https://github.com/acme/api/tree/main"},
		{"empty branch yields empty", "https://github.com/acme/api", "", ""},
		{"unresolvable remote yields empty", "file:///tmp/x", "main", ""},
		{"empty remote yields empty", "", "main", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := WebBranchURL(c.remote, c.branch); got != c.want {
				t.Errorf("WebBranchURL(%q, %q) = %q, want %q", c.remote, c.branch, got, c.want)
			}
		})
	}
}
