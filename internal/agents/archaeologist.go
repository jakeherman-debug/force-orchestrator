// Deliverable 9 — Archaeologist (Track A) agent.
//
// The Archaeologist is a claim-loop agent that consumes two task
// types:
//
//   - ArchaeologistSweep: per-repo debt-pattern sweep. Walks every
//     registered Pattern (internal/archaeologist/patterns/) against
//     the repo's working tree; persists hits into ArchaeologistFindings
//     (status='open'); fans out an ArchaeologistProposeMigration task
//     for any pattern whose post-sweep open-count exceeds
//     Pattern.MinHitsForFeature().
//
//   - ArchaeologistProposeMigration: calls librarian.Client.EmitCandidate
//     with a pre-decomposed migration hypothesis. Operator ratifies via
//     the existing PromotionProposal flow (anti-cheat #1: no
//     auto-dispatch). On success, marks the cluster's findings as
//     status='proposed' so the next sweep doesn't re-fire.
//
// Pattern (Diplomat shape): a single SpawnArchaeologist goroutine
// loops, claiming both task types in turn. No LLM call; the agent is
// pure Go pattern-scanning + librarian.Client.EmitCandidate. The
// Librarian Client is injected at Spawn time (constructor injection
// per CLAUDE.md § "Cross-agent service interfaces").
package agents

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"force-orchestrator/internal/archaeologist"
	"force-orchestrator/internal/archaeologist/patterns"
	"force-orchestrator/internal/clients/graph"
	"force-orchestrator/internal/clients/librarian"
	"force-orchestrator/internal/store"
)

// archaeologistSweepPayload carries the per-task target repo for an
// ArchaeologistSweep claim. RepoID is the SQLite rowid; the handler
// re-resolves the canonical name + local_path via
// store.GetArchaeologistRepoByID.
type archaeologistSweepPayload struct {
	RepoID int `json:"repo_id"`
}

// archaeologistProposeMigrationPayload carries the (pattern, repo)
// pair the proposal handler should hand off to Librarian.EmitCandidate.
type archaeologistProposeMigrationPayload struct {
	PatternID string `json:"pattern_id"`
	RepoID    int    `json:"repo_id"`
}

// SpawnArchaeologist runs the Archaeologist claim loop. One goroutine
// per spawned agent. Diplomat-pattern: poll for ArchaeologistSweep
// first, then ArchaeologistProposeMigration. No LLM call site, so no
// capability profile is loaded.
//
// The graph.Client is constructed at spawn-time (mirroring Chancellor's
// shape in chancellor.go) and threaded into the propose-migration handler
// so the EvidenceJSON payload can carry a blast-radius snapshot of which
// downstream consumer repos a ratified migration would ripple into. Per
// CLAUDE.md "Cross-agent service interfaces" + Pattern P16: agents
// construct graph.Client via the package's NewInProcess factory.
func SpawnArchaeologist(ctx context.Context, db *sql.DB, lib librarian.Client, name string) {
	logger := NewLogger(name)
	logger.Printf("Archaeologist %s coming online", name)
	gc := graph.NewInProcess(db)
	// Wire the ARCH-002 (unused-exports) pattern to the cross-repo
	// graph. The Pattern interface deliberately does not carry a DB
	// handle; ARCH-002 reads it via the package-level injection point
	// mirroring claude.SetTranscriptDB. Skipping this call leaves
	// ARCH-002 in its "graph unavailable, return nil + log once" mode
	// (P9: never emit findings against missing data).
	patterns.SetCrossRepoGraphDB(db)
	for {
		if ctx.Err() != nil {
			logger.Printf("Archaeologist %s exiting: %v", name, ctx.Err())
			return
		}
		if IsEstopped(db) {
			time.Sleep(5 * time.Second)
			continue
		}
		if SpendCapExceeded(db) {
			time.Sleep(10 * time.Second)
			continue
		}
		if bounty, claimed := store.ClaimBounty(db, "ArchaeologistSweep", name); claimed {
			runArchaeologistSweep(ctx, db, name, bounty, logger)
			continue
		}
		if bounty, claimed := store.ClaimBounty(db, "ArchaeologistProposeMigration", name); claimed {
			runArchaeologistProposeMigration(ctx, db, lib, gc, name, bounty, logger)
			continue
		}
		time.Sleep(time.Duration(3000+rand.Intn(1000)) * time.Millisecond)
	}
}

