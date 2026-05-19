// Package store: D8 Track 2 — Features.blast_radius_json read/write helpers.
//
// blast_radius_json lives on BountyBoard (Features are BountyBoard rows
// with type='Feature') and stores the per-Feature blast-radius computed
// at convoy-creation time. Schema shape:
//
//	{
//	  "modified_symbols":         [{"symbol_path","kind","file_path","line_number"}],
//	  "affected_consumer_repos":  ["repo-a","repo-b"],
//	  "auto_included_tasks":      [<task_id>, ...]
//	}
//
// SetFeatureBlastRadius marshals the Go struct to JSON and writes it.
// GetFeatureBlastRadius reads the column and unmarshals it; missing
// rows / empty strings yield the zero BlastRadiusRecord (intentional —
// pre-T2 Features have empty '{}' which decodes to the zero value).
//
// Per CLAUDE.md "no silent failures": both helpers return error.
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
)

// BlastRadiusRecord is the on-disk shape of Features.blast_radius_json.
// The fields mirror the roadmap line 2186-2192 schema. Empty slices are
// preserved on round-trip (json.Marshal emits [] not null) so consumers
// can branch on len() rather than nil-vs-empty.
type BlastRadiusRecord struct {
	ModifiedSymbols       []BlastRadiusSymbol `json:"modified_symbols"`
	AffectedConsumerRepos []string            `json:"affected_consumer_repos"`
	AutoIncludedTasks     []int               `json:"auto_included_tasks"`
	// APIConsumers is the D15 additive extension: repos that consume this
	// Feature's provider APIs (from CrossRepoAPIDependencies). Populated
	// by PostProcessBlastRadius after the symbol-level computation.
	// Empty on pre-D15 records; callers must treat nil and [] as equivalent.
	APIConsumers []string `json:"api_consumers,omitempty"`
}

// BlastRadiusSymbol is one entry in BlastRadiusRecord.ModifiedSymbols.
// Mirrors the (subset of) graph.Symbol fields the dashboard cares
// about — the graph package's full Symbol type carries Repo too, but
// every modified symbol in a per-Feature record is rooted in the
// Feature's known target repos so we omit it here for compactness.
type BlastRadiusSymbol struct {
	SymbolPath string `json:"symbol_path"`
	Kind       string `json:"kind"`
	FilePath   string `json:"file_path"`
	LineNumber int    `json:"line_number"`
}

// SetFeatureBlastRadius marshals rec and writes it to BountyBoard.blast_radius_json
// for featureID. Empty rec is allowed — it serializes to a JSON object with
// empty arrays, which is the canonical "no blast radius computed" shape.
//
// Returns error per CLAUDE.md "no silent failures"; the caller surfaces.
func SetFeatureBlastRadius(db *sql.DB, featureID int, rec BlastRadiusRecord) error {
	if db == nil {
		return fmt.Errorf("SetFeatureBlastRadius: db is nil")
	}
	if featureID <= 0 {
		return fmt.Errorf("SetFeatureBlastRadius: featureID required")
	}
	// Normalize nil slices → empty so the JSON payload never carries
	// `null` (which would parse back as nil, breaking len() callers).
	if rec.ModifiedSymbols == nil {
		rec.ModifiedSymbols = []BlastRadiusSymbol{}
	}
	if rec.AffectedConsumerRepos == nil {
		rec.AffectedConsumerRepos = []string{}
	}
	if rec.AutoIncludedTasks == nil {
		rec.AutoIncludedTasks = []int{}
	}
	body, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("SetFeatureBlastRadius(feature=%d): marshal: %w", featureID, err)
	}
	res, err := db.Exec(`UPDATE BountyBoard SET blast_radius_json = ? WHERE id = ?`,
		string(body), featureID)
	if err != nil {
		return fmt.Errorf("SetFeatureBlastRadius(feature=%d): %w", featureID, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("SetFeatureBlastRadius(feature=%d): no row updated (Feature missing?)", featureID)
	}
	return nil
}

// GetFeatureBlastRadius reads BountyBoard.blast_radius_json for featureID.
// Returns the zero BlastRadiusRecord when the column is empty / '{}' /
// missing — pre-T2 Features have the default value and that decodes to
// the zero record.
//
// sql.ErrNoRows is returned wrapped when featureID does not exist.
func GetFeatureBlastRadius(db *sql.DB, featureID int) (BlastRadiusRecord, error) {
	if db == nil {
		return BlastRadiusRecord{}, fmt.Errorf("GetFeatureBlastRadius: db is nil")
	}
	var raw string
	err := db.QueryRow(`SELECT IFNULL(blast_radius_json, '{}') FROM BountyBoard WHERE id = ?`,
		featureID).Scan(&raw)
	if err != nil {
		return BlastRadiusRecord{}, fmt.Errorf("GetFeatureBlastRadius(feature=%d): %w", featureID, err)
	}
	if raw == "" || raw == "{}" {
		return BlastRadiusRecord{}, nil
	}
	var rec BlastRadiusRecord
	if uErr := json.Unmarshal([]byte(raw), &rec); uErr != nil {
		return BlastRadiusRecord{}, fmt.Errorf("GetFeatureBlastRadius(feature=%d): unmarshal: %w", featureID, uErr)
	}
	return rec, nil
}
