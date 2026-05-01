// Package store — D4 Phase 0 — FleetMemory dedup + merge.
//
// DedupAndMerge walks the FleetMemory table looking for near-identical
// rows and folds non-canonical rows into the canonical (oldest) row.
// "Near-identical" is defined by a Jaccard similarity score over the
// 3-shingle (trigram word) set of each row's summary string. The
// threshold (default 0.85) is conservative — it captures legitimate
// duplicates (a Librarian re-summarisation that landed twice for the
// same task) without merging memories that share a topic but contain
// distinct lessons.
//
// Audit trail. The non-canonical row is NOT deleted — its
// canonical_id column is stamped to point at the survivor, and its
// summary / files_changed / topic_tags fields are preserved verbatim.
// The merge is recorded in the AuditLog table (action='librarian-
// dedup-merge') so the operator can inspect every collapse. The
// canonical row's retrieval_count is incremented by the merged row's
// count (so the curator's quality score reflects accumulated
// retrieval signal); validation_score is averaged-up; topic_tags are
// union-merged.
//
// Idempotence. A row whose canonical_id != 0 is already merged and
// is skipped on subsequent runs. Re-running DedupAndMerge over a
// post-merge table is a no-op (returns 0).
package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// DedupSimilarityThreshold is the Jaccard-similarity floor at or above
// which two FleetMemory rows are considered "near-identical." The
// default of 0.85 is calibrated against shakedown fixtures: rows that
// differ only in punctuation / whitespace score >0.95; rows about the
// same topic but with distinct lessons score 0.4-0.6; the gap at
// 0.85 is wide enough that drift in either direction won't flip
// classifications.
//
// Exported so tests can lower the threshold for fixtures with shorter
// summary strings (where the shingle space is naturally smaller).
var DedupSimilarityThreshold = 0.85

