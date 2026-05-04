package graph

import (
	"context"
	"database/sql"
	"errors"
	"sort"

	"force-orchestrator/internal/store"
)

// inProcessClient is the in-process Client backing.
//
// D0 shipped a placeholder that returned ErrNotImplemented from every
// method. D8 Track 1 added the schema (CrossRepoSymbols / CrossRepoDependencies)
// + dog (dogRepoGraphScan) that populates them. D8 Track 2 (this file's
// real body) wires the reader path: BlastRadius / BlastRadiusForModifications
// query the store helpers (ListProvidersOfSymbol / ListConsumersOfSymbol)
// directly so Chancellor's plan-decomposition post-process can decide
// which downstream consumers a Feature's modifications affect.
//
// The struct is unexported per CLAUDE.md "Cross-agent service interfaces"
// + Pattern P16 — agents construct via NewInProcess(db) only. A nil db
// is permitted and yields ErrIndexNotReady on every read so callers that
// haven't wired the dog yet (legacy paths, fast-path tests) fail safe
// rather than panicking.
type inProcessClient struct {
	db *sql.DB
}

// NewInProcess returns the in-process Client backed by the supplied
// holocron.db handle. Passing nil yields a Client that returns
// ErrIndexNotReady from every read — useful for daemon-startup phases
// where the DB hasn't been opened yet but a non-nil Client is required
// by constructor injection.
func NewInProcess(db *sql.DB) Client { return &inProcessClient{db: db} }

func (c *inProcessClient) Consumers(ctx context.Context, symbol Symbol) ([]Consumer, error) {
	if c.db == nil {
		return nil, ErrIndexNotReady
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if symbol.Repo == "" || symbol.Name == "" {
		return nil, ErrSymbolNotFound
	}
	id, err := store.LookupCrossRepoSymbolID(c.db, symbol.Repo, symbol.Name)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrSymbolNotFound
		}
		return nil, err
	}
	deps, err := store.ListConsumersOfSymbol(c.db, id)
	if err != nil {
		return nil, err
	}
	out := make([]Consumer, 0, len(deps))
	for _, d := range deps {
		out = append(out, Consumer{
			Symbol: Symbol{
				Repo: d.ConsumerRepoName,
				Path: d.ConsumerFile,
				Name: d.ConsumerFile, // edge-row carries no symbol-level identifier
				Line: d.ConsumerLine,
			},
			Via: "direct",
		})
	}
	return out, nil
}

func (c *inProcessClient) Definers(ctx context.Context, symbol Symbol) ([]Symbol, error) {
	if c.db == nil {
		return nil, ErrIndexNotReady
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if symbol.Repo == "" || symbol.Name == "" {
		return nil, ErrSymbolNotFound
	}
	rows, err := store.ListProvidersOfSymbol(c.db, symbol.Repo, symbol.Name)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, ErrSymbolNotFound
	}
	out := make([]Symbol, 0, len(rows))
	for _, r := range rows {
		out = append(out, Symbol{
			Repo: r.RepoName,
			Path: r.FilePath,
			Name: r.SymbolPath,
			Kind: r.SymbolKind,
			Line: r.LineNumber,
		})
	}
	return out, nil
}

func (c *inProcessClient) BlastRadius(ctx context.Context, modifiedSymbol Symbol) (BlastRadius, error) {
	br, err := c.BlastRadiusForModifications(ctx, []SymbolModification{{
		Repo:       modifiedSymbol.Repo,
		FilePath:   modifiedSymbol.Path,
		SymbolPath: modifiedSymbol.Name,
	}})
	if err != nil {
		return BlastRadius{}, err
	}
	br.Modified = modifiedSymbol
	if sites, ok := br.ConsumersBySymbol[modifiedSymbol.Name]; ok {
		direct := make([]Consumer, 0, len(sites))
		for _, s := range sites {
			direct = append(direct, Consumer{
				Symbol: Symbol{Repo: s.Repo, Path: s.FilePath, Name: s.FilePath, Line: s.Line},
				Via:    "direct",
			})
		}
		br.Direct = direct
	}
	return br, nil
}

func (c *inProcessClient) BlastRadiusForModifications(ctx context.Context, mods []SymbolModification) (BlastRadius, error) {
	if c.db == nil {
		return BlastRadius{}, ErrIndexNotReady
	}
	if err := ctx.Err(); err != nil {
		return BlastRadius{}, err
	}
	br := BlastRadius{
		ConsumersBySymbol: map[string][]ConsumerSite{},
	}
	repoSet := map[string]struct{}{}
	for _, m := range mods {
		if m.Repo == "" || m.SymbolPath == "" {
			// Skip malformed modifications silently — Chancellor's
			// payload-extraction step may yield partial tuples on tasks
			// whose payload doesn't match the symbol-bearing template.
			continue
		}
		providers, err := store.ListProvidersOfSymbol(c.db, m.Repo, m.SymbolPath)
		if err != nil {
			return BlastRadius{}, err
		}
		if len(providers) == 0 {
			// No CrossRepoSymbols row for this (repo, symbol_path) — the
			// dog hasn't indexed it yet, or it's a non-public symbol the
			// dog skipped. Blast-radius is "best known consumers"; skip.
			continue
		}
		for _, p := range providers {
			br.ModifiedSymbols = append(br.ModifiedSymbols, Symbol{
				Repo: p.RepoName,
				Path: p.FilePath,
				Name: p.SymbolPath,
				Kind: p.SymbolKind,
				Line: p.LineNumber,
			})
			deps, dErr := store.ListConsumersOfSymbol(c.db, p.ID)
			if dErr != nil {
				return BlastRadius{}, dErr
			}
			for _, d := range deps {
				site := ConsumerSite{
					Repo:     d.ConsumerRepoName,
					FilePath: d.ConsumerFile,
					Line:     d.ConsumerLine,
				}
				br.ConsumersBySymbol[p.SymbolPath] = append(br.ConsumersBySymbol[p.SymbolPath], site)
				repoSet[d.ConsumerRepoName] = struct{}{}
			}
		}
	}
	br.AffectedConsumerRepos = make([]string, 0, len(repoSet))
	for r := range repoSet {
		br.AffectedConsumerRepos = append(br.AffectedConsumerRepos, r)
	}
	sort.Strings(br.AffectedConsumerRepos)
	return br, nil
}

func (c *inProcessClient) IndexHealth(ctx context.Context) (Health, error) {
	if c.db == nil {
		return Health{}, ErrIndexNotReady
	}
	if err := ctx.Err(); err != nil {
		return Health{}, err
	}
	nodes, err := store.CountCrossRepoSymbols(c.db)
	if err != nil {
		return Health{}, err
	}
	edges, err := store.CountCrossRepoDependencies(c.db, false)
	if err != nil {
		return Health{}, err
	}
	return Health{NodeCount: nodes, EdgeCount: edges}, nil
}
