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

	// IndexHealth returns metadata about the graph's freshness
	// (last-rebuild timestamp, repos covered, repos missing). The
	// dashboard surfaces this so the operator can spot a stale index.
	IndexHealth(ctx context.Context) (Health, error)
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
type BlastRadius struct {
	Modified Symbol
	Direct   []Consumer // first-degree consumers
	Indirect []Consumer // transitive consumers (capped per request)
	Tests    []Symbol   // test functions in the closure
	Depth    int        // depth at which the search was capped
	Truncated bool      // true when the search hit a node-count cap
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
