// D14 Phase 4 — Tags / TagSuggestions / Rules dashboard API handlers.
//
// Tags API:
//   GET    /api/tags                      — list all tags
//   POST   /api/tags                      — create a tag
//   DELETE /api/tags/{name}               — remove a tag
//   GET    /api/repos/{name}/tags         — tags for a repo
//   POST   /api/repos/{name}/tags         — add tag to repo
//   DELETE /api/repos/{name}/tags/{tag}   — remove tag from repo
//
// Tag Suggestions API:
//   GET  /api/tag-suggestions?status=…   — list suggestions
//   POST /api/tag-suggestions/{id}/accept  — accept (creates RepoTag)
//   POST /api/tag-suggestions/{id}/dismiss — dismiss
//
// Rules API:
//   GET  /api/rules?repo={name}          — resolved rules for repo (or all active)
//   POST /api/rules/{key}/upgrade-scope  — update agent_scope

package dashboard

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"force-orchestrator/internal/store"
)

// ── Tags ──────────────────────────────────────────────────────────────────────

// handleTags serves:
//
//	GET    /api/tags  — list all tags
//	POST   /api/tags  — create a tag (body: {"name":"...","description":"..."})
func handleTags(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		switch r.Method {
		case http.MethodGet:
			tags, err := store.ListTags(db)
			if err != nil {
				log.Printf("handleTags GET: %v", err)
				http.Error(w, `{"error":"list tags failed"}`, http.StatusInternalServerError)
				return
			}
			if tags == nil {
				tags = []store.Tag{}
			}
			json.NewEncoder(w).Encode(tags)

		case http.MethodPost:
			var body struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				if writeBodyReadError(w, err) {
					return
				}
			}
			if strings.TrimSpace(body.Name) == "" {
				http.Error(w, `{"error":"name is required"}`, http.StatusBadRequest)
				return
			}
			if err := store.CreateTag(db, body.Name, body.Description, "operator"); err != nil {
				log.Printf("handleTags POST: %v", err)
				http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
				return
			}
			store.LogAudit(db, "dashboard", "create-tag", 0, "created tag "+body.Name)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"ok":true,"name":%q}`, body.Name)

		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}

// handleTagsSubroutes serves:
//
//	DELETE /api/tags/{name} — remove a tag
func handleTagsSubroutes(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		// Path: /api/tags/{name}
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) < 3 {
			http.NotFound(w, r)
			return
		}
		tagName := parts[2]
		if tagName == "" {
			http.Error(w, `{"error":"missing tag name"}`, http.StatusBadRequest)
			return
		}

		switch r.Method {
		case http.MethodDelete:
			if err := store.DeleteTag(db, tagName); err != nil {
				if strings.Contains(err.Error(), "not found") {
					http.Error(w, `{"error":"tag not found"}`, http.StatusNotFound)
					return
				}
				if strings.Contains(err.Error(), "FOREIGN KEY") {
					http.Error(w, `{"error":"tag is in use by one or more repos"}`, http.StatusConflict)
					return
				}
				log.Printf("handleTagsSubroutes DELETE: %v", err)
				http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
				return
			}
			store.LogAudit(db, "dashboard", "delete-tag", 0, "deleted tag "+tagName)
			fmt.Fprintf(w, `{"ok":true,"name":%q}`, tagName)

		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}

// handleRepoTagsSubroutes serves:
//
//	GET    /api/repos/{name}/tags          — list tags for a repo
//	POST   /api/repos/{name}/tags          — add tag to repo
//	DELETE /api/repos/{name}/tags/{tag}    — remove tag from repo
//
// NOTE: this handler is registered at /api/repos/ alongside handleReposSubroutes.
// The mux matches the longer prefix when there are 5+ path segments.
// It is called from handleReposSubroutes via dispatch to avoid mux conflicts.
func handleRepoTagsSubroutes(db *sql.DB, w http.ResponseWriter, r *http.Request, parts []string) {
	// parts[0]=api, parts[1]=repos, parts[2]=<name>, parts[3]=tags, [parts[4]=<tag>]
	repoName := parts[2]
	if repoName == "" {
		http.Error(w, `{"error":"missing repo name"}`, http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodGet:
		repoTags, err := store.ListTagsForRepo(db, repoName)
		if err != nil {
			log.Printf("handleRepoTagsSubroutes GET: %v", err)
			http.Error(w, `{"error":"list repo tags failed"}`, http.StatusInternalServerError)
			return
		}
		if repoTags == nil {
			repoTags = []store.RepoTag{}
		}
		json.NewEncoder(w).Encode(repoTags)

	case http.MethodPost:
		var body struct {
			Tag    string `json:"tag"`
			Source string `json:"source"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			if writeBodyReadError(w, err) {
				return
			}
		}
		if strings.TrimSpace(body.Tag) == "" {
			http.Error(w, `{"error":"tag is required"}`, http.StatusBadRequest)
			return
		}
		source := body.Source
		if source == "" {
			source = "operator"
		}
		if err := store.AddRepoTag(db, repoName, body.Tag, "operator", source); err != nil {
			log.Printf("handleRepoTagsSubroutes POST: %v", err)
			http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
			return
		}
		store.LogAudit(db, "dashboard", "add-repo-tag", 0,
			fmt.Sprintf("added tag %q to repo %q", body.Tag, repoName))
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"ok":true,"repo":%q,"tag":%q}`, repoName, body.Tag)

	case http.MethodDelete:
		// DELETE /api/repos/{name}/tags/{tag}
		if len(parts) < 5 {
			http.Error(w, `{"error":"missing tag name"}`, http.StatusBadRequest)
			return
		}
		tagName := parts[4]
		if tagName == "" {
			http.Error(w, `{"error":"missing tag name"}`, http.StatusBadRequest)
			return
		}
		if err := store.RemoveRepoTag(db, repoName, tagName); err != nil {
			if strings.Contains(err.Error(), "not found") {
				http.Error(w, `{"error":"repo-tag association not found"}`, http.StatusNotFound)
				return
			}
			log.Printf("handleRepoTagsSubroutes DELETE: %v", err)
			http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
			return
		}
		store.LogAudit(db, "dashboard", "remove-repo-tag", 0,
			fmt.Sprintf("removed tag %q from repo %q", tagName, repoName))
		fmt.Fprintf(w, `{"ok":true,"repo":%q,"tag":%q}`, repoName, tagName)

	default:
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
	}
}

// ── Tag Suggestions ───────────────────────────────────────────────────────────

// handleTagSuggestions serves:
//
//	GET  /api/tag-suggestions?status=…
//	POST /api/tag-suggestions/{id}/accept
//	POST /api/tag-suggestions/{id}/dismiss
func handleTagSuggestions(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)

		// Determine if this is a sub-route (/{id}/accept or /{id}/dismiss)
		// or the list route.
		path := strings.TrimPrefix(r.URL.Path, "/api/tag-suggestions")
		path = strings.Trim(path, "/")

		if path == "" {
			// GET /api/tag-suggestions[?status=...]
			if r.Method != http.MethodGet {
				http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
				return
			}
			status := r.URL.Query().Get("status")
			suggestions, err := store.ListTagSuggestions(db, status)
			if err != nil {
				log.Printf("handleTagSuggestions GET: %v", err)
				http.Error(w, `{"error":"list tag suggestions failed"}`, http.StatusInternalServerError)
				return
			}
			if suggestions == nil {
				suggestions = []store.TagSuggestion{}
			}
			json.NewEncoder(w).Encode(suggestions)
			return
		}

		// Sub-routes: /{id}/accept or /{id}/dismiss
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		parts := strings.Split(path, "/")
		if len(parts) != 2 {
			http.NotFound(w, r)
			return
		}
		id, err := strconv.Atoi(parts[0])
		if err != nil || id <= 0 {
			http.Error(w, `{"error":"invalid suggestion id"}`, http.StatusBadRequest)
			return
		}
		action := parts[1]

		switch action {
		case "accept":
			// Resolve to 'accepted' and also create a RepoTag.
			// First load the suggestion so we know which repo+tag to attach.
			suggestions, err := store.ListTagSuggestions(db, "")
			if err != nil {
				http.Error(w, `{"error":"load suggestion failed"}`, http.StatusInternalServerError)
				return
			}
			var found *store.TagSuggestion
			for i := range suggestions {
				if suggestions[i].ID == id {
					found = &suggestions[i]
					break
				}
			}
			if found == nil {
				http.Error(w, `{"error":"suggestion not found"}`, http.StatusNotFound)
				return
			}
			// Resolve first.
			if err := store.ResolveTagSuggestion(db, id, "accepted", "operator"); err != nil {
				log.Printf("handleTagSuggestions accept: %v", err)
				http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
				return
			}
			// Ensure the Tag exists (create if missing, ignore if already exists).
			_ = store.CreateTag(db, found.Tag, "", "operator")
			// Attach the tag to the repo.
			if err := store.AddRepoTag(db, found.RepoName, found.Tag, "operator", "suggestion"); err != nil {
				log.Printf("handleTagSuggestions accept AddRepoTag: %v", err)
				// Non-fatal: the suggestion is already accepted; log and continue.
			}
			store.LogAudit(db, "dashboard", "accept-tag-suggestion", id,
				fmt.Sprintf("accepted tag suggestion #%d (%s → %s)", id, found.Tag, found.RepoName))
			fmt.Fprintf(w, `{"ok":true,"id":%d}`, id)

		case "dismiss":
			if err := store.ResolveTagSuggestion(db, id, "dismissed", "operator"); err != nil {
				if strings.Contains(err.Error(), "not found") {
					http.Error(w, `{"error":"suggestion not found"}`, http.StatusNotFound)
					return
				}
				log.Printf("handleTagSuggestions dismiss: %v", err)
				http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
				return
			}
			store.LogAudit(db, "dashboard", "dismiss-tag-suggestion", id,
				fmt.Sprintf("dismissed tag suggestion #%d", id))
			fmt.Fprintf(w, `{"ok":true,"id":%d}`, id)

		default:
			http.NotFound(w, r)
		}
	}
}

// ── Rules ─────────────────────────────────────────────────────────────────────

// handleRules serves:
//
//	GET  /api/rules?repo={name}  — resolved rules for the named repo, or all active rules
//	POST /api/rules/{key}/upgrade-scope  — body: {"to_scope":"senate:*"}, updates agent_scope
func handleRules(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)

		// Dispatch on sub-path.
		path := strings.TrimPrefix(r.URL.Path, "/api/rules")
		path = strings.Trim(path, "/")

		if path == "" {
			// GET /api/rules[?repo=...]
			if r.Method != http.MethodGet {
				http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
				return
			}
			repoName := r.URL.Query().Get("repo")
			var rules []store.FleetRulesRow
			var err error
			if repoName != "" {
				rules, err = store.ResolveRulesForRepo(db, repoName)
			} else {
				rules, err = listAllActiveFleetRules(db)
			}
			if err != nil {
				log.Printf("handleRules GET: %v", err)
				http.Error(w, `{"error":"list rules failed"}`, http.StatusInternalServerError)
				return
			}
			if rules == nil {
				rules = []store.FleetRulesRow{}
			}
			json.NewEncoder(w).Encode(rules)
			return
		}

		// Sub-routes: /{key}/upgrade-scope
		parts := strings.Split(path, "/")
		if len(parts) == 2 && parts[1] == "upgrade-scope" && r.Method == http.MethodPost {
			ruleKey := parts[0]
			var body struct {
				ToScope string `json:"to_scope"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				if writeBodyReadError(w, err) {
					return
				}
			}
			if strings.TrimSpace(body.ToScope) == "" {
				http.Error(w, `{"error":"to_scope is required"}`, http.StatusBadRequest)
				return
			}
			// Validate scope format.
			if err := validateAgentScope(body.ToScope); err != nil {
				http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
				return
			}
			res, err := db.Exec(
				`UPDATE FleetRules SET agent_scope = ? WHERE rule_key = ? AND active_until = ''`,
				body.ToScope, ruleKey,
			)
			if err != nil {
				log.Printf("handleRules upgrade-scope: %v", err)
				http.Error(w, `{"error":"update failed"}`, http.StatusInternalServerError)
				return
			}
			n, _ := res.RowsAffected()
			if n == 0 {
				http.Error(w, `{"error":"rule not found or inactive"}`, http.StatusNotFound)
				return
			}
			store.LogAudit(db, "dashboard", "upgrade-rule-scope", 0,
				fmt.Sprintf("set rule %q agent_scope to %q", ruleKey, body.ToScope))
			fmt.Fprintf(w, `{"ok":true,"rule_key":%q,"agent_scope":%q}`, ruleKey, body.ToScope)
			return
		}

		http.NotFound(w, r)
	}
}

