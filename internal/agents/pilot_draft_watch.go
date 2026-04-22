package agents

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"force-orchestrator/internal/store"
)

// ── draft-pr-watch dog ──────────────────────────────────────────────────────
//
// Polls every DraftPROpen convoy's per-repo draft PRs, transitioning the
// convoy state machine on what it sees:
//
//   draft PR merged   → convoy=Shipped, cleanup ask-branches, spawn Librarian
//                       memory (success flavour)
//   draft PR closed   → convoy=Abandoned, cleanup ask-branches, spawn Librarian
//                       memory (self-healing-gap flavour)
//   draft PR open     → no action (ship-it-nag handles operator reminders)
//
// Multi-repo convoys: a convoy is Shipped only when ALL per-repo draft PRs are
// merged; it becomes Abandoned when at least one is closed unmerged AND none
// remain open. Mixed states (some merged, one still open) keep DraftPROpen.

// draftPRViewFn is what the dog calls to fetch a PR's state. Swapped by tests.
// Returns (state, merged, err) where state is OPEN|CLOSED|MERGED as reported
// by `gh pr view --json state,merged`.
var draftPRViewFn = defaultDraftPRView

func defaultDraftPRView(cwd, repo string, number int) (string, bool, error) {
	c := newGHClient()
	v, err := c.PRView(cwd, repo, number)
	if err != nil {
		return "", false, err
	}
	return v.State, v.Merged, nil
}

func dogDraftPRWatch(db *sql.DB, logger interface{ Printf(string, ...any) }) error {
	rows, err := db.Query(`SELECT id, name FROM Convoys WHERE status = 'DraftPROpen'`)
	if err != nil {
		return err
	}
	type convoyRef struct {
		id   int
		name string
	}
	var convoys []convoyRef
	for rows.Next() {
		var c convoyRef
		rows.Scan(&c.id, &c.name)
		convoys = append(convoys, c)
	}
	rows.Close()
	if len(convoys) == 0 {
		return nil
	}

	for _, c := range convoys {
		pollConvoyDraftPRs(db, c.id, c.name, logger)
	}
	return nil
}

// pollConvoyDraftPRs handles one convoy's per-repo draft PRs. Split out for
// unit testing without needing to set up many convoys.
func pollConvoyDraftPRs(db *sql.DB, convoyID int, convoyName string, logger interface{ Printf(string, ...any) }) {
	branches := store.ListConvoyAskBranches(db, convoyID)
	if len(branches) == 0 {
		return
	}

	var total, merged, closed, open int
	for _, ab := range branches {
		if ab.DraftPRNumber == 0 {
			continue
		}
		total++
		repo := store.GetRepo(db, ab.Repo)
		var cwd, ghRepo string
		if repo != nil {
			cwd = repo.LocalPath
			ghRepo = deriveGHRepoFromRemoteURL(repo.RemoteURL)
		}
		state, isMerged, err := draftPRViewFn(cwd, ghRepo, ab.DraftPRNumber)
		if err != nil {
			logger.Printf("draft-pr-watch: pr view #%d (%s) failed: %v", ab.DraftPRNumber, ab.Repo, err)
			// Treat transient view failures as "still open" — don't advance state.
			open++
			continue
		}
		switch {
		case isMerged:
			if ab.DraftPRState != "Merged" {
				_ = store.UpdateConvoyAskBranchDraftState(db, ab.ConvoyID, ab.Repo, "Merged")
				logger.Printf("draft-pr-watch: %s draft PR #%d merged", ab.Repo, ab.DraftPRNumber)
			}
			merged++
		case strings.EqualFold(state, "CLOSED"):
			if ab.DraftPRState != "Closed" {
				_ = store.UpdateConvoyAskBranchDraftState(db, ab.ConvoyID, ab.Repo, "Closed")
				logger.Printf("draft-pr-watch: %s draft PR #%d closed without merge", ab.Repo, ab.DraftPRNumber)
			}
			closed++
		default:
			open++
		}
	}

	if total == 0 {
		return
	}
	switch {
	case merged == total:
		transitionConvoyToShipped(db, convoyID, convoyName, logger)
	case closed > 0 && open == 0:
		transitionConvoyToAbandoned(db, convoyID, convoyName, logger)
	}
}

// ── terminal transitions ────────────────────────────────────────────────────

func transitionConvoyToShipped(db *sql.DB, convoyID int, convoyName string, logger interface{ Printf(string, ...any) }) {
	if err := store.SetConvoyStatus(db, convoyID, "Shipped"); err != nil {
		logger.Printf("draft-pr-watch: set Shipped for convoy %d: %v", convoyID, err)
		return
	}
	if _, err := QueueCleanupAskBranch(db, convoyID); err != nil {
		logger.Printf("draft-pr-watch: queue CleanupAskBranch: %v", err)
	}
	queueLibrarianConvoyMemory(db, convoyID, convoyName, "shipped",
		"Convoy shipped via draft PR(s).", logger)

	store.SendMail(db, "draft-pr-watch", "operator",
		fmt.Sprintf("[SHIPPED] Convoy '%s' is live on main", convoyName),
		fmt.Sprintf("Convoy '%s' draft PR(s) merged to main. Ask-branch cleanup has been queued.", convoyName),
		0, store.MailTypeInfo)
	logger.Printf("draft-pr-watch: convoy %d → Shipped, cleanup queued", convoyID)
}

