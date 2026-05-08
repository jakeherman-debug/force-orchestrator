package main

// add_repos_cmd.go — `force add-repos <directory>` batch import.
//
// Sweep D, Part 2: instead of typing 17 `force add-repo` invocations to
// register every git repo under `~/code/`, the operator runs
// `force add-repos ~/code` once. Each immediate subdirectory that
// contains a `.git/` is enumerated, smart-default name + description are
// derived (same helpers as cmdAddRepo), a preview table is printed, and
// the operator confirms with `yes` (or `--assume-yes` for unattended
// scripted use).
//
// Already-registered repos (matching by `name`) are surfaced in the
// preview as "[skipped: already registered]" and bypassed on the write
// pass — the second invocation is a no-op, which is the contract the
// behavioral test pins.

import (
	"bufio"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"force-orchestrator/internal/store"
)

// repoBatchEntry is one row in the preview table — what cmdAddRepos has
// derived for a single candidate repo.
type repoBatchEntry struct {
	Path        string // absolute
	Name        string // derived; "" if derivation failed
	Description string // derived; may be ""
	Skip        bool   // true when a repo with this name is already registered
	SkipReason  string // operator-readable explanation for the preview
}

// cmdAddRepos enumerates immediate subdirectories of <dir> that contain
// a `.git/` (single-level, no recursion), derives smart defaults per
// repo, prints a preview, and (after confirmation) registers each one
// via the same `registerRepo` write path used by cmdAddRepo.
func cmdAddRepos(db *sql.DB, args []string) {
	// Hoist flags ahead of positionals so `force add-repos <dir> -y`
	// parses the same as `force add-repos -y <dir>` (Go's flag package
	// stops at the first positional otherwise).
	args = reorderFlagsFirst(args, addRepoBoolFlags)

	fs := flag.NewFlagSet("add-repos", flag.ContinueOnError)
	assumeYes := fs.Bool("assume-yes", false, "Skip the confirmation prompt (unattended)")
	assumeYesShort := fs.Bool("y", false, "Alias for --assume-yes")
	helped, perr := parseSubcommandFlags(fs, args, "add-repos",
		"Bulk-register every git repo found in the immediate subdirectories of <directory>. Each repo is added with smart-default name (git remote tail or basename) and description (README first paragraph); already-registered repos are skipped.",
		[]flagDoc{
			{Name: "--assume-yes, -y", Desc: "skip the preview confirmation prompt"},
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{
			"force add-repos ~/code",
			"force add-repos ~/code --assume-yes",
		})
	if helped {
		return
	}
	if perr != nil {
		os.Exit(2)
	}
	rest := fs.Args()
	if len(rest) != 1 {
		fmt.Println("Usage: force add-repos <directory> [--assume-yes]")
		os.Exit(1)
	}

	dir, absErr := filepath.Abs(rest[0])
	if absErr != nil {
		fmt.Printf("Cannot resolve %q: %v\n", rest[0], absErr)
		os.Exit(1)
	}
	info, statErr := os.Stat(dir)
	if statErr != nil {
		fmt.Printf("Directory does not exist: %s\n", dir)
		os.Exit(1)
	}
	if !info.IsDir() {
		fmt.Printf("Not a directory: %s\n", dir)
		os.Exit(1)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		fmt.Printf("Cannot read %s: %v\n", dir, err)
		os.Exit(1)
	}

	// Collect candidate repos: every immediate subdir that contains a
	// `.git/` entry (file or directory — git submodules use a `.git`
	// FILE pointing back to the parent).
	var candidates []repoBatchEntry
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// Skip dotfiles like `.cache/`, `.git/`, `.DS_Store/` — operators
		// don't want their `.config` or `.cargo` walked.
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		repoPath := filepath.Join(dir, e.Name())
		if _, err := os.Stat(filepath.Join(repoPath, ".git")); err != nil {
			continue
		}
		entry := repoBatchEntry{Path: repoPath}
		entry.Name = deriveRepoName(repoPath)
		entry.Description = deriveRepoDescription(repoPath)
		if entry.Name == "" {
			entry.Skip = true
			entry.SkipReason = "could not derive name"
		} else if existing := store.GetRepo(db, entry.Name); existing != nil {
			entry.Skip = true
			entry.SkipReason = "already registered"
		}
		candidates = append(candidates, entry)
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Path < candidates[j].Path })

	if len(candidates) == 0 {
		fmt.Printf("No git repositories found in %s\n", dir)
		return
	}

	// Preview table. Columns: name (≤22), path (≤45 truncated), desc
	// (≤60 truncated), status (when skipped).
	fmt.Printf("Found %d git repo(s) under %s:\n\n", len(candidates), dir)
	fmt.Printf("  %-22s %-45s %s\n", "NAME", "PATH", "DESCRIPTION / STATUS")
	fmt.Println("  " + strings.Repeat("-", 90))
	addable := 0
	for _, c := range candidates {
		nameCol := c.Name
		if nameCol == "" {
			nameCol = "<unknown>"
		}
		descCol := truncate(c.Description, 60)
		if c.Skip {
			descCol = fmt.Sprintf("[skipped: %s]", c.SkipReason)
		} else {
			addable++
		}
		fmt.Printf("  %-22s %-45s %s\n", truncate(nameCol, 22), truncate(c.Path, 45), descCol)
	}
	fmt.Println()

	if addable == 0 {
		fmt.Println("Nothing to add — every candidate is skipped.")
		return
	}

	// Confirm (or skip when --assume-yes / -y).
	if !(*assumeYes || *assumeYesShort) {
		fmt.Printf("Add these %d repo(s)? [yes/no] ", addable)
		reader := bufio.NewReader(os.Stdin)
		line, _ := reader.ReadString('\n')
		ans := strings.ToLower(strings.TrimSpace(line))
		if ans != "yes" && ans != "y" {
			fmt.Println("Aborted — no repos were registered.")
			return
		}
	}

	// Write pass — call registerRepo for each non-skipped candidate. A
	// per-repo failure is reported but doesn't stop the batch.
	added := 0
	failed := 0
	for _, c := range candidates {
		if c.Skip {
			continue
		}
		fmt.Printf("\n--- %s ---\n", c.Name)
		err := registerRepo(db, &repoRegistration{
			Name:        c.Name,
			Path:        c.Path,
			Description: c.Description,
			Out:         os.Stdout,
		})
		if err != nil {
			fmt.Printf("  ERROR: %v\n", err)
			failed++
			continue
		}
		added++
	}
	fmt.Printf("\nDone — %d added, %d failed, %d skipped.\n", added, failed, len(candidates)-addable)
}
