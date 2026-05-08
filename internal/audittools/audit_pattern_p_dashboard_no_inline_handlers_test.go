// Package audittools: Pattern P_DashboardNoInlineHandlers — the dashboard
// SPA must contain ZERO inline event-handler attributes (`onclick="..."`,
// `onchange="..."`, `oninput="..."`, etc.) in any of its static assets:
//
//   - internal/dashboard/static/index.html      (top-level shell)
//   - internal/dashboard/static/help-overlay.html
//   - internal/dashboard/static/app.js          (renderer; inline handlers
//                                                inside JS template strings
//                                                are produced as inline
//                                                attributes when the
//                                                template is interpolated,
//                                                so they count too)
//   - internal/dashboard/static/keymap.js
//
// Why an audit guard? The dashboard's CSP is `script-src 'self'`. Inline
// event handlers (`onclick="foo()"`) are treated by the browser as inline
// script and blocked under that policy — even though they live in HTML
// attributes, they execute JS on the document.
//
// On 2026-05-05 the SPA carried 122+ inline handlers in index.html alone.
// A hot-fix at d54cb2a temporarily added `'unsafe-inline'` to script-src
// to keep the dashboard usable; Sweep B refactored every handler to use a
// delegated dispatcher driven by `data-action` / `data-arg` attributes so
// `'unsafe-inline'` could be removed.
//
// This pattern is the regression guard. A future copy-paste of an inline
// handler ("just one") would silently re-break the CSP — the CSP-violation
// console message is easy to miss because the rest of the SPA still works.
// CI failure here surfaces it on the PR instead.
//
// Detection rule: any `\bon[a-z]+="` outside of:
//   - HTML/JS comments (`<!-- onclick="..." -->`, `// onclick="..."`)
//   - JS string literals that document the legacy form (e.g. dispatcher
//     comment block referring to the prior inline handler shape)
//
// We're conservative: the regex matches `on<word>="` literally. Comments
// are stripped before matching. The pattern is not foolproof (it can't
// detect `el.setAttribute('onclick', '…')` — but that wasn't part of the
// SPA before either, and Pattern P10 covers DOM injection sinks).
package audittools

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// dashboardStaticAssetsToScan returns the list of dashboard SPA assets
// that must be inline-handler-free, relative to the repo root.
func dashboardStaticAssetsToScan() []string {
	return []string{
		"internal/dashboard/static/index.html",
		"internal/dashboard/static/help-overlay.html",
		"internal/dashboard/static/app.js",
		"internal/dashboard/static/keymap.js",
	}
}

// inlineHandlerRe matches `on<word>="` (the start of any HTML inline
// event-handler attribute). Anchored with a word boundary so we don't
// match `pylon="...` or similar non-handler attribute names. The
// attribute names we care about are HTML standard events — onclick,
// onchange, oninput, onsubmit, etc. — but the more permissive form
// catches anything new browsers introduce too.
var inlineHandlerRe = regexp.MustCompile(`\bon[a-z]+="`)

// scanDashboardForInlineHandlers walks each known SPA asset and returns
// the list of (file, line, snippet) hits. Comments are stripped first
// so reference-only mentions in the dispatcher's doc block don't trip
// the audit.
func scanDashboardForInlineHandlers(rootDir string) ([]string, error) {
	var hits []string
	for _, rel := range dashboardStaticAssetsToScan() {
		full := filepath.Join(rootDir, rel)
		raw, err := os.ReadFile(full)
		if err != nil {
			// Missing files are a hard error — we want CI to flag a
			// reorg that drops one of the SPA assets without updating
			// this list (otherwise the new file goes un-audited).
			return nil, fmt.Errorf("read %s: %w", full, err)
		}
		stripped := stripCommentsForHandlerScan(rel, string(raw))
		for i, line := range strings.Split(stripped, "\n") {
			if loc := inlineHandlerRe.FindStringIndex(line); loc != nil {
				hits = append(hits, fmt.Sprintf("%s:%d  %s", rel, i+1, strings.TrimSpace(line)))
			}
		}
	}
	return hits, nil
}

// stripCommentsForHandlerScan removes comments from the source text before
// the inline-handler regex runs. We strip:
//
//   - `<!-- ... -->` blocks (HTML comments — multi-line)
//   - `// ...` to end-of-line (JS line comments)
//   - `/* ... */` blocks (JS block comments — multi-line)
//
// We replace each comment span with whitespace of equivalent length so
// line numbers stay aligned (the `i+1` in the caller maps to the original
// file).
//
// kind is determined from the file extension. For .html we strip HTML
// comments; for .js we strip both flavors of JS comment.
func stripCommentsForHandlerScan(rel, src string) string {
	if strings.HasSuffix(rel, ".html") {
		return stripHTMLComments(src)
	}
	if strings.HasSuffix(rel, ".js") {
		return stripJSComments(src)
	}
	return src
}