func transitionConvoyToAbandoned(db *sql.DB, convoyID int, convoyName string, logger interface{ Printf(string, ...any) }) {
	if err := store.SetConvoyStatus(db, convoyID, "Abandoned"); err != nil {
		logger.Printf("draft-pr-watch: set Abandoned for convoy %d: %v", convoyID, err)
		return
	}
	if _, err := QueueCleanupAskBranch(db, convoyID); err != nil {
		logger.Printf("draft-pr-watch: queue CleanupAskBranch: %v", err)
	}
	queueLibrarianConvoyMemory(db, convoyID, convoyName, "abandoned",
		"Convoy's draft PR was closed without merging — review for scoping/design issues.",
		logger)

	store.SendMail(db, "draft-pr-watch", "operator",
		fmt.Sprintf("[ABANDONED] Convoy '%s' draft PR closed", convoyName),
		fmt.Sprintf("Convoy '%s' had its draft PR closed without merging. Ask-branches will be cleaned up. Review the convoy for scoping issues before re-queuing similar work.",
			convoyName),
		0, store.MailTypeAlert)
	logger.Printf("draft-pr-watch: convoy %d → Abandoned, cleanup queued", convoyID)
}

// queueLibrarianConvoyMemory writes a fleet-memory entry tagged with the convoy
// outcome. Uses the standard WriteMemory bounty so the existing Librarian loop
// picks it up without modification.
//
// A failure here is non-fatal for the convoy lifecycle (the draft PR has
// already merged/closed) but costs us a memory record, so we log the
// miss so an operator can diagnose pattern issues.
func queueLibrarianConvoyMemory(db *sql.DB, convoyID int, convoyName, outcome, summary string, logger interface{ Printf(string, ...any) }) {
	branches := store.ListConvoyAskBranches(db, convoyID)
	repo := ""
	if len(branches) > 0 {
		repo = branches[0].Repo
	}
	payload := fmt.Sprintf(`{"task":%q,"files":"","feedback":%q,"diff":"","repo":%q}`,
		fmt.Sprintf("[convoy-%s] %s", outcome, convoyName),
		summary,
		repo)
	id := store.AddBounty(db, 0, "WriteMemory", payload)
	if id == 0 {
		logger.Printf("queueLibrarianConvoyMemory: failed to queue WriteMemory for convoy %d (%s) — memory lost",
			convoyID, outcome)
	}
}

// ── ship-it-nag dog ─────────────────────────────────────────────────────────
//
// For each DraftPROpen convoy, send the operator a reminder when the draft PR
// has been sitting unshipped for 24h, 72h, or 1 week. One mail per threshold
// crossing, not on every dog tick.

const (
	shipItNag24h = 24 * time.Hour
	shipItNag72h = 72 * time.Hour
	shipItNag1wk = 7 * 24 * time.Hour
)

func dogShipItNag(db *sql.DB, logger interface{ Printf(string, ...any) }) error {
	rows, err := db.Query(`SELECT id, name FROM Convoys WHERE status = 'DraftPROpen'`)
	if err != nil {
		return err
	}
	type entry struct {
		id   int
		name string
	}
	var convoys []entry
	for rows.Next() {
		var e entry
		rows.Scan(&e.id, &e.name)
		convoys = append(convoys, e)
	}
	rows.Close()

	for _, c := range convoys {
		// earliest draft_pr's created_at across all the convoy's branches
		var earliest string
		db.QueryRow(`SELECT MIN(created_at) FROM ConvoyAskBranches
			WHERE convoy_id = ? AND draft_pr_number > 0`, c.id).Scan(&earliest)
		if earliest == "" {
			continue
		}
		t, perr := time.Parse("2006-01-02 15:04:05", earliest)
		if perr != nil {
			continue
		}
		age := time.Since(t)

		var threshold time.Duration
		var key string
		switch {
		case age >= shipItNag1wk:
			threshold = shipItNag1wk
			key = fmt.Sprintf("nag:convoy:%d:1wk", c.id)
		case age >= shipItNag72h:
			threshold = shipItNag72h
			key = fmt.Sprintf("nag:convoy:%d:72h", c.id)
		case age >= shipItNag24h:
			threshold = shipItNag24h
			key = fmt.Sprintf("nag:convoy:%d:24h", c.id)
		default:
			continue
		}
		if store.GetConfig(db, key, "") != "" {
			continue
		}
		// Pull the PR URLs so the operator has direct links, not just a reminder.
		var urls []string
		for _, ab := range store.ListConvoyAskBranches(db, c.id) {
			if ab.DraftPRURL != "" {
				urls = append(urls, fmt.Sprintf("  - %s: %s", ab.Repo, ab.DraftPRURL))
			}
		}
		urlSection := ""
		if len(urls) > 0 {
			urlSection = "\n\nDraft PR(s):\n" + strings.Join(urls, "\n")
		}
		store.SendMail(db, "ship-it-nag", "operator",
			fmt.Sprintf("[SHIP IT REMINDER] Convoy '%s' draft PR open for %v", c.name, threshold),
			fmt.Sprintf("Convoy '%s' has had its draft PR(s) open for %v. Review on GitHub and click Ship it when ready, or close the PR if the work is abandoned.%s\n\nRun `force convoy ship %d` to promote, or `force convoy pr %d` for status.",
				c.name, threshold, urlSection, c.id, c.id),
			0, store.MailTypeAlert)
		store.SetConfig(db, key, time.Now().UTC().Format(time.RFC3339))
		logger.Printf("ship-it-nag: convoy %d nagged at %v threshold", c.id, threshold)
	}
	return nil
}
