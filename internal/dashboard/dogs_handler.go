package dashboard

import (
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"force-orchestrator/internal/agents"
	"force-orchestrator/internal/clients/codeartifact"
	"force-orchestrator/internal/clients/librarian"
)

// dogInfo is the row returned to the dashboard dogs list.
type dogInfo struct {
	Name     string `json:"name"`
	RunCount int    `json:"run_count"`
	LastRun  string `json:"last_run"`
	NextRun  string `json:"next_run"`
}

// handleDogsList returns the known dogs and their last-run / next-run times.
func handleDogsList(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		dogs := agents.ListDogs(db)
		out := make([]dogInfo, 0, len(dogs))
		for _, d := range dogs {
			out = append(out, dogInfo{Name: d.Name, RunCount: d.RunCount, LastRun: d.LastRun, NextRun: d.NextRun})
		}
		json.NewEncoder(w).Encode(map[string]any{"dogs": out})
	}
}

// handleDogsRun force-runs a dog by name. Expects POST /api/dogs/<name>/run.
// Body is ignored. Runs synchronously — request returns when the dog finishes.
func handleDogsRun(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		// Expected path: /api/dogs/<name>/run
		path := strings.TrimPrefix(r.URL.Path, "/api/dogs/")
		parts := strings.Split(path, "/")
		if len(parts) != 2 || parts[1] != "run" {
			http.Error(w, "expected /api/dogs/<name>/run", http.StatusNotFound)
			return
		}
		name := parts[0]
		valid := false
		for _, v := range agents.DogNames() {
			if v == name {
				valid = true
				break
			}
		}
		if !valid {
			http.Error(w, "unknown dog: "+name, http.StatusNotFound)
			return
		}

		// Capture the dog's log output in a buffer so we can return it in the response.
		var buf strings.Builder
		logger := log.New(&buf, "["+name+"] ", log.LstdFlags)
		// D0-B: construct an in-process librarian.Client for this one-shot
		// dog invocation. The dashboard is single-user / single-tenant, so
		// short-lived clients are fine — they share the same *sql.DB pool.
		libClient := librarian.NewInProcess(db)
		// D5 Phase 4 (slice α): construct the CodeArtifact client too.
		// On constructor failure (no AWS config) keep nil — the supply-*
		// dogs detect nil and log/skip rather than crash, so dashboard
		// invocations of unrelated dogs still succeed.
		caClient, caErr := codeartifact.NewInProcess(r.Context(), db)
		if caErr != nil {
			caClient = nil
		}
		runErr := agents.RunDogByName(r.Context(), db, name, libClient, caClient, nil, nil, logger)

		resp := map[string]any{
			"dog":    name,
			"output": buf.String(),
		}
		if runErr != nil {
			resp["error"] = runErr.Error()
			w.WriteHeader(http.StatusInternalServerError)
		}
		json.NewEncoder(w).Encode(resp)
	}
}
