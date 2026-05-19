package main

// tags_cmds.go — D14 Phase 3 CLI surface for Tags, RepoTags.
//
// Subcommands added here:
//
//	force tags list
//	force tags create <name> [--description "..."]
//	force tags remove <name> [-y]
//
//	force repos tag <repo-name> <tag> [--added-by operator]
//	force repos untag <repo-name> <tag>
//	force repos tags [--repo <name>] [--tag <tag>]

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strings"

	"force-orchestrator/internal/store"
)

// ── force tags ────────────────────────────────────────────────────────────────

func cmdTags(db *sql.DB, args []string) int {
	if len(args) == 0 {
		tagsUsage()
		return 1
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "--help", "-h", "help":
		tagsUsage()
		return 0
	case "list":
		return tagsListCmd(db, rest)
	case "create":
		return tagsCreateCmd(db, rest)
	case "remove":
		return tagsRemoveCmd(db, rest)
	default:
		fmt.Fprintf(os.Stderr, "unknown tags subcommand: %s\n", sub)
		tagsUsage()
		return 1
	}
}

func tagsUsage() {
	fmt.Fprintln(os.Stderr, `Usage: force tags <subcommand>

Subcommands:
  list
        List all tags in the Tags registry with description and created_by.

  create <name> [--description "..."]
        Create a new tag. Fails if the tag already exists.

  remove <name> [-y]
        Remove a tag. Fails if any RepoTags rows reference it.
        -y / --assume-yes: skip confirmation prompt.`)
}

