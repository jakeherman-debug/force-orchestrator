package agents

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"force-orchestrator/internal/claude"
	"force-orchestrator/internal/store"
)

// Pilot is the git-ops steward for ask-branches and the infra handler for PR-flow
// tasks that do not require code synthesis. Task types it claims:
//
//   - FindPRTemplate       — locate the PR template file for a repo
//   - CreateAskBranch      — cut and push a convoy's integration branch per repo
//   - CleanupAskBranch     — delete shipped/abandoned ask-branches
//   - RebaseAskBranch      — rebase ask-branch onto main, force-push
//   - RevalidateRepoConfig — revalidate remote, default branch, template path
//
// Pilot is deliberately non-LLM for the happy path — all git/gh operations are
// direct shell-outs. The FindPRTemplate handler is the one exception: when the
// deterministic filesystem search doesn't match any of the well-known template
// locations, Pilot falls back to a single Claude call to widen the net.

// Exported for tests and for operator CLI commands that want to trigger the
// same discovery logic synchronously.

// FindPRTemplatePath searches the repository at repoPath for a pull request
// template file. Returns the absolute path if found, or "" if the repo has no
// template. The search runs in two passes:
//
//  1. Deterministic check of the well-known locations (resolves 95%+ of repos).
//  2. If nothing found, a bounded filesystem walk for case-insensitive matches
//     against /pull.?request.?template/i, /PR.?template/i. If multiple files
//     match, ones under .github/ win over root, which win over elsewhere;
//     among equal-priority matches, the lexicographically earliest path wins
//     so the pick is stable across runs.
//
// An LLM fallback is layered on top of this via FindPRTemplatePathLLM —
// callers that want the extra net (Pilot does) use that wrapper.
func FindPRTemplatePath(repoPath string) (string, error) {
	if repoPath == "" {
		return "", fmt.Errorf("FindPRTemplatePath: repoPath required")
	}
	if stat, err := os.Stat(repoPath); err != nil || !stat.IsDir() {
		return "", fmt.Errorf("FindPRTemplatePath: %s is not a readable directory", repoPath)
	}

	// Pass 1: well-known locations. GitHub resolves PR templates in this order,
	// so we check the same places. Case matters on Linux — try both.
	wellKnown := []string{
		".github/pull_request_template.md",
		".github/PULL_REQUEST_TEMPLATE.md",
		".github/Pull_Request_Template.md",
		"docs/pull_request_template.md",
		"docs/PULL_REQUEST_TEMPLATE.md",
		"pull_request_template.md",
		"PULL_REQUEST_TEMPLATE.md",
	}
	for _, rel := range wellKnown {
		candidate := filepath.Join(repoPath, rel)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}

	// Pass 2: bounded walk. Limit depth and total files scanned so we never
	// explode on a giant monorepo.
	const maxDepth = 4
	const maxFilesScanned = 10_000

	// Match any filename plausibly a PR template. We deliberately exclude
	// directory names — GitHub supports .github/PULL_REQUEST_TEMPLATE/ as a
	// directory of multiple templates, but that's a different product decision
	// (multiple templates per repo). Pilot picks one default template file;
	// when we encounter the directory form, we pick the first file inside.
	filenameRe := regexp.MustCompile(`(?i)^(pull[._-]?request[._-]?template|pr[._-]?template)\.(md|markdown|txt)$`)

	type match struct {
		path     string
		priority int // lower = better
	}
	var matches []match
	filesScanned := 0

	_ = filepath.WalkDir(repoPath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable paths, don't fail the whole search
		}
		// Enforce a file-count ceiling so pathological repos don't hang us.
		if filesScanned++; filesScanned > maxFilesScanned {
			return filepath.SkipDir
		}
		// Compute depth relative to repoPath.
		rel, relErr := filepath.Rel(repoPath, path)
		if relErr != nil {
			return nil
		}
		depth := 0
		if rel != "." {
			depth = strings.Count(rel, string(os.PathSeparator)) + 1
		}
		if depth > maxDepth {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		// Skip noisy dirs that never host a PR template.
		if d.IsDir() {
			name := filepath.Base(path)
			switch name {
			case ".git", "node_modules", "vendor", "target", "build", "dist", ".next":
				return filepath.SkipDir
			}
			// Special case: .github/PULL_REQUEST_TEMPLATE/ as a directory. We
			// do NOT recurse into it here — the well-known pass would have
			// caught files at its top level via a dedicated check below.
			if strings.EqualFold(name, "PULL_REQUEST_TEMPLATE") &&
				strings.Contains(path, ".github") {
				// Pick the first .md file in the directory, stable-sorted.
				entries, _ := os.ReadDir(path)
				var candidates []string
				for _, e := range entries {
					if e.IsDir() {
						continue
					}
					if strings.HasSuffix(strings.ToLower(e.Name()), ".md") {
						candidates = append(candidates, filepath.Join(path, e.Name()))
					}
				}
				if len(candidates) > 0 {
					sort.Strings(candidates)
					matches = append(matches, match{candidates[0], 0})
				}
				return filepath.SkipDir
			}
			return nil
		}
		if !filenameRe.MatchString(filepath.Base(path)) {
			return nil
		}
		// Assign priority: .github/ > root > docs/ > elsewhere.
		p := 4
		switch {
		case strings.HasPrefix(rel, ".github"+string(os.PathSeparator)):
			p = 0
		case !strings.ContainsRune(rel, os.PathSeparator): // root
			p = 1
		case strings.HasPrefix(rel, "docs"+string(os.PathSeparator)):
			p = 2
		}
		matches = append(matches, match{path, p})
		return nil
	})

	if len(matches) == 0 {
		return "", nil
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].priority != matches[j].priority {
			return matches[i].priority < matches[j].priority
		}
		return matches[i].path < matches[j].path
	})
	return matches[0].path, nil
}

