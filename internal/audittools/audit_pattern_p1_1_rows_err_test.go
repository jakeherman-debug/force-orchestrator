// Package audittools: pattern test for rows.Err() iteration-end checking.
package audittools

import (
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestPattern_P1_1_RowsErrCheckedAfterIteration is the Fix #8e regression
// guard for the rows.Err() invariant the CLAUDE.md rows.Scan paragraph
// promises: every `for <name>.Next() { ... }` loop in production code
// MUST be followed (within a reasonable window after the closing brace)
// by `<name>.Err()` whose result is observed — returned, logged with a
// recovery hint, or error-wrapped. A silent loop that drops rows.Err()
// swallows iteration-time errors (broken statement, driver disconnect)
// in the exact place where SQL-side issues most often surface.
//
// The check walks every production *.go file, finds each `for <name>.Next() {`
// loop, traces forward until the matching close brace at the same indent,
// and asserts `<name>.Err()` is referenced within 10 lines of that close.
// Test files are exempt — the invariant is about production paths, not
// test fixtures.
//
// Acceptable forms (one of):
//   1. `if err := <name>.Err(); err != nil { return err }` (or any named-err alias)
//   2. `<name>.Err()` whose result is assigned and observed (e.g., `err = <name>.Err()`)
//   3. `if rErr := <name>.Err(); rErr != nil { ... }` (named error inspected)
//
// Unacceptable:
//   - No reference to `<name>.Err()` at all in the close-brace window
//   - `_ = <name>.Err()` (silent discard)
//
// Anti-cheat: this test does NOT carry an allowlist. Adding one would
// re-open the original gap. Every production iteration loop is in scope.
func TestPattern_P1_1_RowsErrCheckedAfterIteration(t *testing.T) {
	root := moduleRoot(t)

	forNextRe := regexp.MustCompile(`^(\s*)for\s+(\w+)\.Next\(\)\s*\{\s*$`)

	type offender struct {
		path string
		line int
		id   string
		why  string
	}
	var offenders []offender

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".build-worktrees" || name == ".force-worktrees" ||
				name == "vendor" || name == ".git" || name == "node_modules" ||
				name == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, rerr := readFile(path)
		if rerr != nil {
			return rerr
		}
		src := string(body)
		lines := strings.Split(src, "\n")
		for i, line := range lines {
			m := forNextRe.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			indent := m[1]
			iter := m[2]
			// Find matching close brace at same indent.
			closeIdx := -1
			closePat := indent + "}"
			for j := i + 1; j < len(lines); j++ {
				if strings.TrimRight(lines[j], " \t") == closePat ||
					strings.HasPrefix(lines[j], closePat+" ") ||
					strings.HasPrefix(lines[j], closePat+"\t") {
					closeIdx = j
					break
				}
			}
			if closeIdx < 0 {
				offenders = append(offenders, offender{
					path: rel(root, path), line: i + 1, id: iter,
					why: "could not locate matching close brace at the for-line indent",
				})
				continue
			}
			// Window: 10 lines past the close brace. Must reference iter.Err().
			// Tightened from 60 → 10 by Fix #8f Track C; the spec said "10
			// lines" and the broader window was masking placement drift.
			windowEnd := closeIdx + 10
			if windowEnd > len(lines) {
				windowEnd = len(lines)
			}
			window := strings.Join(lines[closeIdx:windowEnd], "\n")
			errCall := iter + ".Err()"
			if !strings.Contains(window, errCall) {
				offenders = append(offenders, offender{
					path: rel(root, path), line: i + 1, id: iter,
					why: "no " + errCall + " reference in the 10 lines after the loop close",
				})
				continue
			}
			// Reject silent-discard forms: `_ = <iter>.Err()` with no
			// surrounding observation. Look for the literal pattern in the
			// window — if present and not paired with anything, fail.
			discardPat := "_ = " + errCall
			if strings.Contains(window, discardPat) {
				offenders = append(offenders, offender{
					path: rel(root, path), line: i + 1, id: iter,
					why: "silent-discard `_ = " + errCall + "` is not a meaningful check",
				})
				continue
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	if len(offenders) == 0 {
		return
	}
	sort.Slice(offenders, func(i, j int) bool {
		if offenders[i].path != offenders[j].path {
			return offenders[i].path < offenders[j].path
		}
		return offenders[i].line < offenders[j].line
	})
	t.Errorf("Pattern P1.1 (Fix #8e): %d for-rows.Next() loop(s) in production lack a meaningful rows.Err() check:", len(offenders))
	for _, o := range offenders {
		t.Errorf("  %s:%d — for %s.Next() { ... } — %s", o.path, o.line, o.id, o.why)
	}
	t.Errorf("\nFix: after the closing brace, add `if rErr := %s.Err(); rErr != nil { log.Printf(...) }` or equivalent. See pr_comments.go:ComputePRReviewRollup for the canonical pattern.", "rows")
}