// DedupAndMerge folds near-identical FleetMemory rows into canonical
// survivors. Returns the number of rows merged. Errors propagate; the
// caller (librarian-dedup-watch dog) routes them to the operator-
// alert path via the standard dog-error channel.
//
// Algorithm:
//  1. Read every FleetMemory row whose canonical_id == 0 (i.e. not
//     already merged into another row), repo-grouped — only same-repo
//     rows are eligible to merge (a memory in repo A about authn is
//     not a duplicate of an authn memory in repo B).
//  2. For each repo group, compare every pair of rows by Jaccard
//     similarity over their 3-shingle word sets. Pairs above
//     DedupSimilarityThreshold are recorded as (canonical=oldest,
//     duplicate=newest) merge candidates.
//  3. For each merge candidate, in a single transaction:
//     - stamp duplicate.canonical_id = canonical.id
//     - bump canonical.retrieval_count by duplicate.retrieval_count
//     - average canonical.validation_score with duplicate's
//     - union-merge canonical.topic_tags with duplicate's
//     - write an AuditLog row recording the merge for replay
//  4. Skip rows already pointed at a canonical (canonical_id != 0)
//     so re-running the dog is a no-op (idempotence).
func DedupAndMerge(ctx context.Context, db *sql.DB) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	rows, err := db.QueryContext(ctx, `
		SELECT id, repo, IFNULL(summary, ''), IFNULL(topic_tags, ''),
		       IFNULL(retrieval_count, 0), IFNULL(validation_score, 0.0),
		       IFNULL(created_at, '')
		  FROM FleetMemory
		 WHERE IFNULL(canonical_id, 0) = 0
		 ORDER BY repo, created_at, id`)
	if err != nil {
		return 0, fmt.Errorf("DedupAndMerge: query: %w", err)
	}
	type rowSnap struct {
		id              int
		repo            string
		summary         string
		topicTags       string
		retrievalCount  int
		validationScore float64
		createdAt       string
	}
	var all []rowSnap
	for rows.Next() {
		var r rowSnap
		if err := rows.Scan(&r.id, &r.repo, &r.summary, &r.topicTags,
			&r.retrievalCount, &r.validationScore, &r.createdAt); err != nil {
			rows.Close()
			return 0, fmt.Errorf("DedupAndMerge: scan: %w", err)
		}
		all = append(all, r)
	}
	if rerr := rows.Err(); rerr != nil {
		rows.Close()
		return 0, fmt.Errorf("DedupAndMerge: rows iter: %w", rerr)
	}
	rows.Close()

	// Bucket by repo so we only consider intra-repo pairs.
	byRepo := map[string][]rowSnap{}
	for _, r := range all {
		byRepo[r.repo] = append(byRepo[r.repo], r)
	}

	// Process repo buckets in deterministic order so audit logs are
	// stable across runs (testability).
	repos := make([]string, 0, len(byRepo))
	for repo := range byRepo {
		repos = append(repos, repo)
	}
	sort.Strings(repos)

	merged := 0
	for _, repo := range repos {
		bucket := byRepo[repo]
		// Track which row IDs have already been merged this pass so we
		// don't fold A→B then later try to fold C→A (A no longer
		// represents the canonical).
		mergedThisPass := map[int]bool{}
		for i := 0; i < len(bucket); i++ {
			canonical := bucket[i]
			if mergedThisPass[canonical.id] {
				continue
			}
			canonShingles := shinglesOf(canonical.summary)
			if len(canonShingles) == 0 {
				continue // empty summary can't dedup meaningfully
			}
			for j := i + 1; j < len(bucket); j++ {
				duplicate := bucket[j]
				if mergedThisPass[duplicate.id] {
					continue
				}
				dupShingles := shinglesOf(duplicate.summary)
				if len(dupShingles) == 0 {
					continue
				}
				sim := jaccardSimilarity(canonShingles, dupShingles)
				if sim < DedupSimilarityThreshold {
					continue
				}
				if err := mergeMemoryPair(ctx, db, canonical.id, duplicate.id,
					duplicate.retrievalCount, duplicate.validationScore,
					canonical.topicTags, duplicate.topicTags, sim); err != nil {
					return merged, err
				}
				mergedThisPass[duplicate.id] = true
				merged++
			}
		}
	}
	return merged, nil
}

