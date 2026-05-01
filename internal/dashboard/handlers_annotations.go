// Package dashboard — D3 P6B.8 OperatorEventAnnotations CRUD endpoints.
//
//   - GET    /api/annotations?kind=<>&ref=<>     list for one event
//   - GET    /api/annotations?flag=<>            list by flag
//   - POST   /api/annotations                    create
//   - PUT    /api/annotations/<id>               update
//   - DELETE /api/annotations/<id>               delete
//
// All operator-only writes — Pattern P-AnnotationOperatorOnly enforces
// the corresponding constraint at the store boundary.

package dashboard

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"force-orchestrator/internal/store"
)

func handleAnnotationsList(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Both list shapes are GET-only; the path /api/annotations
		// without a sub-segment routes here.
		if r.Method != http.MethodGet && r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		if r.Method == http.MethodPost {
			handleAnnotationsCreate(db)(w, r)
			return
		}
		flag := r.URL.Query().Get("flag")
		if flag != "" || r.URL.Query().Get("kind") == "" {
			limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
			rows, err := store.ListAnnotationsByFlag(r.Context(), db, flag, limit)
			if err != nil {
				http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
				return
			}
			writeJSON(w, map[string]any{"annotations": rows})
			return
		}
		kind := r.URL.Query().Get("kind")
		ref := r.URL.Query().Get("ref")
		if kind == "" || ref == "" {
			http.Error(w, `{"error":"kind+ref or flag required"}`, http.StatusBadRequest)
			return
		}
		rows, err := store.ListAnnotationsForEvent(r.Context(), db, kind, ref)
		if err != nil {
			http.Error(w, `{"error":"db read failed"}`, http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"annotations": rows})
	}
}

func handleAnnotationsCreate(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			OperatorEmail string `json:"operator_email"`
			EventKind     string `json:"event_kind"`
			EventRef      string `json:"event_ref"`
			NoteText      string `json:"note_text"`
			Flag          string `json:"flag"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			if writeBodyReadError(w, err) {
				return
			}
			http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
			return
		}
		id, err := store.InsertAnnotation(r.Context(), db, store.Annotation{
			OperatorEmail: req.OperatorEmail,
			EventKind:     req.EventKind,
			EventRef:      req.EventRef,
			NoteText:      req.NoteText,
			Flag:          req.Flag,
		})
		if err != nil {
			http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]any{"id": id})
	}
}

// handleAnnotationsItem dispatches PUT/DELETE on /api/annotations/<id>.
func handleAnnotationsItem(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Path: /api/annotations/<id>
		rest := strings.TrimPrefix(r.URL.Path, "/api/annotations/")
		idStr := strings.TrimSuffix(rest, "/")
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil || id <= 0 {
			// /api/annotations/  with no id → list
			if idStr == "" {
				handleAnnotationsList(db)(w, r)
				return
			}
			http.Error(w, `{"error":"invalid id"}`, http.StatusBadRequest)
			return
		}
		switch r.Method {
		case http.MethodPut:
			var req struct {
				OperatorEmail string `json:"operator_email"`
				NoteText      string `json:"note_text"`
				Flag          string `json:"flag"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				if writeBodyReadError(w, err) {
					return
				}
				http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
				return
			}
			if err := store.UpdateAnnotation(r.Context(), db, id, req.OperatorEmail, req.NoteText, req.Flag); err != nil {
				if err == sql.ErrNoRows {
					http.Error(w, `{"error":"not found or not yours"}`, http.StatusNotFound)
					return
				}
				http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
				return
			}
			writeJSON(w, map[string]any{"ok": true})
		case http.MethodDelete:
			operator := r.URL.Query().Get("operator")
			if operator == "" {
				http.Error(w, `{"error":"operator query param required"}`, http.StatusBadRequest)
				return
			}
			if err := store.DeleteAnnotation(r.Context(), db, id, operator); err != nil {
				if err == sql.ErrNoRows {
					http.Error(w, `{"error":"not found or not yours"}`, http.StatusNotFound)
					return
				}
				http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
				return
			}
			writeJSON(w, map[string]any{"ok": true})
		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}