func tagsListCmd(db *sql.DB, args []string) int {
	fs := flag.NewFlagSet("tags list", flag.ContinueOnError)
	helped, perr := parseSubcommandFlags(fs, args, "tags list",
		"List all tags in the Tags registry.",
		[]flagDoc{
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{"force tags list"})
	if helped {
		return 0
	}
	if perr != nil {
		return 2
	}

	tags, err := store.ListTags(db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tags list: %v\n", err)
		return 1
	}
	if len(tags) == 0 {
		fmt.Println("(no tags defined — use: force tags create <name>)")
		return 0
	}
	fmt.Printf("%-20s %-40s %-20s %s\n", "NAME", "DESCRIPTION", "CREATED_BY", "CREATED_AT")
	fmt.Println(strings.Repeat("-", 95))
	for _, t := range tags {
		fmt.Printf("%-20s %-40s %-20s %s\n",
			t.Name, truncate(t.Description, 40), t.CreatedBy, t.CreatedAt)
	}
	return 0
}

func tagsCreateCmd(db *sql.DB, args []string) int {
	// Reorder flags before positionals so `force tags create <name> --description "..."` works.
	args = reorderFlagsFirst(args, map[string]bool{})
	fs := flag.NewFlagSet("tags create", flag.ContinueOnError)
	descFlag := fs.String("description", "", "tag description")
	createdByFlag := fs.String("created-by", "operator", "who is creating this tag")
	helped, perr := parseSubcommandFlags(fs, args, "tags create",
		"Create a new tag in the Tags registry. Fails if the tag already exists.",
		[]flagDoc{
			{Name: "--description D", Desc: "tag description (optional)"},
			{Name: "--created-by E", Desc: "creator identity (default: operator)"},
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{"force tags create payments --description \"Payment-related repos\""})
	if helped {
		return 0
	}
	if perr != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: force tags create <name> [--description \"...\"]")
		return 1
	}
	name := rest[0]
	if err := store.CreateTag(db, name, *descFlag, *createdByFlag); err != nil {
		fmt.Fprintf(os.Stderr, "tags create: %v\n", err)
		return 1
	}
	fmt.Printf("Tag %q created.\n", name)
	return 0
}

func tagsRemoveCmd(db *sql.DB, args []string) int {
	// Reorder flags before positionals so `force tags remove <name> --assume-yes` works.
	assumeYesBoolFlags := map[string]bool{"--assume-yes": true, "-y": true}
	args = reorderFlagsFirst(args, assumeYesBoolFlags)
	fs := flag.NewFlagSet("tags remove", flag.ContinueOnError)
	assumeYes := fs.Bool("assume-yes", false, "skip confirmation prompt")
	yFlag := fs.Bool("y", false, "skip confirmation prompt (short form)")
	helped, perr := parseSubcommandFlags(fs, args, "tags remove",
		"Remove a tag from the Tags registry. Fails if any RepoTags rows still reference it.",
		[]flagDoc{
			{Name: "-y, --assume-yes", Desc: "skip confirmation prompt"},
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{"force tags remove payments -y"})
	if helped {
		return 0
	}
	if perr != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: force tags remove <name> [-y]")
		return 1
	}
	name := rest[0]

	if !*assumeYes && !*yFlag {
		fmt.Printf("Remove tag %q? This will fail if any repos still use it. [y/N] ", name)
		var resp string
		fmt.Scanln(&resp)
		if strings.ToLower(strings.TrimSpace(resp)) != "y" {
			fmt.Println("Aborted.")
			return 0
		}
	}

	if err := store.DeleteTag(db, name); err != nil {
		fmt.Fprintf(os.Stderr, "tags remove: %v\n", err)
		return 1
	}
	fmt.Printf("Tag %q removed.\n", name)
	return 0
}

// ── force repos tag / untag / tags ────────────────────────────────────────────

// cmdReposTag handles `force repos tag <repo-name> <tag> [--added-by operator]`.
// Called from cmdRepos switch when subCmd == "tag".
func cmdReposTag(db *sql.DB, args []string) {
	// Reorder flags before positionals so `force repos tag <repo> <tag> --added-by ...` works.
	args = reorderFlagsFirst(args, map[string]bool{})
	fs := flag.NewFlagSet("repos tag", flag.ContinueOnError)
	addedByFlag := fs.String("added-by", "operator", "who is adding the tag")
	helped, perr := parseSubcommandFlags(fs, args, "repos tag",
		"Add a tag to a repository. Creates the tag in the Tags table if it does not exist.",
		[]flagDoc{
			{Name: "--added-by E", Desc: "identity recorded in RepoTags (default: operator)"},
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{"force repos tag myrepo payments", "force repos tag myrepo payments --added-by jake"})
	if helped {
		return
	}
	if perr != nil {
		os.Exit(2)
	}
	rest := fs.Args()
	if len(rest) < 2 {
		fmt.Fprintln(os.Stderr, "Usage: force repos tag <repo-name> <tag> [--added-by <who>]")
		os.Exit(1)
	}
	repoName := rest[0]
	tag := rest[1]

	// Ensure the tag exists in the Tags table; create it silently if not.
	if _, err := store.GetTag(db, tag); err != nil {
		// sql.ErrNoRows or any other error — attempt to create it.
		if cerr := store.CreateTag(db, tag, "", *addedByFlag); cerr != nil {
			// Ignore "already exists" races; other errors are real.
			if !strings.Contains(cerr.Error(), "UNIQUE constraint") {
				fmt.Fprintf(os.Stderr, "repos tag: creating tag %q: %v\n", tag, cerr)
				os.Exit(1)
			}
		}
	}

	if err := store.AddRepoTag(db, repoName, tag, *addedByFlag, "operator"); err != nil {
		fmt.Fprintf(os.Stderr, "repos tag: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Tag %q added to repo %q.\n", tag, repoName)
}

// cmdReposUntag handles `force repos untag <repo-name> <tag>`.
func cmdReposUntag(db *sql.DB, args []string) {
	fs := flag.NewFlagSet("repos untag", flag.ContinueOnError)
	helped, perr := parseSubcommandFlags(fs, args, "repos untag",
		"Remove a tag from a repository.",
		[]flagDoc{{Name: "--help, -h", Desc: "show this help and exit"}},
		[]string{"force repos untag myrepo payments"})
	if helped {
		return
	}
	if perr != nil {
		os.Exit(2)
	}
	rest := fs.Args()
	if len(rest) < 2 {
		fmt.Fprintln(os.Stderr, "Usage: force repos untag <repo-name> <tag>")
		os.Exit(1)
	}
	repoName := rest[0]
	tag := rest[1]
	if err := store.RemoveRepoTag(db, repoName, tag); err != nil {
		fmt.Fprintf(os.Stderr, "repos untag: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Tag %q removed from repo %q.\n", tag, repoName)
}

// cmdReposTags handles `force repos tags [--repo <name>] [--tag <tag>]`.
func cmdReposTags(db *sql.DB, args []string) {
	fs := flag.NewFlagSet("repos tags", flag.ContinueOnError)
	repoFlag := fs.String("repo", "", "filter by repo name")
	tagFlag := fs.String("tag", "", "filter by tag name")
	helped, perr := parseSubcommandFlags(fs, args, "repos tags",
		"List repo↔tag associations. With --repo: tags for that repo. With --tag: repos with that tag. Both: intersection.",
		[]flagDoc{
			{Name: "--repo R", Desc: "filter to show only tags for this repo"},
			{Name: "--tag T", Desc: "filter to show only repos with this tag"},
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{"force repos tags", "force repos tags --repo myrepo", "force repos tags --tag payments"})
	if helped {
		return
	}
	if perr != nil {
		os.Exit(2)
	}

	switch {
	case *repoFlag != "" && *tagFlag != "":
		// Intersection: check whether this specific repo has this specific tag.
		rows, err := store.ListTagsForRepo(db, *repoFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "repos tags: %v\n", err)
			os.Exit(1)
		}
		var found []store.RepoTag
		for _, rt := range rows {
			if rt.Tag == *tagFlag {
				found = append(found, rt)
			}
		}
		printRepoTagRows(found)

	case *repoFlag != "":
		rows, err := store.ListTagsForRepo(db, *repoFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "repos tags: %v\n", err)
			os.Exit(1)
		}
		printRepoTagRows(rows)

	case *tagFlag != "":
		rows, err := store.ListReposForTag(db, *tagFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "repos tags: %v\n", err)
			os.Exit(1)
		}
		printRepoTagRows(rows)

	default:
		// No filter: list all RepoTags rows.
		dbRows, err := db.Query(
			`SELECT repo_name, tag, IFNULL(added_at,''), IFNULL(added_by,''), IFNULL(source,'')
			   FROM RepoTags ORDER BY repo_name, tag`,
		)
		if err != nil {
			fmt.Fprintf(os.Stderr, "repos tags: %v\n", err)
			os.Exit(1)
		}
		defer dbRows.Close()
		var all []store.RepoTag
		for dbRows.Next() {
			var rt store.RepoTag
			if err := dbRows.Scan(&rt.RepoName, &rt.Tag, &rt.AddedAt, &rt.AddedBy, &rt.Source); err != nil {
				fmt.Fprintf(os.Stderr, "repos tags: scan: %v\n", err)
				continue
			}
			all = append(all, rt)
		}
		if err := dbRows.Err(); err != nil {
			fmt.Fprintf(os.Stderr, "repos tags: rows err: %v\n", err)
		}
		printRepoTagRows(all)
	}
}

func printRepoTagRows(rows []store.RepoTag) {
	if len(rows) == 0 {
		fmt.Println("(no repo↔tag associations)")
		return
	}
	fmt.Printf("%-20s %-20s %-10s %-20s %s\n", "REPO", "TAG", "SOURCE", "ADDED_BY", "ADDED_AT")
	fmt.Println(strings.Repeat("-", 85))
	for _, rt := range rows {
		fmt.Printf("%-20s %-20s %-10s %-20s %s\n",
			truncate(rt.RepoName, 20), truncate(rt.Tag, 20),
			truncate(rt.Source, 10), truncate(rt.AddedBy, 20), rt.AddedAt)
	}
}
