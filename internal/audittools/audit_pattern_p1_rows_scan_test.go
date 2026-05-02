// Package audittools: pattern test for rows.Scan error checking.
package audittools

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestPattern_P1_RowsScanErrorsChecked is the grep-based regression guard
// for the Fix #8d rows.Scan sweep (AUDIT-090, AUDIT-091, AUDIT-094,
// AUDIT-095, AUDIT-100). Every *.Scan( call inside a `for <name>.Next()`
// loop in production code (non-_test.go) must either:
//
//  1. Be part of an `if err := <name>.Scan(...); err != nil` error-check
//     form (the error is named and observed), OR
//  2. Live inside a line that starts with `if <name>.Scan(...)` pattern
//     used as a boolean gate (legacy but non-silent), OR
//  3. Live inside a helper function that wraps the scan and returns error
//     to the caller (captured in the caller's scope).
//
// The test walks every production *.go file, extracts `for <rows>.Next()`
// loop bodies, and asserts the subsequent Scan on that identifier is
// error-checked.
func TestPattern_P1_RowsScanErrorsChecked(t *testing.T) {
	root := moduleRoot(t)

	// for <name>.Next() opens a loop; we then look for <name>.Scan( within
	// the same ~20 lines.
	forNextRe := regexp.MustCompile(`for\s+(\w+)\.Next\(\)\s*\{`)
	type offender struct {
		path string
		line int
		id   string
	}
	var offenders []offender

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".fix-worktrees" || name == ".force-worktrees" || name == ".claude" ||
				name == ".build-worktrees" || name == "vendor" || name == ".git" ||
				name == "node_modules" || name == "testdata" {
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
			iter := m[1]
			// Window: from the for-line up to 25 lines forward (covers
			// the scan call + any inner helpers).
			end := i + 25
			if end > len(lines) {
				end = len(lines)
			}
			windowLines := lines[i:end]
			// Find the first <iter>.Scan( in the window.
			scanCall := iter + ".Scan("
			var scanLineIdx = -1
			for j, l := range windowLines {
				if strings.Contains(l, scanCall) {
					scanLineIdx = j
					break
				}
			}
			if scanLineIdx < 0 {
				continue
			}
			scanLine := windowLines[scanLineIdx]
			// Acceptable forms:
			// 1. `if err := <iter>.Scan(...); err != nil`
			// 2. `if sErr := <iter>.Scan(...); sErr != nil`
			// 3. `if ... := <iter>.Scan(...); ... != nil` (any named error)
			// 4. Bool test: `if <iter>.Scan(...) == nil` / `!= nil`
			// Unacceptable:
			// 5. Bare `<iter>.Scan(...)` with no error check on same line
			// 6. `_ = <iter>.Scan(...)` with no deferral comment
			trimmed := strings.TrimSpace(scanLine)
			if strings.HasPrefix(trimmed, "if err :=") ||
				strings.HasPrefix(trimmed, "if sErr :=") ||
				strings.HasPrefix(trimmed, "if rErr :=") ||
				strings.HasPrefix(trimmed, "if scanErr :=") ||
				strings.HasPrefix(trimmed, "if e :=") {
				continue
			}
			// Boolean gate: `if <iter>.Scan(...) == nil {` pattern.
			if strings.HasPrefix(trimmed, "if "+iter+".Scan(") {
				continue
			}
			// Multi-var pattern: err := <iter>.Scan(...); if err != nil
			// Look at scanLine + next line.
			nextLine := ""
			if scanLineIdx+1 < len(windowLines) {
				nextLine = strings.TrimSpace(windowLines[scanLineIdx+1])
			}
			if strings.HasPrefix(trimmed, "err :=") ||
				strings.HasPrefix(trimmed, "err = ") ||
				strings.Contains(trimmed, ", err := "+iter+".Scan(") {
				if strings.HasPrefix(nextLine, "if err") {
					continue
				}
			}
			// Check for deferral comment directly above.
			if scanLineIdx > 0 {
				prev := strings.TrimSpace(windowLines[scanLineIdx-1])
				if strings.Contains(prev, "deferral-comment(Fix #8b)") {
					continue
				}
			}
			// Offender.
			offenders = append(offenders, offender{
				path: rel(root, path),
				line: i + 1 + scanLineIdx,
				id:   iter,
			})
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
	t.Errorf("Pattern P1 (Fix #8d): %d rows.Scan call(s) inside a for-.Next() loop are not error-checked:", len(offenders))
	for _, o := range offenders {
		t.Errorf("  %s:%d — %s.Scan(...) in for %s.Next() { ... } has no `if err := ...; err != nil` guard",
			o.path, o.line, o.id, o.id)
	}
	t.Errorf("\nFix: capture the error and log (or continue) when it fires. See dogs.go's dogGitHygiene for the canonical pattern.")
}

func readFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}
