// Package audittools: Pattern P_DashboardModalPattern — every modal-style
// container in the dashboard SPA MUST use the canonical .modal-backdrop +
// .hidden class pattern (style.css:573), NOT inline display:flex with the
// HTML hidden attribute.
//
// Why an audit guard? Twice in production this same antipattern was
// reported by the operator as a "modal stuck open / blocking clicks" bug:
//
//   - commit d58860a (saved-filter modals): the inline display:flex on a
//     <div hidden style="...display:flex...> wins over the UA rule
//     [hidden] { display: none }, so the modal was always-on overlay.
//   - commit 50fd5d8 (notification modals): same shape, different ids.
//
// Both fixes were surface fixes (touched the JS to set
// modal.style.display='none' alongside modal.hidden=true). The
// fundamental issue is that the markup pattern itself is wrong — the
// canonical .modal-backdrop class already encapsulates the entire
// "centered fixed overlay" concern, and .hidden on it forces display:none.
// Mixing the two patterns is the bug.
//
// What this test does:
//
//  1. Reads internal/dashboard/static/index.html.
//  2. Finds every <div ... id="..."> that LOOKS like a modal container,
//     defined as: id ends in "-modal" OR the inline style contains
//     position:fixed.
//  3. For each, asserts the element does NOT have inline `display:` in
//     its style attribute, and DOES carry the modal-backdrop class. The
//     class is the single source of truth; inline display: would override
//     .hidden's display:none and resurrect the bug.
//
// Sibling _DetectsInjectedDrift sentinel proves the check is load-bearing
// by feeding it synthetic HTML with each prohibited shape.
package audittools

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// modalDivRe matches an opening <div ...> tag. We deliberately scan
// every div and post-filter on id/style — id="...-modal" naming is the
// strongest signal a div is a modal container. We also inspect inline
// styles for position:fixed, because two of the historically-buggy
// modals use that as the "I am an overlay" tell even before the id
// suffix convention took hold.
var modalDivRe = regexp.MustCompile(`<div\b[^>]*>`)

// inlineStyleRe extracts the value of a style="..." attribute on a tag.
var inlineStyleRe = regexp.MustCompile(`\sstyle\s*=\s*"([^"]*)"`)

// idAttrRe extracts the value of an id="..." attribute.
var idAttrRe = regexp.MustCompile(`\sid\s*=\s*"([^"]*)"`)

// classAttrRe extracts the value of a class="..." attribute.
var classAttrRe = regexp.MustCompile(`\sclass\s*=\s*"([^"]*)"`)

// displayInStyleRe matches a CSS `display:` property inside an inline
// style value (whitespace-tolerant). We use this rather than a naive
// substring "display:" match because inline styles can have spaces.
var displayInStyleRe = regexp.MustCompile(`(?i)\bdisplay\s*:`)

// looksLikeModal reports whether a parsed <div> tag (id, style attrs)
// fits the heuristic for a modal-style overlay. Two signals trigger:
//
//   - the id ends in "-modal" (e.g. add-modal, notif-dnd-modal,
//     saved-filter-save-modal) — the dashboard naming convention.
//   - the inline style contains `position:fixed` — historically the
//     antipattern modals used this with a manually-rolled fixed-overlay
//     style block instead of the .modal-backdrop class.
//
// reflection-pane / banner divs use neither tell and are correctly
// excluded.
func looksLikeModal(id, style string) bool {
	if strings.HasSuffix(id, "-modal") {
		return true
	}
	if strings.Contains(strings.ReplaceAll(strings.ToLower(style), " ", ""), "position:fixed") {
		return true
	}
	return false
}