// runArchaeologistSweep handles one ArchaeologistSweep bounty. Loads
// every registered Pattern from the static registry (anti-cheat #4
// — registry is authoritative); scans the repo; persists hits;
// fan-outs migration proposals for clusters past threshold.
func runArchaeologistSweep(ctx context.Context, db *sql.DB, agentName string, bounty *store.Bounty, logger interface{ Printf(string, ...any) }) {
	_ = ctx
	var payload archaeologistSweepPayload
	if err := json.Unmarshal([]byte(bounty.Payload), &payload); err != nil {
		if fbErr := store.FailBounty(db, bounty.ID, fmt.Sprintf("ArchaeologistSweep: invalid payload: %v", err)); fbErr != nil {
			logger.Printf("ArchaeologistSweep #%d: FailBounty after payload parse error: %v", bounty.ID, fbErr)
		}
		return
	}
	target, err := store.GetArchaeologistRepoByID(db, payload.RepoID)
	if err != nil {
		if fbErr := store.FailBounty(db, bounty.ID, fmt.Sprintf("ArchaeologistSweep: repo %d not found: %v", payload.RepoID, err)); fbErr != nil {
			logger.Printf("ArchaeologistSweep #%d: FailBounty after missing repo: %v", bounty.ID, fbErr)
		}
		return
	}
	if target.LocalPath == "" {
		if fbErr := store.FailBounty(db, bounty.ID, fmt.Sprintf("ArchaeologistSweep: repo %s has empty local_path", target.Name)); fbErr != nil {
			logger.Printf("ArchaeologistSweep #%d: FailBounty after empty local_path: %v", bounty.ID, fbErr)
		}
		return
	}

	repo := &archaeologist.Repo{
		ID:        target.ID,
		Name:      target.Name,
		LocalPath: target.LocalPath,
	}
	allPatterns := patterns.All()
	totalNew := 0
	totalDeduped := 0
	var fanouts []string
	for _, p := range allPatterns {
		hits := safeScan(p, repo, logger)
		newCount, dedupCount := persistArchaeologistHits(db, p.ID(), repo.ID, hits, logger)
		totalNew += newCount
		totalDeduped += dedupCount

		// Threshold check. Re-count from the DB (not from `hits` alone)
		// so a cluster that grew across multiple sweep cycles is
		// detected correctly.
		open, cErr := store.CountOpenFindingsForPattern(db, p.ID(), repo.ID)
		if cErr != nil {
			logger.Printf("ArchaeologistSweep #%d: %s count failed: %v — skipping fan-out for this pattern", bounty.ID, p.ID(), cErr)
			continue
		}
		if open < p.MinHitsForFeature() {
			continue
		}
		// Fan-out the migration proposal. Idempotent across sweeps:
		// the proposal handler flips the cluster to status='proposed'
		// on success, dropping it out of the open count next cycle.
		id, qErr := store.QueueArchaeologistProposeMigration(db, p.ID(), repo.ID, repo.Name)
		if qErr != nil {
			logger.Printf("ArchaeologistSweep #%d: QueueArchaeologistProposeMigration(%s,%s) failed: %v", bounty.ID, p.ID(), repo.Name, qErr)
		} else if id > 0 {
			fanouts = append(fanouts, fmt.Sprintf("%s→#%d", p.ID(), id))
		}
	}

	logger.Printf("ArchaeologistSweep #%d: repo=%s patterns=%d new_hits=%d deduped=%d fanouts=%v",
		bounty.ID, repo.Name, len(allPatterns), totalNew, totalDeduped, fanouts)
	if err := store.UpdateBountyStatus(db, bounty.ID, "Completed"); err != nil {
		logger.Printf("ArchaeologistSweep #%d: Completed update failed: %v", bounty.ID, err)
	}
}

// safeScan invokes p.Scan with a panic recover so a faulty pattern
// (e.g. nil-deref on a malformed AST) doesn't crash the sweep loop —
// the rest of the patterns still run for the same repo.
func safeScan(p archaeologist.Pattern, repo *archaeologist.Repo, logger interface{ Printf(string, ...any) }) (hits []archaeologist.Hit) {
	defer func() {
		if r := recover(); r != nil {
			logger.Printf("Pattern %s panicked on repo %s: %v — returning zero hits", p.ID(), repo.Name, r)
			hits = nil
		}
	}()
	return p.Scan(repo)
}

