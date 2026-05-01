// D3 fix-loop-1 β2 — `force proposed-features` CLI parity for the
// dashboard's /api/proposed-features mutating endpoints (Pattern P25).
//
// Subcommands:
//
//   force proposed-features list [--status <pending|promoted|archived|all>]
//   force proposed-features suppress <id> --rationale <txt> [--operator <email>] [--days <N>]
//   force proposed-features score <id> --value <low|medium|high> --complexity <low|medium|high>
//                                       --rationale <txt> [--operator <email>]
//   force proposed-features promote <id> [--deadline <date>] [--operator <email>]
package main

import (
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"force-orchestrator/internal/store"
)

func cmdProposedFeatures(db *sql.DB, args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: force proposed-features <list|suppress|score|promote> [args]")
		return 1
	}
	verb := args[0]
	rest := args[1:]
	switch verb {
	case "list":
		return cmdProposedFeaturesList(db, rest)
	case "suppress":
		return cmdProposedFeaturesSuppress(db, rest)
	case "score":
		return cmdProposedFeaturesScore(db, rest)
	case "promote":
		return cmdProposedFeaturesPromote(db, rest)
	default:
		fmt.Fprintf(os.Stderr, "unknown verb: %s (want list|suppress|score|promote)\n", verb)
		return 1
	}
}

func cmdProposedFeaturesList(db *sql.DB, args []string) int {
	status := ""
	for i := 0; i < len(args); i++ {
		if args[i] == "--status" && i+1 < len(args) {
			status = args[i+1]
			i++
		}
	}
	if status == "all" {
		status = ""
	}
	rows, err := store.ListProposedFeatures(db, status)
	if err != nil {
		fmt.Fprintf(os.Stderr, "list failed: %v\n", err)
		return 1
	}
	if len(rows) == 0 {
		fmt.Println("No proposed features.")
		return 0
	}
	for _, r := range rows {
		fmt.Printf("[%d] (%s) %s — value=%s complexity=%s occ=%d status=%s\n",
			r.ID, r.Source, truncatePF(r.ObservationSummary, 80),
			r.ValueScore, r.ComplexityScore, r.OccurrenceCount, r.Status)
	}
	return 0
}

func cmdProposedFeaturesSuppress(db *sql.DB, args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: force proposed-features suppress <id> --rationale <txt> [--operator <email>] [--days <N>]")
		return 1
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil || id <= 0 {
		fmt.Fprintln(os.Stderr, "invalid feature id")
		return 1
	}
	operator := "default@operator"
	rationale := ""
	days := 0
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--operator":
			if i+1 < len(args) {
				operator = args[i+1]
				i++
			}
		case "--rationale":
			if i+1 < len(args) {
				rationale = args[i+1]
				i++
			}
		case "--days":
			if i+1 < len(args) {
				if d, err := strconv.Atoi(args[i+1]); err == nil {
					days = d
				}
				i++
			}
		}
	}
	if len(strings.TrimSpace(rationale)) < 20 {
		fmt.Fprintln(os.Stderr, "rationale must be ≥ 20 chars")
		return 1
	}
	// Resolve the fingerprint from the DB.
	var fp string
	err = db.QueryRow(`SELECT IFNULL(fingerprint,'') FROM ProposedFeatures WHERE id = ?`, id).Scan(&fp)
	if err == sql.ErrNoRows || fp == "" {
		fmt.Fprintf(os.Stderr, "feature %d not found or has empty fingerprint\n", id)
		return 1
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "query failed: %v\n", err)
		return 1
	}
	until := time.Time{}
	if days > 0 {
		until = time.Now().UTC().Add(time.Duration(days) * 24 * time.Hour)
	}
	suppID, err := store.SuppressProposedFeature(db, fp, rationale, until, operator)
	if err != nil {
		fmt.Fprintf(os.Stderr, "suppress failed: %v\n", err)
		return 1
	}
	store.LogAudit(db, operator, "proposed-feature-suppress", int(id),
		fmt.Sprintf("suppression %d installed via CLI", suppID))
	fmt.Printf("OK — suppression %d installed for fingerprint %s\n", suppID, fp)
	return 0
}

func cmdProposedFeaturesScore(db *sql.DB, args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: force proposed-features score <id> [--value <low|medium|high>] [--complexity <low|medium|high>] --rationale <txt> [--operator <email>]")
		return 1
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil || id <= 0 {
		fmt.Fprintln(os.Stderr, "invalid feature id")
		return 1
	}
	value, complexity, rationale := "", "", ""
	operator := "default@operator"
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--value":
			if i+1 < len(args) {
				value = args[i+1]
				i++
			}
		case "--complexity":
			if i+1 < len(args) {
				complexity = args[i+1]
				i++
			}
		case "--rationale":
			if i+1 < len(args) {
				rationale = args[i+1]
				i++
			}
		case "--operator":
			if i+1 < len(args) {
				operator = args[i+1]
				i++
			}
		}
	}
	if rationale == "" {
		fmt.Fprintln(os.Stderr, "--rationale required")
		return 1
	}
	err = store.OverrideProposedFeatureScore(db, id, value, complexity, rationale, operator)
	if err != nil {
		fmt.Fprintf(os.Stderr, "score override failed: %v\n", err)
		return 1
	}
	store.LogAudit(db, operator, "proposed-feature-score-override", int(id),
		fmt.Sprintf("value=%s complexity=%s via CLI", value, complexity))
	fmt.Printf("OK — feature %d score updated\n", id)
	return 0
}

func cmdProposedFeaturesPromote(db *sql.DB, args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: force proposed-features promote <id> [--deadline <date>] [--operator <email>]")
		return 1
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil || id <= 0 {
		fmt.Fprintln(os.Stderr, "invalid feature id")
		return 1
	}
	deadline := ""
	operator := "default@operator"
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--deadline":
			if i+1 < len(args) {
				deadline = args[i+1]
				i++
			}
		case "--operator":
			if i+1 < len(args) {
				operator = args[i+1]
				i++
			}
		}
	}
	if err := store.PromoteProposedFeature(db, id, deadline, operator); err != nil {
		fmt.Fprintf(os.Stderr, "promote failed: %v\n", err)
		return 1
	}
	store.LogAudit(db, operator, "proposed-feature-promote", int(id),
		fmt.Sprintf("deadline=%s via CLI", deadline))
	fmt.Printf("OK — feature %d promoted (deadline=%s)\n", id, deadline)
	return 0
}

// truncatePF is a small helper for list output. The internal/util package
// has TruncateStr but cmd/force/ doesn't otherwise import util; renamed
// to avoid shadowing watch.go's existing `truncate`.
func truncatePF(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
