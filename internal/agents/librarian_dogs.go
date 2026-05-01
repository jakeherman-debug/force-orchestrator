// Package agents — D4 Phase 0 — Librarian-evolution dogs.
//
// Five new dogs land in this slice:
//
//  1. librarian-dedup-watch       — store.DedupAndMerge
//  2. librarian-quality-recompute — store.RecomputeFreshnessScores
//  3. librarian-conflict-watch    — store.DetectConflicts
//  4. librarian-hypothesis-emit   — store.EmitHypothesisCandidates
//  5. claude-md-drift-watch       — scans CLAUDE.md invariants vs code
//
// Each dog is registered in dogs.go (cooldowns + dogOrder + the
// runDog switch) so RunDogs picks them up at the inquisitor cadence.
//
// The CLAUDE.md drift dog is the only one that emits PromotionProposals
// (kind='candidate', authored_by='librarian'); it uses the librarian
// Client surface so the emit goes through the same pipeline as the
// store-helper-driven dogs (consistency invariant — every Librarian-
// emitted candidate looks the same on the EC ratification side).
package agents

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"force-orchestrator/internal/clients/librarian"
	"force-orchestrator/internal/store"
)

// dogLibrarianDedup runs the FleetMemory dedup-and-merge pass.
func dogLibrarianDedup(ctx context.Context, db *sql.DB, logger interface{ Printf(string, ...any) }) error {
	merged, err := store.DedupAndMerge(ctx, db)
	if err != nil {
		return fmt.Errorf("librarian-dedup-watch: %w", err)
	}
	if merged > 0 {
		logger.Printf("Dog librarian-dedup-watch: merged %d row(s)", merged)
	} else {
		logger.Printf("Dog librarian-dedup-watch: no merges")
	}
	return nil
}

// dogLibrarianQualityRecompute decays freshness_score for every row.
func dogLibrarianQualityRecompute(ctx context.Context, db *sql.DB, logger interface{ Printf(string, ...any) }) error {
	updated, err := store.RecomputeFreshnessScores(ctx, db)
	if err != nil {
		return fmt.Errorf("librarian-quality-recompute: %w", err)
	}
	logger.Printf("Dog librarian-quality-recompute: updated %d row(s)", updated)
	return nil
}

// dogLibrarianConflictWatch fires the deterministic contradiction
// detector. The LLM-judge upgrade path is reserved for Phase 3 (Senate),
// where the Senator's own LLM call is the natural place to escalate
// pre-screen-positive pairs to deeper analysis.
func dogLibrarianConflictWatch(ctx context.Context, db *sql.DB, logger interface{ Printf(string, ...any) }) error {
	inserted, err := store.DetectConflicts(ctx, db)
	if err != nil {
		return fmt.Errorf("librarian-conflict-watch: %w", err)
	}
	if inserted > 0 {
		logger.Printf("Dog librarian-conflict-watch: %d new ticket(s)", inserted)
	} else {
		logger.Printf("Dog librarian-conflict-watch: no new tickets")
	}
	return nil
}

// dogLibrarianHypothesisEmit walks high-signal memories and emits
// candidate PromotionProposals. The store helper is idempotent
// (hypothesis_emitted_at + source_memory_id stamp) so re-runs are no-ops.
func dogLibrarianHypothesisEmit(ctx context.Context, db *sql.DB, logger interface{ Printf(string, ...any) }) error {
	emitted, err := store.EmitHypothesisCandidates(ctx, db)
	if err != nil {
		return fmt.Errorf("librarian-hypothesis-emit: %w", err)
	}
	logger.Printf("Dog librarian-hypothesis-emit: emitted %d candidate(s)", emitted)
	return nil
}

