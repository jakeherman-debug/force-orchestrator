package store

// at_lookup.go — D3 fix-loop-1 (slice δ).
//
// Acceptance-Test (AT) identifier lookup helper. The roadmap concern
// #8 (exit criterion 14c) requires that every AT lookup in production
// be SCOPED BY CONVOY: a bare `at_id` is ambiguous because the AT id
// space is convoy-local (the convoy spec authors AT ids inside its
// verification_spec_json blob; the same id can re-occur in a
// different convoy with a different meaning).
//
// This file publishes the lookup contract; consumers (slice γ's
// verification_spec_json reader, future fix-task spawners) MUST go
// through GetATByID rather than parsing the JSON inline with a bare
// id key.
//
// Pattern P20 — TestPattern_P20_ATIdScopeIntegrity (authored by slice
// α) is the AST regression that walks production code and rejects
// bare-at_id lookups. This helper is the canonical replacement.
//
// Storage shape: AT specs live in Convoys.verification_spec_json. The
// JSON shape is `{"acceptance_tests": [{"at_id": "AT-1", "title":
// "...", "rubric": "..."}, ...]}`. We deliberately do NOT promote
// these into a relational table — they are immutable per-convoy
// contracts authored at convoy creation time; a JOIN-able table
// would be the wrong shape (no cross-convoy queries are needed).

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ATSpec is the in-memory shape of a single acceptance test parsed
// out of a convoy's verification_spec_json blob. Field names match
// the JSON keys exactly so the same struct can decode the wire shape.
type ATSpec struct {
	ATID      string `json:"at_id"`
	Title     string `json:"title"`
	Rubric    string `json:"rubric"`
	OracleSQL string `json:"oracle_sql,omitempty"`
}

// verificationSpecEnvelope is the top-level JSON object of the
// verification_spec_json column.
type verificationSpecEnvelope struct {
	AcceptanceTests []ATSpec `json:"acceptance_tests"`
}

// ErrATNotFound is returned by GetATByID when the (convoy_id, at_id)
// pair does not resolve to a row in the convoy's spec. Callers
// SHOULD distinguish this from a "convoy missing" error so they can
// route the right operator-visible signal.
var ErrATNotFound = errors.New("store: AT not found in convoy spec")

// ErrConvoyMissing is returned by GetATByID when the convoy_id
// does not resolve to a Convoys row.
var ErrConvoyMissing = errors.New("store: convoy not found")

// ErrSpecEmpty is returned when the convoy exists but its
// verification_spec_json is empty / unset. Distinguishes "no spec
// authored yet" from "spec authored but at_id absent" — the former
// is a convoy-lifecycle condition (Captain hasn't authored), the
// latter is a real lookup miss.
var ErrSpecEmpty = errors.New("store: convoy verification_spec_json is empty")

// ErrATIDRequired is the fail-closed sentinel returned when a caller
// passes empty at_id. The whole point of this helper is to refuse
// bare lookups; an empty at_id is a degenerate case of that.
var ErrATIDRequired = errors.New("store: at_id is required (compound-key contract)")

// ErrConvoyIDRequired is the fail-closed sentinel for convoy_id <= 0.
// The compound-key contract requires a real convoy scope.
var ErrConvoyIDRequired = errors.New("store: convoy_id must be > 0 (compound-key contract)")

// GetATByID resolves an acceptance test by the compound key
// (convoy_id, at_id). Both are required; passing zero / empty for
// either returns the matching ErrConvoyIDRequired / ErrATIDRequired
// fail-closed sentinel.
//
// This is the ONLY supported AT lookup in production code. A bare
// at_id query (e.g. SELECT ... FROM ConvoyATs WHERE at_id = ?) is
// rejected by Pattern P20's AST regression — same rationale as the
// `payload LIKE '%"convoy_id":N,%'` ban in CLAUDE.md: the at_id
// space is convoy-local, so a global lookup is structurally a bug.
func GetATByID(db *sql.DB, convoyID int, atID string) (ATSpec, error) {
	if db == nil {
		return ATSpec{}, fmt.Errorf("store.GetATByID: db is required")
	}
	if convoyID <= 0 {
		return ATSpec{}, ErrConvoyIDRequired
	}
	atID = strings.TrimSpace(atID)
	if atID == "" {
		return ATSpec{}, ErrATIDRequired
	}

	var specJSON string
	err := db.QueryRow(`SELECT IFNULL(verification_spec_json, '') FROM Convoys WHERE id = ?`, convoyID).Scan(&specJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return ATSpec{}, ErrConvoyMissing
	}
	if err != nil {
		return ATSpec{}, fmt.Errorf("store.GetATByID: load convoy %d: %w", convoyID, err)
	}
	if strings.TrimSpace(specJSON) == "" {
		return ATSpec{}, ErrSpecEmpty
	}

	var env verificationSpecEnvelope
	if err := json.Unmarshal([]byte(specJSON), &env); err != nil {
		return ATSpec{}, fmt.Errorf("store.GetATByID: parse spec for convoy %d: %w", convoyID, err)
	}
	for _, at := range env.AcceptanceTests {
		if at.ATID == atID {
			return at, nil
		}
	}
	return ATSpec{}, fmt.Errorf("%w: convoy=%d at_id=%s", ErrATNotFound, convoyID, atID)
}

// ListATsForConvoy returns all acceptance tests in a convoy's spec,
// in spec-declared order. Empty list (with nil error) is returned
// when the convoy has no spec authored yet — an empty slice is the
// right shape for "iterate over the ATs the convoy currently has."
//
// Use this for "list every AT" callers (e.g. the dashboard panel,
// the orthogonal-overlap scheduler). Use GetATByID for "I have an
// at_id from somewhere, look it up." Mixing those concerns is what
// led to the bare-lookup pattern in the first place.
func ListATsForConvoy(db *sql.DB, convoyID int) ([]ATSpec, error) {
	if db == nil {
		return nil, fmt.Errorf("store.ListATsForConvoy: db is required")
	}
	if convoyID <= 0 {
		return nil, ErrConvoyIDRequired
	}
	var specJSON string
	err := db.QueryRow(`SELECT IFNULL(verification_spec_json, '') FROM Convoys WHERE id = ?`, convoyID).Scan(&specJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrConvoyMissing
	}
	if err != nil {
		return nil, fmt.Errorf("store.ListATsForConvoy: load convoy %d: %w", convoyID, err)
	}
	if strings.TrimSpace(specJSON) == "" {
		// Empty spec is not a lookup error — the caller wants the
		// list, and the list is empty.
		return nil, nil
	}
	var env verificationSpecEnvelope
	if err := json.Unmarshal([]byte(specJSON), &env); err != nil {
		return nil, fmt.Errorf("store.ListATsForConvoy: parse spec for convoy %d: %w", convoyID, err)
	}
	return env.AcceptanceTests, nil
}
