// Pattern ARCH-003 — duplicate-abstractions.
//
// Detects functions that share a structural shape — same set of
// parameter type kinds, same return-type kinds, and a similar body
// signature. v1 implementation: per-Go-file AST parse, per-function
// fingerprint = (parameter-types, result-types, statement-kind histogram).
// Functions with the same fingerprint across files (or in different
// packages within the same repo) are surfaced as candidates for
// abstraction unification.
//
// Language-aware (anti-cheat #2): Go-only — the AST parser only opens
// .go files. A Rust-equivalent would land as ARCH-003-rust later.

package patterns

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"

	"force-orchestrator/internal/archaeologist"
)

type arch003 struct{}

// NewARCH003 returns the ARCH-003 pattern.
func NewARCH003() archaeologist.Pattern { return arch003{} }

func (arch003) ID() string             { return "ARCH-003" }
func (arch003) MinHitsForFeature() int { return 4 } // 2 pairs == 4 sites is the minimum interesting cluster

// arch003Func captures one detected function for fingerprinting.
type arch003Func struct {
	relPath  string
	line     int
	funcName string
	hash     string
}

func (p arch003) Scan(repo *archaeologist.Repo) []archaeologist.Hit {
	if repo == nil || repo.LocalPath == "" {
		return nil
	}
	var fns []arch003Func
	_ = walkRepoFiles(repo.LocalPath, []string{".go"}, func(absPath, relPath string) error {
		// Skip _test.go files — duplicate test helpers are normal and
		// not actionable as a debt-migration candidate.
		if strings.HasSuffix(relPath, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, absPath, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			h := arch003Fingerprint(fn)
			pos := fset.Position(fn.Pos())
			fns = append(fns, arch003Func{
				relPath:  relPath,
				line:     pos.Line,
				funcName: fn.Name.Name,
				hash:     h,
			})
		}
		return nil
	})

	// Group by hash; emit a Hit for each function in any cluster of
	// size >= 2.
	byHash := map[string][]arch003Func{}
	for _, fn := range fns {
		byHash[fn.hash] = append(byHash[fn.hash], fn)
	}
	var hits []archaeologist.Hit
	for hash, cluster := range byHash {
		if len(cluster) < 2 {
			continue
		}
		// Stable order for deterministic output.
		sort.Slice(cluster, func(i, j int) bool {
			if cluster[i].relPath != cluster[j].relPath {
				return cluster[i].relPath < cluster[j].relPath
			}
			return cluster[i].line < cluster[j].line
		})
		peers := make([]string, 0, len(cluster))
		for _, fn := range cluster {
			peers = append(peers, fmt.Sprintf("%s:%d", fn.relPath, fn.line))
		}
		for _, fn := range cluster {
			detail, _ := json.Marshal(map[string]any{
				"signature_hash": hash,
				"function_name":  fn.funcName,
				"cluster_size":   len(cluster),
				"peers":          peers,
				"language":       "go",
			})
			hits = append(hits, archaeologist.Hit{
				FilePath:   fn.relPath,
				LineNumber: fn.line,
				DetailJSON: string(detail),
			})
		}
	}
	return hits
}

// arch003Fingerprint returns a structural-shape hash for a Go function
// declaration. The hash captures:
//   - parameter type-expression strings, in order
//   - result type-expression strings, in order
//   - top-level statement kind histogram (e.g. *ast.IfStmt:2, *ast.ForStmt:1)
//
// Hash collisions are acceptable — patterns are operator-ratified
// (anti-cheat #1) so a false positive is one extra item on the
// operator's review queue, not a regression.
func arch003Fingerprint(fn *ast.FuncDecl) string {
	var b strings.Builder
	if fn.Type.Params != nil {
		for _, field := range fn.Type.Params.List {
			b.WriteString(arch003TypeString(field.Type))
			// Repeat once per parameter name in the Names slice
			// (Go AST collapses `a, b T` into a single Field).
			n := len(field.Names)
			if n == 0 {
				n = 1
			}
			for i := 1; i < n; i++ {
				b.WriteString(",")
				b.WriteString(arch003TypeString(field.Type))
			}
			b.WriteString("|")
		}
	}
	b.WriteString("->")
	if fn.Type.Results != nil {
		for _, field := range fn.Type.Results.List {
			b.WriteString(arch003TypeString(field.Type))
			n := len(field.Names)
			if n == 0 {
				n = 1
			}
			for i := 1; i < n; i++ {
				b.WriteString(",")
				b.WriteString(arch003TypeString(field.Type))
			}
			b.WriteString("|")
		}
	}
	// Statement-kind histogram. Top-level statements only (not the
	// recursive walk) — keeps the fingerprint cheap and stable.
	hist := map[string]int{}
	if fn.Body != nil {
		for _, stmt := range fn.Body.List {
			kind := fmt.Sprintf("%T", stmt)
			hist[kind]++
		}
	}
	keys := make([]string, 0, len(hist))
	for k := range hist {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	b.WriteString("#")
	for _, k := range keys {
		fmt.Fprintf(&b, "%s:%d;", k, hist[k])
	}
	sum := sha1.Sum([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// arch003TypeString renders a Go type expression as a stable string.
// Kept tight — no formatter/printer needed; we only care about
// structural equivalence of identifiers.
func arch003TypeString(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return "*" + arch003TypeString(t.X)
	case *ast.SelectorExpr:
		return arch003TypeString(t.X) + "." + t.Sel.Name
	case *ast.ArrayType:
		return "[]" + arch003TypeString(t.Elt)
	case *ast.MapType:
		return "map[" + arch003TypeString(t.Key) + "]" + arch003TypeString(t.Value)
	case *ast.InterfaceType:
		return "interface{}"
	case *ast.FuncType:
		return "func"
	case *ast.ChanType:
		return "chan " + arch003TypeString(t.Value)
	case *ast.Ellipsis:
		return "..." + arch003TypeString(t.Elt)
	default:
		return fmt.Sprintf("%T", e)
	}
}

func init() { Register(NewARCH003()) }
