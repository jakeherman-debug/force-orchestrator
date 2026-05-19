package main

// rules_cmds.go — D14 Phase 3 CLI surface for FleetRules scoping.
//
// Subcommands:
//
//	force rules list [--scope global|tag:<t>|repo:<r>] [--repo <name>]
//	force rules upgrade <rule-key> --to-scope <senate:*|senate:tag:<t>|senate:<repo>>

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strings"

	"force-orchestrator/internal/store"
)

func cmdRules(db *sql.DB, args []string) int {
	if len(args) == 0 {
		rulesUsage()
		return 1
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "--help", "-h", "help":
		rulesUsage()
		return 0
	case "list":
		return rulesListCmd(db, rest)
	case "upgrade":
		return rulesUpgradeCmd(db, rest)
	default:
		fmt.Fprintf(os.Stderr, "unknown rules subcommand: %s\n", sub)
		rulesUsage()
		return 1
	}
}

func rulesUsage() {
	fmt.Fprintln(os.Stderr, `Usage: force rules <subcommand>

Subcommands:
  list [--scope global|tag:<t>|repo:<r>] [--repo <name>]
        List active FleetRules rows. With --repo: show all rules applicable to
        that repo (global + repo-specific + tag-derived). With --scope: filter
        by scope type.

  upgrade <rule-key> --to-scope <senate:*|senate:tag:<t>|senate:<repo>>
        Update FleetRules.agent_scope for the given rule key. Validates the
        new scope syntax before writing.`)
}

// validateRuleScope returns a non-nil error when scope is not one of the
// three valid forms: "senate:*", "senate:tag:<name>", "senate:<repo>".
func validateRuleScope(scope string) error {
	if scope == "" {
		return fmt.Errorf("scope must not be empty")
	}
	if !strings.HasPrefix(scope, "senate:") {
		return fmt.Errorf("scope must start with 'senate:' (got %q)", scope)
	}
	rest := strings.TrimPrefix(scope, "senate:")
	if rest == "*" {
		return nil // global
	}
	if strings.HasPrefix(rest, "tag:") {
		tagName := strings.TrimPrefix(rest, "tag:")
		if strings.TrimSpace(tagName) == "" {
			return fmt.Errorf("scope 'senate:tag:' must have a non-empty tag name")
		}
		return nil
	}
	// repo-specific — just validate non-empty
	if strings.TrimSpace(rest) == "" {
		return fmt.Errorf("scope 'senate:<repo>' must have a non-empty repo name")
	}
	return nil
}

// scopeToFilter converts a human-friendly --scope token to a WHERE clause
// fragment and args. Accepts the same three-form vocabulary as the internal
// agent_scope column plus the shorthands "global", "tag:<t>", "repo:<r>".
func scopeToFilter(scope string) (clause string, args []any, err error) {
	switch {
	case scope == "global" || scope == "senate:*":
		return `agent_scope = 'senate:*'`, nil, nil
	case strings.HasPrefix(scope, "tag:"):
		tagName := strings.TrimPrefix(scope, "tag:")
		if tagName == "" {
			return "", nil, fmt.Errorf("scope 'tag:<name>' requires a non-empty tag name")
		}
		return `agent_scope = ?`, []any{"senate:tag:" + tagName}, nil
	case strings.HasPrefix(scope, "senate:tag:"):
		tagName := strings.TrimPrefix(scope, "senate:tag:")
		return `agent_scope = ?`, []any{"senate:tag:" + tagName}, nil
	case strings.HasPrefix(scope, "repo:"):
		repoName := strings.TrimPrefix(scope, "repo:")
		if repoName == "" {
			return "", nil, fmt.Errorf("scope 'repo:<name>' requires a non-empty repo name")
		}
		return `agent_scope = ?`, []any{"senate:" + repoName}, nil
	case strings.HasPrefix(scope, "senate:") && !strings.HasPrefix(scope, "senate:tag:") && scope != "senate:*":
		// Direct senate:<repo> form.
		return `agent_scope = ?`, []any{scope}, nil
	default:
		return "", nil, fmt.Errorf("unrecognized scope %q — use global, tag:<t>, repo:<r>, senate:*, senate:tag:<t>, or senate:<repo>", scope)
	}
}

