// Package store: Tags, RepoTags, TagSuggestions — D14 Phase 1.
//
// Tags are operator-defined labels applied to repos. FleetRules rows with
// agent_scope = 'senate:tag:<name>' fire only on repos that carry that tag.
//
// Every mutator returns error per CLAUDE.md § "No silent failures".
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ── Types ─────────────────────────────────────────────────────────────────────

// Tag is the in-memory shape of one Tags row.
type Tag struct {
	Name        string
	Description string
	CreatedAt   string
	CreatedBy   string
}

// RepoTag is one row from the RepoTags join table.
type RepoTag struct {
	RepoName string
	Tag      string
	AddedAt  string
	AddedBy  string
	Source   string
}

// TagSuggestion is one row from the TagSuggestions table.
// Status ∈ {'pending', 'accepted', 'dismissed'}.
type TagSuggestion struct {
	ID          int
	RepoName    string
	Tag         string
	Rationale   string
	SuggestedAt string
	SuggestedBy string
	Status      string
	ResolvedAt  string
	ResolvedBy  string
}

// FleetRulesRow is the in-memory shape of one FleetRules row.
// Used by ResolveRulesForRepo to return applicable rules for a repo.
type FleetRulesRow struct {
	ID                       int
	RuleKey                  string
	Category                 string
	AgentScope               string
	RenderTo                 string
	EnforcedBy               string
	Content                  string
	ContentHash              string
	Version                  int
	ActiveFrom               string
	ActiveUntil              string
	PromotedByExperimentID   int
	CreatedBy                string
	CreatedAt                string
}

// ── Tags CRUD ─────────────────────────────────────────────────────────────────

// CreateTag inserts a new tag into the Tags table. Returns an error if the
// tag already exists (PRIMARY KEY conflict) or if name is empty.
func CreateTag(db *sql.DB, name, description, createdBy string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("CreateTag: name must not be empty")
	}
	_, err := db.Exec(
		`INSERT INTO Tags (name, description, created_by) VALUES (?, ?, ?)`,
		name, description, createdBy,
	)
	if err != nil {
		return fmt.Errorf("CreateTag(%q): %w", name, err)
	}
	return nil
}

// GetTag loads one tag by name. Returns (Tag{}, sql.ErrNoRows) when not found.
func GetTag(db *sql.DB, name string) (Tag, error) {
	var t Tag
	err := db.QueryRow(
		`SELECT name, IFNULL(description,''), IFNULL(created_at,''), IFNULL(created_by,'')
		   FROM Tags WHERE name = ?`, name,
	).Scan(&t.Name, &t.Description, &t.CreatedAt, &t.CreatedBy)
	if err != nil {
		return Tag{}, fmt.Errorf("GetTag(%q): %w", name, err)
	}
	return t, nil
}

// ListTags returns all tags ordered by name.
func ListTags(db *sql.DB) ([]Tag, error) {
	rows, err := db.Query(
		`SELECT name, IFNULL(description,''), IFNULL(created_at,''), IFNULL(created_by,'')
		   FROM Tags ORDER BY name`,
	)
	if err != nil {
		return nil, fmt.Errorf("ListTags: %w", err)
	}
	defer rows.Close()

	var tags []Tag
	for rows.Next() {
		var t Tag
		if err := rows.Scan(&t.Name, &t.Description, &t.CreatedAt, &t.CreatedBy); err != nil {
			return nil, fmt.Errorf("ListTags scan: %w", err)
		}
		tags = append(tags, t)
	}
	return tags, rows.Err()
}

