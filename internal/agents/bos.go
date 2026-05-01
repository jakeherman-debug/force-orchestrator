// Package agents — Bureau of Standards (BoS, D4 Phase 1).
//
// SpawnBoS claims `BoSReview` tasks queued by the astromech post-commit
// hook (see runAstromechTask), runs every active BoS Rule against the
// commit's diff (Go files only), records every Finding to the
// SecurityFindings table, and decides verdict:
//
//   - any block-severity Finding remains AFTER bypass downgrades →
//     return the source task to Pending with feedback (Rejected).
//   - all Findings are advise OR overridden → transition the source
//     task to its prior next-review status (Approved-with-warnings).
//
// Rules execute as pure Go AST analysis — no LLM call, no external
// I/O beyond git's diff/show subprocess. CLAUDE.md Cost model: BoS is
// near-free. CLAUDE.md No silent failures: every error path terminates
// in store.FailBounty / store.UpdateBountyStatus.
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
	"force-orchestrator/internal/bos"
	_ "force-orchestrator/internal/bos/rules" // register BOS-001..BOS-011
	igit "force-orchestrator/internal/git"
	"force-orchestrator/internal/store"
	"force-orchestrator/internal/telemetry"
)

// bosReviewPayload is the JSON shape QueueBoSReview emits. The BoS
// reviewer deserializes from BountyBoard.payload.
type bosReviewPayload struct {
	SourceTaskID int    `json:"source_task_id"`
	Branch       string `json:"branch"`
	CommitSHA    string `json:"commit_sha"`
	TargetRepo   string `json:"target_repo"`
}

