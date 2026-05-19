// Pattern ARCH-002 — unused-exports.
//
// Detects exported Go symbols (top-level Func / Type / Var / Const)
// that have ZERO consumers across the D8 cross-repo dependency graph.
//
// Wiring shape:
//   - The agent that runs the sweep (internal/agents/archaeologist.go)
//     calls SetCrossRepoGraphDB(db) before invoking patterns.All() so
//     this pattern can query CrossRepoSymbols / CrossRepoDependencies.
//     The injection is package-level (mirrors claude.SetTranscriptDB)
//     because the Pattern interface deliberately does not carry a DB
//     handle.
//   - If SetCrossRepoGraphDB has not been called (e.g. legacy callers,
//     fast-path tests), Scan returns nil and logs once. Pattern P9
//     ("operator-gated; never noisy on missing data") forbids
//     emitting findings against a graph the dog hasn't populated yet.
//   - If the graph has been initialised but the current repo's symbols
//     are not yet indexed (`ListCrossRepoSymbolsByRepo` returns empty),
//     Scan returns nil — we cannot know whether a symbol is unused
//     when no consumer scan has run.
//
// Anti-cheat #4 (no dynamic discovery) is upheld — the pattern is
// statically registered in the same registry as the rest.

package patterns

import (
	"database/sql"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"log"
	"strings"
	"sync"

	"force-orchestrator/internal/archaeologist"
	"force-orchestrator/internal/store"
)

// arch002MinSymbolAgeDays is the floor under which a freshly-added
// exported symbol is excluded from the unused-exports check. The dog
// graph-scan and the symbol's first commit may not have completed a
// full cycle yet, so a 0-consumer result is a false-positive risk.
// The check uses CrossRepoSymbols.last_scanned_at as the proxy —
// symbols scanned within the last N days are skipped.
//
// Currently set to 0 (no minimum age) because the graph dog runs every
// 30m and ages out edges in 24h. If false-positive rate from
// just-landed symbols proves too noisy, raise this to 7 or 14.
const arch002MinSymbolAgeDays = 0

var (
	arch002Mu          sync.RWMutex
	arch002GraphDB     *sql.DB
	arch002WarnedNoDB  bool // once-per-process log gate
	arch002WarnedNoIdx bool // once-per-(process,repo) gate would be tighter; process-level is fine for now
)

// SetCrossRepoGraphDB installs the *sql.DB the ARCH-002 pattern uses
// to query the cross-repo dependency graph. Production wires this from
// the archaeologist agent's setup; tests can inject a per-test in-
// memory holocron. Passing nil clears the injection (returns the
// pattern to its "graph unavailable" log-and-skip mode).
func SetCrossRepoGraphDB(db *sql.DB) {
	arch002Mu.Lock()
	defer arch002Mu.Unlock()
	arch002GraphDB = db
	// Reset the once-warned gate so a subsequent re-injection emits a
	// fresh log if the DB is later cleared.
	if db != nil {
		arch002WarnedNoDB = false
	}
}

// crossRepoGraphDB returns the currently injected DB handle (may be
// nil). Exposed as a small accessor so the test for the no-DB log
// gate can reset state without holding the mutex.
func crossRepoGraphDB() *sql.DB {
	arch002Mu.RLock()
	defer arch002Mu.RUnlock()
	return arch002GraphDB
}

type arch002 struct{}

// NewARCH002 returns the ARCH-002 pattern.
func NewARCH002() archaeologist.Pattern { return arch002{} }

func (arch002) ID() string             { return "ARCH-002" }
func (arch002) MinHitsForFeature() int { return 10 }

// Scan walks the repo's .go files for exported top-level symbols and
// queries the cross-repo graph for each. Symbols with zero live
// consumer edges (after soft-delete filtering — ListConsumersOfSymbol
// already excludes tombstoned rows) are emitted as hits.
//
// Operator-gated: if the graph DB has not been injected, returns nil
// with a once-per-process warning. Pattern P9 forbids emitting hits
// against missing data.
func (p arch002) Scan(repo *archaeologist.Repo) []archaeologist.Hit {
	if repo == nil || repo.LocalPath == "" {
		return nil
	}
	db := crossRepoGraphDB()
	if db == nil {
		arch002Mu.Lock()
		if !arch002WarnedNoDB {
			arch002WarnedNoDB = true
			log.Printf("ARCH-002: cross-repo graph DB not injected — skipping (call patterns.SetCrossRepoGraphDB at sweep startup to enable)")
		}
		arch002Mu.Unlock()
		return nil
	}
	// Walk the repo and collect (file, line, symbol) for every
	// exported top-level Go identifier. Build a lookup so each AST
	// symbol resolves to its repo-relative file:line for the Hit.
	symbols := collectExportedSymbols(repo)
	if len(symbols) == 0 {
		return nil
	}

	var hits []archaeologist.Hit
	for _, s := range symbols {
		// Look up the CrossRepoSymbols row. Missing row means the dog
		// hasn't indexed this symbol yet — skip (P9: don't emit
		// against unknown data).
		id, err := store.LookupCrossRepoSymbolID(db, repo.Name, s.qualifiedName)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			// Other lookup errors are transient — log once and
			// continue (don't fail the whole sweep).
			arch002Mu.Lock()
			if !arch002WarnedNoIdx {
				arch002WarnedNoIdx = true
				log.Printf("ARCH-002: LookupCrossRepoSymbolID(%s, %s) failed (continuing): %v", repo.Name, s.qualifiedName, err)
			}
			arch002Mu.Unlock()
			continue
		}
		consumers, err := store.ListConsumersOfSymbol(db, id)
		if err != nil {
			continue
		}
		if len(consumers) > 0 {
			continue
		}
		// Zero live consumers — emit the hit. detail_json records the
		// qualified-symbol name and kind so the migration proposal has
		// a stable reference.
		detail, _ := json.Marshal(map[string]any{
			"symbol_path": s.qualifiedName,
			"symbol_kind": s.kind,
			"language":    "go",
		})
		hits = append(hits, archaeologist.Hit{
			FilePath:   s.relPath,
			LineNumber: s.lineNumber,
			DetailJSON: string(detail),
		})
	}
	return hits
}

