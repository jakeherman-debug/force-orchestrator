// Package senate — registry helpers (D4 Phase 3).
//
// Convenience helpers for enumerating active Senators and routing a
// Feature plan to the matching Senators. The Senate router (per
// docs/next-gen-agents.md § "Trigger") identifies the affected Senators
// of a plan as:
//
//	affected_senators(plan) = {senator ∈ Senate : plan.touches(senator.repo)}
//	                        ∪ {team_senator : plan.cross_repo(team_senator.team)}
//
// Phase 3 ships the per-repo router. Team-Senators are deferred (no
// production team Senator at launch).
package senate

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// ListActiveSenators returns every active Senator's repo ID + chamber
// status, ordered by repo for stable test output. Status filter is
// 'active' — onboarding / suspended / retired Senators don't review.
func ListActiveSenators(db *sql.DB) ([]string, error) {
	if db == nil {
		return nil, fmt.Errorf("senate.ListActiveSenators: nil db")
	}
	rows, err := db.Query(`
		SELECT senator_name FROM SenateChambers
		 WHERE status = 'active'
		 ORDER BY senator_name ASC`)
	if err != nil {
		return nil, fmt.Errorf("senate.ListActiveSenators: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if scanErr := rows.Scan(&name); scanErr != nil {
			return nil, fmt.Errorf("senate.ListActiveSenators: scan: %w", scanErr)
		}
		out = append(out, name)
	}
	if rErr := rows.Err(); rErr != nil {
		return nil, fmt.Errorf("senate.ListActiveSenators: rows.Err: %w", rErr)
	}
	return out, nil
}

// AffectedSenators returns the subset of active Senators whose repo
// shows up in `plan_repos` (the set of repos any task in the plan
// touches). The Feature row's TargetRepo is also folded in so a
// no-task-plan / single-repo Feature still routes to the right Senator.
//
// Returns an alphabetically-sorted slice of repo IDs (== Senator IDs).
func AffectedSenators(db *sql.DB, planRepos []string, featureRepo string) ([]string, error) {
	active, err := ListActiveSenators(db)
	if err != nil {
		return nil, err
	}
	if len(active) == 0 {
		return nil, nil
	}
	want := make(map[string]struct{})
	for _, r := range planRepos {
		r = strings.TrimSpace(r)
		if r != "" {
			want[r] = struct{}{}
		}
	}
	if featureRepo = strings.TrimSpace(featureRepo); featureRepo != "" {
		want[featureRepo] = struct{}{}
	}
	if len(want) == 0 {
		// Plan touches no concrete repo (very rare; e.g. a dependency-
		// only refresh). Fan out to every active Senator so the spec's
		// "any concerns?" gate stays honest.
		out := append([]string(nil), active...)
		sort.Strings(out)
		return out, nil
	}
	var matched []string
	for _, s := range active {
		if _, ok := want[s]; ok {
			matched = append(matched, s)
		}
	}
	sort.Strings(matched)
	return matched, nil
}