// dogClaudeMDDriftWatch is the weekly drift scanner. It reads the
// rendered CLAUDE.md, extracts the invariant section bodies (top-level
// H2 headers), and produces a Librarian-emitted candidate
// PromotionProposal whenever an invariant looks under-enforced (no
// matching FleetRules row OR no matching code-time check).
//
// Phase 0 ships a minimal scanner: it walks CLAUDE.md sections and
// emits a candidate per section whose body contains a distinctive
// invariant marker ("MUST", "NEVER", "Pattern P*") AND whose key is
// not already represented in FleetRules.rule_key (case-insensitive
// substring match). This is the deterministic floor; Phase 3
// (Senate) replaces the matcher with an LLM-judge that reasons
// about whether the invariant is enforced in committed code.
func dogClaudeMDDriftWatch(ctx context.Context, db *sql.DB, lib librarian.Client, logger interface{ Printf(string, ...any) }) error {
	claudemdPath := findClaudeMDPath()
	if claudemdPath == "" {
		// Daemon CWD is force-orchestrator/, so a missing CLAUDE.md is
		// odd but not fatal — log and exit.
		logger.Printf("Dog claude-md-drift-watch: CLAUDE.md not found in CWD or parents — skipping")
		return nil
	}
	sections, err := parseClaudeMDSections(claudemdPath)
	if err != nil {
		return fmt.Errorf("claude-md-drift-watch: parse: %w", err)
	}

	// Collect existing FleetRules keys for the substring match.
	existingKeys, err := loadFleetRulesKeys(ctx, db)
	if err != nil {
		return fmt.Errorf("claude-md-drift-watch: load FleetRules keys: %w", err)
	}

	emitted := 0
	for _, section := range sections {
		if !sectionHasInvariantMarker(section.Body) {
			continue
		}
		if sectionAlreadyRepresented(section.Title, existingKeys) {
			continue
		}
		// Construct candidate.
		ruleKey := claudeMDDriftRuleKey(section.Title)
		// Skip if a candidate already exists for this drift signal
		// (idempotence — re-running the dog after a week shouldn't
		// produce duplicate candidates).
		var pendingCount int
		_ = db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM PromotionProposals
			 WHERE kind = 'candidate'
			   AND authored_by = 'librarian'
			   AND rule_key = ?
			   AND IFNULL(ratified_at, '') = ''
			   AND IFNULL(rejected_at, '') = ''`,
			ruleKey).Scan(&pendingCount)
		if pendingCount > 0 {
			continue
		}
		evidence := fmt.Sprintf(
			`{"source":"claude-md-drift-watch","section_title":%q,"claudemd_path":%q,"body_excerpt":%q}`,
			section.Title, claudemdPath, truncateForJSON(section.Body, 500))
		_, emitErr := lib.EmitCandidate(ctx, librarian.Candidate{
			HypothesisKey: ruleKey,
			HypothesisRaw: section.Body,
			EvidenceJSON:  evidence,
		})
		if emitErr != nil {
			return fmt.Errorf("claude-md-drift-watch: emit candidate for %q: %w", section.Title, emitErr)
		}
		emitted++
	}
	logger.Printf("Dog claude-md-drift-watch: emitted %d new candidate(s) from %s", emitted, claudemdPath)
	return nil
}

// claudeMDSection is one extracted H2 section from CLAUDE.md.
type claudeMDSection struct {
	Title string // text after the leading "## "
	Body  string // everything between this H2 and the next H1/H2
}

// parseClaudeMDSections does a minimal markdown walk: lines starting
// with "## " open a new section; H1 ("# ") closes the current section
// without opening a new one (Phase 0's CLAUDE.md uses H1 only for the
// document title).
func parseClaudeMDSections(path string) ([]claudeMDSection, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)
	var sections []claudeMDSection
	var cur *claudeMDSection
	var bodyB strings.Builder
	flush := func() {
		if cur == nil {
			return
		}
		cur.Body = strings.TrimSpace(bodyB.String())
		sections = append(sections, *cur)
		cur = nil
		bodyB.Reset()
	}
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "## ") {
			flush()
			cur = &claudeMDSection{Title: strings.TrimSpace(strings.TrimPrefix(line, "## "))}
			continue
		}
		if strings.HasPrefix(line, "# ") {
			flush()
			continue
		}
		if cur != nil {
			bodyB.WriteString(line)
			bodyB.WriteByte('\n')
		}
	}
	flush()
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return sections, nil
}

// sectionHasInvariantMarker returns true if the section body contains
// at least one of the markers indicating an enforceable invariant.
func sectionHasInvariantMarker(body string) bool {
	lower := strings.ToLower(body)
	for _, marker := range []string{"must ", "must not", "never ", "always ", "pattern p"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// sectionAlreadyRepresented checks whether any FleetRules.rule_key
// substring-matches the section title (case-insensitive). This is
// Phase 0's deterministic floor; Phase 3 graduates to a stronger
// match (LLM judge or AST-walked enforcement check).
func sectionAlreadyRepresented(title string, existingKeys []string) bool {
	t := strings.ToLower(title)
	// Strip punctuation so "Per-agent capability profiles" matches
	// rule keys like "agent-capability-profiles".
	t = strings.NewReplacer(",", "", ".", "", "'", "").Replace(t)
	for _, key := range existingKeys {
		k := strings.ReplaceAll(strings.ToLower(key), "-", " ")
		k = strings.ReplaceAll(k, "_", " ")
		// Loose substring match in either direction.
		if strings.Contains(t, k) || strings.Contains(k, slugForMatch(title)) {
			return true
		}
	}
	return false
}

// slugForMatch produces a hyphen-light search key from a title for
// the reverse-direction match.
func slugForMatch(title string) string {
	out := strings.ToLower(title)
	out = strings.ReplaceAll(out, " ", " ")
	return out
}

// claudeMDDriftRuleKey builds the candidate rule_key for an emitted
// drift candidate from the section title. Stable across runs so the
// idempotence check (count of pending candidates with the same key)
// works.
func claudeMDDriftRuleKey(title string) string {
	slug := strings.ToLower(title)
	slug = strings.NewReplacer(
		" ", "-",
		"/", "-",
		",", "",
		".", "",
		"(", "",
		")", "",
		"'", "",
	).Replace(slug)
	for strings.Contains(slug, "--") {
		slug = strings.ReplaceAll(slug, "--", "-")
	}
	slug = strings.Trim(slug, "-")
	return "claude-md-drift-" + slug
}

// truncateForJSON returns a JSON-safe (escaped quotes) excerpt no
// longer than n bytes.
func truncateForJSON(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// findClaudeMDPath looks for CLAUDE.md in the current working dir
// and walks up to 4 parents (covers the daemon-CWD case + the
// worktree-rooted run case). Returns "" if not found.
func findClaudeMDPath() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for i := 0; i < 5; i++ {
		candidate := filepath.Join(dir, "CLAUDE.md")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

// loadFleetRulesKeys returns every active FleetRules.rule_key.
func loadFleetRulesKeys(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT rule_key FROM FleetRules WHERE IFNULL(active_until,'') = ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}