// persistArchaeologistHits inserts every hit through the deduping
// store helper. Returns (newRowsCount, dedupedCount). A scan error on
// any single hit is logged but does not abort the others (the per-row
// errors are independent).
func persistArchaeologistHits(db *sql.DB, patternID string, repoID int, hits []archaeologist.Hit, logger interface{ Printf(string, ...any) }) (int, int) {
	created := 0
	deduped := 0
	for _, h := range hits {
		id, err := store.InsertArchaeologistFinding(db, store.ArchaeologistFinding{
			PatternID:  patternID,
			RepoID:     repoID,
			FilePath:   h.FilePath,
			LineNumber: h.LineNumber,
			DetailJSON: h.DetailJSON,
		})
		if err != nil {
			logger.Printf("InsertArchaeologistFinding(%s,%s:%d): %v", patternID, h.FilePath, h.LineNumber, err)
			continue
		}
		if id == 0 {
			deduped++
			continue
		}
		created++
	}
	return created, deduped
}

// runArchaeologistProposeMigration handles one
// ArchaeologistProposeMigration bounty. Calls librarian.Client.EmitCandidate
// (anti-cheat #1: operator-ratified handoff; the Archaeologist NEVER
// auto-dispatches the migration). On success, flips the cluster's
// findings to status='proposed' so the next sweep doesn't re-fire.
//
// Pattern P-ArchaeologistOperatorGated (audit pattern in
// internal/audittools/) asserts at AST level that this is the ONLY
// place internal/archaeologist (and the agent) hands off to a
// proposal-emission seam.
//
// The supplied graph.Client (gc) is consulted via
// computeArchaeologistBlastRadius to enrich the proposal's EvidenceJSON
// with a blast-radius snapshot — the set of downstream consumer repos
// (and per-symbol consumer call-sites) that depend on the producer
// symbols whose source files contain the deprecated-API hits. This is
// pure DATA enrichment: the seam is unchanged (still EmitCandidate),
// and Pattern P-ArchaeologistOperatorGated remains satisfied. A
// graph-side failure (ErrIndexNotReady, ErrNotImplemented, etc.) is
// degraded — we log + emit the candidate without the blast_radius
// block rather than failing the bounty.
func runArchaeologistProposeMigration(ctx context.Context, db *sql.DB, lib librarian.Client, gc graph.Client, agentName string, bounty *store.Bounty, logger interface{ Printf(string, ...any) }) {
	var payload archaeologistProposeMigrationPayload
	if err := json.Unmarshal([]byte(bounty.Payload), &payload); err != nil {
		if fbErr := store.FailBounty(db, bounty.ID, fmt.Sprintf("ArchaeologistProposeMigration: invalid payload: %v", err)); fbErr != nil {
			logger.Printf("ArchaeologistProposeMigration #%d: FailBounty after payload parse: %v", bounty.ID, fbErr)
		}
		return
	}
	pattern := patterns.ByID(payload.PatternID)
	if pattern == nil {
		if fbErr := store.FailBounty(db, bounty.ID, fmt.Sprintf("ArchaeologistProposeMigration: unknown pattern %s", payload.PatternID)); fbErr != nil {
			logger.Printf("ArchaeologistProposeMigration #%d: FailBounty after unknown pattern: %v", bounty.ID, fbErr)
		}
		return
	}
	repoTarget, err := store.GetArchaeologistRepoByID(db, payload.RepoID)
	if err != nil {
		if fbErr := store.FailBounty(db, bounty.ID, fmt.Sprintf("ArchaeologistProposeMigration: repo %d not found: %v", payload.RepoID, err)); fbErr != nil {
			logger.Printf("ArchaeologistProposeMigration #%d: FailBounty after missing repo: %v", bounty.ID, fbErr)
		}
		return
	}
	findings, err := store.ListOpenArchaeologistFindings(db, payload.PatternID, payload.RepoID)
	if err != nil {
		if fbErr := store.FailBounty(db, bounty.ID, fmt.Sprintf("ArchaeologistProposeMigration: list findings failed: %v", err)); fbErr != nil {
			logger.Printf("ArchaeologistProposeMigration #%d: FailBounty after list: %v", bounty.ID, fbErr)
		}
		return
	}
	if len(findings) < pattern.MinHitsForFeature() {
		// Threshold no longer met (operator manually rejected some
		// findings, or a follow-up sweep dropped them). Complete as a
		// no-op so the operator queue isn't cluttered.
		logger.Printf("ArchaeologistProposeMigration #%d: %s on %s — only %d open hits (< MinHitsForFeature=%d); skipping",
			bounty.ID, payload.PatternID, repoTarget.Name, len(findings), pattern.MinHitsForFeature())
		if uErr := store.UpdateBountyStatus(db, bounty.ID, "Completed"); uErr != nil {
			logger.Printf("ArchaeologistProposeMigration #%d: Completed update failed on threshold-miss no-op: %v", bounty.ID, uErr)
		}
		return
	}

	hypothesisKey := fmt.Sprintf("archaeologist-%s-%s", strings.ToLower(payload.PatternID), strings.ToLower(repoTarget.Name))
	body := buildArchaeologistMigrationBody(payload.PatternID, repoTarget.Name, findings, pattern.MinHitsForFeature())
	// D9 exit-#5 — enrich the candidate's evidence with a blast-radius
	// snapshot. computeArchaeologistBlastRadius walks the producer's
	// CrossRepoSymbols rows whose file_path matches a finding file_path
	// and asks the graph for the consumer set. On graph failure
	// (ErrIndexNotReady / dog hasn't run / non-Go repo) we degrade to the
	// legacy evidence shape rather than failing the bounty — the
	// candidate still reaches the operator queue.
	br, brErr := computeArchaeologistBlastRadius(ctx, db, gc, repoTarget.Name, findings)
	if brErr != nil {
		logger.Printf("ArchaeologistProposeMigration #%d: blast-radius enrich failed (%v) — emitting candidate without blast_radius block", bounty.ID, brErr)
	}
	evidence := buildArchaeologistMigrationEvidence(payload.PatternID, repoTarget, findings, br, brErr == nil)

	proposalID, eErr := lib.EmitCandidate(ctx, librarian.Candidate{
		HypothesisKey: hypothesisKey,
		HypothesisRaw: body,
		EvidenceJSON:  evidence,
	})
	if eErr != nil {
		if fbErr := store.FailBounty(db, bounty.ID, fmt.Sprintf("ArchaeologistProposeMigration: EmitCandidate(%s): %v", hypothesisKey, eErr)); fbErr != nil {
			logger.Printf("ArchaeologistProposeMigration #%d: FailBounty after EmitCandidate failure: %v", bounty.ID, fbErr)
		}
		return
	}

	flipped, sErr := store.SetArchaeologistFindingsStatus(db, payload.PatternID, payload.RepoID, "open", "proposed")
	if sErr != nil {
		// Don't fail the bounty — the proposal was successfully
		// emitted. Log + continue; the next sweep's threshold check
		// will see open hits and try to re-emit, which is operator-
		// noticeable but not corruption-causing (EmitCandidate writes
		// a fresh PromotionProposals row each call).
		logger.Printf("ArchaeologistProposeMigration #%d: SetArchaeologistFindingsStatus failed (%v) — proposal #%d still emitted, next sweep may re-fire", bounty.ID, sErr, proposalID)
	}
	logger.Printf("ArchaeologistProposeMigration #%d: %s on %s — emitted candidate #%d (%d findings, %d flipped to proposed)",
		bounty.ID, payload.PatternID, repoTarget.Name, proposalID, len(findings), flipped)

	if err := store.UpdateBountyStatus(db, bounty.ID, "Completed"); err != nil {
		logger.Printf("ArchaeologistProposeMigration #%d: Completed update failed after EmitCandidate: %v", bounty.ID, err)
	}
}