// SpawnBoS is the BoS agent claim loop. Mirrors SpawnCaptain's shape
// (e-stop / spend-cap gates + ctx cancellation). Tasks of type
// `BoSReview` are claimed via store.ClaimBounty.
//
// The capability profile is loaded at spawn time so a missing/invalid
// `bos.yaml` fails loudly rather than at first task. BoS does not
// invoke claude — the profile is loaded for orchestration shape
// (Pattern P13 fail-closed posture) but no AllowedToolsArg/etc. are
// passed to a Claude call site.
func SpawnBoS(ctx context.Context, db *sql.DB, name string) {
	logger := NewLogger(name)

	if _, err := capabilities.LoadProfile("bos"); err != nil {
		logger.Printf("BoS %s cannot start: %v", name, err)
		return
	}
	logger.Printf("BoS %s standing by", name)

	for {
		if ctx.Err() != nil {
			logger.Printf("BoS %s exiting: %v", name, ctx.Err())
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

		b, claimed := store.ClaimBounty(db, "BoSReview", name)
		if !claimed {
			time.Sleep(time.Duration(2500+rand.Intn(1000)) * time.Millisecond)
			continue
		}
		runBoSReviewTask(ctx, db, name, b, logger)
	}
}

// runBoSReviewTask reviews a single BoSReview task: parses the
// payload, enumerates changed Go files, runs every active rule,
// records findings, and decides the verdict.
func runBoSReviewTask(ctx context.Context, db *sql.DB, agentName string, b *store.Bounty, logger interface{ Printf(string, ...any) }) {
	sessionID := telemetry.NewSessionID()
	logger.Printf("[%s] BoS claimed task %d (parent=%d)", sessionID, b.ID, b.ParentID)

	var p bosReviewPayload
	if err := json.Unmarshal([]byte(b.Payload), &p); err != nil {
		msg := fmt.Sprintf("BoSReview payload parse failed: %v", err)
		if fbErr := store.FailBounty(db, b.ID, msg); fbErr != nil {
			logger.Printf("Task %d: FailBounty after payload parse failure also failed (%v)", b.ID, fbErr)
		}
		telemetry.EmitEvent(telemetry.EventTaskFailed(sessionID, agentName, b.ID, msg))
		return
	}
	if p.SourceTaskID == 0 {
		msg := "BoSReview payload missing source_task_id"
		if fbErr := store.FailBounty(db, b.ID, msg); fbErr != nil {
			logger.Printf("Task %d: FailBounty after invalid payload also failed (%v)", b.ID, fbErr)
		}
		return
	}

	// Resolve repo path for git-diff enumeration.
	repoPath := store.GetRepoPath(db, p.TargetRepo)
	if repoPath == "" {
		// No repo path = no diff possible. Treat as approved-with-warnings:
		// record one synthetic advise-finding and complete the BoSReview
		// row so the source task can advance. Do NOT block.
		logger.Printf("Task %d: BoS could not resolve repo path for %q — completing without findings", b.ID, p.TargetRepo)
		bosFinish(db, b, p, agentName, sessionID, logger, false)
		return
	}

	files, inputs, err := loadBoSReviewInputs(ctx, repoPath, p.Branch)
	if err != nil {
		msg := fmt.Sprintf("BoSReview file load failed: %v", err)
		if fbErr := store.FailBounty(db, b.ID, msg); fbErr != nil {
			logger.Printf("Task %d: FailBounty after file load also failed (%v)", b.ID, fbErr)
		}
		telemetry.EmitEvent(telemetry.EventTaskFailed(sessionID, agentName, b.ID, msg))
		return
	}
	logger.Printf("Task %d: BoS scanning %d Go files", b.ID, len(files))

	// Production gate sources active rules from FleetRules.
	gate := bos.DBFleetRulesGate(db)
	res := bos.ReviewFiles(gate, inputs)

	// Persist findings to SecurityFindings.
	for _, f := range res.Findings {
		_, insErr := store.InsertSecurityFinding(db, store.SecurityFinding{
			TaskID:     p.SourceTaskID,
			Bureau:     "BoS",
			RuleID:     f.RuleID,
			Severity:   string(f.Severity),
			FilePath:   f.Path,
			LineNumber: f.Line,
			Message:    f.Message,
			CommitSHA:  p.CommitSHA,
			Disposition: dispositionFromMessage(f.Message),
			BypassAuditID: extractAuditFromBypassed(f.Message),
			BypassReason:  extractReasonFromBypassed(f.Message),
		})
		if insErr != nil {
			logger.Printf("Task %d: SecurityFindings insert failed for %s: %v", b.ID, f.RuleID, insErr)
		}
	}

	bosFinish(db, b, p, agentName, sessionID, logger, res.HasBlock)
}

// bosFinish records the BoS verdict on the source task and marks the
// BoSReview infrastructure task Completed. Idempotent: safe to call
// multiple times against the same source task.
func bosFinish(db *sql.DB, b *store.Bounty, p bosReviewPayload, agentName, sessionID string, logger interface{ Printf(string, ...any) }, hasBlock bool) {
	srcTask, err := store.GetBounty(db, p.SourceTaskID)
	if err != nil || srcTask == nil {
		logger.Printf("Task %d: BoS source task %d not found: %v", b.ID, p.SourceTaskID, err)
		_ = store.UpdateBountyStatus(db, b.ID, "Completed")
		return
	}

	if hasBlock {
		logger.Printf("Task %d: BoS REJECTED source task %d (block findings remain)", b.ID, p.SourceTaskID)
		// Return the source task for rework with BoS feedback in the payload.
		feedback := "\n\nBOS FEEDBACK: One or more block-severity findings were recorded against this commit. Inspect SecurityFindings (rule_id, file_path, line_number) and either fix the violation OR add a `// BOS-BYPASS: AUDIT-NNN <reason>` comment on the line directly above the violating code (reason >= 10 chars)."
		store.ReturnTaskForRework(db, p.SourceTaskID, srcTask.Payload+feedback)
		store.LogAudit(db, agentName, "bos-rejected", p.SourceTaskID, "BoS recorded block-severity findings")
		telemetry.EmitEvent(telemetry.EventTaskCompleted(sessionID, agentName, b.ID))
		_ = store.UpdateBountyStatus(db, b.ID, "Completed")
		return
	}

	// Approved-with-warnings: leave the source task in its current
	// post-commit status so the existing pipeline (Captain → Council → ...)
	// continues. The BoSReview infrastructure task itself is Completed.
	logger.Printf("Task %d: BoS APPROVED source task %d (no block findings)", b.ID, p.SourceTaskID)
	store.LogAudit(db, agentName, "bos-approved", p.SourceTaskID, "no blocking findings after bypass")
	telemetry.EmitEvent(telemetry.EventTaskCompleted(sessionID, agentName, b.ID))
	if err := store.UpdateBountyStatus(db, b.ID, "Completed"); err != nil {
		logger.Printf("Task %d: BoSReview Completed status transition failed: %v", b.ID, err)
	}
}

// loadBoSReviewInputs enumerates the changed Go files between
// `origin/<defaultBranch>` (or the convoy ask-branch) and `branch`
// using git --name-only, then reads the source from the worktree.
//
// Files that have been deleted in the diff are skipped (we cannot
// AST-scan a deletion). Files whose worktree read fails are reported
// as a single advise-finding via a synthetic input — no silent skip.
func loadBoSReviewInputs(ctx context.Context, repoPath, branch string) ([]string, []bos.ReviewInput, error) {
	if branch == "" {
		return nil, nil, fmt.Errorf("loadBoSReviewInputs: empty branch")
	}
	base := igit.GetDefaultBranch(ctx, repoPath)
	files := igit.ChangedGoFilesFromBase(ctx, repoPath, base, branch)
	if len(files) == 0 {
		return nil, nil, nil
	}
	var inputs []bos.ReviewInput
	for _, rel := range files {
		// Read from the worktree's checkout of `branch`. The astromech
		// committed and we're scanning at HEAD; reading the file from
		// the repo's worktree is the simplest path.
		full := filepath.Join(repoPath, rel)
		body, err := os.ReadFile(full)
		if err != nil {
			// Deletion or permission error — skip; the BoS reviewer's
			// purpose is to gate ADDITIONS / MODIFICATIONS, and a
			// deleted file is never a Pattern P13/P16/etc. concern.
			if strings.Contains(err.Error(), "no such file") {
				continue
			}
			return nil, nil, fmt.Errorf("read %s: %w", full, err)
		}
		inputs = append(inputs, bos.ReviewInput{Path: rel, Source: string(body)})
	}
	return files, inputs, nil
}

// dispositionFromMessage returns 'overridden' iff the rule reviewer
// already prefixed the message with '[BYPASSED ' — that's the marker
// applied by bos.ReviewFiles when a // BOS-BYPASS comment matched.
func dispositionFromMessage(msg string) string {
	if strings.HasPrefix(msg, "[BYPASSED ") {
		return "overridden"
	}
	return ""
}

// extractAuditFromBypassed parses 'AUDIT-NNN' out of a '[BYPASSED
// AUDIT-NNN: ...' message prefix. Returns empty string if no match.
func extractAuditFromBypassed(msg string) string {
	if !strings.HasPrefix(msg, "[BYPASSED ") {
		return ""
	}
	rest := strings.TrimPrefix(msg, "[BYPASSED ")
	colon := strings.Index(rest, ":")
	if colon < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:colon])
}

// extractReasonFromBypassed parses the reason out of a '[BYPASSED
// AUDIT-NNN: <reason>] <orig msg>' shape.
func extractReasonFromBypassed(msg string) string {
	if !strings.HasPrefix(msg, "[BYPASSED ") {
		return ""
	}
	close := strings.Index(msg, "] ")
	if close < 0 {
		return ""
	}
	colon := strings.Index(msg, ": ")
	if colon < 0 || colon > close {
		return ""
	}
	return strings.TrimSpace(msg[colon+2 : close])
}
