// Package senate — repo-scoped Senator review layer (D4 Phase 3).
//
// A Senator advises the Chancellor on a proposed Feature plan that
// touches its repo / team domain. Each Senator carries:
//
//   - a SenateChambers row (identity + status)
//   - a SenateMemory shelf (recent rejections, escalations, commits)
//   - a slice of FleetRules with agent_scope='senate:<repo>'
//   - a recent-commits digest from the Librarian Client
//
// At review time the Senator's prompt is assembled from these inputs
// and sent through claude.CallWithTranscript via the senate capability
// profile. The response is unmarshalled into a Verdict (verdict.go) and
// persisted to the SenateReview table.
//
// The Senator's own rules are NEVER auto-edited from inside this
// package — promotion routes through the operator-ratified pipeline
// (Librarian.EmitCandidate → ExperimentAuthor → operator ratifies). The
// "no Senator auto-editing own rules" anti-cheat (D4 exit criterion 3)
// is enforced AST-side by Pattern P34 (audit_pattern_p34_senate_no_self_promote_test.go).
package senate

import (
	"database/sql"
	"fmt"
)

// PromotionProposal is the Phase-3-internal review subject — a
// thin in-memory view of a Feature whose plan has been written to
// ProposedConvoys but not yet advanced to AwaitingChancellorReview.
//
// Phase 3 ships the shape; the runSenateReviewTask handler in
// internal/agents/senate.go fills it in from the Feature row + its
// proposed plan + the active-convoy context.
type PromotionProposal struct {
	FeatureID     int
	FeaturePayload string
	TargetRepo    string  // repo the Feature primarily touches; routed to the matching Senator
	PlanJSON      string  // raw ProposedConvoys.plan_json (TaskPlan slice)
}

// Senator is the loaded view of one repo-scoped reviewer. Built by
// LoadSenator(db, repo) so callers don't have to assemble the
// chamber + memory + rule slices by hand.
type Senator struct {
	RepoID     string                  // canonical repo name; matches SenateChambers.senator_name
	Scope      string                  // 'repo:<name>' | 'team:<name>'
	Status     string                  // 'onboarding' | 'active' | 'suspended' | 'retired'
	Memory     []MemoryEntry           // top-K SenateMemory rows
	RuleKeys   []string                // FleetRules.rule_key entries with agent_scope='senate:<repo>'
	RuleBodies map[string]string       // rule_key → content (parallel to RuleKeys)
}

// MemoryEntry is the senate-package view of one SenateMemory row. The
// store package owns the persistence shape; this is the slimmer
// in-memory shape the prompt-builder consumes.
type MemoryEntry struct {
	ID      int
	Topic   string
	Summary string
	Source  string
	Weight  float64
}

// LoadSenator assembles the full review-time view for one Senator from
// the database. Returns (nil, nil) when no chamber exists for the given
// repoID — the caller treats this as "no Senator yet, skip" per spec.
//
// LoadSenator does NOT call the Librarian Client; the recent-commits
// digest is assembled by the runSenateReviewTask handler that has the
// Librarian Client wired in. This separation lets the senate package
// stay focused on the data shape and keeps it testable without
// fabricating a librarian.Client mock for every store-helper test.
func LoadSenator(db *sql.DB, repoID string) (*Senator, error) {
	if db == nil {
		return nil, fmt.Errorf("senate.LoadSenator: nil db")
	}
	if repoID == "" {
		return nil, fmt.Errorf("senate.LoadSenator: repoID required")
	}

	chamber, err := loadChamber(db, repoID)
	if err != nil {
		return nil, err
	}
	if chamber == nil {
		return nil, nil
	}

	mem, err := loadMemory(db, repoID, 50)
	if err != nil {
		return nil, err
	}

	keys, bodies, err := loadRules(db, repoID)
	if err != nil {
		return nil, err
	}

	return &Senator{
		RepoID:     chamber.SenatorName,
		Scope:      chamber.Scope,
		Status:     chamber.Status,
		Memory:     mem,
		RuleKeys:   keys,
		RuleBodies: bodies,
	}, nil
}

// chamberRow is the senate-package-private view of SenateChambers, kept
// here to avoid an import cycle with internal/store at registry time.
type chamberRow struct {
	SenatorName string
	Scope       string
	Status      string
}

func loadChamber(db *sql.DB, repoID string) (*chamberRow, error) {
	var c chamberRow
	err := db.QueryRow(`
		SELECT senator_name, scope, status
		  FROM SenateChambers
		 WHERE senator_name = ?`, repoID).
		Scan(&c.SenatorName, &c.Scope, &c.Status)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("senate.LoadSenator(chamber=%s): %w", repoID, err)
	}
	return &c, nil
}

func loadMemory(db *sql.DB, repoID string, k int) ([]MemoryEntry, error) {
	rows, err := db.Query(`
		SELECT id, IFNULL(topic,''), summary, IFNULL(source,'manual'), IFNULL(weight, 1.0)
		  FROM SenateMemory
		 WHERE senator = ?
		 ORDER BY weight DESC, created_at DESC, id DESC
		 LIMIT ?`, repoID, k)
	if err != nil {
		return nil, fmt.Errorf("senate.LoadSenator(memory=%s): %w", repoID, err)
	}
	defer rows.Close()
	var out []MemoryEntry
	for rows.Next() {
		var e MemoryEntry
		if scanErr := rows.Scan(&e.ID, &e.Topic, &e.Summary, &e.Source, &e.Weight); scanErr != nil {
			return nil, fmt.Errorf("senate.LoadSenator(memory=%s): scan: %w", repoID, scanErr)
		}
		out = append(out, e)
	}
	if rErr := rows.Err(); rErr != nil {
		return nil, fmt.Errorf("senate.LoadSenator(memory=%s): rows.Err: %w", repoID, rErr)
	}
	return out, nil
}

// loadRules pulls the active senate-scoped FleetRules rows for repoID.
// Filters on active_until='' so retired rules don't pollute the
// Senator's prompt. Versioning in FleetRules is per (rule_key, version)
// — we pull the highest version that is still active.
func loadRules(db *sql.DB, repoID string) ([]string, map[string]string, error) {
	scope := fmt.Sprintf("senate:%s", repoID)
	rows, err := db.Query(`
		SELECT rule_key, content
		  FROM FleetRules
		 WHERE agent_scope = ?
		   AND IFNULL(active_until, '') = ''
		 ORDER BY rule_key ASC, version DESC`, scope)
	if err != nil {
		return nil, nil, fmt.Errorf("senate.LoadSenator(rules=%s): %w", repoID, err)
	}
	defer rows.Close()
	var keys []string
	bodies := make(map[string]string)
	for rows.Next() {
		var key, content string
		if scanErr := rows.Scan(&key, &content); scanErr != nil {
			return nil, nil, fmt.Errorf("senate.LoadSenator(rules=%s): scan: %w", repoID, scanErr)
		}
		// Only keep the highest version of each rule_key (the loop is
		// ordered DESC by version, so the FIRST occurrence is kept).
		if _, seen := bodies[key]; seen {
			continue
		}
		keys = append(keys, key)
		bodies[key] = content
	}
	if rErr := rows.Err(); rErr != nil {
		return nil, nil, fmt.Errorf("senate.LoadSenator(rules=%s): rows.Err: %w", repoID, rErr)
	}
	return keys, bodies, nil
}
