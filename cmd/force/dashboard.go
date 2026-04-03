package main

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"

	"force-orchestrator/internal/agents"
	"force-orchestrator/internal/store"
)


// DashboardStatus is the payload for GET /api/status
type DashboardStatus struct {
	Timestamp       string         `json:"timestamp"`
	DaemonRunning   bool           `json:"daemon_running"`
	DaemonPID       int            `json:"daemon_pid,omitempty"`
	Estopped        bool           `json:"estopped"`
	Tasks           map[string]int `json:"tasks"`
	OpenEscalations int            `json:"open_escalations"`
	HighEscalations int            `json:"high_escalations"`
	ActiveConvoys   int            `json:"active_convoys"`
	UnreadMail      int            `json:"unread_mail"`
}

// DashboardTask is one row in GET /api/tasks
type DashboardTask struct {
	ID         int    `json:"id"`
	Type       string `json:"type"`
	Status     string `json:"status"`
	Repo       string `json:"repo"`
	Owner      string `json:"owner"`
	RetryCount int    `json:"retry_count"`
	ConvoyID   int    `json:"convoy_id"`
	Payload    string `json:"payload"`
	ErrorLog   string `json:"error_log,omitempty"`
}

func handleStatus(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		s := DashboardStatus{
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Estopped:  agents.IsEstopped(db),
			Tasks:     map[string]int{},
		}

		rows, _ := db.Query(`SELECT status, COUNT(*) FROM BountyBoard GROUP BY status`)
		if rows != nil {
			for rows.Next() {
				var status string
				var n int
				rows.Scan(&status, &n)
				s.Tasks[status] = n
			}
			rows.Close()
		}

		db.QueryRow(`SELECT COUNT(*) FROM Escalations WHERE status = 'Open'`).Scan(&s.OpenEscalations)
		db.QueryRow(`SELECT COUNT(*) FROM Escalations WHERE status = 'Open' AND severity = 'HIGH'`).Scan(&s.HighEscalations)
		db.QueryRow(`SELECT COUNT(*) FROM Convoys WHERE status = 'Active'`).Scan(&s.ActiveConvoys)
		unread, _ := store.MailStats(db, "")
		s.UnreadMail = unread

		if pidBytes, err := os.ReadFile("fleet.pid"); err == nil {
			var pid int
			fmt.Sscanf(strings.TrimSpace(string(pidBytes)), "%d", &pid)
			if pid > 0 {
				if proc, err := os.FindProcess(pid); err == nil {
					if proc.Signal(syscall.Signal(0)) == nil {
						s.DaemonRunning = true
						s.DaemonPID = pid
					}
				}
			}
		}

		json.NewEncoder(w).Encode(s)
	}
}

func handleTasks(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		statusFilter := r.URL.Query().Get("status")
		query := `SELECT id, type, status, target_repo, owner, retry_count, convoy_id, payload, IFNULL(error_log,'')
			FROM BountyBoard`
		args := []any{}
		if statusFilter != "" {
			query += ` WHERE status = ?`
			args = append(args, statusFilter)
		}
		query += ` ORDER BY id DESC LIMIT 200`

		rows, err := db.Query(query, args...)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer rows.Close()

		var tasks []DashboardTask
		for rows.Next() {
			var t DashboardTask
			rows.Scan(&t.ID, &t.Type, &t.Status, &t.Repo, &t.Owner, &t.RetryCount,
				&t.ConvoyID, &t.Payload, &t.ErrorLog)
			if len(t.Payload) > 300 {
				t.Payload = t.Payload[:300] + "…"
			}
			tasks = append(tasks, t)
		}
		if tasks == nil {
			tasks = []DashboardTask{}
		}
		json.NewEncoder(w).Encode(tasks)
	}
}

