// D3 P6A.3 — Pattern P26: keyboard shortcut consistency.
//
// Every binding registered in keymap.js MUST appear in help-overlay.html
// and vice-versa. The audit parses both files and asserts the key sets
// agree exactly. Drift between them — a shortcut bound but not
// documented, or documented but not bound — fails the test.
package audittools

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// resolveStaticDir locates internal/dashboard/static relative to wherever
// the test is invoked from. Mirrors the resolution other audit tests use.
func resolveStaticDir(t *testing.T) string {
	t.Helper()
	candidates := []string{
		"../dashboard/static",
		"../../internal/dashboard/static",
		"internal/dashboard/static",
	}
	for _, c := range candidates {
		abs, err := filepath.Abs(c)
		if err != nil {
			continue
		}
		if _, err := os.Stat(abs); err == nil {
			return abs
		}
	}
	t.Fatalf("could not locate internal/dashboard/static directory")
	return ""
}

func TestPattern_P26_KeyboardShortcutConsistency(t *testing.T) {
	dir := resolveStaticDir(t)
	keymap, err := os.ReadFile(filepath.Join(dir, "keymap.js"))
	if err != nil {
		t.Fatalf("read keymap.js: %v", err)
	}
	overlay, err := os.ReadFile(filepath.Join(dir, "help-overlay.html"))
	if err != nil {
		t.Fatalf("read help-overlay.html: %v", err)
	}

	// Extract every `bind('<key>', ...` from keymap.js.
	// Captures "Cmd-1", "/", "?", "Esc", "g p", "Enter", "j", "k", "a", "r", "D"...
	bindRe := regexp.MustCompile(`bind\(\s*'([^']+)'`)
	keymapKeys := keysFromMatches(bindRe.FindAllStringSubmatch(string(keymap), -1))
	if len(keymapKeys) == 0 {
		t.Fatalf("keymap.js: no `bind('...', ...)` calls found — did the format change?")
	}

	// Extract every `data-help-key="<key>"` from help-overlay.html.
	overlayRe := regexp.MustCompile(`data-help-key="([^"]+)"`)
	overlayKeys := keysFromMatches(overlayRe.FindAllStringSubmatch(string(overlay), -1))
	if len(overlayKeys) == 0 {
		t.Fatalf("help-overlay.html: no rows with `data-help-key=` found")
	}

	// In keymap.js we may bind the same key in multiple contexts (e.g.,
	// `j` under briefing-list AND reflection). The help table only needs
	// it once. Dedup before comparing.
	keymapSet := uniqueSet(keymapKeys)
	overlaySet := uniqueSet(overlayKeys)

	missingFromOverlay := setDiff(keymapSet, overlaySet)
	missingFromKeymap := setDiff(overlaySet, keymapSet)

	if len(missingFromOverlay) > 0 {
		t.Errorf("Pattern P26 violation: bindings in keymap.js but not in help-overlay.html:\n  %s\n"+
			"Add a row with data-help-key=\"<key>\" for each, or remove the binding.",
			strings.Join(missingFromOverlay, ", "))
	}
	if len(missingFromKeymap) > 0 {
		t.Errorf("Pattern P26 violation: keys documented in help-overlay.html but not bound in keymap.js:\n  %s\n"+
			"Add a bind('<key>', ...) call for each, or remove the help-overlay row.",
			strings.Join(missingFromKeymap, ", "))
	}
}

func keysFromMatches(matches [][]string) []string {
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if len(m) >= 2 {
			out = append(out, m[1])
		}
	}
	return out
}

func uniqueSet(in []string) map[string]struct{} {
	out := make(map[string]struct{}, len(in))
	for _, s := range in {
		out[s] = struct{}{}
	}
	return out
}

func setDiff(a, b map[string]struct{}) []string {
	var out []string
	for k := range a {
		if _, ok := b[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
