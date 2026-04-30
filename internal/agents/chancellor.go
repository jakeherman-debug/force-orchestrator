package agents

import (
	"log"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"force-orchestrator/internal/agents/capabilities"
	"force-orchestrator/internal/claude"
	"force-orchestrator/internal/store"
	"force-orchestrator/internal/util"
)

const chancellorName = "Supreme-Chancellor-Palpatine"

const chancellorSystemPrompt = `You are the Supreme Chancellor of the Galactic Fleet's software operations.
Your role is to review proposed deployment plans from Commanders before they become active convoys.

You have full visibility into all currently active work and all other pending proposals.
Your job is to prevent clobbering, reduce rework, and maximize fleet efficiency.

For each proposed plan, decide one of:
- APPROVE: The plan is safe to execute as-is. No meaningful conflicts with active work.
- SEQUENCE: The plan is correct but depends on work in one or more active convoys finishing first.
  Approve it as a blocked convoy — the new tasks will not start until the specified convoy's tail
  tasks complete. Use this instead of REJECT when the only issue is "wait for X to finish first."
  Specify which convoy IDs to sequence after in sequence_after_convoy_ids.
- REJECT: The plan has a fundamental design conflict that requires Commander to replan. Use only
  when the plan itself is wrong — not merely when it needs to wait for other work to finish.
- MERGE: This plan and another pending proposal are closely related and should be combined into
  a single convoy. Specify the feature_id to merge with.

Respond with ONLY a JSON object in exactly this format — no preamble, no markdown:
{"action":"APPROVE"|"SEQUENCE"|"REJECT"|"MERGE","reason":"...","merge_with_feature_id":0,"sequence_after_convoy_ids":[],"sequence_after_feature_ids":[],"hold_convoy_ids":[]}

Set merge_with_feature_id to the feature ID to merge with (only for MERGE), 0 otherwise.
Set sequence_after_convoy_ids to active convoy IDs this proposal must wait on (SEQUENCE only).
Set sequence_after_feature_ids to pending Feature IDs (not yet planned) this proposal depends on.
  Use this when a pending Feature in the queue is a prerequisite — the system will block this
  convoy until that Feature's convoy is created and completes.
Set hold_convoy_ids to active convoy IDs that depend on THIS proposal's work and should be
  blocked immediately. Use when you detect a running convoy that built against work this
  proposal introduces — it needs to be held and replanned once this convoy lands.

sequence_after_feature_ids and hold_convoy_ids are independent of the main action — set them
alongside APPROVE/SEQUENCE/REJECT/MERGE whenever the dependency situation warrants it.

The "action" field is REQUIRED and MUST be exactly one of APPROVE, SEQUENCE, REJECT, or MERGE.` + promptInjectionClause

type chancellorRuling struct {
	Action                  string `json:"action"`
	Reason                  string `json:"reason"`
	MergeWithFeatureID      int    `json:"merge_with_feature_id"`
	SequenceAfterConvoyIDs  []int  `json:"sequence_after_convoy_ids"`
	SequenceAfterFeatureIDs []int  `json:"sequence_after_feature_ids"`
	HoldConvoyIDs           []int  `json:"hold_convoy_ids"`
}

