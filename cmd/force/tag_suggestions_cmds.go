package main

// tag_suggestions_cmds.go — D14 Phase 3 CLI surface for TagSuggestions.
//
// Subcommands:
//
//	force tag-suggestions list [--repo <name>] [--status pending|accepted|dismissed]
//	force tag-suggestions accept <id>
//	force tag-suggestions dismiss <id> [-y]

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strings"

	"force-orchestrator/internal/store"
)

func cmdTagSuggestions(db *sql.DB, args []string) int {
	if len(args) == 0 {
		tagSuggestionsUsage()
		return 1
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "--help", "-h", "help":
		tagSuggestionsUsage()
		return 0
	case "list":
		return tagSuggestionsListCmd(db, rest)
	case "accept":
		return tagSuggestionsAcceptCmd(db, rest)
	case "dismiss":
		return tagSuggestionsDismissCmd(db, rest)
	default:
		fmt.Fprintf(os.Stderr, "unknown tag-suggestions subcommand: %s\n", sub)
		tagSuggestionsUsage()
		return 1
	}
}

func tagSuggestionsUsage() {
	fmt.Fprintln(os.Stderr, `Usage: force tag-suggestions <subcommand>

Subcommands:
  list [--repo <name>] [--status pending|accepted|dismissed]
        List tag suggestions. Default: pending only.

  accept <id>
        Accept a tag suggestion: sets status=accepted, creates the RepoTag,
        creates the Tag in the Tags table if it does not exist.

  dismiss <id> [-y]
        Dismiss a tag suggestion: sets status=dismissed.`)
}

