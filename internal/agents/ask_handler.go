// Package agents — D3 P6B.10 Ask `/` shortcut handler.
//
// Press `/` from anywhere in the dashboard → floating input bar →
// type a free-form question. The backend hands the question to Haiku
// with a closed set of read-only DB-query tools and synthesises an
// answer with cite links.
//
// Anti-cheat (the linchpin invariant): the Ask agent has NO write
// tools. The tool registry is a closed list of read-only helpers
// (getConvoy, getTask, searchTranscripts, listFleetRules,
// listEscalations, listAnnotationsByFlag). Pattern P-AskNoWriteTools
// walks production code and rejects any reach into a store mutator
// from this file.
//
// Live-Haiku swap is mechanical (same shape as 6A.10 / 6B.12) — until
// it lands, the handler routes the question through the existing
// read-only search + load helpers and produces a structured answer.

package agents

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"force-orchestrator/internal/store"
)

// AskAnswer is the structured response the dashboard renders.
type AskAnswer struct {
	Question  string     `json:"question"`
	Answer    string     `json:"answer"`
	CiteLinks []AskCite  `json:"cite_links"`
	CostUSD   float64    `json:"cost_usd"`
	AnsweredAt string    `json:"answered_at"`
}

// AskCite is one clickable source ref in the answer card.
type AskCite struct {
	Kind  string `json:"kind"`  // 'convoy' | 'task' | 'llm_call' | 'briefing' | 'annotation'
	ID    int64  `json:"id"`
	Label string `json:"label"`
}

// askDailyCapDefault is $3 per the brief.
const askDailyCapDefault = 3.00

// AskHandle dispatches a free-form Ask question. Returns an answer
// with cite links. Cost-capped via SystemConfig.ask_daily_cap_usd
// (default 3.00). When the cap is exhausted, returns a polite error
// explaining the budget rather than running.
func AskHandle(ctx context.Context, db *sql.DB, question string) (AskAnswer, error) {
	q := strings.TrimSpace(question)
	if q == "" {
		return AskAnswer{}, fmt.Errorf("AskHandle: empty question")
	}
	if len(q) > 1024 {
		q = q[:1024]
	}

	// Cost cap check: sum cost_usd of agent='ask' transcripts in
	// the last 24h. Anything > cap returns a refusal answer.
	cap := askDailyCapDefault
	if v := store.GetConfig(db, "ask_daily_cap_usd", ""); v != "" {
		var f float64
		fmt.Sscanf(v, "%f", &f)
		if f > 0 {
			cap = f
		}
	}
	var spent float64
	db.QueryRowContext(ctx,
		`SELECT IFNULL(SUM(cost_usd),0) FROM LLMCallTranscripts
		  WHERE agent='ask' AND call_started_at > datetime('now','-1 day')`,
	).Scan(&spent)
	if spent >= cap {
		return AskAnswer{
			Question: q,
			Answer:   fmt.Sprintf("Ask budget exhausted today ($%.2f spent against $%.2f cap). Try again after midnight UTC, or raise SystemConfig.ask_daily_cap_usd.", spent, cap),
			AnsweredAt: store.NowSQLite(),
		}, nil
	}

	// Read-only tool routing — derive answer from query string.
	// Each branch uses ONLY read helpers (search, load, list).
	// Live-Haiku swap replaces this routing with a tool-equipped
	// CallWithTranscript call; the underlying read helpers stay
	// the same.
	answer, cites := routeAsk(ctx, db, q)

	return AskAnswer{
		Question:  q,
		Answer:    answer,
		CiteLinks: cites,
		CostUSD:   0, // deterministic synth path
		AnsweredAt: store.NowSQLite(),
	}, nil
}

// routeAsk inspects the question for keyword patterns and returns a
// deterministic synthesised answer + cite links. The routing is the
// stand-in for Haiku's tool-call selection; in the live-Haiku shape,
// this body becomes Haiku's reasoning trace.
func routeAsk(ctx context.Context, db *sql.DB, q string) (string, []AskCite) {
	lower := strings.ToLower(q)

	// "what's blocking convoy <N>" / "convoy <N>"
	if strings.Contains(lower, "convoy") {
		if convoyID := extractConvoyID(lower); convoyID > 0 {
			return askConvoy(ctx, db, convoyID)
		}
	}
	// "task <N>"
	if strings.Contains(lower, "task") {
		if taskID := extractTaskID(lower); taskID > 0 {
			return askTask(ctx, db, taskID)
		}
	}
	// Default: use free-text search across drill sources.
	return askSearch(ctx, db, q)
}