// checkDashboardModalPattern walks the index.html under rootDir and
// returns a non-nil error on the first modal-shaped div that violates
// the canonical pattern. Returns nil when every modal is on the
// .modal-backdrop class (no inline display:).
func checkDashboardModalPattern(rootDir string) error {
	htmlPath := filepath.Join(rootDir, "internal", "dashboard", "static", "index.html")
	bytesHTML, err := os.ReadFile(htmlPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", htmlPath, err)
	}
	html := string(bytesHTML)

	for _, tag := range modalDivRe.FindAllString(html, -1) {
		idMatch := idAttrRe.FindStringSubmatch(tag)
		styleMatch := inlineStyleRe.FindStringSubmatch(tag)
		classMatch := classAttrRe.FindStringSubmatch(tag)

		id := ""
		if len(idMatch) == 2 {
			id = idMatch[1]
		}
		style := ""
		if len(styleMatch) == 2 {
			style = styleMatch[1]
		}
		class := ""
		if len(classMatch) == 2 {
			class = classMatch[1]
		}

		if !looksLikeModal(id, style) {
			continue
		}

		// (1) Inline display: is forbidden — it would override
		//     .modal-backdrop.hidden { display: none } and resurrect
		//     the recurring "modal stuck open" operator bug.
		if style != "" && displayInStyleRe.MatchString(style) {
			return fmt.Errorf(
				"modal #%s uses inline style 'display:...' instead of the canonical .modal-backdrop class — see style.css:582",
				id,
			)
		}

		// (2) Modal-shaped divs MUST carry the modal-backdrop class.
		//     A modal that is missing the class entirely (but has a
		//     -modal suffix or position:fixed inline) is the same
		//     historical antipattern in disguise.
		hasBackdrop := false
		for _, c := range strings.Fields(class) {
			if c == "modal-backdrop" {
				hasBackdrop = true
				break
			}
		}
		if !hasBackdrop {
			return fmt.Errorf(
				"modal #%s is missing the canonical .modal-backdrop class — see style.css:573",
				id,
			)
		}
	}
	return nil
}

func TestPattern_P_DashboardModalPattern(t *testing.T) {
	if err := checkDashboardModalPattern(moduleRoot(t)); err != nil {
		t.Errorf("Pattern P_DashboardModalPattern: %v", err)
	}
}

// TestPattern_P_DashboardModalPattern_DetectsInjectedDrift proves the
// checker is load-bearing: for each prohibited shape, we synthesize a
// minimal index.html under t.TempDir(), run the checker, and assert it
// fires with the expected error fragment. If the checker stopped
// caring about one of these shapes, this sentinel goes red — which is
// exactly the antipattern-recurrence guarantee Sweep A buys.
func TestPattern_P_DashboardModalPattern_DetectsInjectedDrift(t *testing.T) {
	cases := []struct {
		name     string
		divLine  string
		wantFrag string
	}{
		{
			name:     "inline_display_flex_with_modal_id_suffix",
			divLine:  `<div id="bogus-modal" hidden style="position:fixed;top:0;left:0;right:0;bottom:0;display:flex"></div>`,
			wantFrag: "uses inline style 'display:...'",
		},
		{
			name:     "inline_display_none_with_modal_id_suffix",
			divLine:  `<div id="bogus-modal" class="modal-backdrop" style="display:none"></div>`,
			wantFrag: "uses inline style 'display:...'",
		},
		{
			name:     "position_fixed_no_modal_id_still_caught",
			divLine:  `<div id="overlay-thing" class="modal-backdrop" style="position:fixed;display:flex"></div>`,
			wantFrag: "uses inline style 'display:...'",
		},
		{
			name:     "modal_id_missing_backdrop_class",
			divLine:  `<div id="naked-modal" hidden></div>`,
			wantFrag: "missing the canonical .modal-backdrop class",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			staticDir := filepath.Join(tmp, "internal", "dashboard", "static")
			if err := os.MkdirAll(staticDir, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			htmlPath := filepath.Join(staticDir, "index.html")
			body := "<!doctype html><html><body>\n" + tc.divLine + "\n</body></html>\n"
			if err := os.WriteFile(htmlPath, []byte(body), 0o644); err != nil {
				t.Fatalf("write index.html: %v", err)
			}

			err := checkDashboardModalPattern(tmp)
			if err == nil {
				t.Fatalf("expected violation for %s, got nil", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantFrag) {
				t.Errorf("error %q missing expected fragment %q", err.Error(), tc.wantFrag)
			}
		})
	}

	// Positive control: a single canonical modal MUST pass.
	t.Run("canonical_modal_passes", func(t *testing.T) {
		tmp := t.TempDir()
		staticDir := filepath.Join(tmp, "internal", "dashboard", "static")
		if err := os.MkdirAll(staticDir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		htmlPath := filepath.Join(staticDir, "index.html")
		body := `<!doctype html><html><body>
<div id="canonical-modal" class="modal-backdrop hidden">
  <div class="modal-box">ok</div>
</div>
</body></html>
`
		if err := os.WriteFile(htmlPath, []byte(body), 0o644); err != nil {
			t.Fatalf("write index.html: %v", err)
		}
		if err := checkDashboardModalPattern(tmp); err != nil {
			t.Fatalf("canonical modal should pass, got: %v", err)
		}
	})
}
