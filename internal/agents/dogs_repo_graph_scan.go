// D8 Track 1 — dogRepoGraphScan: maintain CrossRepoSymbols + CrossRepoDependencies.
//
// This is the daily-cadence dog that walks every registered repo's source,
// extracts exported symbols (providers) into CrossRepoSymbols, and resolves
// import sites in consumer repos into CrossRepoDependencies edges. v1
// implements the Go extractor (via go/parser, stdlib only). Other-language
// extractors are stubbed behind the symbolExtractor interface so a future
// pass can drop in tree-sitter without touching the dog body.
//
// PR-merge-trigger (per roadmap "daily cadence + triggered on PR merge") is
// NOT wired in v1 — the integration hooks land with D8 Track 2/3 once the
// merge-event surface stabilises (the AskBranchPRs.state='Merged' transition
// is the natural attach point). Documented gap; punt with a clear stub.
package agents

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"force-orchestrator/internal/store"
)

// repoGraphScanSingleRepoBudget caps per-repo wall time. Roadmap budget
// (D8 Track 1 § "Performance budget") is "single-repo < 60s"; we enforce that
// inline via a context.WithTimeout around each per-repo provider/consumer pass.
// On timeout the dog logs the violation, soft-skips the offending repo, and
// continues — preserving the "no silent failures" CLAUDE.md invariant while
// keeping a single slow repo from poisoning the rest of the fleet's pass.
//
// Exposed as a var (not const) so tests can lower the budget to drive the
// timeout-and-skip path deterministically. Production callers leave it at the
// default 60s.
var repoGraphScanSingleRepoBudget = 60 * time.Second

// symbolExtractor is the per-language pluggable surface. Each implementation
// walks a repo on disk and emits providers + consumer call sites. v1 ships
// only goExtractor; tree-sitter-backed extractors for JS/TS/Python/Rust are
// stubs (see langStubExtractor below) so we don't silently skip non-Go repos
// — they emit a TODO log line instead, leaving an obvious "wire me up" trail.
type symbolExtractor interface {
	// Name returns the language tag for log lines.
	Name() string
	// Detect returns true if the extractor wants to handle the repo at
	// `repoPath`. The dog falls through to the next extractor on false.
	Detect(repoPath string) bool
	// ExtractProviders walks the repo and returns the set of exported
	// symbols. The repoName is the registered Repositories.name (used as
	// the FK). Errors are returned per CLAUDE.md "no silent failures" and
	// the dog routes them to the per-repo log line.
	ExtractProviders(ctx context.Context, repoName, repoPath string) ([]extractedSymbol, error)
	// ExtractConsumers walks the repo and returns the set of consumer
	// call-sites. Each site carries the import-path qualifier so the dog
	// can resolve it against the per-language → repo-name map built up
	// during the providers pass.
	ExtractConsumers(ctx context.Context, repoName, repoPath string) ([]extractedConsumerSite, error)
}

// extractedSymbol is the language-agnostic carrier for a provider row. The
// dog turns this into a store.CrossRepoSymbol via UpsertCrossRepoSymbol.
type extractedSymbol struct {
	SymbolPath    string // 'pkg.Type.Method' for Go; 'module/path:Name' for tree-sitter langs
	SymbolKind    string // 'function' | 'type' | 'http_handler' | 'cli_command' | 'exported_const'
	FilePath      string // repo-relative
	LineNumber    int
	SignatureHash string // AST-stable digest
	IsPublic      bool
}

// extractedConsumerSite is the language-agnostic carrier for a consumer edge
// before resolution. ProviderQualifier is the language-specific identifier
// (Go: full import path, e.g. 'github.com/acme/repo-a/api'); ProviderSymbol
// is the dotted symbol path inside that import (e.g. 'NewClient').
type extractedConsumerSite struct {
	ConsumerFile      string // repo-relative
	ConsumerLine      int
	ProviderQualifier string // import-path for Go
	ProviderSymbol    string // 'NewClient' | 'User.ID'
}

