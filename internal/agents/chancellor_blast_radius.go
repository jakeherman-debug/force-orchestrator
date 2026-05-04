// Package agents — Chancellor blast-radius post-process (D8 Track 2).
//
// After Chancellor's APPROVE / SEQUENCE / MERGE path lands a convoy
// (insertConvoyAndTasks committed), this file's PostProcessBlastRadius
// pass runs to:
//
//  1. Identify which CrossRepoSymbols rows the convoy's planned tasks
//     would modify (heuristic: per-task Repo + symbol-name matches in
//     the task payload against the dog-populated CrossRepoSymbols set).
//  2. Query graph.BlastRadiusForModifications for the affected consumer
//     set (file:line lists + repos).
//  3. Insert one downstream consumer-update task per affected consumer
//     repo into the same convoy, parented at the Feature.
//  4. Persist the full record (modified_symbols, affected_consumer_repos,
//     auto_included_tasks) to BountyBoard.blast_radius_json on the
//     Feature row.
//  5. Fan out per-affected-consumer-Senator SenateReview tasks so the
//     Senate's per-repo Senator gets a chance to weigh in on the
//     downstream impact.
//
// Anti-cheat (per task spec):
//   - The blast-radius computation is data-driven; no LLM call inside
//     the post-process. Step 1's symbol extraction is a deterministic
//     scan of the task payload against the indexed symbol set.
//   - auto_included_tasks IDs are recorded durably in blast_radius_json
//     so the operator can see the convoy's expansion. No silent
//     insertion.
//   - D8 Track 1's dog (`dogRepoGraphScan`) keeps running unchanged;
//     this branch only consumes its CrossRepoSymbols / CrossRepoDependencies
//     output.
//
// CLAUDE.md "no silent failures": every error path returns wrapped err
// to the caller. The caller (chancellor.go's approve/sequence/merge)
// logs the failure and the stale-lock detector retries the Feature.
package agents

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"force-orchestrator/internal/clients/graph"
	"force-orchestrator/internal/store"
)

// blastRadiusLogger is the narrow interface chancellor.go's logger
// satisfies. Defined locally so the post-process doesn't pull in the
// agents package's full logger surface.
type blastRadiusLogger interface {
	Printf(string, ...any)
}

// ExtractSymbolModifications scans the task payload for any CrossRepoSymbols
// row registered under task.Repo whose symbol_path appears in the payload.
// Each match becomes one SymbolModification.
//
// The match is a substring scan — symbol_path is a qualified name like
// `pkg.Type.Method`, and Commander's task payloads commonly mention
// the qualified symbol name in prose ("update auth.LoginHandler to ..."
// or "rename ProfileService.GetByID to ...").
//
// Limits:
//   - Skips short symbol_paths (< 4 chars) so common substrings like "go"
//     don't match every payload.
//   - De-duplicates by symbol_path.
//   - Returns an empty slice (not nil) on no match — callers can branch
//     on len() without nil-vs-empty hazards.
func ExtractSymbolModifications(db *sql.DB, task store.TaskPlan) ([]graph.SymbolModification, error) {
	out := make([]graph.SymbolModification, 0)
	if task.Repo == "" {
		return out, nil
	}
	syms, err := store.ListCrossRepoSymbolsByRepo(db, task.Repo)
	if err != nil {
		return nil, fmt.Errorf("ExtractSymbolModifications(repo=%s): %w", task.Repo, err)
	}
	if len(syms) == 0 {
		return out, nil
	}
	payload := task.Task
	seen := map[string]struct{}{}
	for _, s := range syms {
		if len(s.SymbolPath) < 4 {
			continue
		}
		if _, dup := seen[s.SymbolPath]; dup {
			continue
		}
		if !strings.Contains(payload, s.SymbolPath) {
			continue
		}
		seen[s.SymbolPath] = struct{}{}
		out = append(out, graph.SymbolModification{
			Repo:       s.RepoName,
			FilePath:   s.FilePath,
			SymbolPath: s.SymbolPath,
		})
	}
	return out, nil
}

