package trust

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoad_MissingFile(t *testing.T) {
	dir := t.TempDir()
	f, err := Load(filepath.Join(dir, "nope"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if f == nil || len(f.Entries) != 0 {
		t.Errorf("expected empty File, got %+v", f)
	}
}

func TestAppend_AndLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trust")

	e := Entry{
		SHA256:           "abc123def456abc123def456abc123def456abc123def456abc123def456abcd",
		TrustedBy:        "operator@example.com",
		GitSHAAtBuild:    "0977061",
		GitBranchAtBuild: "main",
	}
	if err := Append(path, e); err != nil {
		t.Fatalf("Append: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(loaded.Entries))
	}
	got := loaded.Entries[0]
	if got.SHA256 != e.SHA256 {
		t.Errorf("SHA = %q, want %q", got.SHA256, e.SHA256)
	}
	if got.TrustedBy != e.TrustedBy {
		t.Errorf("TrustedBy = %q, want %q", got.TrustedBy, e.TrustedBy)
	}
	if got.GitSHAAtBuild != e.GitSHAAtBuild {
		t.Errorf("GitSHAAtBuild = %q", got.GitSHAAtBuild)
	}
	if got.GitBranchAtBuild != e.GitBranchAtBuild {
		t.Errorf("GitBranchAtBuild = %q", got.GitBranchAtBuild)
	}
}

func TestHasSHA(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trust")
	_ = Append(path, Entry{SHA256: "deadbeef00000000000000000000000000000000000000000000000000000000", TrustedBy: "u"})
	f, _ := Load(path)
	if !f.HasSHA("DEADBEEF00000000000000000000000000000000000000000000000000000000") {
		t.Errorf("HasSHA should be case-insensitive")
	}
	if f.HasSHA("0000000000000000000000000000000000000000000000000000000000000000") {
		t.Errorf("HasSHA should reject unknown")
	}
}

func TestMostRecent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trust")
	_ = Append(path, Entry{
		SHA256:    "1111111111111111111111111111111111111111111111111111111111111111",
		Timestamp: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	_ = Append(path, Entry{
		SHA256:    "2222222222222222222222222222222222222222222222222222222222222222",
		Timestamp: time.Date(2026, 5, 5, 0, 0, 0, 0, time.UTC),
	})
	f, _ := Load(path)
	mr := f.MostRecent()
	if mr == nil {
		t.Fatalf("MostRecent nil")
	}
	if !strings.HasPrefix(mr.SHA256, "2222") {
		t.Errorf("MostRecent.SHA256 = %q, want 2222...", mr.SHA256)
	}
}

func TestRemoveSHA(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trust")
	_ = Append(path, Entry{SHA256: "aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111", TrustedBy: "u"})
	_ = Append(path, Entry{SHA256: "bbbb2222bbbb2222bbbb2222bbbb2222bbbb2222bbbb2222bbbb2222bbbb2222", TrustedBy: "u"})

	n, err := RemoveSHA(path, "aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111")
	if err != nil {
		t.Fatalf("RemoveSHA: %v", err)
	}
	if n != 1 {
		t.Errorf("removed = %d, want 1", n)
	}

	f, _ := Load(path)
	if len(f.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(f.Entries))
	}
	if !strings.HasPrefix(f.Entries[0].SHA256, "bbbb") {
		t.Errorf("survivor = %q, want bbbb...", f.Entries[0].SHA256)
	}
}

func TestRemoveSHA_NotFound(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trust")
	_ = Append(path, Entry{SHA256: "aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111"})
	n, err := RemoveSHA(path, "ffff9999ffff9999ffff9999ffff9999ffff9999ffff9999ffff9999ffff9999")
	if err != nil {
		t.Fatalf("RemoveSHA: %v", err)
	}
	if n != 0 {
		t.Errorf("removed = %d, want 0", n)
	}
}

func TestLoad_MalformedLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trust")
	content := "# comment\n" +
		"abc123\n" + // too short
		"abc123def456abc123def456abc123def456abc123def456abc123def456abcd 2026-05-05T00:00:00Z user 0977061 main\n"
	_ = os.WriteFile(path, []byte(content), 0o600)
	f, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(f.Entries) != 1 {
		t.Errorf("good entries = %d, want 1", len(f.Entries))
	}
	if len(f.MalformedLines) != 1 {
		t.Errorf("malformed = %d, want 1", len(f.MalformedLines))
	}
}

func TestHashFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "binary")
	if err := os.WriteFile(path, []byte("hello world"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := HashFile(path)
	if err != nil {
		t.Fatalf("HashFile: %v", err)
	}
	// SHA256("hello world")
	want := "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9"
	if got != want {
		t.Errorf("SHA = %q, want %q", got, want)
	}
}

func TestDefaultPath(t *testing.T) {
	p := DefaultPath()
	if p == "" {
		t.Errorf("DefaultPath empty")
	}
	if !strings.HasSuffix(p, "trusted-binary-hashes") {
		t.Errorf("DefaultPath = %q, want suffix", p)
	}
}