// FindPRTemplatePathLLM is FindPRTemplatePath with an LLM fallback. When the
// deterministic search returns "" and the repo clearly has documentation
// directories that might hold a template, Pilot consults Claude. This costs one
// Claude call per repo at most, and only fires when the deterministic search is
// inconclusive — for repos without any template at all, the LLM call is also
// skipped because the directory heuristic finds nothing worth asking about.
//
// The runCLI parameter lets tests inject a deterministic stub; production code
// passes claude.AskClaudeCLI.
func FindPRTemplatePathLLM(repoPath string, runCLI func(systemPrompt, userPrompt, tools string, maxTurns int) (string, error)) (string, error) {
	path, err := FindPRTemplatePath(repoPath)
	if err != nil {
		return "", err
	}
	if path != "" {
		return path, nil
	}
	if runCLI == nil {
		return "", nil // no LLM available → accept "no template"
	}

	// Only consult Claude if there's SOMETHING in the repo that looks like a
	// template-containing directory. Otherwise we're paying a token cost for
	// nothing.
	hasHints := false
	for _, hint := range []string{".github", "docs", "CONTRIBUTING.md", ".gitlab"} {
		if _, statErr := os.Stat(filepath.Join(repoPath, hint)); statErr == nil {
			hasHints = true
			break
		}
	}
	if !hasHints {
		return "", nil
	}

	// Build a shallow directory listing so Claude has concrete paths to pick from.
	var listing strings.Builder
	listing.WriteString("Directory listing of relevant subdirs (first 200 entries):\n")
	count := 0
	for _, sub := range []string{".github", "docs", ".gitlab"} {
		base := filepath.Join(repoPath, sub)
		if _, err := os.Stat(base); err != nil {
			continue
		}
		_ = filepath.WalkDir(base, func(p string, d os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return nil
			}
			if count++; count > 200 {
				return filepath.SkipDir
			}
			rel, _ := filepath.Rel(repoPath, p)
			if d.IsDir() {
				listing.WriteString(rel + "/\n")
			} else {
				listing.WriteString(rel + "\n")
			}
			return nil
		})
	}

	systemPrompt := `You are a filesystem inspector. Given a directory listing, locate the repo's pull request template file.

Return EXACTLY ONE LINE:
- The relative path (e.g. ".github/pull_request_template.md") if you find a PR template
- The word "none" if no PR template exists

Do not include explanations, reasoning, or any other text.`
	userPrompt := listing.String()

	out, cliErr := runCLI(systemPrompt, userPrompt, "", 1)
	if cliErr != nil {
		// LLM unavailable is not fatal — callers want "no template" over an error.
		return "", nil
	}
	answer := strings.TrimSpace(strings.SplitN(strings.TrimSpace(out), "\n", 2)[0])
	answer = strings.Trim(answer, "\"'`")
	if answer == "" || strings.EqualFold(answer, "none") {
		return "", nil
	}
	// Normalise to absolute path and verify the file actually exists — Claude is
	// allowed to be wrong, we are not.
	abs := filepath.Join(repoPath, filepath.FromSlash(answer))
	if info, err := os.Stat(abs); err != nil || info.IsDir() {
		return "", nil
	}
	return abs, nil
}