// SpawnChancellor runs the Supreme Chancellor agent loop.
// Single instance — deliberate serialization point for convoy creation.
func SpawnChancellor(ctx context.Context, db *sql.DB) {
	logger := NewLogger(chancellorName)

	// D1 T0-1: load Chancellor's capability profile once at spawn-time.
	profile, err := capabilities.LoadProfile("chancellor")
	if err != nil {
		logger.Printf("Chancellor cannot start: %v", err)
		return
	}
	logger.Printf("Supreme Chancellor online — reviewing proposed convoys")

	for {
		if ctx.Err() != nil {
			logger.Printf("Chancellor exiting: %v", ctx.Err())
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

		feature, tasks, claimed := store.ClaimChancellorTask(db, chancellorName)
		if !claimed {
			time.Sleep(3 * time.Second)
			continue
		}

		runChancellorReview(ctx, db, feature, tasks, profile, logger)
	}
}

func runChancellorReview(ctx context.Context, db *sql.DB, feature *store.Bounty, tasks []store.TaskPlan, profile *capabilities.Profile, logger interface{ Printf(string, ...any) }) {
	logger.Printf("Reviewing Feature #%d (%d proposed task(s)): %s",
		feature.ID, len(tasks), util.TruncateStr(feature.Payload, 80))

	// Build context: active convoys + other pending proposals + pending Features.
	activeConvoys := store.GetActiveConvoyContext(db)
	pendingProposals := store.GetPendingProposals(db, feature.ID)
	pendingFeatures := store.GetPendingFeatures(db, feature.ID)

	// Fix #8.5 — wrap the prompt body in <user_content> sentinels. The
	// feature payload is operator-supplied; the tasks/active-convoys/
	// pending-proposals sections may contain agent-authored text that
	// originated from earlier LLM calls. The outer wrapper tells
	// Chancellor to treat everything inside as data.
	userPrompt := WrapUserContent("chancellor_context",
		buildChancellorPrompt(feature, tasks, activeConvoys, pendingProposals, pendingFeatures))

	mcpConfig, mcpErr := profile.MCPConfigArg()
	if mcpErr != nil {
		logger.Printf("Feature #%d: chancellor MCP config write failed (%v) — proceeding without --mcp-config", feature.ID, mcpErr)
	}
	// D3 P1 follow-up B: ctx threads from SpawnChancellor → runChancellorReview
	// so daemon shutdown / e-stop cancels the in-flight LLM call.
	systemPrompt := AppendFleetRulesToPrompt(ctx, db, "chancellor", chancellorSystemPrompt, nil)
	response, err := claude.AskClaudeCLIContext(ctx, systemPrompt, userPrompt,
		profile.AllowedToolsArg(), profile.DisallowedToolsArg(), mcpConfig, 1)
	if err != nil {
		// Fix #8.5 (AUDIT-116) — fail CLOSED on Claude CLI failure.
		// Pre-fix: "auto-approve to avoid blocking the pipeline" meant a
		// systemic LLM outage silently approved every Feature. Now the
		// Feature is routed to operator review via FailBounty + operator
		// mail; approval requires a working LLM (or explicit operator
		// action).
		msg := fmt.Sprintf("Chancellor Claude call failed: %v — routing to operator review (fail-closed)", err)
		logger.Printf("Feature #%d: %s", feature.ID, msg)
		if failErr := store.FailBounty(db, feature.ID, msg); failErr != nil {
			// Feature row did not transition to Failed. Log and continue —
			// the stale-lock detector will re-evaluate this feature on the
			// next sweep, and operator mail (below) still fires so the
			// human is notified regardless of the DB write outcome.
			logger.Printf("Feature #%d: FailBounty write failed (%v) — stale-lock detector will recover", feature.ID, failErr)
		}
		store.SendMail(db, chancellorName, "operator",
			fmt.Sprintf("[CHANCELLOR FAIL-CLOSED] Feature #%d — LLM unavailable, operator review required", feature.ID),
			fmt.Sprintf("The Supreme Chancellor's LLM call failed. The Feature has been failed rather than auto-approved (Fix #8.5 fail-closed). Reset the Feature to Pending once the LLM outage is resolved.\n\nError: %v\n\nOriginal request:\n%s",
				err, util.TruncateStr(feature.Payload, 500)),
			feature.ID, store.MailTypeAlert)
		return
	}

	jsonStr := claude.ExtractJSON(response)
	var ruling chancellorRuling
	// Fix #8.5 — strict-field decode so model-upgrade drift surfaces as
	// a parse error rather than silently accepting new fields.
	if err := strictJSONUnmarshal([]byte(jsonStr), &ruling); err != nil {
		// Fix #8.5 (AUDIT-116) — fail CLOSED on parse failure.
		msg := fmt.Sprintf("Chancellor ruling parse failed: %v — routing to operator review (fail-closed)", err)
		logger.Printf("Feature #%d: %s", feature.ID, msg)
		if failErr := store.FailBounty(db, feature.ID, msg); failErr != nil {
			// Feature row did not transition to Failed. Log and continue —
			// the stale-lock detector will re-evaluate this feature on the
			// next sweep; operator mail still fires.
			logger.Printf("Feature #%d: FailBounty write failed (%v) — stale-lock detector will recover", feature.ID, failErr)
		}
		store.SendMail(db, chancellorName, "operator",
			fmt.Sprintf("[CHANCELLOR FAIL-CLOSED] Feature #%d — LLM returned unparseable ruling, operator review required", feature.ID),
			fmt.Sprintf("The Supreme Chancellor's LLM produced a response that could not be parsed. The Feature has been failed rather than auto-approved (Fix #8.5 fail-closed).\n\nParse error: %v\n\nRaw response (first 500 bytes): %.500s\n\nOriginal request:\n%s",
				err, jsonStr, util.TruncateStr(feature.Payload, 500)),
			feature.ID, store.MailTypeAlert)
		return
	}

	var actionErr error
	switch ruling.Action {
	case "APPROVE":
		logger.Printf("Feature #%d: APPROVED — %s", feature.ID, ruling.Reason)
		actionErr = approveProposal(db, feature, tasks, ruling, logger)

	case "SEQUENCE":
		if len(ruling.SequenceAfterConvoyIDs) == 0 {
			// Fix #8d (Track H): fail CLOSED on empty required subfield
			// instead of silently re-routing to APPROVE. Pre-fix, a LLM
			// SEQUENCE ruling with a missing sequence_after_convoy_ids
			// array dropped into the auto-approve fall-through — the
			// sequencing intent was lost and the convoy landed wired as
			// a top-level feature. Post-fix, the operator sees the
			// [CHANCELLOR FAIL-CLOSED] mail and can correct the ruling
			// manually.
			msg := fmt.Sprintf("Chancellor returned SEQUENCE with empty sequence_after_convoy_ids — failing closed for operator review")
			logger.Printf("Feature #%d: %s", feature.ID, msg)
			if failErr := store.FailBounty(db, feature.ID, msg); failErr != nil {
				logger.Printf("Feature #%d: FailBounty after SEQUENCE empty-subfield failed (%v); stale-lock detector will recover", feature.ID, failErr)
			}
			store.SendMail(db, "Chancellor", "operator",
				fmt.Sprintf("[CHANCELLOR FAIL-CLOSED] Feature #%d — SEQUENCE with empty sequence_after_convoy_ids", feature.ID),
				fmt.Sprintf("Feature #%d: Chancellor's LLM returned action=SEQUENCE but the sequence_after_convoy_ids array was empty. This is a structural error — we cannot sequence after no-op. The feature has been marked Failed for operator review.\n\nReason: %s\n\nFeature payload:\n%s",
					feature.ID, ruling.Reason, util.TruncateStr(feature.Payload, 500)),
				feature.ID, store.MailTypeAlert)
			return
		}
		logger.Printf("Feature #%d: SEQUENCE after convoy(s) %v — %s", feature.ID, ruling.SequenceAfterConvoyIDs, ruling.Reason)
		actionErr = sequenceProposal(db, feature, tasks, ruling, logger)

	case "REJECT":
		logger.Printf("Feature #%d: REJECTED — %s", feature.ID, ruling.Reason)
		// Still enforce holds even on reject — hold_convoy_ids may be set.
		enforceHolds(db, 0, ruling, feature, logger)
		actionErr = rejectProposal(db, feature, ruling.Reason, logger)

	case "MERGE":
		if ruling.MergeWithFeatureID <= 0 {
			// Fix #8d (Track H): fail CLOSED on empty required subfield.
			// Pre-fix, MERGE with no target auto-approved, losing the
			// merge intent entirely. Post-fix, the operator is alerted
			// and the feature sits Failed until they decide.
			msg := fmt.Sprintf("Chancellor returned MERGE with empty merge_with_feature_id — failing closed for operator review")
			logger.Printf("Feature #%d: %s", feature.ID, msg)
			if failErr := store.FailBounty(db, feature.ID, msg); failErr != nil {
				logger.Printf("Feature #%d: FailBounty after MERGE empty-subfield failed (%v); stale-lock detector will recover", feature.ID, failErr)
			}
			store.SendMail(db, "Chancellor", "operator",
				fmt.Sprintf("[CHANCELLOR FAIL-CLOSED] Feature #%d — MERGE with empty merge_with_feature_id", feature.ID),
				fmt.Sprintf("Feature #%d: Chancellor's LLM returned action=MERGE but the merge_with_feature_id field was <= 0. This is a structural error — we cannot merge into a nonexistent target. The feature has been marked Failed for operator review.\n\nReason: %s\n\nFeature payload:\n%s",
					feature.ID, ruling.Reason, util.TruncateStr(feature.Payload, 500)),
				feature.ID, store.MailTypeAlert)
			return
		}
		logger.Printf("Feature #%d: MERGE with Feature #%d — %s", feature.ID, ruling.MergeWithFeatureID, ruling.Reason)
		actionErr = mergeProposals(ctx, db, feature, tasks, ruling, profile, logger)

	default:
		// Fix #8.5 (AUDIT-116) — fail CLOSED on unknown action.
		// Pre-fix: unknown action strings silently approved with a
		// zero-value ruling; Chancellor's one job was gated on the LLM
		// emitting an empty action field.
		msg := fmt.Sprintf("Chancellor returned unknown action %q — routing to operator review (fail-closed)", ruling.Action)
		logger.Printf("Feature #%d: %s", feature.ID, msg)
		if failErr := store.FailBounty(db, feature.ID, msg); failErr != nil {
			// Feature row did not transition to Failed. Log and continue —
			// the stale-lock detector will re-evaluate this feature on the
			// next sweep; operator mail still fires.
			logger.Printf("Feature #%d: FailBounty write failed (%v) — stale-lock detector will recover", feature.ID, failErr)
		}
		store.SendMail(db, chancellorName, "operator",
			fmt.Sprintf("[CHANCELLOR FAIL-CLOSED] Feature #%d — unknown action %q, operator review required", feature.ID, ruling.Action),
			fmt.Sprintf("The Supreme Chancellor returned an action value not in the schema. The Feature has been failed rather than auto-approved (Fix #8.5 fail-closed).\n\nAction: %q\nReason: %s\n\nOriginal request:\n%s",
				ruling.Action, ruling.Reason, util.TruncateStr(feature.Payload, 500)),
			feature.ID, store.MailTypeAlert)
	}

	if actionErr != nil {
		// A helper (approve/sequence/reject/merge) hit a DB error. Operator
		// mail has already been sent by the helper where applicable; log
		// here so the claim-loop owner sees the failure in the daily
		// review, and the stale-lock detector will re-evaluate this
		// feature on the next sweep.
		logger.Printf("Feature #%d: %s path reported error (%v) — stale-lock detector will recover", feature.ID, ruling.Action, actionErr)
	}
}

// approveProposal creates the convoy and subtasks from an approved plan.
// Fix #8b: returns error so runChancellorReview can observe DB-write failures
// that leave the Feature in an inconsistent state. A non-nil return means at
// least one terminator (FailBounty / UpdateBountyStatus) did not land; the
// caller logs and the stale-lock detector re-evaluates on the next sweep.
func approveProposal(db *sql.DB, feature *store.Bounty, tasks []store.TaskPlan, ruling chancellorRuling, logger interface{ Printf(string, ...any) }) error {
	convoyPreview := strings.ReplaceAll(feature.Payload, "\n", " ")
	if len(convoyPreview) > 50 {
		convoyPreview = convoyPreview[:50]
	}
	convoyName := fmt.Sprintf("[%d] %s", feature.ID, convoyPreview)
	convoyID, convoyErr := store.CreateConvoy(db, convoyName)
	if convoyErr != nil {
		for i := 2; i <= 10; i++ {
			convoyID, convoyErr = store.CreateConvoy(db, fmt.Sprintf("%s (re-plan %d)", convoyName, i))
			if convoyErr == nil {
				break
			}
		}
		if convoyErr != nil {
			if failErr := store.FailBounty(db, feature.ID, fmt.Sprintf("Chancellor Err: could not create convoy: %v", convoyErr)); failErr != nil {
				return fmt.Errorf("approveProposal feature #%d: convoy creation failed (%v) and FailBounty failed: %w", feature.ID, convoyErr, failErr)
			}
			return fmt.Errorf("approveProposal feature #%d: could not create convoy: %w", feature.ID, convoyErr)
		}
	}
	store.SetConvoyCoordinated(db, convoyID)

	idMapping, err := insertConvoyAndTasks(db, tasks, feature, convoyID)
	if err != nil {
		if failErr := store.FailBounty(db, feature.ID, "Chancellor Err: "+err.Error()); failErr != nil {
			return fmt.Errorf("approveProposal feature #%d: insertConvoyAndTasks failed (%v) and FailBounty failed: %w", feature.ID, err, failErr)
		}
		return fmt.Errorf("approveProposal feature #%d: insertConvoyAndTasks: %w", feature.ID, err)
	}

	store.SetProposedConvoyStatus(db, feature.ID, "approved")
	if err := store.UpdateBountyStatus(db, feature.ID, "Completed"); err != nil {
		// Convoy is on disk but the Feature row did not transition. Surface
		// so the caller can log — the stale-lock detector will reconcile.
		return fmt.Errorf("approveProposal feature #%d: convoy #%d created but UpdateBountyStatus Completed failed: %w", feature.ID, convoyID, err)
	}
	logger.Printf("Feature #%d: convoy #%d created with %d task(s)", feature.ID, convoyID, len(tasks))

	// Resolve any FeatureBlockers that were waiting on this Feature to get a convoy.
	if n := store.ResolveFeatureBlockers(db, feature.ID, convoyID); n > 0 {
		logger.Printf("Feature #%d: resolved FeatureBlockers — %d cross-convoy dep(s) wired", feature.ID, n)
	}

	// Apply hold enforcement from the ruling.
	enforceHolds(db, convoyID, ruling, feature, logger)

	var taskLines []string
	for _, t := range tasks {
		line := fmt.Sprintf("  #%d [%s] %s", idMapping[t.TempID], t.Repo, util.TruncateStr(t.Task, 80))
		taskLines = append(taskLines, line)
	}
	store.SendMail(db, chancellorName, "operator",
		fmt.Sprintf("[APPROVED] Feature #%d → convoy #%d (%d task(s))", feature.ID, convoyID, len(tasks)),
		fmt.Sprintf("Supreme Chancellor approved Feature #%d.\nConvoy #%d created with %d task(s):\n\n%s\n\nOriginal request:\n%s",
			feature.ID, convoyID, len(tasks), strings.Join(taskLines, "\n"), util.TruncateStr(feature.Payload, 500)),
		feature.ID, store.MailTypeInfo)
	return nil
}

// rejectProposal resets the Feature to Pending and mails the rejection to Commander.
// Fix #8b: returns error so the caller can observe an UpdateBountyStatus
// failure — the mail still fires either way, but a stuck Feature row needs
// to be visible to the claim-loop owner.
func rejectProposal(db *sql.DB, feature *store.Bounty, reason string, logger interface{ Printf(string, ...any) }) error {
	store.SetProposedConvoyStatus(db, feature.ID, "rejected")
	var retErr error
	if err := store.UpdateBountyStatus(db, feature.ID, "Pending"); err != nil {
		// Feature row did not revert to Pending. The rejection mail below
		// still fires so Commander sees the feedback; surface the write
		// failure so the caller logs it — the stale-lock detector will
		// reclaim the row on the next sweep.
		retErr = fmt.Errorf("rejectProposal feature #%d: UpdateBountyStatus Pending failed: %w", feature.ID, err)
	}

	store.SendMail(db, chancellorName, "commander",
		fmt.Sprintf("[CHANCELLOR REJECTED] Feature #%d plan", feature.ID),
		fmt.Sprintf("The Supreme Chancellor has rejected your plan for Feature #%d.\n\nReason:\n%s\n\nReplan with this context in mind. The task has been reset to Pending for you to re-claim.",
			feature.ID, reason),
		feature.ID, store.MailTypeFeedback)

	store.SendMail(db, chancellorName, "operator",
		fmt.Sprintf("[REJECTED] Feature #%d plan sent back for replanning", feature.ID),
		fmt.Sprintf("Supreme Chancellor rejected the plan for Feature #%d.\n\nReason:\n%s\n\nTask reset to Pending — Commander will replan.",
			feature.ID, reason),
		feature.ID, store.MailTypeAlert)
	return retErr
}

// enforceHolds applies FeatureBlockers and ConvoyHolds from a Chancellor ruling.
// Called after any approval path (approve, sequence, merge) that creates a convoy.
// newConvoyID is the convoy just created (0 if the proposal was rejected/not yet a convoy).
func enforceHolds(db *sql.DB, newConvoyID int, ruling chancellorRuling, feature *store.Bounty, logger interface{ Printf(string, ...any) }) {
	// sequence_after_feature_ids: block the new convoy on pending Features with no convoy yet.
	for _, featureID := range ruling.SequenceAfterFeatureIDs {
		if newConvoyID == 0 {
			break
		}
		reason := fmt.Sprintf("Convoy #%d blocked — depends on Feature #%d completing first (Chancellor directive)", newConvoyID, featureID)
		store.CreateFeatureBlocker(db, newConvoyID, featureID, reason)
		logger.Printf("Feature #%d: convoy #%d blocked on Feature #%d (FeatureBlocker created)", feature.ID, newConvoyID, featureID)
	}

	// hold_convoy_ids: retroactively block active convoys that depend on this proposal's work.
	for _, holdConvoyID := range ruling.HoldConvoyIDs {
		reason := fmt.Sprintf("Held by Chancellor — convoy depends on Feature #%d's work (convoy #%d). Replanning required once Feature #%d lands.", feature.ID, newConvoyID, feature.ID)
		if newConvoyID > 0 {
			// Wire real TaskDependencies immediately: held convoy's Pending tasks → new convoy's tail tasks.
			tailIDs := store.GetConvoyTailTaskIDs(db, newConvoyID)
			heldRootRows, err := db.Query(`
				SELECT id FROM BountyBoard
				WHERE convoy_id = ? AND status IN ('Pending','Planned')
				  AND id NOT IN (SELECT task_id FROM TaskDependencies)`, holdConvoyID)
			if err == nil {
				var rootIDs []int
				for heldRootRows.Next() {
					var id int
					if err := heldRootRows.Scan(&id); err != nil {
						logger.Printf("chancellor: scan failed in heldRootRows: %v", err)
						continue
					}
					rootIDs = append(rootIDs, id)
				}
				if rErr := heldRootRows.Err(); rErr != nil {
					log.Printf("chancellor.go:enforceHolds: rows iter error: %v", rErr)
				}
				heldRootRows.Close()
				for _, rootID := range rootIDs {
					for _, tailID := range tailIDs {
						store.AddDependency(db, rootID, tailID)
					}
				}
			}
		}
		store.SetConvoyHold(db, holdConvoyID, reason)
		logger.Printf("Feature #%d: convoy #%d placed on hold (depends on this work)", feature.ID, holdConvoyID)

		store.SendMail(db, chancellorName, "operator",
			fmt.Sprintf("[CHANCELLOR HOLD] Convoy #%d held — depends on Feature #%d", holdConvoyID, feature.ID),
			fmt.Sprintf("Supreme Chancellor has placed convoy #%d on hold.\n\nReason: convoy #%d was started before Feature #%d's work was available. Any in-flight tasks will be rejected by the Captain and Council. Pending tasks are blocked.\n\nConvoy #%d will be unblocked automatically once Feature #%d's convoy completes.",
				holdConvoyID, holdConvoyID, feature.ID, holdConvoyID, feature.ID),
			feature.ID, store.MailTypeAlert)
	}

	if len(ruling.SequenceAfterFeatureIDs) > 0 || len(ruling.HoldConvoyIDs) > 0 {
		store.SendMail(db, chancellorName, "operator",
			fmt.Sprintf("[CHANCELLOR ORDERING] Feature #%d convoy dependency enforcement applied", feature.ID),
			fmt.Sprintf("Chancellor applied convoy ordering for Feature #%d:\n- Blocked on features: %v\n- Held convoys: %v",
				feature.ID, ruling.SequenceAfterFeatureIDs, ruling.HoldConvoyIDs),
			feature.ID, store.MailTypeInfo)
	}
}

// sequenceProposal creates the convoy immediately but wires cross-convoy blocking dependencies
// so the new convoy's root tasks cannot start until the tail tasks of each specified convoy complete.
// Fix #8b: returns error so runChancellorReview can log DB-write failures.
func sequenceProposal(db *sql.DB, feature *store.Bounty, tasks []store.TaskPlan, ruling chancellorRuling, logger interface{ Printf(string, ...any) }) error {
	blockingConvoyIDs := ruling.SequenceAfterConvoyIDs
	reason := ruling.Reason
	convoyPreview := strings.ReplaceAll(feature.Payload, "\n", " ")
	if len(convoyPreview) > 50 {
		convoyPreview = convoyPreview[:50]
	}
	convoyName := fmt.Sprintf("[%d] %s", feature.ID, convoyPreview)
	convoyID, convoyErr := store.CreateConvoy(db, convoyName)
	if convoyErr != nil {
		for i := 2; i <= 10; i++ {
			convoyID, convoyErr = store.CreateConvoy(db, fmt.Sprintf("%s (re-plan %d)", convoyName, i))
			if convoyErr == nil {
				break
			}
		}
		if convoyErr != nil {
			if failErr := store.FailBounty(db, feature.ID, fmt.Sprintf("Chancellor Err: could not create convoy: %v", convoyErr)); failErr != nil {
				return fmt.Errorf("sequenceProposal feature #%d: convoy creation failed (%v) and FailBounty failed: %w", feature.ID, convoyErr, failErr)
			}
			return fmt.Errorf("sequenceProposal feature #%d: could not create convoy: %w", feature.ID, convoyErr)
		}
	}
	store.SetConvoyCoordinated(db, convoyID)

	idMapping, err := insertConvoyAndTasks(db, tasks, feature, convoyID)
	if err != nil {
		if failErr := store.FailBounty(db, feature.ID, "Chancellor Err: "+err.Error()); failErr != nil {
			return fmt.Errorf("sequenceProposal feature #%d: insertConvoyAndTasks failed (%v) and FailBounty failed: %w", feature.ID, err, failErr)
		}
		return fmt.Errorf("sequenceProposal feature #%d: insertConvoyAndTasks: %w", feature.ID, err)
	}

	// Find root tasks in the new plan (those with no blocked_by in the plan).
	rootTaskIDs := []int{}
	for _, t := range tasks {
		if len(t.BlockedBy) == 0 {
			if realID, ok := idMapping[t.TempID]; ok {
				rootTaskIDs = append(rootTaskIDs, realID)
			}
		}
	}

	// For each blocking convoy, inject its tail task IDs as dependencies on every root task.
	injected := 0
	for _, blockingConvoyID := range blockingConvoyIDs {
		tailIDs := store.GetConvoyTailTaskIDs(db, blockingConvoyID)
		for _, rootID := range rootTaskIDs {
			for _, tailID := range tailIDs {
				store.AddDependency(db, rootID, tailID)
				injected++
			}
		}
	}

	store.SetProposedConvoyStatus(db, feature.ID, "approved")
	var retErr error
	if err := store.UpdateBountyStatus(db, feature.ID, "Completed"); err != nil {
		// Convoy is on disk with correct cross-convoy deps but the Feature
		// row did not transition. Surface so caller logs; stale-lock
		// detector will reconcile on the next sweep.
		retErr = fmt.Errorf("sequenceProposal feature #%d: convoy #%d created but UpdateBountyStatus Completed failed: %w", feature.ID, convoyID, err)
	}
	logger.Printf("Feature #%d: convoy #%d created (sequenced after convoy(s) %v, %d cross-convoy dep(s) injected, %d task(s))",
		feature.ID, convoyID, blockingConvoyIDs, injected, len(tasks))

	if n := store.ResolveFeatureBlockers(db, feature.ID, convoyID); n > 0 {
		logger.Printf("Feature #%d: resolved FeatureBlockers — %d cross-convoy dep(s) wired", feature.ID, n)
	}
	enforceHolds(db, convoyID, ruling, feature, logger)

	var taskLines []string
	for _, t := range tasks {
		line := fmt.Sprintf("  #%d [%s] %s", idMapping[t.TempID], t.Repo, util.TruncateStr(t.Task, 80))
		taskLines = append(taskLines, line)
	}
	store.SendMail(db, chancellorName, "operator",
		fmt.Sprintf("[SEQUENCED] Feature #%d → convoy #%d (blocked on convoy(s) %v)", feature.ID, convoyID, blockingConvoyIDs),
		fmt.Sprintf("Supreme Chancellor sequenced Feature #%d after convoy(s) %v.\nConvoy #%d created with %d task(s) — root tasks blocked until upstream work completes.\n\nReason: %s\n\nTasks:\n%s\n\nOriginal request:\n%s",
			feature.ID, blockingConvoyIDs, convoyID, len(tasks), reason, strings.Join(taskLines, "\n"), util.TruncateStr(feature.Payload, 500)),
		feature.ID, store.MailTypeInfo)
	return retErr
}

// mergeProposals synthesizes two proposed plans into a single convoy.
// Fix #8b: returns error so runChancellorReview can log DB-write failures.
// When the merge path falls back to independent approval, the fallback
// errors are joined and returned together so neither is lost.
func mergeProposals(ctx context.Context, db *sql.DB, featureA *store.Bounty, tasksA []store.TaskPlan, ruling chancellorRuling, profile *capabilities.Profile, logger interface{ Printf(string, ...any) }) error {
	featureBID := ruling.MergeWithFeatureID
	reason := ruling.Reason
	featureB, tasksB, ok := store.ClaimMergeTarget(db, featureBID, chancellorName)
	if !ok {
		// Target already gone or claimed — approve A independently.
		logger.Printf("Feature #%d: merge target #%d unavailable — approving independently", featureA.ID, featureBID)
		return approveProposal(db, featureA, tasksA, ruling, logger)
	}

	logger.Printf("Merging Feature #%d + Feature #%d", featureA.ID, featureB.ID)

	mergedTasks := synthesizeMergedPlan(ctx, db, featureA, tasksA, featureB, tasksB, profile, logger)
	if mergedTasks == nil {
		// Synthesis failed — approve both independently. Join errors so a
		// single-leg failure doesn't silently succeed under a two-leg return.
		logger.Printf("Merge synthesis failed — approving Feature #%d and #%d independently", featureA.ID, featureB.ID)
		errA := approveProposal(db, featureA, tasksA, ruling, logger)
		errB := approveProposal(db, featureB, tasksB, chancellorRuling{}, logger)
		switch {
		case errA != nil && errB != nil:
			return fmt.Errorf("mergeProposals fallback: featureA=%v; featureB=%v", errA, errB)
		case errA != nil:
			return errA
		case errB != nil:
			return errB
		}
		return nil
	}

	convoyName := fmt.Sprintf("[%d+%d] merged", featureA.ID, featureB.ID)
	convoyID, convoyErr := store.CreateConvoy(db, convoyName)
	if convoyErr != nil {
		logger.Printf("Merge convoy creation failed — approving independently")
		errA := approveProposal(db, featureA, tasksA, ruling, logger)
		errB := approveProposal(db, featureB, tasksB, chancellorRuling{}, logger)
		switch {
		case errA != nil && errB != nil:
			return fmt.Errorf("mergeProposals fallback after CreateConvoy %v: featureA=%v; featureB=%v", convoyErr, errA, errB)
		case errA != nil:
			return errA
		case errB != nil:
			return errB
		}
		return nil
	}
	store.SetConvoyCoordinated(db, convoyID)

	idMapping, err := insertConvoyAndTasks(db, mergedTasks, featureA, convoyID)
	if err != nil {
		failA := store.FailBounty(db, featureA.ID, "Chancellor Err (merge): "+err.Error())
		failB := store.FailBounty(db, featureB.ID, "Chancellor Err (merge): "+err.Error())
		// Both Feature rows should end in Failed; surface any DB-write
		// errors so the stale-lock detector (which also retries) has a
		// corresponding log line.
		switch {
		case failA != nil && failB != nil:
			return fmt.Errorf("mergeProposals feature #%d/#%d: insertConvoyAndTasks failed (%v); FailBounty A=%v; FailBounty B=%v", featureA.ID, featureB.ID, err, failA, failB)
		case failA != nil:
			return fmt.Errorf("mergeProposals feature #%d: insertConvoyAndTasks failed (%v); FailBounty A failed: %w", featureA.ID, err, failA)
		case failB != nil:
			return fmt.Errorf("mergeProposals feature #%d: insertConvoyAndTasks failed (%v); FailBounty B failed: %w", featureB.ID, err, failB)
		}
		return fmt.Errorf("mergeProposals feature #%d/#%d: insertConvoyAndTasks: %w", featureA.ID, featureB.ID, err)
	}

	store.SetProposedConvoyStatus(db, featureA.ID, "merged")
	store.SetProposedConvoyStatus(db, featureB.ID, "merged")
	updA := store.UpdateBountyStatus(db, featureA.ID, "Completed")
	updB := store.UpdateBountyStatus(db, featureB.ID, "Completed")
	var retErr error
	switch {
	case updA != nil && updB != nil:
		retErr = fmt.Errorf("mergeProposals: convoy #%d created but UpdateBountyStatus failed for both features (A=%v; B=%v) — stale-lock detector will recover", convoyID, updA, updB)
	case updA != nil:
		retErr = fmt.Errorf("mergeProposals feature #%d: convoy #%d created but UpdateBountyStatus Completed failed: %w", featureA.ID, convoyID, updA)
	case updB != nil:
		retErr = fmt.Errorf("mergeProposals feature #%d: convoy #%d created but UpdateBountyStatus Completed failed: %w", featureB.ID, convoyID, updB)
	}

	store.ResolveFeatureBlockers(db, featureA.ID, convoyID)
	store.ResolveFeatureBlockers(db, featureB.ID, convoyID)
	enforceHolds(db, convoyID, ruling, featureA, logger)

	logger.Printf("Merged convoy #%d: %d task(s) from Feature #%d + #%d",
		convoyID, len(mergedTasks), featureA.ID, featureB.ID)

	var taskLines []string
	for _, t := range mergedTasks {
		taskLines = append(taskLines, fmt.Sprintf("  #%d [%s] %s", idMapping[t.TempID], t.Repo, util.TruncateStr(t.Task, 80)))
	}
	store.SendMail(db, chancellorName, "operator",
		fmt.Sprintf("[MERGED] Feature #%d + #%d → convoy #%d (%d task(s))", featureA.ID, featureB.ID, convoyID, len(mergedTasks)),
		fmt.Sprintf("Supreme Chancellor merged Feature #%d and #%d into a single convoy #%d.\n\nReason: %s\n\nTasks:\n%s",
			featureA.ID, featureB.ID, convoyID, reason, strings.Join(taskLines, "\n")),
		featureA.ID, store.MailTypeInfo)
	return retErr
}

// synthesizeMergedPlan calls Claude to produce a unified task list from two plans.
func synthesizeMergedPlan(ctx context.Context, db *sql.DB, featureA *store.Bounty, tasksA []store.TaskPlan, featureB *store.Bounty, tasksB []store.TaskPlan, profile *capabilities.Profile, logger interface{ Printf(string, ...any) }) []store.TaskPlan {
	planAJSON, _ := json.MarshalIndent(tasksA, "", "  ")
	planBJSON, _ := json.MarshalIndent(tasksB, "", "  ")

	// Fix #8.5 — wrap the plan bodies (attacker-reachable via the
	// operator-supplied Feature payloads + prior Commander LLM output)
	// in <user_content> sentinel markers.
	mergePrompt := fmt.Sprintf(`You are merging two feature plans into a single optimized convoy.

FEATURE A (#%d): %s
PLAN A:
%s

FEATURE B (#%d): %s
PLAN B:
%s

Produce a single merged JSON array that:
1. Eliminates duplicate or redundant tasks (e.g. two tasks that both modify the same file for related reasons become one)
2. Preserves all necessary work from both plans
3. Sets correct blocked_by dependencies between tasks
4. Numbers tasks starting from 1

Respond with ONLY the raw JSON array — no explanation, no markdown, no code fences.`,
		featureA.ID, WrapUserContent("feature_a", util.TruncateStr(featureA.Payload, 200)), WrapUserContent("plan_a", string(planAJSON)),
		featureB.ID, WrapUserContent("feature_b", util.TruncateStr(featureB.Payload, 200)), WrapUserContent("plan_b", string(planBJSON)))

	mcpConfig, mcpErr := profile.MCPConfigArg()
	if mcpErr != nil {
		logger.Printf("Merge synthesis MCP config write failed: %v — proceeding without --mcp-config", mcpErr)
	}
	// D3 P1 follow-up B: ctx threads from runChancellorReview → mergeProposals
	// → synthesizeMergedPlan so daemon shutdown cancels the merge LLM call.
	systemPrompt := AppendFleetRulesToPrompt(ctx, db, "chancellor", chancellorSystemPrompt, nil)
	response, err := claude.AskClaudeCLIContext(ctx, systemPrompt, mergePrompt,
		profile.AllowedToolsArg(), profile.DisallowedToolsArg(), mcpConfig, 1)
	if err != nil {
		logger.Printf("Merge synthesis Claude call failed: %v", err)
		return nil
	}

	jsonStr := claude.ExtractJSON(response)
	var merged []store.TaskPlan
	// Fix #8.5 — strict-field decode.
	if err := strictJSONUnmarshal([]byte(jsonStr), &merged); err != nil {
		logger.Printf("Merge synthesis parse failed: %v", err)
		return nil
	}
	// Fix #8.5 — sanitize LLM-authored task strings in the merged plan.
	for _, t := range merged {
		if err := SanitizeLLMPayload(t.Task); err != nil {
			logger.Printf("Merge synthesis task rejected (%v) — refusing merge", err)
			return nil
		}
	}

	knownRepos := loadKnownRepos(db)
	if err := validateTaskPlan(merged, knownRepos); err != nil {
		logger.Printf("Merge synthesis plan invalid: %v", err)
		return nil
	}
	return merged
}

// buildChancellorPrompt constructs the user prompt with full context.
func buildChancellorPrompt(feature *store.Bounty, tasks []store.TaskPlan, activeConvoys []store.ActiveConvoyInfo, pendingProposals []store.PendingProposalInfo, pendingFeatures []store.PendingFeatureInfo) string {
	var b strings.Builder

	fmt.Fprintf(&b, "## Proposed Plan\n\n")
	fmt.Fprintf(&b, "**Feature #%d:** %s\n\n", feature.ID, feature.Payload)
	fmt.Fprintf(&b, "**Tasks (%d):**\n", len(tasks))
	for _, t := range tasks {
		deps := ""
		if len(t.BlockedBy) > 0 {
			deps = fmt.Sprintf(" (after %v)", t.BlockedBy)
		}
		fmt.Fprintf(&b, "  %d. [%s] %s%s\n", t.TempID, t.Repo, t.Task, deps)
	}

	if len(activeConvoys) > 0 {
		fmt.Fprintf(&b, "\n## Active Convoys\n\n")
		for _, c := range activeConvoys {
			fmt.Fprintf(&b, "**Convoy #%d — %s** (%d active task(s)):\n", c.ID, c.Name, len(c.Tasks))
			for _, task := range c.Tasks {
				fmt.Fprintf(&b, "  - %s\n", task)
			}
			fmt.Fprintf(&b, "  (Use convoy_id=%d in sequence_after_convoy_ids to block on this convoy)\n", c.ID)
		}
	} else {
		fmt.Fprintf(&b, "\n## Active Convoys\n\nNone.\n")
	}

	if len(pendingProposals) > 0 {
		fmt.Fprintf(&b, "\n## Other Pending Proposals (awaiting Chancellor review)\n\n")
		for _, p := range pendingProposals {
			fmt.Fprintf(&b, "**Feature #%d:** %s\n", p.FeatureID, util.TruncateStr(p.Payload, 150))
			var planTasks []store.TaskPlan
			if json.Unmarshal([]byte(p.PlanJSON), &planTasks) == nil {
				for _, t := range planTasks {
					fmt.Fprintf(&b, "  - [%s] %s\n", t.Repo, util.TruncateStr(t.Task, 80))
				}
			}
		}
	} else {
		fmt.Fprintf(&b, "\n## Other Pending Proposals\n\nNone.\n")
	}

	if len(pendingFeatures) > 0 {
		fmt.Fprintf(&b, "\n## Queued Features (not yet planned by Commander)\n\n")
		fmt.Fprintf(&b, "These Features are in the queue but have no convoy yet. If this proposal\n")
		fmt.Fprintf(&b, "depends on any of them, use sequence_after_feature_ids to block on that Feature ID.\n")
		fmt.Fprintf(&b, "If any of them appear to depend on THIS proposal's work, use hold_convoy_ids\n")
		fmt.Fprintf(&b, "when that convoy is eventually created — or flag it now with a note in reason.\n\n")
		for _, f := range pendingFeatures {
			fmt.Fprintf(&b, "**Feature #%d:** %s\n", f.FeatureID, util.TruncateStr(f.Payload, 150))
		}
	} else {
		fmt.Fprintf(&b, "\n## Queued Features\n\nNone.\n")
	}

	return b.String()
}
