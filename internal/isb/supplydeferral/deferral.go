// Package supplydeferral owns the SUPPLY-* deferral payload schema +
// the RecordDeferral / ListPendingDeferrals helpers (D5 Phase 0).
//
// When a SUPPLY-* rule queries CodeArtifact and the SDK call returns
// a credentials/auth error, the rule's deferral path inserts a
// SecurityFindings row with disposition='token_expired' and a JSON
// payload capturing the dep-set + branch + commit context. The
// supply-token-recheck dog (P4) and ConvoyReview gate (P4) consume
// these rows when CodeArtifact recovers.
//
// P0 ships the schema + helpers; P1+ wires them into the actual
// SUPPLY-001..005 rule bodies. Defining the helpers in a stable
// package now means every rule's deferral path lands through one
// audited choke-point — Pattern P-SupplyDeferral (per docs/roadmap.md
// § D5 anti-cheat) walks the rule files and rejects sites that
// don't route through here.
//
// Package separation: this lives OUTSIDE internal/isb/rules to break
// the import cycle (internal/store's test imports internal/isb/rules,
// and we need to import internal/store here).
package supplydeferral

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"force-orchestrator/internal/isb/scanners/manifests"
	"force-orchestrator/internal/store"
)

// DeferralDisposition is the SecurityFindings disposition string used
// when a SUPPLY-* rule defers to a future re-check because the AWS
// token expired (or any other auth-class condition the codeartifact
// client maps to ErrTokenExpired).
const DeferralDisposition = "token_expired"

// DeferralPayload is the typed JSON shape stored in
// SecurityFindings.message when disposition='token_expired'. The dog
// + ConvoyReview gate read this back on recovery to re-resolve the
// dep set. Keep the JSON shape backward-compatible — D5 P4 + P5
// both round-trip these payloads.
type DeferralPayload struct {
	RuleKey      string                  `json:"rule_key"`
	ManifestPath string                  `json:"manifest_path"`
	DepsAdded    []manifests.Dependency  `json:"deps_added"`
	Branch       string                  `json:"branch"`
	CommitSHA    string                  `json:"commit_sha"`
	DeferredAt   time.Time               `json:"deferred_at"`
}

// MarshalJSON renders the payload as compact JSON for the
// SecurityFindings.message column. Stable key order makes the row
// hash deterministic for the dedup helper below.
func (p DeferralPayload) MarshalJSON() ([]byte, error) {
	type alias DeferralPayload
	// Sort deps by (name, version) so two equivalent payloads emit
	// the same JSON (downstream dedup hashes the bytes).
	cp := alias(p)
	sort.SliceStable(cp.DepsAdded, func(i, j int) bool {
		if cp.DepsAdded[i].Name == cp.DepsAdded[j].Name {
			return cp.DepsAdded[i].Version < cp.DepsAdded[j].Version
		}
		return cp.DepsAdded[i].Name < cp.DepsAdded[j].Name
	})
	return json.Marshal(cp)
}

// DeferralRow is the read-side shape returned by ListPendingDeferrals.
// Combines the SecurityFindings row identity + the parsed payload.
type DeferralRow struct {
	FindingID int
	TaskID    int
	Payload   DeferralPayload
	CreatedAt string
}