// findPRTemplatePayload is the JSON payload for a FindPRTemplate task.
type findPRTemplatePayload struct {
	Repo      string `json:"repo"`
	LocalPath string `json:"local_path"`
}

// QueueFindPRTemplate enqueues a FindPRTemplate task for a repo. Returns the
// task ID. Idempotent at the enqueue level — callers may race multiple queues
// and rely on the Pilot handler to tolerate repeat runs (the handler simply
// overwrites pr_template_path).
func QueueFindPRTemplate(db *sql.DB, repoName, localPath string) (int, error) {
	if repoName == "" || localPath == "" {
		return 0, fmt.Errorf("QueueFindPRTemplate: repoName and localPath required")
	}
	payloadBytes, _ := json.Marshal(findPRTemplatePayload{Repo: repoName, LocalPath: localPath})
	res, err := db.Exec(
		`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, priority, created_at)
		 VALUES (0, ?, 'FindPRTemplate', 'Pending', ?, 0, datetime('now'))`,
		repoName, string(payloadBytes))
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return int(id), nil
}

// SpawnPilot runs a Pilot agent loop. Pilot claims infra task types in turn
// (FindPRTemplate in Phase 1; later phases add CreateAskBranch, RebaseAskBranch,
// CleanupAskBranch, RevalidateRepoConfig).
func SpawnPilot(db *sql.DB, name string) {
	logger := NewLogger(name)
	logger.Printf("Pilot %s coming online", name)

	for {
		if IsEstopped(db) {
			time.Sleep(5 * time.Second)
			continue
		}

		if bounty, claimed := store.ClaimBounty(db, "FindPRTemplate", name); claimed {
			runFindPRTemplate(db, bounty, logger)
			continue
		}
		if bounty, claimed := store.ClaimBounty(db, "CreateAskBranch", name); claimed {
			runCreateAskBranch(db, bounty, logger)
			continue
		}
		if bounty, claimed := store.ClaimBounty(db, "CleanupAskBranch", name); claimed {
			runCleanupAskBranch(db, bounty, logger)
			continue
		}
		if bounty, claimed := store.ClaimBounty(db, "RebaseAskBranch", name); claimed {
			runRebaseAskBranch(db, bounty, logger)
			continue
		}
		if bounty, claimed := store.ClaimBounty(db, "RevalidateRepoConfig", name); claimed {
			runRevalidateRepoConfig(db, bounty, logger)
			continue
		}

		time.Sleep(time.Duration(3000+rand.Intn(1000)) * time.Millisecond)
	}
}

// runFindPRTemplate handles a single FindPRTemplate claim.
func runFindPRTemplate(db *sql.DB, bounty *store.Bounty, logger interface{ Printf(string, ...any) }) {
	var payload findPRTemplatePayload
	if err := json.Unmarshal([]byte(bounty.Payload), &payload); err != nil {
		store.FailBounty(db, bounty.ID, fmt.Sprintf("invalid payload: %v", err))
		return
	}
	if payload.Repo == "" || payload.LocalPath == "" {
		store.FailBounty(db, bounty.ID, "payload missing repo or local_path")
		return
	}

	path, err := FindPRTemplatePathLLM(payload.LocalPath, claude.AskClaudeCLI)
	if err != nil {
		// Directory-level failure (not-a-dir etc.) — escalate via normal retry path.
		store.FailBounty(db, bounty.ID, fmt.Sprintf("discovery failed: %v", err))
		return
	}

	if storeErr := store.SetRepoPRTemplatePath(db, payload.Repo, path); storeErr != nil {
		store.FailBounty(db, bounty.ID, fmt.Sprintf("could not persist template path: %v", storeErr))
		return
	}

	if path == "" {
		logger.Printf("FindPRTemplate #%d: no template found for %s (Diplomat fallback body will be used)",
			bounty.ID, payload.Repo)
	} else {
		logger.Printf("FindPRTemplate #%d: template for %s → %s", bounty.ID, payload.Repo, path)
	}
	store.UpdateBountyStatus(db, bounty.ID, "Completed")
}