// exportedSymbol is the small shape collected from the AST walk.
type exportedSymbol struct {
	relPath       string // repo-relative file path
	lineNumber    int    // 1-indexed source line
	qualifiedName string // e.g. "pkg.FuncName" or "pkg.Type.Method"
	kind          string // "function" | "type" | "var" | "const" | "method"
}

// collectExportedSymbols walks the repo's .go files (skipping _test.go
// and vendored / build directories per walkRepoFiles) and yields every
// exported top-level identifier. The qualified name uses the package
// name (per the source file's `package` declaration) — matching the
// shape the dog uses when populating CrossRepoSymbols.
func collectExportedSymbols(repo *archaeologist.Repo) []exportedSymbol {
	var out []exportedSymbol
	_ = walkRepoFiles(repo.LocalPath, []string{".go"}, func(absPath, relPath string) error {
		if strings.HasSuffix(relPath, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, absPath, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil // skip un-parseable files; pattern P9 says don't emit on bad data
		}
		pkgName := ""
		if f.Name != nil {
			pkgName = f.Name.Name
		}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Name == nil || !d.Name.IsExported() {
					continue
				}
				// Methods on exported receivers are emitted as
				// "pkg.Recv.Method"; functions as "pkg.Func".
				name := d.Name.Name
				kind := "function"
				if d.Recv != nil && len(d.Recv.List) > 0 {
					if recv := exprToTypeName(d.Recv.List[0].Type); recv != "" && ast.IsExported(recv) {
						name = recv + "." + name
						kind = "method"
					} else {
						// Method on unexported type — skip; the
						// containing type isn't part of the public
						// surface so the method isn't either.
						continue
					}
				}
				pos := fset.Position(d.Pos())
				out = append(out, exportedSymbol{
					relPath:       relPath,
					lineNumber:    pos.Line,
					qualifiedName: pkgName + "." + name,
					kind:          kind,
				})
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						if s.Name == nil || !s.Name.IsExported() {
							continue
						}
						pos := fset.Position(s.Pos())
						out = append(out, exportedSymbol{
							relPath:       relPath,
							lineNumber:    pos.Line,
							qualifiedName: pkgName + "." + s.Name.Name,
							kind:          "type",
						})
					case *ast.ValueSpec:
						kind := "var"
						if d.Tok == token.CONST {
							kind = "const"
						}
						for _, name := range s.Names {
							if name == nil || !name.IsExported() {
								continue
							}
							pos := fset.Position(name.Pos())
							out = append(out, exportedSymbol{
								relPath:       relPath,
								lineNumber:    pos.Line,
								qualifiedName: pkgName + "." + name.Name,
								kind:          kind,
							})
						}
					}
				}
			}
		}
		return nil
	})
	return out
}

// exprToTypeName extracts the receiver type name from a *ast.StarExpr
// or *ast.Ident. Returns "" for anonymous / parameterised receivers
// the v1 walker doesn't handle.
func exprToTypeName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		if id, ok := t.X.(*ast.Ident); ok {
			return id.Name
		}
		// Generic receivers like *T[U] arrive as *ast.IndexExpr —
		// extract the base identifier.
		if idx, ok := t.X.(*ast.IndexExpr); ok {
			if id, ok2 := idx.X.(*ast.Ident); ok2 {
				return id.Name
			}
		}
	case *ast.IndexExpr:
		if id, ok := t.X.(*ast.Ident); ok {
			return id.Name
		}
	}
	return ""
}

// lookupCrossRepoConsumers is the legacy seam kept for the
// D8-merge-gate test (which pins it returns -1 in stub mode). With
// the D8 graph live, the call has been folded into Scan above; this
// remains as the "graph unavailable" sentinel only.
//
//nolint:unused // referenced by D8-merge-gate test only.
func lookupCrossRepoConsumers(repoName, symbolFQN string) int {
	_ = repoName
	_ = symbolFQN
	return -1
}

func init() { Register(NewARCH002()) }
