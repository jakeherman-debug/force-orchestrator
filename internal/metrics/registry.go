// Package metrics implements the YAML-loadable metric registry.
//
// Metrics are reviewed code, not runtime SQL — they live under
// metrics/<name>/<date>.{sql,test.sql,manifest.yaml,changelog.md}. The
// registry round-trips them into MetricVersions rows so experiments
// can reference (metric_name, version) pairs immutably; once
// published, a (name, version) pair never changes (deprecation marks
// retirement but leaves the SQL frozen for replay).
//
// D3 Phase 1 ships the skeleton: a manifest parser, a single sample
// metric (captain_rejection_rate) exercising the round-trip, and the
// register / lookup / list helpers. EC-proposed metrics in future
// phases use the same path.
package metrics

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var (
	// ErrMetricNotFound — LookupMetric called against an unknown
	// (name, version) pair, or the latest-active query returned no row.
	ErrMetricNotFound = errors.New("metrics: metric version not found")

	// ErrMetricExists — RegisterMetric called for a (name, version)
	// pair that already exists. Versions are immutable.
	ErrMetricExists = errors.New("metrics: metric version already registered")

	// ErrManifestInvalid — manifest YAML failed to parse or is missing
	// required fields.
	ErrManifestInvalid = errors.New("metrics: manifest invalid")
)

// Manifest mirrors the YAML shape from paired-runs.md § Metric Registry.
// The YAML loader is deliberately minimal — full YAML support is not
// needed here; the file uses a small, controlled subset of keys.
type Manifest struct {
	Name        string
	Version     string
	Direction   string // 'higher_is_better' | 'lower_is_better'
	Unit        string
	Description string
}

// Metric is the in-memory shape of a registered metric version.
// Maps 1:1 to a MetricVersions row.
type Metric struct {
	Name         string
	Version      string
	SQLBody      string
	TestSQL      string
	ManifestJSON string
	PublishedAt  string
	PublishedBy  string
	Description  string
	DeprecatedAt string
}

// LoadManifest parses a metric manifest file. The format is a thin
// `key: value` YAML subset — sufficient for the registry skeleton.
func LoadManifest(path string) (Manifest, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("read manifest %s: %w", path, err)
	}
	return parseManifest(string(body))
}

func parseManifest(src string) (Manifest, error) {
	m := Manifest{}
	for i, raw := range strings.Split(src, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Block-scalar `description: |\n  …` continuation isn't
		// supported in this minimal loader — descriptions go on one
		// line. EC-proposed metrics in later phases can graduate to a
		// real YAML parser.
		idx := strings.Index(line, ":")
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		val = strings.TrimSpace(strings.TrimPrefix(val, "|"))
		switch key {
		case "name":
			m.Name = val
		case "version":
			m.Version = val
		case "direction":
			m.Direction = val
		case "unit":
			m.Unit = val
		case "description":
			m.Description = val
		case "parameters":
			// Skip — `parameters:` is followed by a YAML list this
			// loader doesn't handle. Consumers that need parameter
			// detail should read the manifest file directly.
		default:
			// Tolerate unknown keys (ignore) — the YAML format is
			// allowed to grow without breaking older consumers.
			_ = i
		}
	}
	if m.Name == "" {
		return m, fmt.Errorf("%w: missing name", ErrManifestInvalid)
	}
	if m.Version == "" {
		return m, fmt.Errorf("%w: missing version", ErrManifestInvalid)
	}
	if m.Direction != "higher_is_better" && m.Direction != "lower_is_better" {
		return m, fmt.Errorf("%w: direction must be higher_is_better or lower_is_better, got %q", ErrManifestInvalid, m.Direction)
	}
	return m, nil
}