// buildArchaeologistMigrationBody composes the human-readable proposal
// body. Operator reads this in the dashboard EC tab to decide whether
// to ratify.
func buildArchaeologistMigrationBody(patternID, repoName string, findings []store.ArchaeologistFinding, threshold int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Pattern %s detected %d sites across %s (>= %d threshold).\n\n",
		patternID, len(findings), repoName, threshold)
	b.WriteString("Sample sites (first 10):\n")
	for i, f := range findings {
		if i >= 10 {
			break
		}
		fmt.Fprintf(&b, "  - %s:%d\n", f.FilePath, f.LineNumber)
	}
	if len(findings) > 10 {
		fmt.Fprintf(&b, "  ... and %d more.\n", len(findings)-10)
	}
	b.WriteString("\nProposed migration: rewrite each site to the modern equivalent. Operator must ratify before any code change is dispatched (anti-cheat #1: archaeologist proposes, operator ratifies).")
	return b.String()
}

// archaeologistBlastRadiusBlock is the JSON shape merged into the
// EvidenceJSON under the "blast_radius" key. It mirrors the subset of
// graph.BlastRadius that survives JSON serialisation cleanly:
//
//   - modified_symbols: the producer's exported symbols whose source
//     files contain at least one ARCH-XXX hit (the symbols a ratified
//     migration would rewrite).
//   - affected_consumer_repos: the alphabetised, de-duplicated set of
//     downstream repos that import any of the modified symbols.
//   - consumers_by_symbol: per-modified-symbol list of consumer
//     (repo, file, line) call-sites — operator reads this to gauge the
//     blast radius before ratifying.
//
// Field tags are snake_case to match the rest of evidence_summary_json.
type archaeologistBlastRadiusBlock struct {
	ModifiedSymbols       []archaeologistBlastRadiusSymbol            `json:"modified_symbols"`
	AffectedConsumerRepos []string                                    `json:"affected_consumer_repos"`
	ConsumersBySymbol     map[string][]archaeologistBlastRadiusSite   `json:"consumers_by_symbol"`
}

