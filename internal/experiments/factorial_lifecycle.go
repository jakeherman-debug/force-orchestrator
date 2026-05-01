// Package experiments — D3 Phase 4 factorial-lifecycle entry points.
//
// AuthorFactorialFromYAML and (later in this commit series)
// EnrollFactorialUnit / TerminateFactorial are typed wrappers around
// the existing single-treatment surface. They exist to give callers
// (CLI, daemon, tests) a way to assert the experiment's kind at the
// API boundary instead of discovering a kind-mismatch deep inside the
// Bayesian framework when the cell-mean shape doesn't match the
// analyzer's expectations.
package experiments

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// ErrNotFactorial is returned when a factorial entry point is invoked
// against an experiment whose kind != 'factorial'. Callers can
// errors.Is on this to fall back to the single-treatment path.
var ErrNotFactorial = errors.New("experiments: experiment is not factorial")

// AuthorFactorialFromYAML parses the manifest at yamlPath, validates
// it declares kind: factorial, and delegates to AuthorFromManifest.
// Returns the new Experiments.id.
//
// Single-treatment manifests (kind blank or 'single') are rejected
// with a typed error so a caller cannot accidentally route a single-
// treatment manifest through the factorial entry point and discover
// the mismatch later (e.g. when the analyzer receives an empty cell
// catalogue).
func AuthorFactorialFromYAML(ctx context.Context, db *sql.DB, yamlPath string) (int, error) {
	body, err := os.ReadFile(yamlPath)
	if err != nil {
		return 0, fmt.Errorf("AuthorFactorialFromYAML: read %s: %w", yamlPath, err)
	}
	return AuthorFactorialFromBytes(ctx, db, body)
}

// AuthorFactorialFromBytes is the byte-shape sibling of
// AuthorFactorialFromYAML — used by tests that build a manifest in
// memory.
func AuthorFactorialFromBytes(ctx context.Context, db *sql.DB, raw []byte) (int, error) {
	var m Manifest
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return 0, fmt.Errorf("AuthorFactorialFromBytes: parse: %w", err)
	}
	declared := strings.TrimSpace(m.Kind)
	if declared != KindFactorial {
		return 0, fmt.Errorf("AuthorFactorialFromBytes: manifest kind must be 'factorial' (got %q) — single-treatment manifests should use AuthorFromYAML", declared)
	}
	return AuthorFromManifest(ctx, db, m)
}

// assertFactorialKind reads the experiment's kind column and returns
// ErrNotFactorial if it is not 'factorial'. The factorial entry points
// call this first so a misrouted call produces a typed error instead
// of a silent shape mismatch downstream.
func assertFactorialKind(ctx context.Context, db *sql.DB, experimentID int) error {
	var kind string
	err := db.QueryRowContext(ctx, `SELECT IFNULL(kind, '') FROM Experiments WHERE id = ?`, experimentID).Scan(&kind)
	if err == sql.ErrNoRows {
		return fmt.Errorf("experiments: experiment %d not found", experimentID)
	}
	if err != nil {
		return fmt.Errorf("experiments: load kind: %w", err)
	}
	if kind != KindFactorial {
		return fmt.Errorf("%w: experiment %d has kind=%q", ErrNotFactorial, experimentID, kind)
	}
	return nil
}
