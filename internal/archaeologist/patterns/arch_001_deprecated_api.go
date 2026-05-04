// Pattern ARCH-001 — deprecated-API.
//
// Per-language list of deprecated identifiers; each language gets its
// own per-extension scan. Anti-cheat #2 (language-aware): a Go API
// list MUST NOT scan Rust files. The TestARCH001_LanguageAware test
// pins this — adding a deprecated-Go-API entry and asserting the
// scanner returns zero hits on a .rs file.
//
// v1 ships Go-only because:
//   - The shakedown repo (force-orchestrator) is Go-only.
//   - Other-language lists land per-deliverable as those repos onboard.
//
// The scan is line-based (substring match against the symbol). Future
// iterations can promote this to AST parsing per-language; the
// substring shape is the v1 floor — false positives on usages inside
// comments or string literals are acceptable in v1 because the
// migration pipeline is operator-gated (anti-cheat #1: operator
// ratifies).

package patterns

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"

	"force-orchestrator/internal/archaeologist"
)

// arch001DeprecatedSymbols is the per-language deprecated-API table.
// Each entry: extension → list of deprecated symbols (case-sensitive
// substring match against each line). Adding a new deprecated symbol
// is a one-line edit; pattern test coverage is a single happy-path.
var arch001DeprecatedSymbols = map[string][]string{
	".go": {
		// io/ioutil was deprecated in Go 1.16 in favour of io and os.
		"ioutil.ReadFile",
		"ioutil.WriteFile",
		"ioutil.ReadAll",
		"ioutil.ReadDir",
		"ioutil.TempDir",
		"ioutil.TempFile",
		"ioutil.NopCloser",
		"ioutil.Discard",
	},
}

type arch001 struct{}

// NewARCH001 returns the ARCH-001 pattern. Exported for the test
// suite; production registration happens via the init() below.
func NewARCH001() archaeologist.Pattern { return arch001{} }

func (arch001) ID() string             { return "ARCH-001" }
func (arch001) MinHitsForFeature() int { return 5 }

func (p arch001) Scan(repo *archaeologist.Repo) []archaeologist.Hit {
	if repo == nil || repo.LocalPath == "" {
		return nil
	}
	var hits []archaeologist.Hit
	// One walk per (extension, list) — the walker filters by extension
	// so a Go list never opens a Rust file. This is the language-aware
	// guarantee.
	for ext, symbols := range arch001DeprecatedSymbols {
		_ = walkRepoFiles(repo.LocalPath, []string{ext}, func(absPath, relPath string) error {
			f, err := os.Open(absPath)
			if err != nil {
				return nil
			}
			defer f.Close()
			scanner := bufio.NewScanner(f)
			scanner.Buffer(make([]byte, 64*1024), 1024*1024)
			lineNum := 0
			for scanner.Scan() {
				lineNum++
				line := scanner.Text()
				for _, sym := range symbols {
					if !strings.Contains(line, sym) {
						continue
					}
					detail, _ := json.Marshal(map[string]string{
						"deprecated_symbol": sym,
						"language":          extToLanguage(ext),
					})
					hits = append(hits, archaeologist.Hit{
						FilePath:   relPath,
						LineNumber: lineNum,
						DetailJSON: string(detail),
					})
				}
			}
			return nil
		})
	}
	return hits
}

// extToLanguage maps a file extension to a human-readable language
// label for the detail_json record. Kept tiny — the caller only ever
// emits an extension we've already opted into above.
func extToLanguage(ext string) string {
	switch strings.ToLower(ext) {
	case ".go":
		return "go"
	case ".rs":
		return "rust"
	case ".py":
		return "python"
	case ".ts", ".tsx":
		return "typescript"
	case ".js", ".jsx":
		return "javascript"
	default:
		return strings.TrimPrefix(ext, ".")
	}
}

func init() { Register(NewARCH001()) }