// PostProcessBlastRadius runs the full D8 T2 post-process on a convoy
// that's just been created. featureID is the Feature whose Bounty row
// gets blast_radius_json populated; convoyID is the convoy
// insertConvoyAndTasks created; tasks is the post-insert plan; gc is
// the graph Client (typically graph.NewInProcess(db)).
//
// Returns the BlastRadiusRecord that was persisted, plus any error.
// On error, blast_radius_json may have been partially written but the
// auto_included_tasks list is always either fully inserted or absent
// (we collect IDs as we go and persist atomically at the end).
func PostProcessBlastRadius(ctx context.Context, db *sql.DB, gc graph.Client, featureID, convoyID int, tasks []store.TaskPlan, idMapping map[int]int, logger blastRadiusLogger) (store.BlastRadiusRecord, error) {
	if db == nil {
		return store.BlastRadiusRecord{}, fmt.Errorf("PostProcessBlastRadius: db is nil")
	}
	if gc == nil {
		return store.BlastRadiusRecord{}, fmt.Errorf("PostProcessBlastRadius: graph client is nil")
	}

	// Step 1 — extract per-task modifications.
	var mods []graph.SymbolModification
	for _, t := range tasks {
		ms, err := ExtractSymbolModifications(db, t)
		if err != nil {
			return store.BlastRadiusRecord{}, fmt.Errorf("PostProcessBlastRadius(feature=%d): extract: %w", featureID, err)
		}
		mods = append(mods, ms...)
	}
	// De-duplicate (Repo, SymbolPath) across the whole batch.
	uniq := map[string]graph.SymbolModification{}
	for _, m := range mods {
		key := m.Repo + "|" + m.SymbolPath
		if _, ok := uniq[key]; !ok {
			uniq[key] = m
		}
	}
	mods = mods[:0]
	for _, v := range uniq {
		mods = append(mods, v)
	}
	sort.Slice(mods, func(i, j int) bool {
		if mods[i].Repo != mods[j].Repo {
			return mods[i].Repo < mods[j].Repo
		}
		return mods[i].SymbolPath < mods[j].SymbolPath
	})

	// Step 2 — query the graph. Empty mods → empty BlastRadius; we still
	// persist the empty record so the Feature row carries '{...}' rather
	// than the default '{}', which makes "post-process ran but found
	// nothing" distinguishable from "post-process never ran" in operator
	// dashboards.
	br, err := gc.BlastRadiusForModifications(ctx, mods)
	if err != nil {
		return store.BlastRadiusRecord{}, fmt.Errorf("PostProcessBlastRadius(feature=%d): graph: %w", featureID, err)
	}

	// Step 3 — auto-include downstream consumer-update tasks. One per
	// (consumer_repo, modified_symbol) pair so each consumer Senator
	// + astromech sees a clearly-scoped task. Skip the Feature's own
	// repos (don't recursively suggest updating the modifying repo).
	modifyingRepos := map[string]struct{}{}
	for _, m := range mods {
		modifyingRepos[m.Repo] = struct{}{}
	}

	// Build per-(consumer_repo, symbol_path) → []ConsumerSite map for
	// the payload breadcrumbs. Track which consumer-symbols we've seen
	// so we don't insert duplicate tasks.
	type insertKey struct{ repo, symbol string }
	keyed := map[insertKey][]graph.ConsumerSite{}
	for _, m := range mods {
		sites := br.ConsumersBySymbol[m.SymbolPath]
		for _, s := range sites {
			if _, isMod := modifyingRepos[s.Repo]; isMod {
				continue
			}
			k := insertKey{repo: s.Repo, symbol: m.SymbolPath}
			keyed[k] = append(keyed[k], s)
		}
	}

	// Stable iteration: sort keys before inserting so test output
	// matches and operator-facing logs are reproducible.
	keys := make([]insertKey, 0, len(keyed))
	for k := range keyed {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].repo != keys[j].repo {
			return keys[i].repo < keys[j].repo
		}
		return keys[i].symbol < keys[j].symbol
	})

	autoTasks := make([]int, 0, len(keys))
	for _, k := range keys {
		sites := keyed[k]
		// Build "<file>:<line>, <file>:<line>" breadcrumb list. Cap at
		// the first 8 sites so a fan-out symbol doesn't produce a
		// 10KB payload.
		breadcrumbs := make([]string, 0, len(sites))
		for i, s := range sites {
			if i >= 8 {
				breadcrumbs = append(breadcrumbs, fmt.Sprintf("(+%d more)", len(sites)-8))
				break
			}
			breadcrumbs = append(breadcrumbs, fmt.Sprintf("%s:%d", s.FilePath, s.Line))
		}
		payload := fmt.Sprintf("[BLAST_RADIUS_UPDATE] from feature #%d modified %s; update your consumer sites at %s accordingly.",
			featureID, k.symbol, strings.Join(breadcrumbs, ", "))
		taskID, insErr := store.AddConvoyTask(db, featureID, k.repo, payload, convoyID, 0, "Pending")
		if insErr != nil {
			return store.BlastRadiusRecord{}, fmt.Errorf("PostProcessBlastRadius(feature=%d): AddConvoyTask consumer=%s symbol=%s: %w",
				featureID, k.repo, k.symbol, insErr)
		}
		autoTasks = append(autoTasks, taskID)
		if logger != nil {
			logger.Printf("Feature #%d: blast-radius auto-included task #%d [%s] %s", featureID, taskID, k.repo, k.symbol)
		}
	}
	_ = idMapping // reserved for a future "wire blast-radius tasks after the Feature's own root tasks" pass

	// Step 4 — assemble + persist BlastRadiusRecord.
	rec := store.BlastRadiusRecord{
		ModifiedSymbols:       make([]store.BlastRadiusSymbol, 0, len(br.ModifiedSymbols)),
		AffectedConsumerRepos: append([]string(nil), br.AffectedConsumerRepos...),
		AutoIncludedTasks:     autoTasks,
	}
	for _, s := range br.ModifiedSymbols {
		rec.ModifiedSymbols = append(rec.ModifiedSymbols, store.BlastRadiusSymbol{
			SymbolPath: s.Name,
			Kind:       s.Kind,
			FilePath:   s.Path,
			LineNumber: s.Line,
		})
	}
	if err := store.SetFeatureBlastRadius(db, featureID, rec); err != nil {
		return rec, fmt.Errorf("PostProcessBlastRadius(feature=%d): persist: %w", featureID, err)
	}

	// Step 5 — fan out per-affected-consumer-Senator SenateReview tasks.
	// Only fires for consumer repos that have an active Senator chamber;
	// the Senate router skips no-active-Senator repos with zero cost
	// anyway, but pre-filtering here keeps the audit trail clean.
	if len(rec.AffectedConsumerRepos) > 0 {
		if err := QueueBlastRadiusSenateConsultations(db, featureID, rec.AffectedConsumerRepos, logger); err != nil {
			// Don't fail the whole post-process — the blast-radius is
			// already persisted and the consumer tasks are inserted.
			// Log + continue so a transient SenateChambers query failure
			// doesn't block the Feature's progression.
			if logger != nil {
				logger.Printf("Feature #%d: blast-radius Senate consultation queue failed (%v) — continuing", featureID, err)
			}
		}
	}

	return rec, nil
}

