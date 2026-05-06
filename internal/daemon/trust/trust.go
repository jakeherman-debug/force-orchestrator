// Package trust manages the operator-curated trust list of binary
// SHAs. D12 P1 — `~/.force/trusted-binary-hashes`.
//
// The file is append-only (writes use O_APPEND). Each line is space-
// separated:
//
//	<sha256> <utc-rfc3339> <trusted-by> <git-sha-at-build-time> <git-branch>
//
// Comment lines (`#`) and blank lines are skipped. Lines that fail to
// parse are returned in MalformedLines so the operator can audit them
// without the daemon refusing to boot.
package trust

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Entry is one line of the trust file.
type Entry struct {
	SHA256          string
	Timestamp       time.Time
	TrustedBy       string
	GitSHAAtBuild   string
	GitBranchAtBuild string
}

// File is the parsed view of the trust file.
type File struct {
	Path           string
	Entries        []Entry
	MalformedLines []string
}

// DefaultPath returns ~/.force/trusted-binary-hashes. Falls back to
// /tmp/trusted-binary-hashes if HOME is unset.
func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "trusted-binary-hashes")
	}
	return filepath.Join(home, ".force", "trusted-binary-hashes")
}

// Load reads and parses the trust file. A missing file is NOT an error
// (returns an empty File); the operator may not have ratified anything
// yet.
func Load(path string) (*File, error) {
	f := &File{Path: path}
	fp, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return f, nil
		}
		return nil, err
	}
	defer fp.Close()

	scanner := bufio.NewScanner(fp)
	scanner.Buffer(make([]byte, 64*1024), 64*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		entry, perr := parseLine(line)
		if perr != nil {
			f.MalformedLines = append(f.MalformedLines, line)
			continue
		}
		f.Entries = append(f.Entries, entry)
	}
	return f, scanner.Err()
}

// HasSHA reports whether `sha` is present in the trust list.
func (f *File) HasSHA(sha string) bool {
	want := strings.ToLower(strings.TrimSpace(sha))
	for _, e := range f.Entries {
		if strings.EqualFold(e.SHA256, want) {
			return true
		}
	}
	return false
}

// MostRecent returns the chronologically newest entry, or nil if the
// file has no entries. Used by `force daemon rollback` to find the
// "second-to-most-recent" entry by walking back from MostRecent.
func (f *File) MostRecent() *Entry {
	if len(f.Entries) == 0 {
		return nil
	}
	idx := 0
	for i, e := range f.Entries {
		if e.Timestamp.After(f.Entries[idx].Timestamp) {
			idx = i
		}
	}
	cp := f.Entries[idx]
	return &cp
}

// Sorted returns entries sorted newest-first.
func (f *File) Sorted() []Entry {
	out := make([]Entry, len(f.Entries))
	copy(out, f.Entries)
	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp.After(out[j].Timestamp) })
	return out
}

// Append writes a new entry to the trust file (creating the file +
// parent dir if needed). Append is the ONLY mutation surface — there
// is no Update; an operator who wants to remove a trusted SHA must use
// RemoveSHA.
func Append(path string, e Entry) error {
	if e.SHA256 == "" {
		return errors.New("trust.Append: SHA256 required")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create trust dir: %w", err)
		}
	}
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now().UTC()
	}
	if e.TrustedBy == "" {
		if u := os.Getenv("USER"); u != "" {
			e.TrustedBy = u
		} else {
			e.TrustedBy = "unknown"
		}
	}
	if e.GitSHAAtBuild == "" {
		e.GitSHAAtBuild = "unknown"
	}
	if e.GitBranchAtBuild == "" {
		e.GitBranchAtBuild = "unknown"
	}
	fp, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer fp.Close()
	_, err = fmt.Fprintf(fp, "%s %s %s %s %s\n",
		strings.ToLower(strings.TrimSpace(e.SHA256)),
		e.Timestamp.UTC().Format(time.RFC3339),
		safeField(e.TrustedBy),
		safeField(e.GitSHAAtBuild),
		safeField(e.GitBranchAtBuild),
	)
	return err
}

// RemoveSHA rewrites the trust file with all entries except those
// matching `sha`. Returns the number of entries removed (0 if not
// found).
func RemoveSHA(path, sha string) (int, error) {
	f, err := Load(path)
	if err != nil {
		return 0, err
	}
	want := strings.ToLower(strings.TrimSpace(sha))
	removed := 0
	kept := f.Entries[:0]
	for _, e := range f.Entries {
		if strings.EqualFold(e.SHA256, want) {
			removed++
			continue
		}
		kept = append(kept, e)
	}
	if removed == 0 {
		return 0, nil
	}
	// Atomic replace: write to tmp + rename.
	tmp := path + ".tmp"
	fp, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, err
	}
	w := bufio.NewWriter(fp)
	for _, e := range kept {
		fmt.Fprintf(w, "%s %s %s %s %s\n",
			e.SHA256,
			e.Timestamp.UTC().Format(time.RFC3339),
			safeField(e.TrustedBy),
			safeField(e.GitSHAAtBuild),
			safeField(e.GitBranchAtBuild),
		)
	}
	if err := w.Flush(); err != nil {
		_ = fp.Close()
		_ = os.Remove(tmp)
		return 0, err
	}
	if err := fp.Close(); err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return 0, err
	}
	return removed, nil
}

// HashFile computes the SHA256 of a binary on disk. Returns the lower-
// case hex digest.
func HashFile(path string) (string, error) {
	fp, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer fp.Close()
	h := sha256.New()
	if _, err := io.Copy(h, fp); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func parseLine(line string) (Entry, error) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return Entry{}, fmt.Errorf("expected >=2 fields, got %d", len(fields))
	}
	e := Entry{SHA256: strings.ToLower(fields[0])}
	if len(e.SHA256) < 16 {
		return Entry{}, fmt.Errorf("sha field too short: %q", e.SHA256)
	}
	t, err := time.Parse(time.RFC3339, fields[1])
	if err != nil {
		return Entry{}, fmt.Errorf("bad timestamp %q: %w", fields[1], err)
	}
	e.Timestamp = t
	if len(fields) >= 3 {
		e.TrustedBy = fields[2]
	}
	if len(fields) >= 4 {
		e.GitSHAAtBuild = fields[3]
	}
	if len(fields) >= 5 {
		e.GitBranchAtBuild = strings.Join(fields[4:], " ")
	}
	return e, nil
}

// safeField strips spaces / newlines so a malformed input can't
// torpedo the line layout. Empty becomes "-".
func safeField(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "-"
	}
	s = strings.ReplaceAll(s, " ", "_")
	s = strings.ReplaceAll(s, "\t", "_")
	s = strings.ReplaceAll(s, "\n", "_")
	return s
}