// DeleteTag removes a tag from Tags. Returns an error if any RepoTags rows
// still reference it (foreign-key constraint with PRAGMA foreign_keys=ON).
func DeleteTag(db *sql.DB, name string) error {
	res, err := db.Exec(`DELETE FROM Tags WHERE name = ?`, name)
	if err != nil {
		// sqlite3 FK violation message contains "FOREIGN KEY constraint failed"
		return fmt.Errorf("DeleteTag(%q): %w", name, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("DeleteTag(%q): tag not found", name)
	}
	return nil
}

// ── RepoTags ──────────────────────────────────────────────────────────────────

// AddRepoTag attaches tag to repoName. Idempotent via INSERT OR IGNORE.
// Returns an error if tag does not exist in Tags (FK constraint).
func AddRepoTag(db *sql.DB, repoName, tag, addedBy, source string) error {
	if repoName == "" {
		return errors.New("AddRepoTag: repoName must not be empty")
	}
	if tag == "" {
		return errors.New("AddRepoTag: tag must not be empty")
	}
	_, err := db.Exec(
		`INSERT OR IGNORE INTO RepoTags (repo_name, tag, added_by, source) VALUES (?, ?, ?, ?)`,
		repoName, tag, addedBy, source,
	)
	if err != nil {
		return fmt.Errorf("AddRepoTag(%q, %q): %w", repoName, tag, err)
	}
	return nil
}

// RemoveRepoTag detaches tag from repoName.
func RemoveRepoTag(db *sql.DB, repoName, tag string) error {
	res, err := db.Exec(
		`DELETE FROM RepoTags WHERE repo_name = ? AND tag = ?`, repoName, tag,
	)
	if err != nil {
		return fmt.Errorf("RemoveRepoTag(%q, %q): %w", repoName, tag, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("RemoveRepoTag(%q, %q): row not found", repoName, tag)
	}
	return nil
}

// ListTagsForRepo returns all RepoTag rows for repoName, ordered by tag name.
func ListTagsForRepo(db *sql.DB, repoName string) ([]RepoTag, error) {
	rows, err := db.Query(
		`SELECT repo_name, tag, IFNULL(added_at,''), IFNULL(added_by,''), IFNULL(source,'')
		   FROM RepoTags WHERE repo_name = ? ORDER BY tag`,
		repoName,
	)
	if err != nil {
		return nil, fmt.Errorf("ListTagsForRepo(%q): %w", repoName, err)
	}
	defer rows.Close()

	var result []RepoTag
	for rows.Next() {
		var rt RepoTag
		if err := rows.Scan(&rt.RepoName, &rt.Tag, &rt.AddedAt, &rt.AddedBy, &rt.Source); err != nil {
			return nil, fmt.Errorf("ListTagsForRepo scan: %w", err)
		}
		result = append(result, rt)
	}
	return result, rows.Err()
}

// ListReposForTag returns all RepoTag rows for a given tag, ordered by repo_name.
func ListReposForTag(db *sql.DB, tag string) ([]RepoTag, error) {
	rows, err := db.Query(
		`SELECT repo_name, tag, IFNULL(added_at,''), IFNULL(added_by,''), IFNULL(source,'')
		   FROM RepoTags WHERE tag = ? ORDER BY repo_name`,
		tag,
	)
	if err != nil {
		return nil, fmt.Errorf("ListReposForTag(%q): %w", tag, err)
	}
	defer rows.Close()

	var result []RepoTag
	for rows.Next() {
		var rt RepoTag
		if err := rows.Scan(&rt.RepoName, &rt.Tag, &rt.AddedAt, &rt.AddedBy, &rt.Source); err != nil {
			return nil, fmt.Errorf("ListReposForTag scan: %w", err)
		}
		result = append(result, rt)
	}
	return result, rows.Err()
}

// ── TagSuggestions ────────────────────────────────────────────────────────────

// CreateTagSuggestion records an LLM-proposed tag for operator review.
// Returns the new row's id.
func CreateTagSuggestion(db *sql.DB, repoName, tag, rationale, suggestedBy string) (int, error) {
	if repoName == "" {
		return 0, errors.New("CreateTagSuggestion: repoName must not be empty")
	}
	if tag == "" {
		return 0, errors.New("CreateTagSuggestion: tag must not be empty")
	}
	if suggestedBy == "" {
		return 0, errors.New("CreateTagSuggestion: suggestedBy must not be empty")
	}
	res, err := db.Exec(
		`INSERT INTO TagSuggestions (repo_name, tag, rationale, suggested_by)
		 VALUES (?, ?, ?, ?)`,
		repoName, tag, rationale, suggestedBy,
	)
	if err != nil {
		return 0, fmt.Errorf("CreateTagSuggestion(%q, %q): %w", repoName, tag, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("CreateTagSuggestion LastInsertId: %w", err)
	}
	return int(id), nil
}

// ListTagSuggestions returns suggestions filtered by status. Pass an empty
// string to return all suggestions regardless of status.
func ListTagSuggestions(db *sql.DB, status string) ([]TagSuggestion, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if status == "" {
		rows, err = db.Query(
			`SELECT id, repo_name, tag, IFNULL(rationale,''), IFNULL(suggested_at,''),
			        IFNULL(suggested_by,''), status, IFNULL(resolved_at,''), IFNULL(resolved_by,'')
			   FROM TagSuggestions ORDER BY suggested_at`,
		)
	} else {
		rows, err = db.Query(
			`SELECT id, repo_name, tag, IFNULL(rationale,''), IFNULL(suggested_at,''),
			        IFNULL(suggested_by,''), status, IFNULL(resolved_at,''), IFNULL(resolved_by,'')
			   FROM TagSuggestions WHERE status = ? ORDER BY suggested_at`,
			status,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("ListTagSuggestions(%q): %w", status, err)
	}
	defer rows.Close()

	var result []TagSuggestion
	for rows.Next() {
		var s TagSuggestion
		if err := rows.Scan(
			&s.ID, &s.RepoName, &s.Tag, &s.Rationale,
			&s.SuggestedAt, &s.SuggestedBy, &s.Status,
			&s.ResolvedAt, &s.ResolvedBy,
		); err != nil {
			return nil, fmt.Errorf("ListTagSuggestions scan: %w", err)
		}
		result = append(result, s)
	}
	return result, rows.Err()
}

// ResolveTagSuggestion marks a suggestion as 'accepted' or 'dismissed'.
// Returns an error if newStatus is not one of those two values or if the
// row does not exist.
func ResolveTagSuggestion(db *sql.DB, id int, newStatus, resolvedBy string) error {
	if newStatus != "accepted" && newStatus != "dismissed" {
		return fmt.Errorf("ResolveTagSuggestion: status must be 'accepted' or 'dismissed', got %q", newStatus)
	}
	res, err := db.Exec(
		`UPDATE TagSuggestions
		    SET status = ?, resolved_at = datetime('now'), resolved_by = ?
		  WHERE id = ?`,
		newStatus, resolvedBy, id,
	)
	if err != nil {
		return fmt.Errorf("ResolveTagSuggestion(%d, %q): %w", id, newStatus, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("ResolveTagSuggestion(%d): suggestion not found", id)
	}
	return nil
}

// ── Rule scoping ──────────────────────────────────────────────────────────────

// ResolveRulesForRepo returns all active FleetRules rows applicable to
// repoName under the D14 tag-driven scoping rules:
//
//   - scope = "senate:*"               — global, applies to all repos
//   - scope = "senate:<repoName>"      — repo-specific
//   - scope = "senate:tag:<t>"         — applies when repoName carries tag <t>
//
// Only rows with active_until = '' (the partial-index "active" predicate)
// are returned. Order is deterministic: rule_key ASC.
func ResolveRulesForRepo(db *sql.DB, repoName string) ([]FleetRulesRow, error) {
	rows, err := db.Query(
		`SELECT fr.id, fr.rule_key, IFNULL(fr.category,''), fr.agent_scope,
		        fr.render_to, IFNULL(fr.enforced_by,''), fr.content,
		        IFNULL(fr.content_hash,''), fr.version,
		        IFNULL(fr.active_from,''), IFNULL(fr.active_until,''),
		        IFNULL(fr.promoted_by_experiment_id,0),
		        IFNULL(fr.created_by,''), IFNULL(fr.created_at,'')
		   FROM FleetRules fr
		  WHERE fr.active_until = ''
		    AND (
		          fr.agent_scope = 'senate:*'
		       OR fr.agent_scope = 'senate:' || ?
		       OR fr.agent_scope IN (
		            SELECT 'senate:tag:' || rt.tag
		              FROM RepoTags rt WHERE rt.repo_name = ?
		          )
		        )
		  ORDER BY fr.rule_key`,
		repoName, repoName,
	)
	if err != nil {
		return nil, fmt.Errorf("ResolveRulesForRepo(%q): %w", repoName, err)
	}
	defer rows.Close()

	var result []FleetRulesRow
	for rows.Next() {
		var r FleetRulesRow
		if err := rows.Scan(
			&r.ID, &r.RuleKey, &r.Category, &r.AgentScope,
			&r.RenderTo, &r.EnforcedBy, &r.Content,
			&r.ContentHash, &r.Version,
			&r.ActiveFrom, &r.ActiveUntil,
			&r.PromotedByExperimentID,
			&r.CreatedBy, &r.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("ResolveRulesForRepo scan: %w", err)
		}
		result = append(result, r)
	}
	return result, rows.Err()
}