// archaeologistBlastRadiusSymbol is one entry in modified_symbols.
type archaeologistBlastRadiusSymbol struct {
	Repo       string `json:"repo"`
	SymbolPath string `json:"symbol_path"`
	Kind       string `json:"kind"`
	FilePath   string `json:"file_path"`
	LineNumber int    `json:"line_number"`
}

// archaeologistBlastRadiusSite is one entry in consumers_by_symbol's
// per-symbol list.
type archaeologistBlastRadiusSite struct {
	Repo     string `json:"repo"`
	FilePath string `json:"file_path"`
	Line     int    `json:"line"`
}

// buildArchaeologistMigrationEvidence emits a JSON evidence block
// consumable by the dashboard / EC ratification pipeline. When
// blastRadiusOK is true, the supplied block is merged under the
// "blast_radius" key; otherwise the block is omitted (the candidate
// still emits, but with the legacy shape) so operators can distinguish
// "graph is wired and saw no consumers" from "graph couldn't run".
func buildArchaeologistMigrationEvidence(patternID string, repoTarget store.ArchaeologistRepoTarget, findings []store.ArchaeologistFinding, blastRadius archaeologistBlastRadiusBlock, blastRadiusOK bool) string {
	type site struct {
		FilePath   string `json:"file_path"`
		LineNumber int    `json:"line_number"`
		DetailJSON string `json:"detail_json"`
	}
	sites := make([]site, 0, len(findings))
	for _, f := range findings {
		sites = append(sites, site{
			FilePath:   f.FilePath,
			LineNumber: f.LineNumber,
			DetailJSON: f.DetailJSON,
		})
	}
	payload := map[string]any{
		"pattern_id":  patternID,
		"agent_scope": "archaeologist",
		"category":    "migration_proposal",
		"origin":      "archaeologist-migration-proposal",
		"repo_id":     repoTarget.ID,
		"repo_name":   repoTarget.Name,
		"site_count":  len(findings),
		"sites":       sites,
	}
	if blastRadiusOK {
		// Normalise nil slices/maps to their empty equivalents so the
		// JSON shape is stable regardless of whether the graph saw zero
		// or N consumers.
		if blastRadius.ModifiedSymbols == nil {
			blastRadius.ModifiedSymbols = []archaeologistBlastRadiusSymbol{}
		}
		if blastRadius.AffectedConsumerRepos == nil {
			blastRadius.AffectedConsumerRepos = []string{}
		}
		if blastRadius.ConsumersBySymbol == nil {
			blastRadius.ConsumersBySymbol = map[string][]archaeologistBlastRadiusSite{}
		}
		payload["blast_radius"] = blastRadius
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return "{}"
	}
	return string(out)
}

