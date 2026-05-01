// Package agents — D4 Phase 0 — Librarian-client ingress helper.
//
// Pattern P33 requires that production agent code retrieve memory
// for prompt assembly through the Librarian Client, not via direct
// store.GetFleetMemories* calls. This file provides the seam: a
// lightweight `getMemoriesForPromptInjection` helper that takes the
// agent's *sql.DB, constructs an in-process Client lazily (cached
// per-process), and routes the read through Client.GetWeightedMemories.
//
// Why a package-level cache rather than threading the Client through
// every Spawn function: the in-process Client is a tiny struct
// holding only the *sql.DB; constructing one per call is cheap, but
// caching one per process avoids re-allocating on every memory
// retrieval. The cache is keyed on the *sql.DB pointer so a test
// that spins up a fresh :memory: DB gets a fresh Client.
//
// Pattern P33 (audit_pattern_p33_agent_memory_via_librarian_client_test.go)
// rejects new agent files that import store and call
// GetFleetMemories / ListAllFleetMemories / GetFleetMemoriesByIDs
// directly (allowlist scoped to the librarian dog handlers + the
// memory-rerank helper).
package agents

import (
	"context"
	"database/sql"
	"sync"

	"force-orchestrator/internal/clients/librarian"
	"force-orchestrator/internal/store"
)

var (
	libClientMu    sync.Mutex
	libClientCache = map[*sql.DB]librarian.Client{}
)

// librarianClientFor returns a process-level cached in-process
// Librarian Client for the given DB handle. Constructs one on first
// call per DB and reuses it for subsequent calls. Tests that need a
// custom Client (e.g. NewMock) should pass theirs explicitly and
// bypass this cache.
func librarianClientFor(db *sql.DB) librarian.Client {
	libClientMu.Lock()
	defer libClientMu.Unlock()
	if c, ok := libClientCache[db]; ok {
		return c
	}
	c := librarian.NewInProcess(db)
	libClientCache[db] = c
	return c
}

// getMemoriesForPromptInjection is the canonical agent-side ingress
// for retrieving memories at prompt-assembly time. Routes through
// Client.GetWeightedMemories so the composite-score sort + canonical_id
// exclusion are honoured.
//
// Returns []store.FleetMemoryEntry (the legacy shape) so existing
// callers that pipeline through RerankFleetMemories don't need to
// change their signature. Internally, we map librarian.Memory back to
// FleetMemoryEntry — cheap (4 fields).
//
// Why we keep returning FleetMemoryEntry rather than librarian.Memory:
// the rerank helper (memory_rerank.go) and the prompt-assembly code
// expect the FleetMemoryEntry shape. A wholesale type-replace is a
// larger refactor; the Phase 0 graduation is to route through the
// Client surface, not to rename the in-memory shape.
func getMemoriesForPromptInjection(ctx context.Context, db *sql.DB, repo string, k int) []store.FleetMemoryEntry {
	if k <= 0 {
		k = 20
	}
	c := librarianClientFor(db)
	memories, err := c.GetWeightedMemories(ctx, librarian.Scope{Repo: repo, Limit: k}, k)
	if err != nil {
		// Pattern: ingress retrieval is best-effort (the prompt still
		// assembles without memory). We return nil rather than
		// surfacing the error — the caller's prompt path is robust to
		// empty memory.
		return nil
	}
	out := make([]store.FleetMemoryEntry, 0, len(memories))
	for _, m := range memories {
		out = append(out, store.FleetMemoryEntry{
			ID:           m.ID,
			Repo:         m.Repo,
			TaskID:       m.ParentTaskID,
			Outcome:      m.Outcome,
			Summary:      m.Summary,
			FilesChanged: m.Files,
			TopicTags:    m.TopicTags,
			CreatedAt:    m.CreatedAt,
		})
	}
	// Stamp retrieval bookkeeping — RecordRetrieval is best-effort and
	// errors here are non-fatal to the prompt.
	for _, m := range memories {
		_ = store.RecordRetrieval(ctx, db, m.ID)
	}
	return out
}