func rulesListCmd(db *sql.DB, args []string) int {
	fs := flag.NewFlagSet("rules list", flag.ContinueOnError)
	scopeFlag := fs.String("scope", "", "global|tag:<t>|repo:<r> — filter by scope type")
	repoFlag := fs.String("repo", "", "show all rules applicable to this repo (ResolveRulesForRepo)")
	helped, perr := parseSubcommandFlags(fs, args, "rules list",
		"List active FleetRules rows. With --repo uses ResolveRulesForRepo to show all applicable rules.",
		[]flagDoc{
			{Name: "--scope S", Desc: "global|tag:<t>|repo:<r> — filter by scope type"},
			{Name: "--repo R", Desc: "show all rules applicable to this repo (global + repo-specific + tag-derived)"},
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{
			"force rules list",
			"force rules list --repo myrepo",
			"force rules list --scope global",
			"force rules list --scope tag:payments",
		})
	if helped {
		return 0
	}
	if perr != nil {
		return 2
	}

	var rules []store.FleetRulesRow

	if *repoFlag != "" {
		// Use ResolveRulesForRepo for the full tag-aware resolution.
		var err error
		rules, err = store.ResolveRulesForRepo(db, *repoFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "rules list: %v\n", err)
			return 1
		}
		if *scopeFlag != "" {
			// Further filter the resolved set by scope.
			clause, sArgs, sErr := scopeToFilter(*scopeFlag)
			if sErr != nil {
				fmt.Fprintf(os.Stderr, "rules list: --scope: %v\n", sErr)
				return 1
			}
			// We have to filter in-memory since we already did the resolution.
			var filtered []store.FleetRulesRow
			for _, r := range rules {
				if matchesScope(r.AgentScope, clause, sArgs) {
					filtered = append(filtered, r)
				}
			}
			rules = filtered
		}
	} else {
		// Direct DB query against active rows.
		query := `SELECT id, rule_key, IFNULL(category,''), agent_scope,
		                 render_to, IFNULL(enforced_by,''), content,
		                 IFNULL(content_hash,''), version,
		                 IFNULL(active_from,''), IFNULL(active_until,''),
		                 IFNULL(promoted_by_experiment_id,0),
		                 IFNULL(created_by,''), IFNULL(created_at,'')
		            FROM FleetRules
		           WHERE active_until = ''`
		queryArgs := []any{}
		if *scopeFlag != "" {
			clause, sArgs, sErr := scopeToFilter(*scopeFlag)
			if sErr != nil {
				fmt.Fprintf(os.Stderr, "rules list: --scope: %v\n", sErr)
				return 1
			}
			query += " AND " + clause
			queryArgs = append(queryArgs, sArgs...)
		}
		query += " ORDER BY rule_key"

		rows, err := db.Query(query, queryArgs...)
		if err != nil {
			fmt.Fprintf(os.Stderr, "rules list: query: %v\n", err)
			return 1
		}
		defer rows.Close()
		for rows.Next() {
			var r store.FleetRulesRow
			if err := rows.Scan(
				&r.ID, &r.RuleKey, &r.Category, &r.AgentScope,
				&r.RenderTo, &r.EnforcedBy, &r.Content,
				&r.ContentHash, &r.Version,
				&r.ActiveFrom, &r.ActiveUntil,
				&r.PromotedByExperimentID,
				&r.CreatedBy, &r.CreatedAt,
			); err != nil {
				fmt.Fprintf(os.Stderr, "rules list: scan: %v\n", err)
				continue
			}
			rules = append(rules, r)
		}
		if err := rows.Err(); err != nil {
			fmt.Fprintf(os.Stderr, "rules list: rows: %v\n", err)
			return 1
		}
	}

	if len(rules) == 0 {
		fmt.Println("(no active FleetRules matching the given filters)")
		return 0
	}

	fmt.Printf("%-5s %-35s %-22s %-12s %s\n", "ID", "RULE_KEY", "AGENT_SCOPE", "CATEGORY", "RENDER_TO")
	fmt.Println(strings.Repeat("-", 95))
	for _, r := range rules {
		fmt.Printf("%-5d %-35s %-22s %-12s %s\n",
			r.ID,
			truncate(r.RuleKey, 35),
			truncate(r.AgentScope, 22),
			truncate(r.Category, 12),
			r.RenderTo)
	}
	return 0
}

// matchesScope does a minimal in-memory check for the scope-filter clause
// after ResolveRulesForRepo has already filtered by the DB. We only need
// to check agent_scope equality.
func matchesScope(agentScope, clause string, args []any) bool {
	if clause == `agent_scope = 'senate:*'` {
		return agentScope == "senate:*"
	}
	if len(args) == 1 {
		return agentScope == args[0]
	}
	return true
}

func rulesUpgradeCmd(db *sql.DB, args []string) int {
	// Reorder flags before positionals so `force rules upgrade <key> --to-scope ...` works.
	args = reorderFlagsFirst(args, map[string]bool{})
	fs := flag.NewFlagSet("rules upgrade", flag.ContinueOnError)
	toScopeFlag := fs.String("to-scope", "", "new agent_scope value (required): senate:*|senate:tag:<t>|senate:<repo>")
	helped, perr := parseSubcommandFlags(fs, args, "rules upgrade",
		"Update FleetRules.agent_scope for the given rule key. Validates the new scope syntax before writing.",
		[]flagDoc{
			{Name: "--to-scope S", Desc: "new agent_scope: senate:*|senate:tag:<t>|senate:<repo> (required)"},
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{
			"force rules upgrade my-rule-key --to-scope senate:*",
			"force rules upgrade my-rule-key --to-scope senate:tag:payments",
			"force rules upgrade my-rule-key --to-scope senate:myrepo",
		})
	if helped {
		return 0
	}
	if perr != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: force rules upgrade <rule-key> --to-scope <scope>")
		return 1
	}
	ruleKey := rest[0]
	if *toScopeFlag == "" {
		fmt.Fprintln(os.Stderr, "rules upgrade: --to-scope is required")
		return 1
	}

	if err := validateRuleScope(*toScopeFlag); err != nil {
		fmt.Fprintf(os.Stderr, "rules upgrade: invalid scope: %v\n", err)
		return 1
	}

	res, err := db.Exec(
		`UPDATE FleetRules SET agent_scope = ? WHERE rule_key = ? AND active_until = ''`,
		*toScopeFlag, ruleKey,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rules upgrade: %v\n", err)
		return 1
	}
	n, err := res.RowsAffected()
	if err != nil {
		fmt.Fprintf(os.Stderr, "rules upgrade: rows affected: %v\n", err)
		return 1
	}
	if n == 0 {
		fmt.Fprintf(os.Stderr, "rules upgrade: no active rule found with key %q\n", ruleKey)
		return 1
	}
	fmt.Printf("Rule %q agent_scope updated to %q (%d row(s) affected).\n", ruleKey, *toScopeFlag, n)
	return 0
}
