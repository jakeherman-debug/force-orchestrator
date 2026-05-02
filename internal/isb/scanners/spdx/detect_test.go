package spdx

import (
	"os"
	"path/filepath"
	"testing"
)

func read(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return body
}

func TestDetect_KnownLicenses(t *testing.T) {
	cases := []struct {
		fixture string
		want    string
	}{
		{"MIT.txt", "MIT"},
		{"Apache-2.0.txt", "Apache-2.0"},
		{"BSD-3-Clause.txt", "BSD-3-Clause"},
		{"GPL-3.0.txt", "GPL-3.0"},
		{"MPL-2.0.txt", "MPL-2.0"},
		{"ISC.txt", "ISC"},
		{"Unlicense.txt", "Unlicense"},
	}
	for _, tc := range cases {
		got := Detect(read(t, tc.fixture))
		if got != tc.want {
			t.Errorf("%s: want %q got %q", tc.fixture, tc.want, got)
		}
	}
}

func TestDetect_NotALicense_ReturnsUnknown(t *testing.T) {
	got := Detect(read(t, "NotALicense.txt"))
	if got != Unknown {
		t.Errorf("expected Unknown for non-license content, got %q", got)
	}
}

func TestDetect_EmptyInput(t *testing.T) {
	if got := Detect(nil); got != Unknown {
		t.Errorf("nil input: want %q got %q", Unknown, got)
	}
	if got := Detect([]byte{}); got != Unknown {
		t.Errorf("empty input: want %q got %q", Unknown, got)
	}
}

func TestIsLicenseFilename(t *testing.T) {
	cases := map[string]bool{
		"LICENSE":         true,
		"LICENSE.md":      true,
		"LICENSE.txt":     true,
		"COPYING":         true,
		"COPYING.md":      true,
		"UNLICENSE":       true,
		"license":         true, // case-insensitive
		"License.MD":      true,
		"README.md":       false,
		"main.go":         false,
		"src/LICENSE":     true, // basename match
		"NOTICE":          false,
	}
	for path, want := range cases {
		if got := IsLicenseFilename(path); got != want {
			t.Errorf("IsLicenseFilename(%q)=%v want %v", path, got, want)
		}
	}
}

// Defensive: weird input must not panic.
func TestDetect_NoPanicOnBadInput(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Detect panicked: %v", r)
		}
	}()
	for _, body := range [][]byte{
		{0x00, 0x01, 0xff, 0xfe},
		[]byte("random garbage no license here"),
		[]byte(""),
	} {
		_ = Detect(body)
	}
}
