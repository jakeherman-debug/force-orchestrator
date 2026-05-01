// D3 P6A.14 — Operator attention tags API.
package dashboard

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"force-orchestrator/internal/store"
)

func handleAttentionList(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		op := r.URL.Query().Get("operator")
		if op == "" {
			op = "default@operator"
		}
		tags, err := store.ListAttentionTags(r.Context(), db, op)
		if err != nil {
			http.Error(w, `{"error":"list failed"}`, http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"tags": tags})
	}
}

func handleAttentionUpsert(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		if r.Method != http.MethodPut {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		// /api/attention/<kind>/<id>
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/attention/"), "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			http.Error(w, `{"error":"path: /api/attention/<kind>/<id>"}`, http.StatusBadRequest)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, `{"error":"body read"}`, http.StatusRequestEntityTooLarge)
			return
		}
		var in struct {
			OperatorEmail  string `json:"operator_email"`
			AttentionLevel string `json:"attention_level"`
			Rationale      string `json:"rationale"`
		}
		if err := json.Unmarshal(body, &in); err != nil {
			http.Error(w, `{"error":"bad JSON"}`, http.StatusBadRequest)
			return
		}
		if in.OperatorEmail == "" {
			in.OperatorEmail = "default@operator"
		}
		err = store.SetAttentionTag(r.Context(), db, store.AttentionTag{
			OperatorEmail:  in.OperatorEmail,
			TargetKind:     parts[0],
			TargetID:       parts[1],
			AttentionLevel: in.AttentionLevel,
			Rationale:      in.Rationale,
		})
		if err != nil {
			http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
