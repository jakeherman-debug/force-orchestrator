// Package patterns — shared file-walking helpers used by every
// Pattern implementation. Centralised so the language-aware skip
// behaviour (anti-cheat #2: language-aware patterns) lands once.
package patterns

import (
	"io/fs"
	"path/filepath"
	"strings"
)

// skipDirs lists directory names that NO pattern should descend into.
// Vendored / build-artefact / VCS state — scanning these would
// produce noise hits unrelated to the repo's first-party code.
var skipDirs = map[string]struct{}{
	".git":         {},
	".hg":          {},
	".svn":         {},
	"node_modules": {},
	"vendor":       {},
	"target":       {}, // Rust / Java build dirs
	"dist":         {},
	"build":        {},
	".idea":        {},
	".vscode":      {},
	".d7-worktrees": {}, // D7 nested worktrees — never descend
}

// walkRepoFiles walks repoRoot, invoking fn(absPath, relPath) on every
// regular file whose extension is in extensions (lowercased, leading
// dot included — e.g. ".go"). Directories in skipDirs are pruned. fn
// returns an error to abort the walk; nil to continue.
//
// Hidden files (basename starts with ".") are also skipped — debt
// patterns operate on first-party source, not editor backup files or
// dotfiles.
//
// extensions is a slice (not a map) so callers can pass nil to mean
// "match every file"; an empty slice has the same meaning. Pattern
// implementations should always pass a concrete extension list to
// keep the language-aware contract intact.
func walkRepoFiles(repoRoot string, extensions []string, fn func(absPath, relPath string) error) error {
	extSet := map[string]struct{}{}
	for _, ext := range extensions {
		extSet[strings.ToLower(ext)] = struct{}{}
	}
	return filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // tolerate transient FS errors per-entry
		}
		if d.IsDir() {
			name := d.Name()
			if _, skip := skipDirs[name]; skip {
				return filepath.SkipDir
			}
			// Skip hidden dirs (anything other than the root that
			// starts with "."). filepath.WalkDir always calls us
			// with the root first; that root has name == filepath.Base(repoRoot).
			if strings.HasPrefix(name, ".") && path != repoRoot {
				return filepath.SkipDir
			}
			return nil
		}
		base := d.Name()
		if strings.HasPrefix(base, ".") {
			return nil
		}
		if len(extSet) > 0 {
			ext := strings.ToLower(filepath.Ext(base))
			if _, ok := extSet[ext]; !ok {
				return nil
			}
		}
		rel, relErr := filepath.Rel(repoRoot, path)
		if relErr != nil {
			rel = path
		}
		return fn(path, rel)
	})
}