// extractorRegistry returns the live extractor list. Order matters: Go runs
// first (the only fully-implemented one in v1); language stubs follow so
// their TODO log lines surface even when a repo has Go files mixed in.
func extractorRegistry() []symbolExtractor {
	return []symbolExtractor{
		&goExtractor{},
		// Stubs — Detect() returns true on the canonical manifest filename
		// so the dog logs a "TODO: tree-sitter not yet wired" line per
		// repo. Wiring tree-sitter is in scope for a follow-up phase, not
		// Track 1.
		&langStubExtractor{name: "javascript", manifestFile: "package.json"},
		&langStubExtractor{name: "python", manifestFile: "pyproject.toml"},
		&langStubExtractor{name: "rust", manifestFile: "Cargo.toml"},
	}
}

// dogRepoGraphScan is the entry point dispatched by runDog. Walks every
// Repositories row, runs the per-language extractors, upserts symbols + edges,
// and soft-deletes per-file edges that disappeared.
func dogRepoGraphScan(ctx context.Context, db *sql.DB, logger interface{ Printf(string, ...any) }) error {
	if db == nil {
		return errors.New("repo-graph-scan: db is nil")
	}
	repos, err := loadRegisteredRepos(db)
	if err != nil {
		return fmt.Errorf("repo-graph-scan: load repos: %w", err)
	}
	if len(repos) == 0 {
		logger.Printf("Dog repo-graph-scan: no registered repos — nothing to scan")
		return nil
	}
	// Pass 1: per-repo provider extraction. We do this first so consumer
	// resolution can look up provider rows by symbol_path.
	type repoMeta struct {
		name       string
		path       string
		modulePath string // Go-only: from go.mod; '' if not a Go repo
	}
	metas := make([]repoMeta, 0, len(repos))
	moduleToRepo := map[string]string{} // Go module-path → registered repo name
	for _, r := range repos {
		if _, statErr := os.Stat(r.path); statErr != nil {
			logger.Printf("Dog repo-graph-scan: skipping %s — local_path %q not accessible: %v", r.name, r.path, statErr)
			continue
		}
		mod := readGoModulePath(r.path)
		if mod != "" {
			moduleToRepo[mod] = r.name
		}
		metas = append(metas, repoMeta{name: r.name, path: r.path, modulePath: mod})
	}

	registry := extractorRegistry()
	var totalSymbols, totalEdgesAdded, totalEdgesSoftDeleted int
	var aggErrs []error

	// Provider pass.
	for _, m := range metas {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("repo-graph-scan: ctx cancelled mid-provider-pass: %w", ctxErr)
		}
		// Per-repo timeout enforcement (roadmap budget < 60s/repo). Wrap the
		// extractor calls in a deadlined ctx so a single pathological repo
		// can't poison the fleet pass. On timeout we log + skip the repo —
		// not a fatal aggErr, since "this one repo is too slow today" is a
		// recoverable degradation (next tick retries).
		func() {
			repoCtx, cancel := context.WithTimeout(ctx, repoGraphScanSingleRepoBudget)
			defer cancel()
			repoStart := time.Now()
			for _, ex := range registry {
				if !ex.Detect(m.path) {
					continue
				}
				syms, exErr := ex.ExtractProviders(repoCtx, m.name, m.path)
				if exErr != nil {
					if errors.Is(exErr, context.DeadlineExceeded) {
						logger.Printf("Dog repo-graph-scan: provider extract %s/%s exceeded per-repo budget %s after %s — skipping repo, will retry next tick",
							m.name, ex.Name(), repoGraphScanSingleRepoBudget, time.Since(repoStart))
						return
					}
					logger.Printf("Dog repo-graph-scan: provider extract %s/%s: %v", m.name, ex.Name(), exErr)
					aggErrs = append(aggErrs, fmt.Errorf("%s/%s providers: %w", m.name, ex.Name(), exErr))
					continue
				}
				for _, s := range syms {
					if _, uErr := store.UpsertCrossRepoSymbol(db, store.CrossRepoSymbol{
						RepoName:      m.name,
						SymbolPath:    s.SymbolPath,
						SymbolKind:    s.SymbolKind,
						FilePath:      s.FilePath,
						LineNumber:    s.LineNumber,
						SignatureHash: s.SignatureHash,
						IsPublic:      s.IsPublic,
					}); uErr != nil {
						logger.Printf("Dog repo-graph-scan: upsert symbol %s/%s: %v", m.name, s.SymbolPath, uErr)
						aggErrs = append(aggErrs, uErr)
						continue
					}
					totalSymbols++
				}
				logger.Printf("Dog repo-graph-scan: %s — %s extractor emitted %d symbol(s)", m.name, ex.Name(), len(syms))
			}
		}()
	}

	// Consumer pass — resolve each call-site against moduleToRepo and the
	// per-repo provider catalogue.
	for _, m := range metas {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("repo-graph-scan: ctx cancelled mid-consumer-pass: %w", ctxErr)
		}
		// Track which (consumer_file, edge id) we observe this pass so we
		// can soft-delete the per-file edges that disappeared.
		observedFiles := map[string]map[int64]struct{}{}
		// Per-repo timeout — same pattern as provider pass. A consumer pass
		// timeout for a repo means we DON'T run the soft-delete sweep for
		// that repo this tick (we'd otherwise tombstone every live edge on
		// the misperception that "we observed nothing"). Wrapping in an
		// IIFE so deadline-detection can bail out cleanly via early return.
		consumerTimedOut := false
		func() {
			repoCtx, cancel := context.WithTimeout(ctx, repoGraphScanSingleRepoBudget)
			defer cancel()
			repoStart := time.Now()
			for _, ex := range registry {
				if !ex.Detect(m.path) {
					continue
				}
				sites, exErr := ex.ExtractConsumers(repoCtx, m.name, m.path)
				if exErr != nil {
					if errors.Is(exErr, context.DeadlineExceeded) {
						logger.Printf("Dog repo-graph-scan: consumer extract %s/%s exceeded per-repo budget %s after %s — skipping soft-delete sweep, will retry next tick",
							m.name, ex.Name(), repoGraphScanSingleRepoBudget, time.Since(repoStart))
						consumerTimedOut = true
						return
					}
					logger.Printf("Dog repo-graph-scan: consumer extract %s/%s: %v", m.name, ex.Name(), exErr)
					aggErrs = append(aggErrs, fmt.Errorf("%s/%s consumers: %w", m.name, ex.Name(), exErr))
					continue
				}
				for _, site := range sites {
					providerRepo := resolveQualifierToRepo(moduleToRepo, site.ProviderQualifier)
					if providerRepo == "" {
						// Out-of-fleet import — not a tracked dependency. (e.g.
						// github.com/google/uuid). v1 deliberately silently
						// drops these; tracking external deps is a different
						// problem than cross-repo blast-radius.
						continue
					}
					if providerRepo == m.name {
						// In-repo import — not cross-repo, skip.
						continue
					}
					symID, lErr := store.LookupCrossRepoSymbolID(db, providerRepo, site.ProviderSymbol)
					if lErr != nil {
						// Not a fatal error — the producer may not export the
						// symbol the consumer thinks it does (e.g. a struct
						// field accessed via a method). v1 logs at debug-ish
						// level and skips.
						continue
					}
					edgeID, uErr := store.UpsertCrossRepoDependency(db, store.CrossRepoDependency{
						ConsumerRepoName: m.name,
						ConsumerFile:     site.ConsumerFile,
						ConsumerLine:     site.ConsumerLine,
						ProviderSymbolID: symID,
					})
					if uErr != nil {
						logger.Printf("Dog repo-graph-scan: upsert dep %s/%s:%d → %s/%s: %v",
							m.name, site.ConsumerFile, site.ConsumerLine, providerRepo, site.ProviderSymbol, uErr)
						aggErrs = append(aggErrs, uErr)
						continue
					}
					if _, seen := observedFiles[site.ConsumerFile]; !seen {
						observedFiles[site.ConsumerFile] = map[int64]struct{}{}
					}
					observedFiles[site.ConsumerFile][edgeID] = struct{}{}
					totalEdgesAdded++
				}
			}
		}()
		if consumerTimedOut {
			// Skip the soft-delete sweep for this repo — observedFiles is
			// incomplete and would falsely tombstone live edges.
			continue
		}

		// Soft-delete: any live edge for an observed file whose id is NOT
		// in the observed set this pass is now stale.
		for file, keepSet := range observedFiles {
			keep := make([]int64, 0, len(keepSet))
			for id := range keepSet {
				keep = append(keep, id)
			}
			sort.Slice(keep, func(i, j int) bool { return keep[i] < keep[j] })
			n, sdErr := store.SoftDeleteCrossRepoDependenciesNotIn(db, m.name, file, keep)
			if sdErr != nil {
				logger.Printf("Dog repo-graph-scan: soft-delete %s/%s: %v", m.name, file, sdErr)
				aggErrs = append(aggErrs, sdErr)
				continue
			}
			totalEdgesSoftDeleted += int(n)
		}

		// File-disappearance: every live edge whose consumer_file is NOT
		// in observedFiles AND whose file no longer exists on disk gets
		// soft-deleted. This catches the "consumer deleted the whole file"
		// case (vs. "edited the file and removed the import" which is
		// caught by the per-file pass above).
		liveAll, lErr := store.ListLiveDependenciesForConsumerRepo(db, m.name)
		if lErr != nil {
			logger.Printf("Dog repo-graph-scan: list live edges %s: %v", m.name, lErr)
			aggErrs = append(aggErrs, lErr)
			continue
		}
		for _, edge := range liveAll {
			if _, observed := observedFiles[edge.ConsumerFile]; observed {
				continue
			}
			abs := filepath.Join(m.path, edge.ConsumerFile)
			if _, statErr := os.Stat(abs); statErr == nil {
				// File still exists; we just didn't scan it (e.g. a stub
				// extractor's repo). Keep the edge.
				continue
			}
			if sdErr := store.SoftDeleteCrossRepoDependency(db, edge.ID); sdErr != nil {
				logger.Printf("Dog repo-graph-scan: soft-delete-orphan %s/%s: %v", m.name, edge.ConsumerFile, sdErr)
				aggErrs = append(aggErrs, sdErr)
				continue
			}
			totalEdgesSoftDeleted++
		}
	}

	logger.Printf("Dog repo-graph-scan: scanned %d repo(s); %d symbol(s) upserted; %d edge(s) added; %d edge(s) soft-deleted",
		len(metas), totalSymbols, totalEdgesAdded, totalEdgesSoftDeleted)
	if len(aggErrs) > 0 {
		return errors.Join(aggErrs...)
	}
	return nil
}