// RegisterMetric inserts a new metric version. Idempotent on (name,
// version): a re-registration of an identical content_hash is a no-op;
// a re-registration with a different SQL body returns ErrMetricExists.
func RegisterMetric(ctx context.Context, db *sql.DB, m Metric) error {
	if m.Name == "" || m.Version == "" {
		return fmt.Errorf("RegisterMetric: name and version required")
	}
	if m.SQLBody == "" {
		return fmt.Errorf("RegisterMetric: %s/%s: sql body required", m.Name, m.Version)
	}

	// Idempotency check: same (name, version) with same SQL body → no-op.
	var existingHash string
	err := db.QueryRowContext(ctx, `SELECT sql_content FROM MetricVersions WHERE metric_name = ? AND version = ?`,
		m.Name, m.Version).Scan(&existingHash)
	switch {
	case err == sql.ErrNoRows:
		// Brand new — insert.
	case err != nil:
		return fmt.Errorf("RegisterMetric: lookup existing: %w", err)
	default:
		if sha256Hex(existingHash) == sha256Hex(m.SQLBody) {
			return nil
		}
		return fmt.Errorf("%w: %s/%s already registered with different SQL body", ErrMetricExists, m.Name, m.Version)
	}

	_, err = db.ExecContext(ctx, `
		INSERT INTO MetricVersions
			(metric_name, version, sql_content, test_content, manifest_json,
			 published_at, published_by, description)
		VALUES (?, ?, ?, ?, ?, datetime('now'), ?, ?)
	`,
		m.Name, m.Version, m.SQLBody, m.TestSQL, m.ManifestJSON,
		nullable(m.PublishedBy), nullable(m.Description),
	)
	if err != nil {
		return fmt.Errorf("RegisterMetric insert: %w", err)
	}
	return nil
}

// LookupMetric returns the latest non-deprecated version of the named
// metric. Returns ErrMetricNotFound if no row matches.
func LookupMetric(ctx context.Context, db *sql.DB, name string) (Metric, error) {
	var m Metric
	err := db.QueryRowContext(ctx, `
		SELECT metric_name, version, sql_content, test_content, manifest_json,
		       IFNULL(published_at, ''), IFNULL(published_by, ''),
		       IFNULL(description, ''), IFNULL(deprecated_at, '')
		FROM MetricVersions
		WHERE metric_name = ? AND IFNULL(deprecated_at, '') = ''
		ORDER BY version DESC
		LIMIT 1
	`, name).Scan(&m.Name, &m.Version, &m.SQLBody, &m.TestSQL, &m.ManifestJSON,
		&m.PublishedAt, &m.PublishedBy, &m.Description, &m.DeprecatedAt)
	if err == sql.ErrNoRows {
		return Metric{}, fmt.Errorf("%w: %s", ErrMetricNotFound, name)
	}
	if err != nil {
		return Metric{}, fmt.Errorf("LookupMetric: %w", err)
	}
	return m, nil
}

// LoadFromDir loads every metric under the given directory tree
// (one subdir per metric, manifest + SQL files inside) and registers
// them. Used at daemon boot or on `force metrics sync`.
func LoadFromDir(ctx context.Context, db *sql.DB, root string) (int, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0, fmt.Errorf("read metrics dir: %w", err)
	}
	registered := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		matches, _ := filepath.Glob(filepath.Join(dir, "*.manifest.yaml"))
		for _, mf := range matches {
			version := strings.TrimSuffix(filepath.Base(mf), ".manifest.yaml")
			manifest, err := LoadManifest(mf)
			if err != nil {
				return registered, fmt.Errorf("load %s: %w", mf, err)
			}
			sqlPath := filepath.Join(dir, version+".sql")
			testPath := filepath.Join(dir, version+".test.sql")
			sqlBody, _ := os.ReadFile(sqlPath)
			testBody, _ := os.ReadFile(testPath)
			m := Metric{
				Name:        manifest.Name,
				Version:     manifest.Version,
				SQLBody:     string(sqlBody),
				TestSQL:     string(testBody),
				Description: manifest.Description,
			}
			if err := RegisterMetric(ctx, db, m); err != nil {
				return registered, err
			}
			registered++
		}
	}
	return registered, nil
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func nullable(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}