// RecordDeferral inserts a SecurityFindings row carrying the
// supplied payload. Returns the new row id. Idempotent under the
// dedup window: a second call with the same (rule_key, branch,
// dep-set) within `dedupWindow` is a no-op (returns 0, nil).
//
// The dedup window is `time.Hour` — short enough that a token
// recovery within the window doesn't double-flag, long enough that
// a flap doesn't burn through the audit table. Tunable via
// SystemConfig key 'supply_deferral_dedup_window' (number of seconds).
//
// taskID is the SOURCE task that triggered the SUPPLY rule (i.e. the
// astromech's commit task), not the ISBReview infrastructure task.
func RecordDeferral(db *sql.DB, taskID int, p DeferralPayload) (int, error) {
	if db == nil {
		return 0, errors.New("RecordDeferral: nil db")
	}
	if p.RuleKey == "" {
		return 0, errors.New("RecordDeferral: RuleKey required")
	}
	if p.Branch == "" {
		return 0, errors.New("RecordDeferral: Branch required")
	}
	if p.DeferredAt.IsZero() {
		p.DeferredAt = time.Now().UTC()
	}

	body, err := json.Marshal(p)
	if err != nil {
		return 0, fmt.Errorf("RecordDeferral: marshal payload: %w", err)
	}
	digest := payloadDigest(p)

	// Dedup: skip insert if we already have a token_expired row with
	// the same digest in the dedup window. The digest covers
	// (RuleKey, Branch, Dep set) — DeferredAt + CommitSHA are
	// excluded from the hash so re-fires from the same astromech
	// re-running on the same branch with a new commit-but-same-dep-
	// set don't re-flag. ManifestPath IS in the hash so renaming a
	// manifest does flag.
	windowSec := 3600
	if cfg := store.GetConfig(db, "supply_deferral_dedup_window", ""); cfg != "" {
		var n int
		if _, err := fmt.Sscanf(cfg, "%d", &n); err == nil && n > 0 {
			windowSec = n
		}
	}
	var dupID int
	err = db.QueryRow(`
		SELECT id FROM SecurityFindings
		WHERE bureau = 'isb'
		  AND rule_id = ?
		  AND disposition = ?
		  AND bypass_reason = ?
		  AND created_at >= datetime('now', ?)
		ORDER BY id DESC LIMIT 1`,
		p.RuleKey, DeferralDisposition, digest, fmt.Sprintf("-%d seconds", windowSec)).Scan(&dupID)
	if err == nil && dupID > 0 {
		return 0, nil
	}

	// We use the SecurityFindings columns:
	//   message       — the JSON payload (read by dog + ConvoyReview)
	//   bypass_reason — the dedup digest (so the lookup above is a
	//                   single index hit; bypass_audit_id stays empty
	//                   to keep the override-audit dashboard clean)
	id, err := store.InsertSecurityFinding(db, store.SecurityFinding{
		TaskID:       taskID,
		Bureau:       "isb",
		RuleID:       p.RuleKey,
		Severity:     "advise",
		FilePath:     p.ManifestPath,
		Message:      string(body),
		CommitSHA:    p.CommitSHA,
		Disposition:  DeferralDisposition,
		BypassReason: digest,
	})
	if err != nil {
		return 0, fmt.Errorf("RecordDeferral: insert: %w", err)
	}
	return id, nil
}

// ListPendingDeferrals returns every SUPPLY-* deferral row matching
// branch (or every row when branch == ""). Rows whose disposition has
// been flipped to 'resolved_late' / 'superseded' / 'closed' are
// excluded — only the still-pending ones come back.
func ListPendingDeferrals(db *sql.DB, branch string) ([]DeferralRow, error) {
	if db == nil {
		return nil, errors.New("ListPendingDeferrals: nil db")
	}
	q := `SELECT id, task_id, message, IFNULL(created_at, '')
	      FROM SecurityFindings
	      WHERE bureau = 'isb'
	        AND disposition = ?
	        AND rule_id LIKE 'SUPPLY-%'`
	args := []any{DeferralDisposition}
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("ListPendingDeferrals: %w", err)
	}
	defer rows.Close()

	var out []DeferralRow
	for rows.Next() {
		var (
			id, taskID int
			msg, ts    string
		)
		if scanErr := rows.Scan(&id, &taskID, &msg, &ts); scanErr != nil {
			return nil, fmt.Errorf("ListPendingDeferrals: scan: %w", scanErr)
		}
		var p DeferralPayload
		if err := json.Unmarshal([]byte(msg), &p); err != nil {
			// Malformed payload: surface the row so the operator can
			// SetDisposition(closed) on it manually. Keep a non-fatal
			// shape so a single bad row doesn't take down the dog.
			p.RuleKey = "SUPPLY-UNREADABLE-PAYLOAD"
		}
		if branch != "" && p.Branch != branch {
			continue
		}
		out = append(out, DeferralRow{
			FindingID: id,
			TaskID:    taskID,
			Payload:   p,
			CreatedAt: ts,
		})
	}
	if rErr := rows.Err(); rErr != nil {
		return nil, fmt.Errorf("ListPendingDeferrals: rows.Err: %w", rErr)
	}
	return out, nil
}

// payloadDigest produces a stable 16-hex-char hash of (rule, branch,
// manifest, dep-set). Used as the dedup key. We hash a normalized
// string rather than json.Marshal output to keep the digest stable
// across JSON-key-ordering changes.
func payloadDigest(p DeferralPayload) string {
	deps := make([]string, 0, len(p.DepsAdded))
	for _, d := range p.DepsAdded {
		deps = append(deps, fmt.Sprintf("%s|%s|%s", d.Ecosystem, d.Name, d.Version))
	}
	sort.Strings(deps)
	raw := strings.Join([]string{p.RuleKey, p.Branch, p.ManifestPath, strings.Join(deps, ",")}, "::")
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:8])
}
