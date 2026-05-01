// Package dashboard — D3 P6B.10 / 6B.11 / 6B.13 Tier-5 handlers.
//
//   - POST /api/ask                                  Ask `/` shortcut
//   - GET  /api/reflection/calibration               calibration scoreboard
//   - POST /api/reflection/retro/generate            5-min retro draft
//   - POST /api/reflection/retro/save                save draft to disk

package dashboard

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"time"

	"force-orchestrator/internal/agents"
	"force-orchestrator/internal/store"
)

// handleAsk serves POST /api/ask {question, context: {current_route?}}.
func handleAsk(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Question string         `json:"question"`
			Context  map[string]any `json:"context"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			if writeBodyReadError(w, err) {
				return
			}
			http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
			return
		}
		ans, err := agents.AskHandle(r.Context(), db, req.Question)
		if err != nil {
			http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
			return
		}
		writeJSON(w, ans)
	}
}

// handleCalibration serves GET /api/reflection/calibration.
func handleCalibration(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		sb, err := store.LoadCalibrationScoreboard(r.Context(), db)
		if err != nil {
			http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusInternalServerError)
			return
		}
		writeJSON(w, sb)
	}
}

// handleRetroGenerate serves POST /api/reflection/retro/generate.
// Generates the markdown draft + returns it for preview; does NOT
// write to disk. /save is a separate step.
func handleRetroGenerate(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		retro, err := agents.GenerateRetro(r.Context(), db, time.Now())
		if err != nil {
			http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusInternalServerError)
			return
		}
		writeJSON(w, retro)
	}
}

// handleRetroSave serves POST /api/reflection/retro/save.
// Body: { markdown, suggested_path }. Writes file to disk.
func handleRetroSave(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Markdown      string `json:"markdown"`
			SuggestedPath string `json:"suggested_path"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			if writeBodyReadError(w, err) {
				return
			}
			http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
			return
		}
		path, err := agents.SaveRetroDraft(req.SuggestedPath, req.Markdown)
		if err != nil {
			http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]any{"saved_path": path})
	}
}
