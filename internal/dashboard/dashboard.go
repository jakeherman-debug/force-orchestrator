package dashboard

import (
	"bufio"
	"context"
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
// Security posture — see CLAUDE.md "Dashboard invariants":
//   - binds 127.0.0.1 only (loopback-gated via loopbackBindAddr);
//   - every response carries the security header set stamped by
//     securityMiddleware (CSP, X-Frame-Options, etc.);
//   - every mutating request (POST/PUT/PATCH/DELETE) is Origin-gated and
//     body-capped at 256 KB before the handler sees it.
func RunDashboard(db *sql.DB, port int) {
	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		fmt.Fprintf(os.Stderr, "dashboard: failed to load static assets: %v\n", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()

	// ── API ──────────────────────────────────────────────────────────────────
	mux.HandleFunc("/api/status", handleStatus(db))
	mux.HandleFunc("/api/stats", handleStats(db))
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
	mux.HandleFunc("/api/repos/", handleReposSubroutes(db))
	mux.HandleFunc("/api/mail", handleMailList(db))
	mux.HandleFunc("/api/mail/", handleMailSubroutes(db))
	mux.HandleFunc("/api/add", handleAdd(db))
	mux.HandleFunc("/api/memories", handleMemories(db))
	mux.HandleFunc("/api/memories/", handleMemories(db))
	mux.HandleFunc("/api/events", handleHolonetStream("holonet.jsonl"))
	mux.HandleFunc("/api/fleet-log", handleFleetLogStream("fleet.log"))
	mux.HandleFunc("/api/dogs", handleDogsList(db))
	mux.HandleFunc("/api/dogs/", handleDogsRun(db))
	mux.HandleFunc("/api/pr-comments/", handlePRCommentsSubroutes(db))
	// D2 T1-2 — per-agent rolling-window prompt-byte budget view.
	mux.HandleFunc("/api/prompt-bytes", handlePromptBytes(db))
	// D3 Phase 2 — experiments + holdout views. Phase 6 rebuilds the
	// dashboard around Pulse / Briefing / Reflection and absorbs
	// these endpoints; for now they ship in the current shape so
	// operators can see authored experiments through the lifecycle.
	mux.HandleFunc("/api/experiments", handleExperimentsList(db))
	mux.HandleFunc("/api/experiments/", handleExperimentsSubroutes(db))
	mux.HandleFunc("/api/fleet-progress", handleFleetProgress(db))
	// D3 Phase 3 — EC ratification surface (operator approves /
	// rejects PromotionProposals). Both Librarian-emitted candidates
	// (kind='candidate') and EC-emitted promotions (kind='promote')
	// flow through the same handlers; operator email is required on
	// every mutation. handlers_ec.go enforces rejection_rationale
	// >= 20 chars when rejection_action != 'leave_as_is' (concern #7).
	mux.HandleFunc("/api/ec/proposals", handleECProposalsList(db))
	mux.HandleFunc("/api/ec/proposals/", handleECProposalsSubroutes(db))
	// D3 Phase 3 — cross-layer disagreement rates (Captain → Council, etc.).
	// Reads the latest DisagreementPairs row per pair × window combination;
	// the dog (dogDisagreementTracker) writes; this endpoint reads.
	mux.HandleFunc("/api/disagreement-rates", handleDisagreementRates(db))
	mux.HandleFunc("/healthz", handleHealthz)

	// ── D3 P6A.14 — Operator attention tags API.
	mux.HandleFunc("/api/attention", handleAttentionList(db))
	mux.HandleFunc("/api/attention/", handleAttentionUpsert(db))

	// ── D3 P6A.13 — Cooldown API.
	mux.HandleFunc("/api/cooldown", handleCooldownList(db))
	mux.HandleFunc("/api/cooldown/", handleCooldownAction(db))

	// ── D3 P6A.10 — Briefing API.
	mux.HandleFunc("/api/briefing/queue", handleBriefingQueue(db))
	mux.HandleFunc("/api/briefing/decision/", handleBriefingDecision(db))
	mux.HandleFunc("/api/briefing/decide", handleBriefingDecide(db))

	// ── D3 P6A.11 — Briefing reject (counter-proposal forcing).
	mux.HandleFunc("/api/briefing/reject", handleBriefingReject(db))

	// ── D3 P6A.9 — Cinematic on detected sleep wake.
	mux.HandleFunc("/api/pulse/cinematic", handlePulseCinematic(db))

	// ── D3 P6A.7 — Pulse narrative panel API (read-side; renderer is in
	// internal/agents/narrative_renderer.go and is spawned by the daemon).
	mux.HandleFunc("/api/pulse/narrative", handlePulseNarrative(db))

	// ── D3 P6A.8 — Pulse fleet panel snapshot.
	mux.HandleFunc("/api/pulse/snapshot", handlePulseSnapshot(db))

	// ── D3 P6A.2 — Dashboard heartbeat + health endpoint.
	// /api/dashboard/health surfaces the most recent heartbeat row so the
	// SPA can show a yellow banner if the dashboard process has been
	// silently restarting. The CLI command `force dashboard status` reads
	// the same row from outside the dashboard process.
	mux.HandleFunc("/api/dashboard/health", handleDashboardHealth(db))

	// ── D3 P6A.4 — Operator notification budget config endpoints.
	mux.HandleFunc("/api/notifications/budgets", handleNotificationBudgets(db))
	mux.HandleFunc("/api/notifications/budgets/", handleNotificationBudgetUpsert(db))

	// ── D3 P6A.5 — OperatorSessionState (resume-where-you-left-off).
	mux.HandleFunc("/api/session/state", handleSessionState(db))

	// ── D3 P6A.6 — Trust dials API.
	mux.HandleFunc("/api/trust-dials", handleTrustDials(db))
	mux.HandleFunc("/api/trust-dials/", handleTrustDialUpsert(db))

	// ── D3 P6B.12 — Reflection: fleet learning panel (Sunday-night
	// auto-render via dog `learning-panel-render`; "Refresh now"
	// trigger via POST). Read endpoint returns the most recent row.
	mux.HandleFunc("/api/reflection/learning", handleReflectionLearning(db))
	mux.HandleFunc("/api/reflection/learning/", handleReflectionLearning(db))

	// ── D3 P6A.1 — Three-surface IA. Top-level navigation is capped at three
	// surfaces forever: Pulse / Briefing / Reflection. Each handler emits a
	// thin HTML shell that loads the SPA at the matching hash fragment.
	// Subsequent 6A tasks fill in the surface-specific rendering.
	mux.HandleFunc("/pulse", handlePulsePage(db))
	mux.HandleFunc("/briefing", handleBriefingPage(db))
	mux.HandleFunc("/reflection", handleReflectionPage(db))

	// ── Static assets + SPA fallback ─────────────────────────────────────────
	mux.Handle("/", http.FileServer(http.FS(sub)))

	// Loopback-only bind (AUDIT-001). The dashboard has no auth; exposing it
	// on 0.0.0.0 was full fleet takeover from any LAN peer. If remote access
	// is ever needed, the correct path is an SSH tunnel, not changing this bind.
	// See CLAUDE.md "Dashboard invariants" before relaxing.
	addr := loopbackBindAddr(port)
	fmt.Printf("Fleet Command Center → http://%s\n", addr)
	fmt.Println("Press Ctrl+C to stop.")

	// D3 P6A.2 — kick off the heartbeat goroutine. context.Background() is
	// the right scope here: RunDashboard blocks on ListenAndServe and only
	// returns on hard shutdown (os.Exit). When ctx threading lands across
	// the daemon boundary, this swaps for the real ctx.
	StartHeartbeat(context.Background(), db, addr)

	// Wrap mux in the security middleware stack: CSP headers, Origin allow-list
	// on mutations, 256 KB body cap. Applied globally so no future handler can
	// opt out by accident.
	handler := securityMiddleware(port, mux)
	if err := http.ListenAndServe(addr, handler); err != nil {
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
// AUDIT-053: the wildcard CORS header was dropped — same-origin fetches don't
// need it, and any other origin has no business reading the fleet log stream.
func handleHolonetStream(path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
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
		// AUDIT-053: wildcard CORS dropped. fleet.log can contain gh-auth
		// stderr (token prefixes) and Claude stdout (env echoes) — no other
		// origin should be able to EventSource this.
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