func tagSuggestionsListCmd(db *sql.DB, args []string) int {
	fs := flag.NewFlagSet("tag-suggestions list", flag.ContinueOnError)
	repoFlag := fs.String("repo", "", "filter by repo name")
	statusFlag := fs.String("status", "pending", "pending|accepted|dismissed (empty = all)")
	helped, perr := parseSubcommandFlags(fs, args, "tag-suggestions list",
		"List tag suggestions. Default shows only pending suggestions.",
		[]flagDoc{
			{Name: "--repo R", Desc: "filter by repo name"},
			{Name: "--status S", Desc: "pending|accepted|dismissed (empty = all, default: pending)"},
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{"force tag-suggestions list", "force tag-suggestions list --repo myrepo --status all"})
	if helped {
		return 0
	}
	if perr != nil {
		return 2
	}

	// Validate status.
	validStatuses := map[string]bool{"pending": true, "accepted": true, "dismissed": true, "": true}
	if !validStatuses[*statusFlag] {
		fmt.Fprintf(os.Stderr, "tag-suggestions list: --status must be pending|accepted|dismissed (got %q)\n", *statusFlag)
		return 1
	}

	suggestions, err := store.ListTagSuggestions(db, *statusFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tag-suggestions list: %v\n", err)
		return 1
	}

	// Apply optional repo filter (store doesn't have a combined query).
	if *repoFlag != "" {
		var filtered []store.TagSuggestion
		for _, s := range suggestions {
			if s.RepoName == *repoFlag {
				filtered = append(filtered, s)
			}
		}
		suggestions = filtered
	}

	if len(suggestions) == 0 {
		fmt.Println("(no tag suggestions)")
		return 0
	}

	fmt.Printf("%-5s %-20s %-15s %-10s %-20s %s\n",
		"ID", "REPO", "TAG", "STATUS", "SUGGESTED_BY", "RATIONALE")
	fmt.Println(strings.Repeat("-", 95))
	for _, s := range suggestions {
		fmt.Printf("%-5d %-20s %-15s %-10s %-20s %s\n",
			s.ID,
			truncate(s.RepoName, 20),
			truncate(s.Tag, 15),
			s.Status,
			truncate(s.SuggestedBy, 20),
			truncate(s.Rationale, 40))
	}
	return 0
}

func tagSuggestionsAcceptCmd(db *sql.DB, args []string) int {
	// Reorder flags before positionals so `force tag-suggestions accept <id> --added-by ...` works.
	args = reorderFlagsFirst(args, map[string]bool{})
	fs := flag.NewFlagSet("tag-suggestions accept", flag.ContinueOnError)
	addedByFlag := fs.String("added-by", "operator", "who is accepting (recorded in RepoTags.added_by)")
	helped, perr := parseSubcommandFlags(fs, args, "tag-suggestions accept",
		"Accept a tag suggestion: marks it accepted, creates the RepoTag (and Tag if needed).",
		[]flagDoc{
			{Name: "--added-by E", Desc: "identity recorded in RepoTags (default: operator)"},
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{"force tag-suggestions accept 7"})
	if helped {
		return 0
	}
	if perr != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: force tag-suggestions accept <id>")
		return 1
	}
	id := mustParseID(rest[0])

	// Load the suggestion so we know the repo + tag.
	suggestions, err := store.ListTagSuggestions(db, "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "tag-suggestions accept: list: %v\n", err)
		return 1
	}
	var target *store.TagSuggestion
	for i := range suggestions {
		if suggestions[i].ID == id {
			target = &suggestions[i]
			break
		}
	}
	if target == nil {
		fmt.Fprintf(os.Stderr, "tag-suggestions accept: suggestion %d not found\n", id)
		return 1
	}
	if target.Status != "pending" {
		fmt.Fprintf(os.Stderr, "tag-suggestions accept: suggestion %d is not pending (status=%s)\n", id, target.Status)
		return 1
	}

	// Ensure the Tag exists.
	if _, getErr := store.GetTag(db, target.Tag); getErr != nil {
		if cerr := store.CreateTag(db, target.Tag, "", *addedByFlag); cerr != nil {
			if !strings.Contains(cerr.Error(), "UNIQUE constraint") {
				fmt.Fprintf(os.Stderr, "tag-suggestions accept: create tag %q: %v\n", target.Tag, cerr)
				return 1
			}
		}
	}

	// Create the RepoTag.
	if err := store.AddRepoTag(db, target.RepoName, target.Tag, *addedByFlag, "tag-suggestion"); err != nil {
		fmt.Fprintf(os.Stderr, "tag-suggestions accept: add repo tag: %v\n", err)
		return 1
	}

	// Mark the suggestion accepted.
	if err := store.ResolveTagSuggestion(db, id, "accepted", *addedByFlag); err != nil {
		fmt.Fprintf(os.Stderr, "tag-suggestions accept: resolve: %v\n", err)
		return 1
	}

	fmt.Printf("Suggestion %d accepted: tag %q added to repo %q.\n", id, target.Tag, target.RepoName)
	return 0
}

func tagSuggestionsDismissCmd(db *sql.DB, args []string) int {
	// Reorder flags before positionals so `force tag-suggestions dismiss <id> --assume-yes` works.
	assumeYesBoolFlags := map[string]bool{"--assume-yes": true, "-y": true}
	args = reorderFlagsFirst(args, assumeYesBoolFlags)
	fs := flag.NewFlagSet("tag-suggestions dismiss", flag.ContinueOnError)
	assumeYes := fs.Bool("assume-yes", false, "skip confirmation prompt")
	yFlag := fs.Bool("y", false, "skip confirmation prompt (short form)")
	resolvedByFlag := fs.String("resolved-by", "operator", "who is dismissing")
	helped, perr := parseSubcommandFlags(fs, args, "tag-suggestions dismiss",
		"Dismiss a tag suggestion: sets status=dismissed.",
		[]flagDoc{
			{Name: "-y, --assume-yes", Desc: "skip confirmation prompt"},
			{Name: "--resolved-by E", Desc: "identity recorded on the suggestion (default: operator)"},
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{"force tag-suggestions dismiss 7 -y"})
	if helped {
		return 0
	}
	if perr != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: force tag-suggestions dismiss <id> [-y]")
		return 1
	}
	id := mustParseID(rest[0])

	if !*assumeYes && !*yFlag {
		fmt.Printf("Dismiss suggestion %d? [y/N] ", id)
		var resp string
		fmt.Scanln(&resp)
		if strings.ToLower(strings.TrimSpace(resp)) != "y" {
			fmt.Println("Aborted.")
			return 0
		}
	}

	if err := store.ResolveTagSuggestion(db, id, "dismissed", *resolvedByFlag); err != nil {
		fmt.Fprintf(os.Stderr, "tag-suggestions dismiss: %v\n", err)
		return 1
	}
	fmt.Printf("Suggestion %d dismissed.\n", id)
	return 0
}