// stripHTMLComments replaces every `<!-- ... -->` span with whitespace
// of identical length (newlines preserved so line numbers don't shift).
func stripHTMLComments(s string) string {
	out := []byte(s)
	for {
		i := strings.Index(string(out), "<!--")
		if i < 0 {
			break
		}
		j := strings.Index(string(out[i+4:]), "-->")
		if j < 0 {
			break
		}
		end := i + 4 + j + 3
		for k := i; k < end; k++ {
			if out[k] != '\n' {
				out[k] = ' '
			}
		}
	}
	return string(out)
}

// stripJSComments replaces JS line comments (`// ...`) and block comments
// (`/* ... */`) with whitespace of identical length, preserving newlines.
// We do NOT strip strings first — instead, we track when we're inside a
// "string-like" region (single quote, double quote, or template literal)
// and skip comment detection while inside. That's enough for the SPA's
// straightforward shape; it does not handle every pathological JS
// (e.g. regex literals that contain `//` chars) but the SPA has no such
// regexes near inline-handler-shaped strings.
func stripJSComments(s string) string {
	out := []byte(s)
	n := len(out)
	i := 0
	inStr := byte(0) // 0 = not in string; otherwise the closing quote
	for i < n {
		c := out[i]
		if inStr != 0 {
			if c == '\\' && i+1 < n {
				i += 2
				continue
			}
			if c == inStr {
				inStr = 0
			}
			i++
			continue
		}
		// Not in string: check for string start.
		if c == '\'' || c == '"' || c == '`' {
			inStr = c
			i++
			continue
		}
		// Check for line comment.
		if c == '/' && i+1 < n && out[i+1] == '/' {
			// Replace through end-of-line.
			j := i
			for j < n && out[j] != '\n' {
				out[j] = ' '
				j++
			}
			i = j
			continue
		}
		// Check for block comment.
		if c == '/' && i+1 < n && out[i+1] == '*' {
			j := i + 2
			for j+1 < n && !(out[j] == '*' && out[j+1] == '/') {
				if out[j] != '\n' {
					out[j] = ' '
				}
				j++
			}
			if j+1 < n {
				out[j] = ' '
				out[j+1] = ' '
				j += 2
			}
			// Replace the leading "/*" too.
			out[i] = ' '
			out[i+1] = ' '
			i = j
			continue
		}
		i++
	}
	return string(out)
}

// TestPattern_P_DashboardNoInlineHandlers is the live audit run.
func TestPattern_P_DashboardNoInlineHandlers(t *testing.T) {
	hits, err := scanDashboardForInlineHandlers(moduleRoot(t))
	if err != nil {
		t.Fatalf("scan failed: %v", err)
	}
	if len(hits) > 0 {
		t.Errorf("Pattern P_DashboardNoInlineHandlers: %d inline event-handler attribute(s) found in the dashboard SPA. "+
			"Inline handlers (onclick=\"...\", onchange=\"...\", oninput=\"...\", etc.) violate the strict CSP "+
			"`script-src 'self'`. Refactor each to use the delegated dispatcher in app.js: "+
			"`<el data-action=\"<funcName>\" data-arg=\"<single-arg>\">` or `data-args='[...]'` for multiple args. "+
			"For non-click events add data-event=\"input|change|submit|contextmenu\". "+
			"Hits:\n  %s", len(hits), strings.Join(hits, "\n  "))
	}
}