// QueueBlastRadiusSenateConsultations fans out a SenateReview task for
// each affected consumer repo that has an active Senator. The shape
// mirrors QueueSenateReviewHook's per-Feature path but is keyed on the
// consumer repo (target_repo) so the Senate router's per-Senator
// dispatch (senate.AffectedSenators) lights up the right Senator.
//
// Per affected_consumer_repos we queue at most one SenateReview task —
// re-running the post-process for the same Feature would re-insert
// these, which is fine: the Senate's idempotency check (the
// runSenateReviewTask handler bails when the Feature is no longer in
// AwaitingSenateReview) absorbs the duplicate.
//
// Returns error per CLAUDE.md "no silent failures".
func QueueBlastRadiusSenateConsultations(db *sql.DB, featureID int, consumerRepos []string, logger blastRadiusLogger) error {
	if db == nil {
		return fmt.Errorf("QueueBlastRadiusSenateConsultations: db is nil")
	}
	if featureID <= 0 {
		return fmt.Errorf("QueueBlastRadiusSenateConsultations: featureID required")
	}
	for _, repo := range consumerRepos {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM SenateChambers
			 WHERE senator_name = ? AND status = 'active'`, repo).Scan(&n); err != nil {
			return fmt.Errorf("QueueBlastRadiusSenateConsultations(feature=%d, repo=%s): chamber query: %w",
				featureID, repo, err)
		}
		if n == 0 {
			// No active Senator for this consumer repo → nothing to do.
			// (The Senate router would skip too — pre-filtering keeps
			// the audit trail clean.)
			continue
		}
		taskID, qErr := store.QueueSenateReview(db, featureID, repo)
		if qErr != nil {
			return fmt.Errorf("QueueBlastRadiusSenateConsultations(feature=%d, repo=%s): %w",
				featureID, repo, qErr)
		}
		if logger != nil {
			logger.Printf("Feature #%d: blast-radius queued Senate review task #%d for affected consumer repo %s",
				featureID, taskID, repo)
		}
	}
	return nil
}