// computeArchaeologistBlastRadius asks the graph client for the
// downstream consumer set of the producer-repo's exported symbols whose
// source files contain at least one ARCH-XXX hit (i.e. the public
// symbols a ratified migration would touch).
//
// The lookup is purely data-driven against the dog-populated
// CrossRepoSymbols / CrossRepoDependencies tables — no LLM, no AST
// re-parsing on the agent side. Algorithm:
//
//  1. Collect the de-duplicated set of file_paths from `findings`.
//  2. Pull every CrossRepoSymbols row for `producerRepo` and keep the
//     ones whose file_path is in the finding set.
//  3. Build a SymbolModification per surviving symbol and call
//     gc.BlastRadiusForModifications.
//  4. Re-shape the result into archaeologistBlastRadiusBlock for the
//     EvidenceJSON.
//
// Returns an empty block + error when the graph isn't ready or the
// store helpers fail; the caller degrades gracefully.
func computeArchaeologistBlastRadius(ctx context.Context, db *sql.DB, gc graph.Client, producerRepo string, findings []store.ArchaeologistFinding) (archaeologistBlastRadiusBlock, error) {
	if gc == nil {
		return archaeologistBlastRadiusBlock{}, fmt.Errorf("computeArchaeologistBlastRadius: graph client is nil")
	}
	if db == nil {
		return archaeologistBlastRadiusBlock{}, fmt.Errorf("computeArchaeologistBlastRadius: db is nil")
	}
	if producerRepo == "" {
		return archaeologistBlastRadiusBlock{}, fmt.Errorf("computeArchaeologistBlastRadius: producerRepo required")
	}
	// Step 1 — collect finding file_paths.
	findingFiles := map[string]struct{}{}
	for _, f := range findings {
		if f.FilePath == "" {
			continue
		}
		findingFiles[f.FilePath] = struct{}{}
	}
	if len(findingFiles) == 0 {
		// No findings → no modifications → empty block (still successful).
		return archaeologistBlastRadiusBlock{}, nil
	}
	// Step 2 — intersect against CrossRepoSymbols for the producer.
	syms, err := store.ListCrossRepoSymbolsByRepo(db, producerRepo)
	if err != nil {
		return archaeologistBlastRadiusBlock{}, fmt.Errorf("computeArchaeologistBlastRadius: list symbols: %w", err)
	}
	mods := make([]graph.SymbolModification, 0, len(syms))
	seenSymbols := map[string]struct{}{}
	for _, s := range syms {
		if _, hit := findingFiles[s.FilePath]; !hit {
			continue
		}
		if _, dup := seenSymbols[s.SymbolPath]; dup {
			continue
		}
		seenSymbols[s.SymbolPath] = struct{}{}
		mods = append(mods, graph.SymbolModification{
			Repo:       s.RepoName,
			FilePath:   s.FilePath,
			SymbolPath: s.SymbolPath,
		})
	}
	if len(mods) == 0 {
		// Producer has no indexed exported symbols matching the finding
		// files (e.g. the dog hasn't run yet, or the deprecated calls
		// live in unexported helpers). Empty block, no error — caller
		// emits the candidate with a {} blast_radius (operator-visible
		// signal that "graph saw nothing", distinct from "graph errored").
		return archaeologistBlastRadiusBlock{}, nil
	}
	// Step 3 — query the graph.
	br, err := gc.BlastRadiusForModifications(ctx, mods)
	if err != nil {
		return archaeologistBlastRadiusBlock{}, fmt.Errorf("computeArchaeologistBlastRadius: graph: %w", err)
	}
	// Step 4 — re-shape into the JSON-friendly block.
	out := archaeologistBlastRadiusBlock{
		ModifiedSymbols:       make([]archaeologistBlastRadiusSymbol, 0, len(br.ModifiedSymbols)),
		AffectedConsumerRepos: append([]string(nil), br.AffectedConsumerRepos...),
		ConsumersBySymbol:     map[string][]archaeologistBlastRadiusSite{},
	}
	for _, s := range br.ModifiedSymbols {
		out.ModifiedSymbols = append(out.ModifiedSymbols, archaeologistBlastRadiusSymbol{
			Repo:       s.Repo,
			SymbolPath: s.Name,
			Kind:       s.Kind,
			FilePath:   s.Path,
			LineNumber: s.Line,
		})
	}
	for sym, sites := range br.ConsumersBySymbol {
		bucket := make([]archaeologistBlastRadiusSite, 0, len(sites))
		for _, s := range sites {
			bucket = append(bucket, archaeologistBlastRadiusSite{
				Repo:     s.Repo,
				FilePath: s.FilePath,
				Line:     s.Line,
			})
		}
		out.ConsumersBySymbol[sym] = bucket
	}
	return out, nil
}