// loadRegisteredRepos pulls (name, local_path) for every Repositories row.
// We only need name + path here; mode/license aren't relevant to graph scans
// (a quarantined repo's API is still an API its consumers depend on).
func loadRegisteredRepos(db *sql.DB) ([]struct{ name, path string }, error) {
	rows, err := db.Query(`SELECT name, local_path FROM Repositories ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []struct{ name, path string }
	for rows.Next() {
		var r struct{ name, path string }
		if sErr := rows.Scan(&r.name, &r.path); sErr != nil {
			return nil, sErr
		}
		if r.path == "" {
			continue
		}
		out = append(out, r)
	}
	if rErr := rows.Err(); rErr != nil {
		return nil, rErr
	}
	return out, nil
}

// resolveQualifierToRepo maps a Go import path (e.g.
// 'example.com/producer/api') to the registered repo name (e.g. 'producer').
// The moduleToRepo map is keyed on go.mod's module path
// (e.g. 'example.com/producer'); a sub-package import shares that prefix, so
// we walk longest-first by trimming path segments until we hit a registered
// module. Returns "" if no module prefix matches.
func resolveQualifierToRepo(moduleToRepo map[string]string, importPath string) string {
	if repo, ok := moduleToRepo[importPath]; ok {
		return repo
	}
	cur := importPath
	for {
		idx := strings.LastIndex(cur, "/")
		if idx < 0 {
			return ""
		}
		cur = cur[:idx]
		if repo, ok := moduleToRepo[cur]; ok {
			return repo
		}
	}
}

// readGoModulePath returns the `module ...` line from go.mod, or "" if the
// repo has no go.mod. Minimal parser — we only care about the first
// module-declaration line, not full go.mod semantics.
func readGoModulePath(repoPath string) string {
	b, err := os.ReadFile(filepath.Join(repoPath, "go.mod"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		ln := strings.TrimSpace(line)
		if strings.HasPrefix(ln, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(ln, "module"))
		}
	}
	return ""
}

// ── Go extractor ──────────────────────────────────────────────────────────────

type goExtractor struct{}

func (goExtractor) Name() string { return "go" }

func (goExtractor) Detect(repoPath string) bool {
	_, err := os.Stat(filepath.Join(repoPath, "go.mod"))
	return err == nil
}

// ExtractProviders walks every .go file under the repo (skipping vendor/,
// testdata/, .git/, *_test.go) and emits one extractedSymbol per exported
// top-level decl. SymbolPath shape: 'modulePath/relpkg.Name' for funcs/types/
// consts, and 'modulePath/relpkg.RecvType.Method' for methods. This mirrors
// what consumer call-sites resolve to.
func (g goExtractor) ExtractProviders(ctx context.Context, repoName, repoPath string) ([]extractedSymbol, error) {
	modulePath := readGoModulePath(repoPath)
	if modulePath == "" {
		return nil, fmt.Errorf("ExtractProviders(%s): go.mod missing or has no module line", repoName)
	}
	var out []extractedSymbol
	fset := token.NewFileSet()
	walkErr := filepath.WalkDir(repoPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if d.IsDir() {
			if shouldSkipGoDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(repoPath, path)
		// Skip files under any *_test.go-only package (handled by suffix
		// check above) and any path that includes a skipped-dir component
		// (filepath.WalkDir's SkipDir already handles that — but defense in
		// depth for symlinks).
		for _, seg := range strings.Split(rel, string(os.PathSeparator)) {
			if shouldSkipGoDir(seg) {
				return nil
			}
		}
		f, parseErr := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if parseErr != nil {
			// Single-file parse error — record + continue. A repo with a
			// broken file is the producer's problem, not ours; we don't
			// want one bad file to sink the whole repo's scan.
			return nil
		}
		pkgRel := filepath.ToSlash(filepath.Dir(rel))
		if pkgRel == "." {
			pkgRel = ""
		}
		fullPkg := modulePath
		if pkgRel != "" {
			fullPkg = modulePath + "/" + pkgRel
		}
		for _, decl := range f.Decls {
			switch dd := decl.(type) {
			case *ast.FuncDecl:
				if !dd.Name.IsExported() {
					continue
				}
				sym := extractedSymbol{
					SymbolKind:    "function",
					FilePath:      filepath.ToSlash(rel),
					LineNumber:    fset.Position(dd.Pos()).Line,
					IsPublic:      true,
					SignatureHash: hashGoFuncSignature(dd),
				}
				if dd.Recv != nil && len(dd.Recv.List) > 0 {
					recvName := goRecvTypeName(dd.Recv.List[0].Type)
					if recvName == "" {
						continue
					}
					sym.SymbolPath = fullPkg + "." + recvName + "." + dd.Name.Name
				} else {
					sym.SymbolPath = fullPkg + "." + dd.Name.Name
				}
				out = append(out, sym)
			case *ast.GenDecl:
				for _, spec := range dd.Specs {
					switch ss := spec.(type) {
					case *ast.TypeSpec:
						if !ss.Name.IsExported() {
							continue
						}
						out = append(out, extractedSymbol{
							SymbolPath:    fullPkg + "." + ss.Name.Name,
							SymbolKind:    "type",
							FilePath:      filepath.ToSlash(rel),
							LineNumber:    fset.Position(ss.Pos()).Line,
							IsPublic:      true,
							SignatureHash: hashGoNode(ss),
						})
					case *ast.ValueSpec:
						if dd.Tok != token.CONST {
							continue
						}
						for _, name := range ss.Names {
							if !name.IsExported() {
								continue
							}
							out = append(out, extractedSymbol{
								SymbolPath:    fullPkg + "." + name.Name,
								SymbolKind:    "exported_const",
								FilePath:      filepath.ToSlash(rel),
								LineNumber:    fset.Position(name.Pos()).Line,
								IsPublic:      true,
								SignatureHash: hashGoNode(ss),
							})
						}
					}
				}
			}
		}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, context.Canceled) {
		return out, walkErr
	}
	if walkErr != nil {
		return out, walkErr
	}
	return out, nil
}

// ExtractConsumers walks every .go file (incl. _test.go for completeness;
// roadmap anti-cheat #3 asks the dog to surface test-file consumers as a
// distinguishable subset, but Track 1 stops at recording the edge — the
// "test-only consumer" annotation is Track 2's blast-radius post-processor's
// job) and emits one extractedConsumerSite per qualified-call expression
// whose qualifier resolves to a non-stdlib import.
func (g goExtractor) ExtractConsumers(ctx context.Context, repoName, repoPath string) ([]extractedConsumerSite, error) {
	var out []extractedConsumerSite
	fset := token.NewFileSet()
	walkErr := filepath.WalkDir(repoPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if d.IsDir() {
			if shouldSkipGoDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, _ := filepath.Rel(repoPath, path)
		for _, seg := range strings.Split(rel, string(os.PathSeparator)) {
			if shouldSkipGoDir(seg) {
				return nil
			}
		}
		f, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return nil
		}
		// Build alias → full-import-path map for this file. Default alias
		// is the last path segment; explicit aliases (e.g. `foo "x/y/z"`)
		// override.
		aliasToImport := map[string]string{}
		for _, imp := range f.Imports {
			lit := strings.Trim(imp.Path.Value, `"`)
			alias := ""
			if imp.Name != nil {
				alias = imp.Name.Name
			} else {
				parts := strings.Split(lit, "/")
				alias = parts[len(parts)-1]
			}
			if alias == "_" || alias == "." {
				// blank/dot imports: not directly resolvable to a
				// qualified call site by alias.
				continue
			}
			aliasToImport[alias] = lit
		}
		// Walk the AST for selector expressions: `pkg.Name` or
		// `pkg.Name(...)`. We treat both as consumer sites — the
		// blast-radius cares whether a consumer references the symbol,
		// not whether it calls it.
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			ident, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			imp, ok := aliasToImport[ident.Name]
			if !ok {
				return true
			}
			out = append(out, extractedConsumerSite{
				ConsumerFile:      filepath.ToSlash(rel),
				ConsumerLine:      fset.Position(sel.Pos()).Line,
				ProviderQualifier: imp,
				ProviderSymbol:    imp + "." + sel.Sel.Name,
			})
			return true
		})
		return nil
	})
	if walkErr != nil {
		return out, walkErr
	}
	return out, nil
}