// TestPattern_P_DashboardNoInlineHandlers_DetectsInjectedDrift proves the
// regex actually fires when an inline handler is reintroduced. We build
// a synthetic SPA tree under t.TempDir() that mirrors
// internal/dashboard/static/, drop a single offending attribute into
// each scannable file, and assert the scanner reports it.
func TestPattern_P_DashboardNoInlineHandlers_DetectsInjectedDrift(t *testing.T) {
	type drift struct {
		name      string
		file      string // relative path under static/
		bodyClean string
		bodyDrift string
		wantHit   bool
	}
	cases := []drift{
		{
			name:      "index.html-clean",
			file:      "internal/dashboard/static/index.html",
			bodyClean: "<!doctype html><body><button data-action=\"foo\">F</button></body>",
			bodyDrift: "<!doctype html><body><button data-action=\"foo\">F</button></body>",
			wantHit:   false,
		},
		{
			name:      "index.html-drift-onclick",
			file:      "internal/dashboard/static/index.html",
			bodyClean: "<!doctype html><body></body>",
			bodyDrift: "<!doctype html><body><button onclick=\"foo()\">X</button></body>",
			wantHit:   true,
		},
		{
			name:      "index.html-drift-oninput",
			file:      "internal/dashboard/static/index.html",
			bodyClean: "<!doctype html><body></body>",
			bodyDrift: "<!doctype html><body><input oninput=\"bar()\"></body>",
			wantHit:   true,
		},
		{
			name:      "index.html-comment-not-drift",
			file:      "internal/dashboard/static/index.html",
			bodyClean: "<!doctype html><body></body>",
			// Inline handler INSIDE an HTML comment must NOT trip the
			// audit — comments don't execute. Otherwise the dispatcher's
			// doc-block (which references the legacy form) would fail.
			bodyDrift: "<!doctype html><body><!-- legacy: onclick=\"old()\" --></body>",
			wantHit:   false,
		},
		{
			name:      "app.js-drift-template-onclick",
			file:      "internal/dashboard/static/app.js",
			bodyClean: "function r() { return '<button data-action=\"f\">x</button>'; }",
			// Inline handler inside a JS string literal — the template
			// renders to inline HTML, which the browser treats as inline
			// script under CSP. MUST trip the audit.
			bodyDrift: "function r() { return '<button onclick=\"f()\">x</button>'; }",
			wantHit:   true,
		},
		{
			name:      "app.js-line-comment-not-drift",
			file:      "internal/dashboard/static/app.js",
			bodyClean: "var x = 1;",
			bodyDrift: "var x = 1; // historical: onclick=\"foo()\" used to be wired here",
			wantHit:   false,
		},
		{
			name:      "app.js-block-comment-not-drift",
			file:      "internal/dashboard/static/app.js",
			bodyClean: "var x = 1;",
			bodyDrift: "/*\n * old form was: <button onclick=\"foo()\">F</button>\n */ var x = 1;",
			wantHit:   false,
		},
		{
			name:      "help-overlay.html-drift",
			file:      "internal/dashboard/static/help-overlay.html",
			bodyClean: "<div></div>",
			bodyDrift: "<div onclick=\"close()\"></div>",
			wantHit:   true,
		},
		{
			name:      "keymap.js-drift",
			file:      "internal/dashboard/static/keymap.js",
			bodyClean: "var k = 1;",
			bodyDrift: "var s = '<button onclick=\"hit()\">k</button>';",
			wantHit:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tempRoot := t.TempDir()
			// Lay down the four expected files with clean bodies, then
			// mutate the one under test.
			for _, rel := range dashboardStaticAssetsToScan() {
				full := filepath.Join(tempRoot, rel)
				if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
					t.Fatalf("mkdirall %s: %v", full, err)
				}
				body := "// clean\n"
				if strings.HasSuffix(rel, ".html") {
					body = "<!doctype html>\n"
				}
				if rel == tc.file {
					body = tc.bodyDrift
				}
				if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
					t.Fatalf("write %s: %v", full, err)
				}
			}
			hits, err := scanDashboardForInlineHandlers(tempRoot)
			if err != nil {
				t.Fatalf("scan: %v", err)
			}
			if tc.wantHit && len(hits) == 0 {
				t.Fatalf("scanner missed inline handler in %s; drift=%q", tc.file, tc.bodyDrift)
			}
			if !tc.wantHit && len(hits) > 0 {
				t.Fatalf("scanner false-positive on clean tree (%s): %v", tc.name, hits)
			}
		})
	}

	// Positive control: a fully-clean synthetic tree must report zero
	// hits. Catches a regex that's too greedy / strips too aggressively.
	tempRoot := t.TempDir()
	for _, rel := range dashboardStaticAssetsToScan() {
		full := filepath.Join(tempRoot, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdirall %s: %v", full, err)
		}
		body := "// clean\n"
		if strings.HasSuffix(rel, ".html") {
			body = "<!doctype html><body><button data-action=\"x\">x</button></body>\n"
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}
	hits, err := scanDashboardForInlineHandlers(tempRoot)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(hits) > 0 {
		t.Fatalf("scanner reported hits on a clean tree: %v", hits)
	}

	// Negative control: a missing asset is a hard error (we want the
	// audit to fail loudly if a refactor drops one of the SPA files
	// without updating this list, otherwise the new file goes un-scanned).
	tempRoot2 := t.TempDir()
	if _, err := scanDashboardForInlineHandlers(tempRoot2); err == nil {
		t.Fatalf("scanner accepted empty tree without reporting missing files; want error")
	}
}
