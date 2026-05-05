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
	// D3 fix-loop-1 / γ3 — spec deprecation flow (concern #9, exit 14d).
	// Operator-only endpoint for moving an AT or EC out of the active
	// verification spec into the deprecated[] archive. Pattern P21
	// (slice α) walks LLM proposal schemas to assert no agent-internal
	// path can synthesise a "remove" intent on AT references; this
	// handler is the only legitimate write path.
	mux.HandleFunc("/api/convoy/", handleSpecDeprecation(db))
	mux.HandleFunc("/api/agents", handleAgents(db))
	mux.HandleFunc("/api/repos", handleRepos(db))
	mux.HandleFunc("/api/repos/", handleReposSubroutes(db))
	mux.HandleFunc("/api/mail", handleMailList(db))
	mux.HandleFunc("/api/mail/", handleMailSubroutes(db))
	mux.HandleFunc("/api/add", handleAdd(db))
	mux.HandleFunc("/api/memories", handleMemories(db))
	mux.HandleFunc("/api/memories/", handleMemories(db))
	// D4 Phase 0 — Librarian conflict tickets. Operators inspect open
	// contradictions and resolve with a note; the resolve transition
	// stamps resolved_at + resolution_note via store.ResolveConflictTicket.
	mux.HandleFunc("/api/conflicts/tickets", handleConflictsTickets(db))
	mux.HandleFunc("/api/conflicts/tickets/", handleConflictsTickets(db))
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
	// D3 fix-loop-1 β2 — ProposedFeatures operator endpoints
	// (concern #10 / exit criterion 14). List / per-feature read /
	// suppress / score-override / promote. Every mutating handler
	// requires operator_email and writes to AuditLog.
	mux.HandleFunc("/api/proposed-features", handleProposedFeaturesList(db))
	mux.HandleFunc("/api/proposed-features/", handleProposedFeaturesSubroutes(db))
	// JIRA-from-UI — POST /api/feature/from-jira. Operator-routed entry
	// point that calls agents.QueueFeatureFromJira (the reusable core
	// extracted from cmd/force/task_cmds.go cmdAddJira). See
	// handlers_feature_from_jira.go for validation + response shape.
	mux.HandleFunc("/api/feature/from-jira", handleFeatureFromJira(db))
	// D8 T2 — per-Feature blast-radius surface. GET only; the writer is
	// the Chancellor blast-radius post-process. Returns the canonical
	// {modified_symbols, affected_consumer_repos, auto_included_tasks}
	// shape with empty arrays for Features that have no blast-radius.
	mux.HandleFunc("/api/features/", handleFeatureBlastRadius(db))
	// ── D4 fix-loop-1 α — Dashboard views for D4 entities (exit criterion 5).
	// Four operator surfaces back the BoS / ISB / Senate stack:
	//   1. Security findings list + per-finding resolve (BoS + ISB rows).
	//   2. Per-rule precision metrics (firings, TP/FP, ramp status).
	//   3. Override-audit log (disposition='overridden' findings).
	//   4. Senate review log (chambers + per-feature reviews).
	mux.HandleFunc("/api/security-findings", handleSecurityFindings(db))
	mux.HandleFunc("/api/security-findings/", handleSecurityFindingsSubroutes(db))
	// ── D9 Phase 1 — Architecture Health endpoints. /api/arch-health/latest
	// returns the most recent ArchHealthAggregates view; /api/arch-health/<YYYY-MM>
	// returns a specific month; /api/arch-health/months returns the distinct
	// month list for a UI picker. The dog `architecture-health-report` is the
	// only writer.
	mux.HandleFunc("/api/arch-health", handleArchHealthRoot(db))
	mux.HandleFunc("/api/arch-health/", handleArchHealthRoot(db))
	mux.HandleFunc("/api/rule-metrics", handleRuleMetrics(db))
	mux.HandleFunc("/api/override-audit", handleOverrideAudit(db))
	mux.HandleFunc("/api/senate/chambers", handleSenateChambers(db))
	mux.HandleFunc("/api/senate/reviews", handleSenateReviews(db))
	mux.HandleFunc("/api/senate/reviews/", handleSenateReviewsSubroutes(db))

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

	// ── D11 Phase 2 — Notifications dashboard tab. Reads the YAML
	// catalog, the resolution-chain SystemConfig keys, and surfaces the
	// preset / DND / per-category override controls. All state mutations
	// route through internal/notify SystemConfig keys (notify.SetDND for
	// DND, store.SetConfig for everything else); none of this fires
	// dispatches, so Pattern P-NotificationDispatch is unaffected.
	mux.HandleFunc("/api/notifications/catalog", handleNotificationsCatalog(db))
	mux.HandleFunc("/api/notifications/state", handleNotificationsState(db))
	mux.HandleFunc("/api/notifications/preset", handleNotificationsPreset(db))
	mux.HandleFunc("/api/notifications/preset/save", handleNotificationsPresetSave(db))
	mux.HandleFunc("/api/notifications/dnd", handleNotificationsDND(db))
	mux.HandleFunc("/api/notifications/dnd/clear", handleNotificationsDNDClear(db))
	mux.HandleFunc("/api/notifications/category/", handleNotificationsCategory(db))

	// ── D11 Phase 3 — Dashboard personalization (substrate sub-task A).
	// GET-only read endpoint that composes YAML defaults
	// (config/dashboard.yaml, parsed at daemon startup) with SystemConfig
	// per-operator overrides. Sub-tasks B (tab + display) and C (saved
	// filters) add the corresponding POST endpoints.
	mux.HandleFunc("/api/dashboard/config", handleDashboardConfig(db))

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

	// ── D3 P6B.3 — Drill convoy view: unified event timeline + per-task
	// spend rollup. /api/drill/convoy/<id> for events;
	// /api/drill/convoy/<id>/spend for the cost breakdown.
	mux.HandleFunc("/api/drill/convoy/", handleDrillConvoy(db))
	// ── D3 P6B.4 — Drill task view: same shape, scoped to one task.
	mux.HandleFunc("/api/drill/task/", handleDrillTask(db))
	// ── D3 P6B.5 — Drill event view: full body for one event by kind+id.
	mux.HandleFunc("/api/drill/event/", handleDrillEvent(db))
	// ── D3 P6B.6 — Drill free-text search via sqlite_fts5.
	mux.HandleFunc("/api/drill/search", handleDrillSearch(db))
	// ── D3 P6B.7 — Drill replay: re-run historical decision under
	// current prompt version. Pure-read on live state.
	mux.HandleFunc("/api/drill/replay/", handleDrillReplay(db))
	// ── D3 P6B.8 — Operator event annotations.
	mux.HandleFunc("/api/annotations", handleAnnotationsList(db))
	mux.HandleFunc("/api/annotations/", handleAnnotationsItem(db))

	// ── D3 P6B.10 — Ask `/` shortcut. POST {question}; the agent
	// has NO write tools (Pattern P-AskNoWriteTools enforces).
	mux.HandleFunc("/api/ask", handleAsk(db))

	// ── D3 P6B.11 — Reflection calibration scoreboard.
	mux.HandleFunc("/api/reflection/calibration", handleCalibration(db))

	// ── D3 P6B.13 — Reflection 5-min retro generator.
	mux.HandleFunc("/api/reflection/retro/generate", handleRetroGenerate(db))
	mux.HandleFunc("/api/reflection/retro/save", handleRetroSave(db))

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