// handleEvents streams holonet.jsonl as server-sent events.
// Handles log rotation: when the file is replaced, the stream reopens it.
func handleEvents(logPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		openLog := func() (*os.File, os.FileInfo) {
			f, err := os.Open(logPath)
			if err != nil {
				return nil, nil
			}
			f.Seek(0, 2) // start at end — only stream new events
			fi, _ := f.Stat()
			return f, fi
		}

		f, fi := openLog()
		if f == nil {
			fmt.Fprintf(w, "data: {\"error\":\"holonet.jsonl not found\"}\n\n")
			return
		}
		defer f.Close()

		flusher, ok := w.(http.Flusher)
		scanner := bufio.NewScanner(f)
		for {
			select {
			case <-r.Context().Done():
				return
			default:
			}
			if scanner.Scan() {
				line := scanner.Text()
				if line != "" {
					fmt.Fprintf(w, "data: %s\n\n", line)
					if ok {
						flusher.Flush()
					}
				}
			} else {
				time.Sleep(500 * time.Millisecond)
				// Check if the file was rotated (inode changed or file is now smaller)
				if newFI, statErr := os.Stat(logPath); statErr == nil && fi != nil {
					if !os.SameFile(fi, newFI) {
						// Rotation detected — reopen and continue from the new file's start
						f.Close()
						if newF, newFInfo := openLog(); newF != nil {
							f = newF
							fi = newFInfo
						}
					}
				}
				scanner = bufio.NewScanner(f)
			}
		}
	}
}

func handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"status":"ok","ts":%d}`, time.Now().Unix())
}

func handleEscalationAck(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Parse id from path: /api/escalations/{id}/ack
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) != 4 || parts[3] != "ack" {
			http.NotFound(w, r)
			return
		}
		var id int
		fmt.Sscanf(parts[2], "%d", &id)
		if id <= 0 {
			http.Error(w, "invalid id", http.StatusBadRequest)
			return
		}
		agents.AckEscalation(db, id)
		store.LogAudit(db, "dashboard", "ack-escalation", id, "acknowledged via dashboard")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ok":true,"id":%d}`, id)
	}
}

func handleTaskRetry(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Parse id from path: /api/tasks/{id}/retry
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) != 4 || parts[3] != "retry" {
			http.NotFound(w, r)
			return
		}
		var id int
		fmt.Sscanf(parts[2], "%d", &id)
		if id <= 0 {
			http.Error(w, "invalid id", http.StatusBadRequest)
			return
		}
		var currentStatus string
		db.QueryRow(`SELECT status FROM BountyBoard WHERE id = ?`, id).Scan(&currentStatus)
		if currentStatus == "Failed" || currentStatus == "Escalated" {
			store.ResetTask(db, id)
		}
		store.LogAudit(db, "dashboard", "retry", id, "retried via dashboard")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ok":true,"id":%d}`, id)
	}
}

func handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	fmt.Fprint(w, dashboardHTML)
}

func RunDashboard(db *sql.DB, port int) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", handleStatus(db))
	mux.HandleFunc("/api/tasks", handleTasks(db))
	mux.HandleFunc("/api/events", handleEvents("holonet.jsonl"))
	mux.HandleFunc("/healthz", handleHealthz)
	mux.HandleFunc("/api/escalations/", handleEscalationAck(db))
	mux.HandleFunc("/api/tasks/", handleTaskRetry(db))
	mux.HandleFunc("/", handleRoot)

	addr := fmt.Sprintf(":%d", port)
	fmt.Printf("Fleet Dashboard running at http://localhost%s\n", addr)
	fmt.Println("  /          — status page")
	fmt.Println("  /api/status — JSON status")
	fmt.Println("  /api/tasks  — JSON task list")
	fmt.Println("  /api/events — SSE telemetry stream")
	fmt.Println("Press Ctrl+C to stop.")
	if err := http.ListenAndServe(addr, mux); err != nil {
		fmt.Fprintf(os.Stderr, "Dashboard server error: %v\n", err)
		os.Exit(1)
	}
}

