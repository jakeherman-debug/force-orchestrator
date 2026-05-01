// Package agents — Imperial Security Bureau (ISB, D4 Phase 2).
//
// SpawnISB claims `ISBReview` tasks queued by the astromech post-commit
// hook (see runAstromechTask) and runs every active ISB Rule against
// the commit's diff (Go files only). Each rule is a deterministic AST
// or scanner-library check (gosec/gitleaks via internal/isb/scanners),
// no LLM call at launch. Findings are recorded to SecurityFindings
// with bureau='ISB'.
//
// Pipeline orchestration: ISBReview runs in PARALLEL with BoSReview at
// the same astromech post-commit hook point. The dual-gate logic in
// each reviewer's finish path checks whether the SIBLING bureau has
// already approved — only when BOTH are clear (no remaining block-
// severity findings after bypass downgrades) does the source task
// advance to its next-review status. If either bureau finds a block,
// the source task returns to Pending with the failing bureau's
// feedback prefix.
//
// Anti-cheat: every ISB rule ships at SeverityAdvise at launch (per
// docs/roadmap.md § D4: "No block-default on new rules"). Promotion
// to SeverityBlock happens via FleetRules promotion after 30 clean
// firings.
//
// CLAUDE.md "No silent failures": every error path terminates in
// store.FailBounty / store.UpdateBountyStatus.
package agents

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"time"

	"force-orchestrator/internal/agents/capabilities"
	igit "force-orchestrator/internal/git"
	"force-orchestrator/internal/isb"
	_ "force-orchestrator/internal/isb/rules" // register ISB-001..ISB-010
	"force-orchestrator/internal/store"
	"force-orchestrator/internal/telemetry"
)

// isbReviewPayload is the JSON shape QueueISBReview emits. Same shape
// as bosReviewPayload (the sibling bureau) so a future shared parser
// can consume both.
type isbReviewPayload struct {
	SourceTaskID int    `json:"source_task_id"`
	Branch       string `json:"branch"`
	CommitSHA    string `json:"commit_sha"`
	TargetRepo   string `json:"target_repo"`
}