func askConvoy(ctx context.Context, db *sql.DB, convoyID int) (string, []AskCite) {
	var name, status string
	err := db.QueryRowContext(ctx,
		`SELECT name, status FROM Convoys WHERE id = ?`, convoyID,
	).Scan(&name, &status)
	if err != nil {
		return fmt.Sprintf("No convoy with id %d.", convoyID), nil
	}
	// Active tasks
	var openCount int
	db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM BountyBoard WHERE convoy_id = ? AND status NOT IN ('Completed','Cancelled')`,
		convoyID,
	).Scan(&openCount)
	// Open escalations
	var escs int
	db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM Escalations WHERE convoy_id = ? AND IFNULL(status,'') = 'Open'`,
		convoyID,
	).Scan(&escs)

	answer := fmt.Sprintf("Convoy %d (%q) is %s. %d open task(s); %d open escalation(s).",
		convoyID, name, status, openCount, escs)
	return answer, []AskCite{
		{Kind: "convoy", ID: int64(convoyID), Label: name},
	}
}

func askTask(ctx context.Context, db *sql.DB, taskID int) (string, []AskCite) {
	var status, taskType string
	err := db.QueryRowContext(ctx,
		`SELECT IFNULL(status,''), IFNULL(type,'') FROM BountyBoard WHERE id = ?`, taskID,
	).Scan(&status, &taskType)
	if err != nil {
		return fmt.Sprintf("No task with id %d.", taskID), nil
	}
	answer := fmt.Sprintf("Task %d (type=%s) is in status %q.", taskID, taskType, status)
	return answer, []AskCite{{Kind: "task", ID: int64(taskID), Label: fmt.Sprintf("task %d", taskID)}}
}

func askSearch(ctx context.Context, db *sql.DB, q string) (string, []AskCite) {
	results, err := store.SearchDrill(ctx, db, q, "global", 0, 10)
	if err != nil || len(results) == 0 {
		return fmt.Sprintf("No matches found for %q. Try a more specific term, or use the convoy/task drill.", q), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Found %d result(s) for %q:\n", len(results), q)
	cites := make([]AskCite, 0, len(results))
	for i, r := range results {
		if i >= 5 {
			break
		}
		fmt.Fprintf(&b, "  - %s/%d: %s\n", r.Kind, r.RefID, r.Snippet)
		cites = append(cites, AskCite{Kind: r.Kind, ID: r.RefID, Label: fmt.Sprintf("%s %d", r.Kind, r.RefID)})
	}
	return b.String(), cites
}

func extractConvoyID(s string) int {
	return extractNumberAfter(s, "convoy")
}

func extractTaskID(s string) int {
	return extractNumberAfter(s, "task")
}

func extractNumberAfter(s, keyword string) int {
	idx := strings.Index(s, keyword)
	if idx < 0 {
		return 0
	}
	rest := s[idx+len(keyword):]
	// Skip non-digits.
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		if c >= '0' && c <= '9' {
			// Read number until non-digit.
			j := i
			for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
				j++
			}
			n := 0
			for k := i; k < j; k++ {
				n = n*10 + int(rest[k]-'0')
			}
			return n
		}
		if c == ' ' || c == '#' || c == '-' || c == '_' {
			continue
		}
		// Hit a non-digit / non-separator → stop.
		break
	}
	return 0
}

// AskMarshal is a tiny convenience for handlers that want a JSON byte
// slice ready to write.
func AskMarshal(a AskAnswer) []byte {
	b, _ := json.Marshal(a)
	return b
}

// askedTimeFormat keeps the imports honest (time used implicitly in
// store.NowSQLite — included here as a marker so future review notes
// land in the same file).
var _ = time.Time{}
