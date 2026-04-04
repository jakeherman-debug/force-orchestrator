package dashboard

import (
	"bufio"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"time"
)

//go:embed static
var staticFiles embed.FS

// RunDashboard starts the HTTP command-center dashboard on the given port.
func RunDashboard(db *sql.DB, port int) {
	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		fmt.Fprintf(os.Stderr, "dashboard: failed to load static assets: %v\n", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()

	// ── API ──────────────────────────────────────────────────────────────────
	mux.HandleFunc("/api/status", handleStatus(db))
	mux.HandleFunc("/api/tasks", handleTasks(db))
	mux.HandleFunc("/api/tasks/", handleTasksSubroutes(db))
	mux.HandleFunc("/api/control/estop", handleEstop(db))
	mux.HandleFunc("/api/control/resume", handleResume(db))
	mux.HandleFunc("/api/escalations", handleEscalationList(db))
	mux.HandleFunc("/api/escalations/", handleEscalationsSubroutes(db))
	mux.HandleFunc("/api/convoys", handleConvoys(db))
	mux.HandleFunc("/api/convoys/", handleConvoysSubroutes(db))
	mux.HandleFunc("/api/agents", handleAgents(db))
	mux.HandleFunc("/api/repos", handleRepos(db))
	mux.HandleFunc("/api/mail", handleMailList(db))
	mux.HandleFunc("/api/mail/", handleMailSubroutes(db))
	mux.HandleFunc("/api/add", handleAdd(db))
	mux.HandleFunc("/api/events", handleHolonetStream("holonet.jsonl"))
	mux.HandleFunc("/api/fleet-log", handleFleetLogStream("fleet.log"))
	mux.HandleFunc("/healthz", handleHealthz)

	// ── Static assets + SPA fallback ─────────────────────────────────────────
	mux.Handle("/", http.FileServer(http.FS(sub)))

	addr := fmt.Sprintf(":%d", port)
	fmt.Printf("Fleet Command Center → http://localhost%s\n", addr)
	fmt.Println("Press Ctrl+C to stop.")
	if err := http.ListenAndServe(addr, mux); err != nil {
		fmt.Fprintf(os.Stderr, "dashboard: %v\n", err)
		os.Exit(1)
	}
}

// ── SSE helpers ───────────────────────────────────────────────────────────────

func openAtEnd(path string) (*os.File, os.FileInfo) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil
	}
	f.Seek(0, 2)
	fi, _ := f.Stat()
	return f, fi
}

func openWithBackfill(path string, backfillBytes int64) (*os.File, os.FileInfo) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil
	}
	fi, _ := f.Stat()
	if fi.Size() > backfillBytes {
		f.Seek(-backfillBytes, 2)
		// discard partial leading line
		bufio.NewScanner(f).Scan()
	}
	return f, fi
}

func sseLoop(w http.ResponseWriter, r *http.Request, f *os.File, fi os.FileInfo, path string, format func(string) string) {
	flusher, canFlush := w.(http.Flusher)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)

	for {
		select {
		case <-r.Context().Done():
			return
		default:
		}
		if scanner.Scan() {
			if line := scanner.Text(); line != "" {
				fmt.Fprintf(w, "data: %s\n\n", format(line))
				if canFlush {
					flusher.Flush()
				}
			}
		} else {
			time.Sleep(500 * time.Millisecond)
			if newFI, statErr := os.Stat(path); statErr == nil && fi != nil {
				if !os.SameFile(fi, newFI) {
					f.Close()
					f, fi = openAtEnd(path)
					if f == nil {
						return
					}
				}
			}
			scanner = bufio.NewScanner(f)
			scanner.Buffer(make([]byte, 256*1024), 256*1024)
		}
	}
}

// handleHolonetStream streams holonet.jsonl as SSE (lines are already JSON).
func handleHolonetStream(path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		f, fi := openAtEnd(path)
		if f == nil {
			fmt.Fprintf(w, "data: {\"error\":\"holonet.jsonl not found\"}\n\n")
			return
		}
		defer f.Close()
		sseLoop(w, r, f, fi, path, func(line string) string { return line })
	}
}

// handleFleetLogStream streams fleet.log as SSE with 32 KB backfill.
// Lines are JSON-encoded strings so the browser can parse them safely.
func handleFleetLogStream(path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		f, fi := openWithBackfill(path, 32*1024)
		if f == nil {
			fmt.Fprintf(w, "data: \"\"\n\n") // empty line so browser knows stream is alive
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			// keep connection open so the client reconnects when the file appears
			<-r.Context().Done()
			return
		}
		defer f.Close()
		sseLoop(w, r, f, fi, path, func(line string) string {
			b, _ := json.Marshal(line)
			return string(b)
		})
	}
}