// dashboardHTML is the single-page dashboard served at /.
// Auto-refreshes status every 3 seconds via fetch.
const dashboardHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<title>Galactic Fleet Command</title>
<style>
  body { font-family: monospace; background: #0a0a0a; color: #00ff88; margin: 20px; }
  h1 { color: #ffcc00; }
  h2 { color: #88ccff; margin-top: 24px; }
  .grid { display: flex; gap: 20px; flex-wrap: wrap; }
  .card { background: #111; border: 1px solid #333; padding: 16px; min-width: 160px; border-radius: 4px; }
  .card .val { font-size: 2em; color: #ffcc00; }
  .card .lbl { font-size: 0.8em; color: #888; margin-top: 4px; }
  .warn { color: #ff4444; }
  table { border-collapse: collapse; width: 100%; margin-top: 8px; }
  th { text-align: left; color: #88ccff; border-bottom: 1px solid #333; padding: 4px 8px; }
  td { padding: 4px 8px; border-bottom: 1px solid #1a1a1a; }
  .status-Locked, .status-UnderReview { color: #ffcc00; }
  .status-Completed { color: #00ff88; }
  .status-Failed { color: #ff4444; }
  .status-Escalated { color: #ff8800; }
  .status-Pending { color: #aaaaaa; }
  .status-AwaitingCaptainReview, .status-UnderCaptainReview { color: #ffaa44; }
  .status-AwaitingCouncilReview { color: #88ccff; }
  #estop-banner { display: none; background: #ff0000; color: white; padding: 8px 16px; font-weight: bold; margin-bottom: 16px; }
  #last-update { color: #555; font-size: 0.8em; margin-top: 8px; }
</style>
</head>
<body>
<h1>&#9733; Galactic Fleet Command Center</h1>
<div id="estop-banner">&#9888; E-STOP ACTIVE — All agents halted. Run: force resume</div>
<div id="last-update">Loading...</div>

<h2>System Status</h2>
<div class="grid" id="status-grid"></div>

<h2>Active Tasks</h2>
<table id="task-table">
  <thead><tr><th>ID</th><th>Status</th><th>Type</th><th>Repo</th><th>Owner</th><th>Task</th><th></th></tr></thead>
  <tbody id="task-body"></tbody>
</table>

<script>
async function refresh() {
  try {
    const [statusRes, tasksRes] = await Promise.all([
      fetch('/api/status'), fetch('/api/tasks?status=')
    ]);
    const s = await statusRes.json();
    const tasks = await tasksRes.json();

    document.getElementById('estop-banner').style.display = s.estopped ? 'block' : 'none';
    document.getElementById('last-update').textContent = 'Last updated: ' + new Date(s.timestamp).toLocaleTimeString();

    const t = s.tasks || {};
    const active = (t.Locked||0) + (t.AwaitingCaptainReview||0) + (t.UnderCaptainReview||0) + (t.UnderReview||0) + (t.AwaitingCouncilReview||0);
    const cards = [
      ['Pending',   t.Pending||0,   ''],
      ['Active',    active,          active > 0 ? '' : ''],
      ['Completed', t.Completed||0, ''],
      ['Failed',    t.Failed||0,    t.Failed > 0 ? 'warn' : ''],
      ['Escalated', t.Escalated||0, t.Escalated > 0 ? 'warn' : ''],
      ['Escalations', s.open_escalations, s.high_escalations > 0 ? 'warn' : ''],
      ['Convoys',   s.active_convoys, ''],
      ['Unread Mail', s.unread_mail, s.unread_mail > 0 ? '' : ''],
    ];
    document.getElementById('status-grid').innerHTML = cards.map(([lbl, val, cls]) =>
      '<div class="card"><div class="val ' + cls + '">' + val + '</div><div class="lbl">' + lbl + '</div></div>'
    ).join('');

    const nonFinal = (tasks || []).filter(t => t.status !== 'Completed');
    document.getElementById('task-body').innerHTML = nonFinal.slice(0, 50).map(t => {
      let action = '';
      if (t.status === 'Failed' || t.status === 'Escalated') {
        action = '<button onclick="retryTask(' + t.id + ')" style="font-size:0.8em;cursor:pointer">↺ retry</button>';
      }
      return '<tr><td>' + t.id + '</td>' +
        '<td class="status-' + t.status + '">' + t.status + '</td>' +
        '<td>' + t.type + '</td>' +
        '<td>' + (t.repo||'') + '</td>' +
        '<td>' + (t.owner||'—') + '</td>' +
        '<td>' + (t.payload||'').replace(/\n/g,' ').substring(0,80) + '</td>' +
        '<td>' + action + '</td></tr>';
    }).join('');
  } catch(e) {
    document.getElementById('last-update').textContent = 'Error: ' + e.message;
  }
}
async function retryTask(id) {
  await fetch('/api/tasks/' + id + '/retry', {method:'POST'});
  refresh();
}
refresh();
setInterval(refresh, 3000);
</script>
</body>
</html>`