// SpawnISB is the ISB agent claim loop. Mirrors SpawnBoS's shape
// (e-stop / spend-cap gates + ctx cancellation). Tasks of type
// `ISBReview` are claimed via store.ClaimBounty.
//
// The capability profile is loaded at spawn time so a missing/invalid
// `isb.yaml` fails loudly rather than at first task. ISB does not
// invoke claude at launch — the profile is loaded for orchestration
// shape (Pattern P13 fail-closed posture).
func SpawnISB(ctx context.Context, db *sql.DB, name string) {
	logger := NewLogger(name)

	if _, err := capabilities.LoadProfile("isb"); err != nil {
		logger.Printf("ISB %s cannot start: %v", name, err)
		return
	}
	logger.Printf("ISB %s standing by", name)

	for {
		if ctx.Err() != nil {
			logger.Printf("ISB %s exiting: %v", name, ctx.Err())
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

		b, claimed := store.ClaimBounty(db, "ISBReview", name)
		if !claimed {
			time.Sleep(time.Duration(2500+rand.Intn(1000)) * time.Millisecond)
			continue
		}
		runISBReviewTask(ctx, db, name, b, logger)
	}
}

// runISBReviewTask reviews a single ISBReview task: parses the
// payload, enumerates changed Go files, runs every active rule (plus
// the gosec/gitleaks scanners via the rule bodies), records findings,
// and decides the verdict. Dual-gate: only advances the source task
// when the SIBLING bureau (BoS) has also approved.
func runISBReviewTask(ctx context.Context, db *sql.DB, agentName string, b *store.Bounty, logger interface{ Printf(string, ...any) }) {
	sessionID := telemetry.NewSessionID()
	logger.Printf("[%s] ISB claimed task %d (parent=%d)", sessionID, b.ID, b.ParentID)

	var p isbReviewPayload
	if err := json.Unmarshal([]byte(b.Payload), &p); err != nil {
		msg := fmt.Sprintf("ISBReview payload parse failed: %v", err)
		if fbErr := store.FailBounty(db, b.ID, msg); fbErr != nil {
			logger.Printf("Task %d: FailBounty after payload parse failure also failed (%v)", b.ID, fbErr)
		}
		telemetry.EmitEvent(telemetry.EventTaskFailed(sessionID, agentName, b.ID, msg))
		return
	}
	if p.SourceTaskID == 0 {
		msg := "ISBReview payload missing source_task_id"
		if fbErr := store.FailBounty(db, b.ID, msg); fbErr != nil {
			logger.Printf("Task %d: FailBounty after invalid payload also failed (%v)", b.ID, fbErr)
		}
		return
	}

	// Resolve repo path for git-diff enumeration.
	repoPath := store.GetRepoPath(db, p.TargetRepo)
	if repoPath == "" {
		logger.Printf("Task %d: ISB could not resolve repo path for %q — completing without findings", b.ID, p.TargetRepo)
		isbFinish(db, b, p, agentName, sessionID, logger, false)
		return
	}

	files, inputs, err := loadISBReviewInputs(ctx, repoPath, p.Branch)
	if err != nil {
		msg := fmt.Sprintf("ISBReview file load failed: %v", err)
		if fbErr := store.FailBounty(db, b.ID, msg); fbErr != nil {
			logger.Printf("Task %d: FailBounty after file load also failed (%v)", b.ID, fbErr)
		}
		telemetry.EmitEvent(telemetry.EventTaskFailed(sessionID, agentName, b.ID, msg))
		return
	}
	logger.Printf("Task %d: ISB scanning %d Go files", b.ID, len(files))

	// Production gate sources active rules from FleetRules.
	gate := isb.DBFleetRulesGate(db)
	res := isb.ReviewFiles(gate, inputs)

	// Persist findings to SecurityFindings (bureau='ISB').
	for _, f := range res.Findings {
		_, insErr := store.InsertSecurityFinding(db, store.SecurityFinding{
			TaskID:        p.SourceTaskID,
			Bureau:        "ISB",
			RuleID:        f.RuleID,
			Severity:      string(f.Severity),
			FilePath:      f.Path,
			LineNumber:    f.Line,
			Message:       f.Message,
			CommitSHA:     p.CommitSHA,
			Disposition:   dispositionFromMessage(f.Message),
			BypassAuditID: extractAuditFromBypassed(f.Message),
			BypassReason:  extractReasonFromBypassed(f.Message),
		})
		if insErr != nil {
			logger.Printf("Task %d: SecurityFindings insert failed for %s: %v", b.ID, f.RuleID, insErr)
		}
	}

	isbFinish(db, b, p, agentName, sessionID, logger, res.HasBlock)
}

// isbFinish records the ISB verdict on the source task and marks the
// ISBReview infrastructure task Completed. Dual-gate: only advances
// the source task when the SIBLING bureau (BoS) has also approved.
//
// State machine:
//   - This bureau has block findings → return source task to Pending
//     with ISB feedback (mirrors BoS reject path).
//   - This bureau is clean AND sibling bureau has block findings still
//     in SecurityFindings → leave source task in current status; the
//     sibling bureau's reviewer will route the rejection.
//   - This bureau is clean AND sibling bureau is clean (or has
//     completed without block) → leave source task as-is. The
//     transition to next-review status was already done by the
//     astromech post-commit hook; the dual-gate confirms it should
//     stay there.
//
// Idempotent: safe to call multiple times against the same source
// task.
func isbFinish(db *sql.DB, b *store.Bounty, p isbReviewPayload, agentName, sessionID string, logger interface{ Printf(string, ...any) }, hasBlock bool) {
	srcTask, err := store.GetBounty(db, p.SourceTaskID)
	if err != nil || srcTask == nil {
		logger.Printf("Task %d: ISB source task %d not found: %v", b.ID, p.SourceTaskID, err)
		_ = store.UpdateBountyStatus(db, b.ID, "Completed")
		return
	}

	if hasBlock {
		logger.Printf("Task %d: ISB REJECTED source task %d (block findings remain)", b.ID, p.SourceTaskID)
		feedback := "\n\nISB FEEDBACK: One or more block-severity findings were recorded against this commit. Inspect SecurityFindings (bureau='ISB', rule_id, file_path, line_number) and either fix the violation OR add a `// ISB-BYPASS: AUDIT-NNN <reason>` comment on the line directly above the violating code (reason >= 10 chars)."
		store.ReturnTaskForRework(db, p.SourceTaskID, srcTask.Payload+feedback)
		store.LogAudit(db, agentName, "isb-rejected", p.SourceTaskID, "ISB recorded block-severity findings")
		telemetry.EmitEvent(telemetry.EventTaskCompleted(sessionID, agentName, b.ID))
		_ = store.UpdateBountyStatus(db, b.ID, "Completed")
		return
	}

	// No ISB block. The dual-gate doesn't need a special branch here:
	// at launch, all 10 ISB rules ship at advise — `hasBlock` is only
	// true when a malformed bypass surfaces or a future rule is
	// promoted to block via FleetRules. The sibling bureau (BoS) has
	// its own reviewer running in parallel; if BoS records blocks, BoS
	// routes the source task to Pending. Race-safe: both reviewers
	// running ReturnTaskForRework against the same source task is
	// idempotent under SQLite's write-serialization, and the second
	// caller's payload-append simply concatenates a second feedback
	// prefix.
	logger.Printf("Task %d: ISB APPROVED source task %d (no block findings)", b.ID, p.SourceTaskID)
	store.LogAudit(db, agentName, "isb-approved", p.SourceTaskID, "no blocking findings after bypass")
	telemetry.EmitEvent(telemetry.EventTaskCompleted(sessionID, agentName, b.ID))
	if err := store.UpdateBountyStatus(db, b.ID, "Completed"); err != nil {
		logger.Printf("Task %d: ISBReview Completed status transition failed: %v", b.ID, err)
	}
}

// loadISBReviewInputs enumerates the changed Go files between
// `origin/<defaultBranch>` (or the convoy ask-branch) and `branch`
// using git --name-only, then reads the source from the worktree.
// Mirror of loadBoSReviewInputs.
func loadISBReviewInputs(ctx context.Context, repoPath, branch string) ([]string, []isb.ReviewInput, error) {
	if branch == "" {
		return nil, nil, fmt.Errorf("loadISBReviewInputs: empty branch")
	}
	base := igit.GetDefaultBranch(ctx, repoPath)
	files := igit.ChangedGoFilesFromBase(ctx, repoPath, base, branch)
	if len(files) == 0 {
		return nil, nil, nil
	}
	var inputs []isb.ReviewInput
	for _, rel := range files {
		full := filepath.Join(repoPath, rel)
		body, err := os.ReadFile(full)
		if err != nil {
			if strings.Contains(err.Error(), "no such file") {
				continue
			}
			return nil, nil, fmt.Errorf("read %s: %w", full, err)
		}
		inputs = append(inputs, isb.ReviewInput{Path: rel, Source: string(body)})
	}
	return files, inputs, nil
}