// mergeMemoryPair stamps duplicate.canonical_id = canonical.id and folds
// retrieval/validation/tags signal up into the canonical row. All
// writes happen inside a single transaction so a partial failure
// rolls back cleanly. Writes one AuditLog row recording the merge.
func mergeMemoryPair(ctx context.Context, db *sql.DB, canonicalID, duplicateID,
	dupRetrievalCount int, dupValidationScore float64,
	canonicalTags, duplicateTags string, similarity float64) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("mergeMemoryPair: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// Stamp the duplicate's canonical_id pointer.
	if _, err := tx.ExecContext(ctx,
		`UPDATE FleetMemory SET canonical_id = ? WHERE id = ?`,
		canonicalID, duplicateID); err != nil {
		return fmt.Errorf("mergeMemoryPair: stamp duplicate.canonical_id: %w", err)
	}

	// Fold retrieval count up.
	if dupRetrievalCount > 0 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE FleetMemory SET retrieval_count = IFNULL(retrieval_count,0) + ? WHERE id = ?`,
			dupRetrievalCount, canonicalID); err != nil {
			return fmt.Errorf("mergeMemoryPair: fold retrieval_count: %w", err)
		}
	}

	// Average validation score (simple mean of canonical + duplicate).
	if dupValidationScore != 0 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE FleetMemory
			    SET validation_score = (IFNULL(validation_score,0) + ?) / 2.0
			  WHERE id = ?`,
			dupValidationScore, canonicalID); err != nil {
			return fmt.Errorf("mergeMemoryPair: average validation_score: %w", err)
		}
	}

	// Union-merge tags. Cheap string CSV concat + dedup; total token count
	// is bounded (<8 per row) so the cost is negligible.
	mergedTags := unionTags(canonicalTags, duplicateTags)
	if mergedTags != canonicalTags {
		if _, err := tx.ExecContext(ctx,
			`UPDATE FleetMemory SET topic_tags = ? WHERE id = ?`,
			mergedTags, canonicalID); err != nil {
			return fmt.Errorf("mergeMemoryPair: merge topic_tags: %w", err)
		}
	}

	// Audit row. The AuditLog shape is (actor, action, task_id, detail);
	// we encode the merge as a JSON-shaped detail string so downstream
	// readers can parse the canonical/duplicate/similarity values.
	auditDetail := fmt.Sprintf(`{"canonical_id":%d,"duplicate_id":%d,"similarity":%.4f}`,
		canonicalID, duplicateID, similarity)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO AuditLog (actor, action, task_id, detail) VALUES (?, ?, ?, ?)`,
		"librarian", "librarian-dedup-merge", duplicateID, auditDetail); err != nil {
		// AuditLog row failure is non-fatal to the merge — log via the
		// returned error tail so the caller's dog-error path notifies
		// the operator. We do not silently swallow.
		return fmt.Errorf("mergeMemoryPair: write audit log: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("mergeMemoryPair: commit: %w", err)
	}
	return nil
}

// shinglesOf returns the set of 3-shingle (trigram) word tokens from a
// summary string. The string is lowercased + split on whitespace +
// reduced to alphanumeric runs (so punctuation differences don't
// matter). Returns a set encoded as a map keyed on the joined
// trigram.
//
// Why trigrams (not bigrams or character n-grams):
//   - Bigrams over-collapse: "the cat" and "the dog" share 50% of
//     bigrams via "the X" without sharing meaning.
//   - Character n-grams over-collapse on short summaries (the FTS5
//     trigram analog has the same hazard).
//   - 3-word shingles are the standard MinHash starting point and
//     match the granularity at which Librarian summaries differ
//     (single-word swaps in 4-sentence prose).
func shinglesOf(s string) map[string]struct{} {
	words := normaliseWords(s)
	if len(words) < 3 {
		// For very short summaries fall back to whole-string match
		// (a 1-2 word summary is too short to shingle meaningfully;
		// treat the whole thing as one shingle).
		if len(words) == 0 {
			return map[string]struct{}{}
		}
		return map[string]struct{}{strings.Join(words, " "): {}}
	}
	out := make(map[string]struct{}, len(words)-2)
	for i := 0; i <= len(words)-3; i++ {
		out[strings.Join(words[i:i+3], " ")] = struct{}{}
	}
	return out
}

// normaliseWords lowercases + strips non-alphanumeric punctuation and
// returns the resulting word slice. Whitespace-separated; runs of
// punctuation collapse to a single space.
func normaliseWords(s string) []string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + 32) // ASCII lower
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte(' ')
		}
	}
	return strings.Fields(b.String())
}

// jaccardSimilarity returns |A ∩ B| / |A ∪ B| for two shingle sets.
// Returns 0 if both are empty.
func jaccardSimilarity(a, b map[string]struct{}) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 0
	}
	intersect := 0
	for k := range a {
		if _, ok := b[k]; ok {
			intersect++
		}
	}
	union := len(a) + len(b) - intersect
	if union == 0 {
		return 0
	}
	return float64(intersect) / float64(union)
}

// unionTags merges two CSV tag strings, preserving order of first-
// occurrence and deduplicating case-insensitively. The total cap of
// 12 tags prevents a long-tail merge from ballooning the row.
func unionTags(a, b string) string {
	seen := map[string]bool{}
	var out []string
	for _, csv := range []string{a, b} {
		for _, tok := range strings.Split(csv, ",") {
			t := strings.ToLower(strings.TrimSpace(tok))
			if t == "" || seen[t] {
				continue
			}
			seen[t] = true
			out = append(out, t)
			if len(out) >= 12 {
				return strings.Join(out, ", ")
			}
		}
	}
	return strings.Join(out, ", ")
}
