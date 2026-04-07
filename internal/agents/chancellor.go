package agents

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

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
- REJECT: The plan conflicts with active convoy work. Explain precisely what conflicts and what
  the Commander should wait for or plan around. The Commander will replan with your feedback.
- MERGE: This plan and another pending proposal are closely related and should be combined into
  a single convoy. Specify the feature_id to merge with.

Respond with ONLY a JSON object in exactly this format — no preamble, no markdown:
{"action":"APPROVE"|"REJECT"|"MERGE","reason":"...","merge_with_feature_id":0}

Set merge_with_feature_id to the feature ID to merge with (only for MERGE action), 0 otherwise.

Be decisive. Prefer APPROVE for independent work. Use REJECT only for genuine conflicts where
parallel execution would cause clobbering or wasted rework. Use MERGE when two proposals clearly
address the same subsystem and combining them would produce a better, more coherent convoy.`

type chancellorRuling struct {
	Action            string `json:"action"`
	Reason            string `json:"reason"`
	MergeWithFeatureID int   `json:"merge_with_feature_id"`
}

// SpawnChancellor runs the Supreme Chancellor agent loop.
// Single instance — deliberate serialization point for convoy creation.
func SpawnChancellor(db *sql.DB) {
	logger := NewLogger(chancellorName)
	logger.Printf("Supreme Chancellor online — reviewing proposed convoys")

	for {
		if IsEstopped(db) {
			time.Sleep(5 * time.Second)
			continue
		}

		feature, tasks, claimed := store.ClaimChancellorTask(db, chancellorName)
		if !claimed {
			time.Sleep(3 * time.Second)
			continue
		}

		runChancellorReview(db, feature, tasks, logger)
	}
}

func runChancellorReview(db *sql.DB, feature *store.Bounty, tasks []store.TaskPlan, logger interface{ Printf(string, ...any) }) {
	logger.Printf("Reviewing Feature #%d (%d proposed task(s)): %s",
		feature.ID, len(tasks), util.TruncateStr(feature.Payload, 80))

	// Build context: active convoys + other pending proposals.
	activeConvoys := store.GetActiveConvoyContext(db)
	pendingProposals := store.GetPendingProposals(db, feature.ID)

	userPrompt := buildChancellorPrompt(feature, tasks, activeConvoys, pendingProposals)

	response, err := claude.AskClaudeCLI(chancellorSystemPrompt, userPrompt, "", 1)
	if err != nil {
		// On Claude failure, approve to avoid blocking the pipeline.
		logger.Printf("Feature #%d: Chancellor Claude call failed (%v) — auto-approving", feature.ID, err)
		approveProposal(db, feature, tasks, logger)
		return
	}

	jsonStr := claude.ExtractJSON(response)
	var ruling chancellorRuling
	if err := json.Unmarshal([]byte(jsonStr), &ruling); err != nil {
		logger.Printf("Feature #%d: could not parse Chancellor ruling (%v) — auto-approving", feature.ID, err)
		approveProposal(db, feature, tasks, logger)
		return
	}

	switch ruling.Action {
	case "APPROVE":
		logger.Printf("Feature #%d: APPROVED — %s", feature.ID, ruling.Reason)
		approveProposal(db, feature, tasks, logger)

	case "REJECT":
		logger.Printf("Feature #%d: REJECTED — %s", feature.ID, ruling.Reason)
		rejectProposal(db, feature, ruling.Reason, logger)

	case "MERGE":
		if ruling.MergeWithFeatureID <= 0 {
			logger.Printf("Feature #%d: MERGE with no target feature_id — auto-approving", feature.ID)
			approveProposal(db, feature, tasks, logger)
			return
		}
		logger.Printf("Feature #%d: MERGE with Feature #%d — %s", feature.ID, ruling.MergeWithFeatureID, ruling.Reason)
		mergeProposals(db, feature, tasks, ruling.MergeWithFeatureID, ruling.Reason, logger)

	default:
		logger.Printf("Feature #%d: unknown action %q — auto-approving", feature.ID, ruling.Action)
		approveProposal(db, feature, tasks, logger)
	}
}

// approveProposal creates the convoy and subtasks from an approved plan.
func approveProposal(db *sql.DB, feature *store.Bounty, tasks []store.TaskPlan, logger interface{ Printf(string, ...any) }) {
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
			store.FailBounty(db, feature.ID, fmt.Sprintf("Chancellor Err: could not create convoy: %v", convoyErr))
			return
		}
	}
	store.SetConvoyCoordinated(db, convoyID)

	idMapping, err := insertConvoyAndTasks(db, tasks, feature, convoyID)
	if err != nil {
		store.FailBounty(db, feature.ID, "Chancellor Err: "+err.Error())
		return
	}

	store.SetProposedConvoyStatus(db, feature.ID, "approved")
	store.UpdateBountyStatus(db, feature.ID, "Completed")
	logger.Printf("Feature #%d: convoy #%d created with %d task(s)", feature.ID, convoyID, len(tasks))

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
}

// rejectProposal resets the Feature to Pending and mails the rejection to Commander.
func rejectProposal(db *sql.DB, feature *store.Bounty, reason string, logger interface{ Printf(string, ...any) }) {
	store.SetProposedConvoyStatus(db, feature.ID, "rejected")
	store.UpdateBountyStatus(db, feature.ID, "Pending")

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
}

// mergeProposals synthesizes two proposed plans into a single convoy.
func mergeProposals(db *sql.DB, featureA *store.Bounty, tasksA []store.TaskPlan, featureBID int, reason string, logger interface{ Printf(string, ...any) }) {
	featureB, tasksB, ok := store.ClaimMergeTarget(db, featureBID, chancellorName)
	if !ok {
		// Target already gone or claimed — approve A independently.
		logger.Printf("Feature #%d: merge target #%d unavailable — approving independently", featureA.ID, featureBID)
		approveProposal(db, featureA, tasksA, logger)
		return
	}

	logger.Printf("Merging Feature #%d + Feature #%d", featureA.ID, featureB.ID)

	mergedTasks := synthesizeMergedPlan(db, featureA, tasksA, featureB, tasksB, logger)
	if mergedTasks == nil {
		// Synthesis failed — approve both independently.
		logger.Printf("Merge synthesis failed — approving Feature #%d and #%d independently", featureA.ID, featureB.ID)
		approveProposal(db, featureA, tasksA, logger)
		approveProposal(db, featureB, tasksB, logger)
		return
	}

	convoyName := fmt.Sprintf("[%d+%d] merged", featureA.ID, featureB.ID)
	convoyID, convoyErr := store.CreateConvoy(db, convoyName)
	if convoyErr != nil {
		logger.Printf("Merge convoy creation failed — approving independently")
		approveProposal(db, featureA, tasksA, logger)
		approveProposal(db, featureB, tasksB, logger)
		return
	}
	store.SetConvoyCoordinated(db, convoyID)

	idMapping, err := insertConvoyAndTasks(db, mergedTasks, featureA, convoyID)
	if err != nil {
		store.FailBounty(db, featureA.ID, "Chancellor Err (merge): "+err.Error())
		store.FailBounty(db, featureB.ID, "Chancellor Err (merge): "+err.Error())
		return
	}

	store.SetProposedConvoyStatus(db, featureA.ID, "merged")
	store.SetProposedConvoyStatus(db, featureB.ID, "merged")
	store.UpdateBountyStatus(db, featureA.ID, "Completed")
	store.UpdateBountyStatus(db, featureB.ID, "Completed")

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
}

// synthesizeMergedPlan calls Claude to produce a unified task list from two plans.
func synthesizeMergedPlan(db *sql.DB, featureA *store.Bounty, tasksA []store.TaskPlan, featureB *store.Bounty, tasksB []store.TaskPlan, logger interface{ Printf(string, ...any) }) []store.TaskPlan {
	planAJSON, _ := json.MarshalIndent(tasksA, "", "  ")
	planBJSON, _ := json.MarshalIndent(tasksB, "", "  ")

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
		featureA.ID, util.TruncateStr(featureA.Payload, 200), string(planAJSON),
		featureB.ID, util.TruncateStr(featureB.Payload, 200), string(planBJSON))

	response, err := claude.AskClaudeCLI(chancellorSystemPrompt, mergePrompt, "", 1)
	if err != nil {
		logger.Printf("Merge synthesis Claude call failed: %v", err)
		return nil
	}

	jsonStr := claude.ExtractJSON(response)
	var merged []store.TaskPlan
	if err := json.Unmarshal([]byte(jsonStr), &merged); err != nil {
		logger.Printf("Merge synthesis parse failed: %v", err)
		return nil
	}

	knownRepos := loadKnownRepos(db)
	if err := validateTaskPlan(merged, knownRepos); err != nil {
		logger.Printf("Merge synthesis plan invalid: %v", err)
		return nil
	}
	return merged
}

// buildChancellorPrompt constructs the user prompt with full context.
func buildChancellorPrompt(feature *store.Bounty, tasks []store.TaskPlan, activeConvoys []store.ActiveConvoyInfo, pendingProposals []store.PendingProposalInfo) string {
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

	return b.String()
}
