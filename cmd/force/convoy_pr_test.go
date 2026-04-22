package main

import (
	"testing"
)

func TestDeriveGHRepoFromRemoteURLForShip(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"git@github.com:acme/api.git", "acme/api"},
		{"git@github.com:acme/api", "acme/api"},
		{"https://github.com/acme/api.git", "acme/api"},
		{"https://github.com/acme/api", "acme/api"},
		{"", ""},
		{"file:///tmp/repo", ""},
	}
	for _, c := range cases {
		if got := deriveGHRepoFromRemoteURLForShip(c.in); got != c.want {
			t.Errorf("%q → %q want %q", c.in, got, c.want)
		}
	}
}

func TestTruncateSHA(t *testing.T) {
	if truncateSHA("abcdef0123456789") != "abcdef01" {
		t.Errorf("truncate to 8 chars")
	}
	if truncateSHA("abc") != "abc" {
		t.Errorf("short string should not be truncated")
	}
	if truncateSHA("") != "" {
		t.Errorf("empty stays empty")
	}
}
