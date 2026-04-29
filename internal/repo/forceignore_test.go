package repo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeForceIgnore writes a .forceignore at the given directory and
// returns the directory path. Helper kept private to the package; the
// integration test in internal/agents/ has its own fixture builder.
func writeForceIgnore(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ".forceignore"), []byte(body), 0o644); err != nil {
		t.Fatalf("write .forceignore: %v", err)
	}
}

func TestForceIgnore_LoadMissingReturnsNilNil(t *testing.T) {
	dir := t.TempDir()
	fi, err := LoadForceIgnore(dir)
	if err != nil {
		t.Fatalf("expected nil error for missing file, got %v", err)
	}
	if fi != nil {
		t.Fatalf("expected nil *ForceIgnore for missing file, got %+v", fi)
	}
}

func TestForceIgnore_NilReceiverNeverIgnores(t *testing.T) {
	var fi *ForceIgnore
	if fi.IsIgnored(".env") {
		t.Fatalf("nil receiver returned true for .env")
	}
	if fi.IsIgnored("anything") {
		t.Fatalf("nil receiver returned true for arbitrary path")
	}
}

func TestForceIgnore_BasicPatterns(t *testing.T) {
	dir := t.TempDir()
	writeForceIgnore(t, dir, `# Comment line — ignored
.env
.env.*
*.env
credentials.json
credentials*.json
*.pem
*.key
secrets/
.aws/
holocron.db
`)
	fi, err := LoadForceIgnore(dir)
	if err != nil {
		t.Fatalf("LoadForceIgnore: %v", err)
	}
	if fi == nil {
		t.Fatal("expected non-nil ForceIgnore")
	}

	cases := []struct {
		path string
		want bool
	}{
		{".env", true},
		{".env.local", true},
		{"foo.env", true},
		{"credentials.json", true},
		{"credentials_prod.json", true},
		{"keys/server.pem", true},
		{"keys/server.key", true},
		{"secrets/db.txt", true},
		{".aws/credentials", true},
		{"holocron.db", true},
		// Negative cases.
		{"README.md", false},
		{"src/main.go", false},
		{"docs/architecture.md", false},
		{"package.json", false},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			if got := fi.IsIgnored(tc.path); got != tc.want {
				t.Errorf("IsIgnored(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

func TestForceIgnore_SymlinkResolution(t *testing.T) {
	// T0-2 anti-cheat: a symlink to .env must NOT bypass the .env rule.
	dir := t.TempDir()
	writeForceIgnore(t, dir, ".env\n")

	// Create the secret target file, then a symlink that points at it.
	envPath := filepath.Join(dir, ".env")
	if err := os.WriteFile(envPath, []byte("API_KEY=hunter2"), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	linkPath := filepath.Join(dir, "secrets-link.txt")
	if err := os.Symlink(envPath, linkPath); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	fi, err := LoadForceIgnore(dir)
	if err != nil {
		t.Fatalf("LoadForceIgnore: %v", err)
	}
	if !fi.IsIgnored("secrets-link.txt") {
		t.Fatalf("symlink to .env not ignored — anti-cheat bypass possible")
	}
	if !fi.IsIgnored(".env") {
		t.Fatalf("direct .env match also failed — sanity check")
	}
}

func TestForceIgnore_SymlinkOutsideRepoFallsBackToOriginalPath(t *testing.T) {
	// A symlink that points OUTSIDE the repo must not let an external
	// non-secret file accidentally match a repo-internal rule. We fall
	// back to matching the link's literal path, NOT the resolved target.
	repoDir := t.TempDir()
	otherDir := t.TempDir()
	writeForceIgnore(t, repoDir, ".env\n")
	target := filepath.Join(otherDir, "innocent.txt")
	if err := os.WriteFile(target, []byte("not a secret"), 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}
	linkPath := filepath.Join(repoDir, "outside-link.txt")
	if err := os.Symlink(target, linkPath); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	fi, err := LoadForceIgnore(repoDir)
	if err != nil {
		t.Fatalf("LoadForceIgnore: %v", err)
	}
	// "outside-link.txt" is not in the .forceignore → should be permitted.
	if fi.IsIgnored("outside-link.txt") {
		t.Fatalf("link to outside-repo file was treated as ignored — would over-block")
	}
}

func TestForceIgnore_CommentAndBlankLinesAreSkipped(t *testing.T) {
	dir := t.TempDir()
	writeForceIgnore(t, dir, `# leading comment

# another comment
.env

# trailing comment
`)
	fi, err := LoadForceIgnore(dir)
	if err != nil {
		t.Fatalf("LoadForceIgnore: %v", err)
	}
	pats := fi.Patterns()
	if len(pats) != 1 || pats[0] != ".env" {
		t.Fatalf("expected effective pattern .env only, got %v", pats)
	}
}

func TestForceIgnore_EmptyRepoPathRejected(t *testing.T) {
	if _, err := LoadForceIgnore(""); err == nil {
		t.Fatal("expected error for empty repoPath")
	}
}

func TestForceIgnore_RepoPathRoundTrip(t *testing.T) {
	dir := t.TempDir()
	writeForceIgnore(t, dir, ".env\n")
	fi, err := LoadForceIgnore(dir)
	if err != nil {
		t.Fatalf("LoadForceIgnore: %v", err)
	}
	if got := fi.RepoPath(); got != dir {
		t.Fatalf("RepoPath() = %q, want %q", got, dir)
	}
}

func TestForceIgnore_MalformedFileSurfacesError(t *testing.T) {
	// sabhiram/go-gitignore is permissive and rarely returns parse errors,
	// but a completely unreadable file (mode 000) should bubble the read
	// error up rather than silently fall back to "no policy".
	if os.Getuid() == 0 {
		t.Skip("running as root — chmod 0 does not block read")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, ".forceignore")
	if err := os.WriteFile(path, []byte(".env\n"), 0o000); err != nil {
		t.Fatalf("write: %v", err)
	}
	defer os.Chmod(path, 0o644) //nolint:errcheck
	_, err := LoadForceIgnore(dir)
	if err == nil {
		t.Fatal("expected read error for unreadable .forceignore")
	}
	if !strings.Contains(err.Error(), "read") {
		t.Fatalf("error did not mention 'read': %v", err)
	}
}