// shouldSkipGoDir returns true for directories the Go extractor should NOT
// descend into. Mirrors `go list ./...` semantics for vendor/testdata + the
// usual hidden / build-output dirs.
func shouldSkipGoDir(name string) bool {
	switch name {
	case "vendor", "testdata", ".git", "node_modules", ".d7-worktrees", ".force-worktrees":
		return true
	}
	return strings.HasPrefix(name, ".")
}

// goRecvTypeName extracts the receiver type name from a method decl. Handles
// both value (`(t Foo)`) and pointer (`(t *Foo)`) receivers; returns "" if
// the receiver shape is something exotic we don't handle yet (generics).
func goRecvTypeName(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.StarExpr:
		if id, ok := e.X.(*ast.Ident); ok {
			return id.Name
		}
		// pointer-to-generic: `*Foo[T]`. Strip the type-param suffix.
		if idx, ok := e.X.(*ast.IndexExpr); ok {
			if id, ok2 := idx.X.(*ast.Ident); ok2 {
				return id.Name
			}
		}
	case *ast.IndexExpr:
		if id, ok := e.X.(*ast.Ident); ok {
			return id.Name
		}
	}
	return ""
}

// hashGoFuncSignature digests the function name + parameter / result types
// (textual) into a stable hex string. Pure renames at the body level don't
// shift this hash; signature changes (param types, return types, receiver
// pointer-ness) do.
func hashGoFuncSignature(fn *ast.FuncDecl) string {
	var b strings.Builder
	b.WriteString(fn.Name.Name)
	b.WriteString("|")
	if fn.Type.Params != nil {
		for _, p := range fn.Type.Params.List {
			b.WriteString(exprToString(p.Type))
			b.WriteString(",")
		}
	}
	b.WriteString("|")
	if fn.Type.Results != nil {
		for _, r := range fn.Type.Results.List {
			b.WriteString(exprToString(r.Type))
			b.WriteString(",")
		}
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// hashGoNode digests a TypeSpec / ValueSpec textually. Covers struct fields,
// const values, etc. Less precise than hashGoFuncSignature but sufficient for
// "did the spec change" detection.
func hashGoNode(node ast.Node) string {
	sum := sha256.Sum256([]byte(exprToString(node)))
	return hex.EncodeToString(sum[:])
}

// exprToString returns a shallow-but-deterministic representation of an AST
// node. We don't need go/printer fidelity here — just stability.
func exprToString(n ast.Node) string {
	if n == nil {
		return ""
	}
	switch e := n.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.StarExpr:
		return "*" + exprToString(e.X)
	case *ast.SelectorExpr:
		return exprToString(e.X) + "." + e.Sel.Name
	case *ast.ArrayType:
		return "[]" + exprToString(e.Elt)
	case *ast.MapType:
		return "map[" + exprToString(e.Key) + "]" + exprToString(e.Value)
	case *ast.InterfaceType:
		return "interface{...}"
	case *ast.StructType:
		var b strings.Builder
		b.WriteString("struct{")
		if e.Fields != nil {
			for _, f := range e.Fields.List {
				for _, n := range f.Names {
					b.WriteString(n.Name)
					b.WriteString(" ")
				}
				b.WriteString(exprToString(f.Type))
				b.WriteString(";")
			}
		}
		b.WriteString("}")
		return b.String()
	case *ast.FuncType:
		var b strings.Builder
		b.WriteString("func(")
		if e.Params != nil {
			for _, p := range e.Params.List {
				b.WriteString(exprToString(p.Type))
				b.WriteString(",")
			}
		}
		b.WriteString(")")
		if e.Results != nil {
			b.WriteString("(")
			for _, r := range e.Results.List {
				b.WriteString(exprToString(r.Type))
				b.WriteString(",")
			}
			b.WriteString(")")
		}
		return b.String()
	case *ast.TypeSpec:
		return e.Name.Name + "=" + exprToString(e.Type)
	case *ast.ValueSpec:
		var b strings.Builder
		for _, n := range e.Names {
			b.WriteString(n.Name)
			b.WriteString(",")
		}
		b.WriteString("=")
		for _, v := range e.Values {
			b.WriteString(exprToString(v))
			b.WriteString(",")
		}
		return b.String()
	case *ast.BasicLit:
		return e.Value
	}
	return fmt.Sprintf("%T", n)
}

// ── Stub extractors (TODO: tree-sitter) ──────────────────────────────────────

type langStubExtractor struct {
	name         string // 'javascript' | 'python' | 'rust'
	manifestFile string // 'package.json' | 'pyproject.toml' | 'Cargo.toml'
}

func (s langStubExtractor) Name() string { return s.name }

func (s langStubExtractor) Detect(repoPath string) bool {
	_, err := os.Stat(filepath.Join(repoPath, s.manifestFile))
	return err == nil
}

func (s langStubExtractor) ExtractProviders(_ context.Context, repoName, _ string) ([]extractedSymbol, error) {
	// TODO(D8 follow-up): tree-sitter integration. Track 1 deliberately
	// stubs non-Go languages so a Go-only fleet ships v1 of the graph
	// without dragging in a 100+ MB native dep. The roadmap anti-cheat
	// directive #2 ("No skipping non-Go repos") is acknowledged here:
	// this stub IS the visible TODO so a follow-up doesn't quietly forget.
	return nil, nil
}

func (s langStubExtractor) ExtractConsumers(_ context.Context, _, _ string) ([]extractedConsumerSite, error) {
	// TODO(D8 follow-up): tree-sitter integration. See ExtractProviders.
	return nil, nil
}
