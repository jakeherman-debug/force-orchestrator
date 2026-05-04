// Package graph defines the client interface for the cross-repo
// symbol-graph service — the queryable index of "who calls whom"
// across the fleet's repositories. D8 builds the real implementation
// (LSP-driven extractor + SQLite-backed graph store + blast-radius
// computation); agents use it via this interface to ask questions
// like "what consumes this symbol?" and "if I change this, what tests
// must I run?"
//
// Implementation timeline:
//   - D0 (this commit): interface definition + ErrNotImplemented stubs.
//   - D8 (cross-repo graph deliverable): the real in-process
//     implementation lands here.
//   - Later: gRPC backing for shared multi-tenant operation.
//
// Pattern P16 (audit_pattern_p16_clients_interfaces_test.go) enforces
// that production agent code references the Client interface only.
package graph

import (
	"context"
	"errors"
)

// Client is the contract between agents and the cross-repo graph.
// The interface is small on purpose — D8 carves it to the operations
// agents actually need at change-impact analysis time.
type Client interface {
	// Consumers returns every consumer of the given symbol across
	// the indexed fleet. Used by Captain / Council / Medic for
	// "who breaks if this changes?" reasoning.
	Consumers(ctx context.Context, symbol Symbol) ([]Consumer, error)

	// Definers returns the canonical definition site(s) for the
	// given symbol. A symbol may be re-exported across packages;
	// Definers returns the actual implementation locations.
	Definers(ctx context.Context, symbol Symbol) ([]Symbol, error)

	// BlastRadius computes the "if I change this, what else needs
	// re-tested" set. Returns the transitive consumer set capped
	// at the supplied depth (0 means "direct consumers only").
	BlastRadius(ctx context.Context, modifiedSymbol Symbol) (BlastRadius, error)

	// BlastRadiusForModifications is the data-driven, multi-modification
	// variant Chancellor uses during plan post-processing (D8 Track 2).
	// Each SymbolModification is a (repo, file_path, symbol_path) tuple
	// representing a change a Feature's plan proposes to make. The result
	// aggregates per-modification consumer file:line lists across the
	// entire batch, and surfaces the de-duplicated AffectedConsumerRepos
	// set Chancellor uses to (a) auto-include downstream consumer-update
	// tasks in the same convoy and (b) fan out per-affected-Senator
	// consultations.
	//
	// Modifications whose (repo, symbol_path) does not match a known
	// CrossRepoSymbol are silently skipped — those represent symbols the
	// dog hasn't indexed yet, or modifications to non-public symbols, and
	// blast-radius is a "best known consumers" query rather than a
	// completeness guarantee.
	BlastRadiusForModifications(ctx context.Context, mods []SymbolModification) (BlastRadius, error)

	// IndexHealth returns metadata about the graph's freshness
	// (last-rebuild timestamp, repos covered, repos missing). The
	// dashboard surfaces this so the operator can spot a stale index.
	IndexHealth(ctx context.Context) (Health, error)
}

// SymbolModification is one (repo, file_path, symbol_path) tuple
// representing a change a Feature's plan proposes to make. Used as
// input to BlastRadiusForModifications. The Repo field is the provider
// repo whose exported symbol is being modified — Chancellor extracts
// this from a CodeEdit task's `Repo` field and pairs it with the
// symbol-path it parses out of the task's payload via lightweight
// regex (Chancellor.ExtractSymbolModifications).
type SymbolModification struct {
	Repo       string // provider repo name (matches CrossRepoSymbols.repo_name)
	FilePath   string // repo-relative file path the modification touches
	SymbolPath string // qualified symbol identifier (e.g. "auth.LoginHandler")
}

// ConsumerSite is one (repo, file_path, line_number) consumer location
// produced by BlastRadiusForModifications. The aggregated BlastRadius
// returns these so Chancellor can render per-symbol "your consumer
// sites at <file>:<line>" breadcrumbs in the auto-included downstream
// task payloads.
type ConsumerSite struct {
	Repo     string
	FilePath string
	Line     int
}

// Symbol identifies one named entity in the cross-repo graph.
// Repo + Path + Name + Kind together uniquely identify a definition
// site (or, for Consumers, a use site).
type Symbol struct {
	Repo string
	Path string // file path within the repo
	Name string // qualified name (e.g. "auth.LoginHandler")
	Kind string // "func" | "method" | "type" | "var" | "const"
	Line int    // source line, 1-indexed
}

// Consumer is one use site that depends on a target Symbol. A
// consumer may itself be a function or a test — Kind disambiguates.
type Consumer struct {
	Symbol Symbol
	Via    string // "direct" | "type-embedding" | "interface" — D8 owns the vocabulary
}

// BlastRadius is the result of a transitive consumer query.
//
// D8 Track 2 extends the type with three additional fields populated
// by BlastRadiusForModifications: ModifiedSymbols (the Symbol-shape
// reflection of every input SymbolModification that resolved to a
// known CrossRepoSymbol), AffectedConsumerRepos (the de-duplicated,
// alphabetically-sorted set of consumer repos across all modifications),
// and ConsumersBySymbol (per modified symbol_path → consumer file:line
// list, used to render auto-included downstream task payloads).
//
// The legacy BlastRadius(ctx, modifiedSymbol Symbol) single-symbol API
// continues to populate Direct / Indirect / Tests; the multi-modification
// API populates ModifiedSymbols / AffectedConsumerRepos / ConsumersBySymbol.
// A given BlastRadius value may carry either set or both depending on
// which API was called.
type BlastRadius struct {
	Modified Symbol
	Direct   []Consumer // first-degree consumers
	Indirect []Consumer // transitive consumers (capped per request)
	Tests    []Symbol   // test functions in the closure
	Depth    int        // depth at which the search was capped
	Truncated bool      // true when the search hit a node-count cap

	// D8 Track 2 — multi-modification aggregate fields.
	ModifiedSymbols       []Symbol                  // every input SymbolModification that resolved to a known CrossRepoSymbol
	AffectedConsumerRepos []string                  // alphabetically-sorted, de-duplicated consumer repo names
	ConsumersBySymbol     map[string][]ConsumerSite // key: SymbolPath → consumer (repo, file, line) list
}

// Health describes the graph index's freshness for the operator
// dashboard.
type Health struct {
	LastRebuildAt string   // SQLite datetime
	ReposIndexed  []string // names of repos in the index
	ReposMissing  []string // registered repos not yet indexed
	NodeCount     int      // total symbols in the graph
	EdgeCount     int      // total uses
}

var (
	// ErrSymbolNotFound — Consumers / Definers called for an unknown
	// symbol.
	ErrSymbolNotFound = errors.New("graph: symbol not found in index")

	// ErrIndexNotReady — the graph hasn't been built yet, or is
	// mid-rebuild. Callers should fall back to "assume blast radius
	// is the whole repo" (the safe default) until the index is up.
	ErrIndexNotReady = errors.New("graph: index not ready")

	// ErrNotImplemented — D0 stub guard.
	ErrNotImplemented = errors.New("graph: not implemented (D8 deliverable)")
)
