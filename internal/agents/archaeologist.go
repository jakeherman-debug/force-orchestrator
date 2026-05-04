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
func SpawnArchaeologist(ctx context.Context, db *sql.DB, lib librarian.Client, name string) {
	logger := NewLogger(name)
	logger.Printf("Archaeologist %s coming online", name)
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
			runArchaeologistProposeMigration(ctx, db, lib, name, bounty, logger)
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
func runArchaeologistProposeMigration(ctx context.Context, db *sql.DB, lib librarian.Client, agentName string, bounty *store.Bounty, logger interface{ Printf(string, ...any) }) {
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
	evidence := buildArchaeologistMigrationEvidence(payload.PatternID, repoTarget, findings)

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

// buildArchaeologistMigrationEvidence emits a JSON evidence block
// consumable by the dashboard / EC ratification pipeline.
func buildArchaeologistMigrationEvidence(patternID string, repoTarget store.ArchaeologistRepoTarget, findings []store.ArchaeologistFinding) string {
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
	out, err := json.Marshal(map[string]any{
		"pattern_id": patternID,
		"repo_id":    repoTarget.ID,
		"repo_name":  repoTarget.Name,
		"site_count": len(findings),
		"sites":      sites,
	})
	if err != nil {
		return "{}"
	}
	return string(out)
}