// listAllActiveFleetRules returns all FleetRules where active_until = ''.
func listAllActiveFleetRules(db *sql.DB) ([]store.FleetRulesRow, error) {
	rows, err := db.Query(
		`SELECT id, rule_key, IFNULL(category,''), agent_scope,
		        render_to, IFNULL(enforced_by,''), content,
		        IFNULL(content_hash,''), version,
		        IFNULL(active_from,''), IFNULL(active_until,''),
		        IFNULL(promoted_by_experiment_id,0),
		        IFNULL(created_by,''), IFNULL(created_at,'')
		   FROM FleetRules
		  WHERE active_until = ''
		  ORDER BY rule_key`,
	)
	if err != nil {
		return nil, fmt.Errorf("listAllActiveFleetRules: %w", err)
	}
	defer rows.Close()

	var result []store.FleetRulesRow
	for rows.Next() {
		var r store.FleetRulesRow
		if err := rows.Scan(
			&r.ID, &r.RuleKey, &r.Category, &r.AgentScope,
			&r.RenderTo, &r.EnforcedBy, &r.Content,
			&r.ContentHash, &r.Version,
			&r.ActiveFrom, &r.ActiveUntil,
			&r.PromotedByExperimentID,
			&r.CreatedBy, &r.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("listAllActiveFleetRules scan: %w", err)
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// validateAgentScope checks that a proposed agent_scope value has one of
// the accepted forms: "senate:*", "senate:<repo>", "senate:tag:<name>".
func validateAgentScope(scope string) error {
	if scope == "senate:*" {
		return nil
	}
	if strings.HasPrefix(scope, "senate:tag:") {
		tag := strings.TrimPrefix(scope, "senate:tag:")
		if strings.TrimSpace(tag) == "" {
			return fmt.Errorf("scope senate:tag: requires a non-empty tag name")
		}
		return nil
	}
	if strings.HasPrefix(scope, "senate:") {
		repo := strings.TrimPrefix(scope, "senate:")
		if strings.TrimSpace(repo) == "" {
			return fmt.Errorf("scope senate: requires a non-empty repo name")
		}
		return nil
	}
	return fmt.Errorf("scope must be senate:*, senate:<repo>, or senate:tag:<name>; got %q", scope)
}
