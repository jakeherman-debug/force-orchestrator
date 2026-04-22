package main

import (
	"database/sql"
	"fmt"
	"os"

	"force-orchestrator/internal/gh"
	"force-orchestrator/internal/store"
)

// cmdConvoyPR prints the PR state for a convoy: per-repo draft PR URLs,
// states, and a rollup of sub-PRs. Lightweight — does not make network calls.
func cmdConvoyPR(db *sql.DB, convoyID int) {
	convoy := store.GetConvoy(db, convoyID)
	if convoy == nil {
		fmt.Printf("Convoy %d not found.\n", convoyID)
		os.Exit(1)
	}
	fmt.Printf("Convoy '%s' (#%d)  status=%s\n", convoy.Name, convoy.ID, convoy.Status)

	branches := store.ListConvoyAskBranches(db, convoyID)
	if len(branches) == 0 {
		fmt.Println("  (no ask-branches — legacy convoy or no PR-flow repos)")
		return
	}
	for _, ab := range branches {
		fmt.Printf("\nRepo: %s\n", ab.Repo)
		fmt.Printf("  Ask-branch:    %s (base %s)\n", ab.AskBranch, truncateSHA(ab.AskBranchBaseSHA))
		if ab.LastRebasedAt != "" {
			fmt.Printf("  Last rebase:   %s\n", ab.LastRebasedAt)
		}
		if ab.DraftPRNumber > 0 {
			fmt.Printf("  Draft PR:      %s  state=%s\n", ab.DraftPRURL, ab.DraftPRState)
			if ab.ShippedAt != "" {
				fmt.Printf("  Shipped at:    %s\n", ab.ShippedAt)
			}
		} else {
			fmt.Println("  Draft PR:      (not yet — convoy still in progress)")
		}
	}

	// Sub-PR rollup.
	rollup := store.RollupAskBranchPRs(db, convoyID)
	fmt.Printf("\nSub-PRs: %d total (open=%d merged=%d closed=%d)\n",
		rollup.Total, rollup.Open, rollup.Merged, rollup.Closed)
	fmt.Printf("  CI: pending=%d success=%d failure=%d\n",
		rollup.ChecksPending, rollup.ChecksSuccess, rollup.ChecksFailure)
}

// cmdConvoyShip flips each of the convoy's draft PRs to ready-for-review, and
// optionally merges them if --merge is supplied. This is the Ship It operation.
//
// If a repo's PR is already non-draft, pr ready is a no-op. Missing draft PRs
// are skipped with a warning.
func cmdConvoyShip(db *sql.DB, convoyID int, mergeStrategy string) {
	convoy := store.GetConvoy(db, convoyID)
	if convoy == nil {
		fmt.Printf("Convoy %d not found.\n", convoyID)
		os.Exit(1)
	}
	if convoy.Status != "DraftPROpen" {
		fmt.Printf("Convoy %d is not in DraftPROpen state (status=%s). Refusing to ship.\n",
			convoyID, convoy.Status)
		os.Exit(1)
	}

	branches := store.ListConvoyAskBranches(db, convoyID)
	ghc := gh.NewClient()
	var shipped, failed int
	for _, ab := range branches {
		if ab.DraftPRNumber == 0 {
			fmt.Printf("  [skip] %s: no draft PR recorded\n", ab.Repo)
			continue
		}
		repo := store.GetRepo(db, ab.Repo)
		var cwd, ghRepo string
		if repo != nil {
			cwd = repo.LocalPath
			ghRepo = deriveGHRepoFromRemoteURLForShip(repo.RemoteURL)
		}
		if err := ghc.PRReady(cwd, ghRepo, ab.DraftPRNumber); err != nil {
			fmt.Printf("  [fail] %s: gh pr ready: %v\n", ab.Repo, err)
			failed++
			continue
		}
		// Include the PR URL so the operator can click through to review / merge.
		fmt.Printf("  [ready] %s: PR #%d promoted from draft → %s\n",
			ab.Repo, ab.DraftPRNumber, ab.DraftPRURL)
		if mergeStrategy != "" {
			if err := ghc.PRMerge(cwd, ghRepo, ab.DraftPRNumber, mergeStrategy); err != nil {
				fmt.Printf("  [fail] %s: gh pr merge: %v\n", ab.Repo, err)
				failed++
				continue
			}
			fmt.Printf("  [merged] %s: PR #%d merged (%s) → %s\n",
				ab.Repo, ab.DraftPRNumber, mergeStrategy, ab.DraftPRURL)
		}
		shipped++
	}
	store.LogAudit(db, "operator", "convoy-ship", convoyID,
		fmt.Sprintf("shipped=%d failed=%d merge=%s", shipped, failed, mergeStrategy))

	if failed > 0 {
		fmt.Printf("\n%d repo(s) failed — see errors above. Convoy stays DraftPROpen.\n", failed)
		os.Exit(1)
	}
	fmt.Printf("\nAll %d draft PR(s) promoted. draft-pr-watch will transition the convoy to Shipped on the next tick once the PRs merge.\n", shipped)
}

// deriveGHRepoFromRemoteURLForShip — the main package can't call the agents-
// scoped helper, so duplicate the tiny parser here.
func deriveGHRepoFromRemoteURLForShip(remoteURL string) string {
	if remoteURL == "" {
		return ""
	}
	if len(remoteURL) > 4 && remoteURL[:4] == "git@" {
		for i := 4; i < len(remoteURL); i++ {
			if remoteURL[i] == ':' {
				path := remoteURL[i+1:]
				if len(path) > 4 && path[len(path)-4:] == ".git" {
					path = path[:len(path)-4]
				}
				return path
			}
		}
		return ""
	}
	for _, scheme := range []string{"https://", "http://"} {
		if len(remoteURL) > len(scheme) && remoteURL[:len(scheme)] == scheme {
			rest := remoteURL[len(scheme):]
			for i := 0; i < len(rest); i++ {
				if rest[i] == '/' {
					path := rest[i+1:]
					if len(path) > 4 && path[len(path)-4:] == ".git" {
						path = path[:len(path)-4]
					}
					return path
				}
			}
		}
	}
	return ""
}

func truncateSHA(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
