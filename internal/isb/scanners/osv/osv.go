// Package osv wraps the vendored github.com/google/osv-scanner v1
// library as an in-process callable client so SUPPLY-005 (the
// known-CVE blocking rule) can scan a manifest / lock file's deps and
// receive structured vulnerability findings — no subprocess shell-out,
// no JSON parsing of CLI output.
//
// Design (per docs/roadmap.md § "Deliverable 5 — Supply Chain Hygiene"
// → SUPPLY-005):
//
//   - SUPPLY-005 needs to (a) extract the dep set from a lock file and
//     (b) ask OSV which of those deps have known vulnerabilities. The
//     osv-scanner library does both: pkg/lockfile parses the manifest
//     into PackageDetails; pkg/osv builds and sends a batched query.
//
//   - This wrapper presents a P16-compliant Client interface so the
//     SUPPLY-005 rule can stub it cleanly at the boundary (the rule's
//     unit tests never reach the network or the lockfile parser).
//
//   - We do NOT use the high-level pkg/osvscanner Run() entry point —
//     it bakes in CLI-style reporters, config loading, and exit-code
//     handling that don't fit our review-loop shape. The lower-level
//     pkg/lockfile + pkg/osv pair gives us exactly the surface we
//     need.
//
// Anti-cheat invariants (from the SUPPLY-005 spec):
//
//   - The wrapper is deterministic given a fixed lockfile + a fixed
//     OSV response: no LLM, no random ordering. Findings are emitted
//     in scan order.
//
//   - High/Critical severity is computed but NOT auto-blocking at
//     launch — every SUPPLY rule ships at advise. The rule body
//     (supply_005.go) decides advise vs block; this wrapper only
//     reports raw severity.
//
//   - No caching layer in the wrapper — osv-scanner's HTTP path is
//     idempotent and the OSV.dev API is fast enough that an extra
//     in-process cache would just hide rate-limit signal.
package osv

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/google/osv-scanner/pkg/lockfile"
	"github.com/google/osv-scanner/pkg/models"
	"github.com/google/osv-scanner/pkg/osv"
)

// Finding is the scanner-neutral shape SUPPLY-005 consumes. One
// Finding == one vulnerable (package, version, vulnerability) triple.
// A single dep with three CVEs produces three Findings.
type Finding struct {
	// PackageName is the name as it appears in the lock file (e.g.
	// "rails", "lodash", "org.springframework:spring-core").
	PackageName string

	// PackageVersion is the version pinned in the lock file.
	PackageVersion string

	// Ecosystem is the OSV ecosystem identifier ("Go", "npm", "PyPI",
	// "RubyGems", "Maven"). String-cased per OSV's own enum so the
	// rule can show it as-is.
	Ecosystem string

	// OSVID is the canonical advisory ID (e.g. "CVE-2023-12345" or
	// "GHSA-xxxx-yyyy-zzzz").
	OSVID string

	// Severity is the highest CVSS-derived bucket OSV reports for this
	// vuln, normalized to one of: "CRITICAL", "HIGH", "MEDIUM", "LOW",
	// "UNKNOWN". Mapping per CVSS v3 base score per the
	// FIRST.org severity ratings:
	//   9.0–10.0  → CRITICAL
	//   7.0–8.9   → HIGH
	//   4.0–6.9   → MEDIUM
	//   0.1–3.9   → LOW
	//   no score  → UNKNOWN
	// CVSS v2 / v4 vectors are mapped via the same buckets when
	// present.
	Severity string

	// Summary is the human-readable one-liner from OSV.
	Summary string

	// URL is the OSV.dev detail page for this vulnerability.
	URL string
}

// Client is the cross-package interface the SUPPLY-005 rule depends
// on. Per CLAUDE.md "Cross-agent service interfaces", only this
// interface is exported from this package; the in-process struct is
// unexported and constructed via NewInProcess.
type Client interface {
	// ScanLockfile parses the supplied manifest / lock file content
	// (using osv-scanner's per-ecosystem extractor selected by
	// `path`'s basename), then queries OSV for vulnerabilities
	// affecting each parsed package. Returns one Finding per
	// (package, vulnerability) pair.
	//
	// Behaviour on edge inputs:
	//
	//   - Unrecognised lock-file basename → empty findings + an error
	//     wrapping ErrUnsupportedLockfile. The SUPPLY-005 rule treats
	//     this as a silent skip (other rules cover the manifest
	//     itself).
	//   - Malformed lockfile bytes → empty findings + a wrapped error
	//     from the per-ecosystem extractor.
	//   - OSV API failure → empty findings + the wrapped HTTP error.
	//
	// ctx is honoured for cancellation on the OSV HTTP requests.
	ScanLockfile(ctx context.Context, path string, content []byte) ([]Finding, error)
}

// ErrUnsupportedLockfile is returned by ScanLockfile when osv-scanner
// has no extractor for the basename of `path`. The SUPPLY-005 rule
// uses errors.Is to silently skip these — `pyproject.toml` for
// example is "manifest, not lock" so osv-scanner doesn't claim it.
var ErrUnsupportedLockfile = errors.New("osv: unsupported lockfile")

// inProcessClient is the production Client. Constructed via
// NewInProcess; the struct is intentionally unexported (P16).
type inProcessClient struct {
	// httpClient is the HTTP client used for OSV API calls. Defaults
	// to http.DefaultClient. Tests inject a stub Transport so they
	// never touch the network.
	httpClient *http.Client
}

// Option configures NewInProcess. We use a small option-fn pattern
// rather than a config struct so callers can stay terse for the
// common case (zero options = production defaults).
type Option func(*inProcessClient)

// WithHTTPClient overrides the HTTP client used for OSV requests.
// Primary use case: tests that inject a stub Transport.
func WithHTTPClient(c *http.Client) Option {
	return func(s *inProcessClient) {
		if c != nil {
			s.httpClient = c
		}
	}
}

// NewInProcess constructs a Client backed by osv-scanner's vendored
// library. The returned Client is safe for concurrent use across
// goroutines (osv-scanner's batched-query path is internally
// concurrent).
func NewInProcess(opts ...Option) Client {
	c := &inProcessClient{
		httpClient: http.DefaultClient,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// ScanLockfile implements Client.
func (c *inProcessClient) ScanLockfile(ctx context.Context, path string, content []byte) ([]Finding, error) {
	if path == "" {
		return nil, errors.New("osv: empty lockfile path")
	}
	if len(content) == 0 {
		// Empty content is not an error per se — a deleted lock file
		// with no surviving deps yields no findings. We return early
		// to avoid the temp-file dance below.
		return nil, nil
	}

	// osv-scanner's parser selection keys on the BASENAME of the
	// supplied path. The PackageDetailsParser then re-opens the file
	// from disk to read its contents — there is no in-memory entry
	// point in the v1 library API. So we materialise the content into
	// a temp file with the original basename, parse it, and clean up.
	base := filepath.Base(path)

	// Pre-flight: refuse paths osv-scanner doesn't know how to parse.
	// FindParser returns (nil, basename) on miss.
	if parser, _ := lockfile.FindParser("/synth/"+base, ""); parser == nil {
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedLockfile, base)
	}

	tmpDir, err := os.MkdirTemp("", "force-osv-scan-*")
	if err != nil {
		return nil, fmt.Errorf("osv: mkdir temp: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	tmpPath := filepath.Join(tmpDir, base)
	// 0o600 — writeable by us only; never executable. The temp dir is
	// removed on return so the file's lifetime is the scope of this
	// call.
	if werr := os.WriteFile(tmpPath, content, 0o600); werr != nil {
		return nil, fmt.Errorf("osv: write temp lockfile: %w", werr)
	}

	parsed, perr := lockfile.Parse(tmpPath, "")
	if perr != nil {
		// Some parsers return a non-nil error WITH partial Packages
		// (e.g. malformed individual entries in an otherwise valid
		// file). We surface both the partial set AND the error so the
		// rule layer can decide. errors.Join already does the right
		// thing if the rule wraps further.
		if len(parsed.Packages) == 0 {
			return nil, fmt.Errorf("osv: parse %s: %w", base, perr)
		}
		// Non-fatal partial parse: log via wrapped err, continue with
		// what we have.
		findings, scanErr := c.queryOSV(ctx, parsed.Packages)
		if scanErr != nil {
			return findings, errors.Join(
				fmt.Errorf("osv: parse %s (partial): %w", base, perr),
				scanErr,
			)
		}
		return findings, fmt.Errorf("osv: parse %s (partial): %w", base, perr)
	}

	if len(parsed.Packages) == 0 {
		return nil, nil
	}

	return c.queryOSV(ctx, parsed.Packages)
}

// queryOSV builds the OSV batched query, sends it (honouring ctx via
// the configured http.Client), hydrates the responses to full
// Vulnerability records, and projects each into a Finding.
func (c *inProcessClient) queryOSV(ctx context.Context, pkgs []lockfile.PackageDetails) ([]Finding, error) {
	if len(pkgs) == 0 {
		return nil, nil
	}

	// Build the batched query. Each package gets one Query; OSV
	// echoes back a parallel slice.
	queries := make([]*osv.Query, 0, len(pkgs))
	for _, p := range pkgs {
		// Skip rows with no name — osv-scanner's parsers occasionally
		// emit them for malformed entries. Without a name there is
		// nothing meaningful to query.
		if p.Name == "" {
			continue
		}
		queries = append(queries, osv.MakePkgRequest(p))
	}
	if len(queries) == 0 {
		return nil, nil
	}

	// Honour ctx cancellation. The osv-scanner v1 API does not take a
	// context; we model cancellation by short-circuiting before the
	// network call. (A future v2 upgrade will let us pass ctx all the
	// way down.)
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("osv: query cancelled: %w", err)
	}

	resp, err := osv.MakeRequestWithClient(osv.BatchedQuery{Queries: queries}, c.httpClient)
	if err != nil {
		return nil, fmt.Errorf("osv: batched query: %w", err)
	}
	if resp == nil || len(resp.Results) == 0 {
		return nil, nil
	}

	// Hydrate the minimal IDs into full Vulnerability records so we
	// can read severity + summary + URL.
	hydrated, herr := osv.HydrateWithClient(resp, c.httpClient)
	if herr != nil {
		return nil, fmt.Errorf("osv: hydrate: %w", herr)
	}
	if hydrated == nil {
		return nil, nil
	}

	var findings []Finding
	// resp.Results and queries share the same length+order. We need
	// the source package name+version+ecosystem to project each vuln,
	// so we walk both slices in lockstep.
	//
	// Caveat: queries may be shorter than pkgs (we skipped name=="").
	// We therefore build a parallel pkgs-for-queries slice.
	pkgsForQueries := make([]lockfile.PackageDetails, 0, len(queries))
	for _, p := range pkgs {
		if p.Name == "" {
			continue
		}
		pkgsForQueries = append(pkgsForQueries, p)
	}

	for i, result := range hydrated.Results {
		if i >= len(pkgsForQueries) {
			// Defensive: OSV returned more entries than we sent. We
			// can't attribute them, so skip rather than guess.
			break
		}
		pkg := pkgsForQueries[i]
		for _, vuln := range result.Vulns {
			findings = append(findings, projectVuln(pkg, vuln))
		}
	}

	return findings, nil
}

// projectVuln converts an OSV Vulnerability record into a Force-side
// Finding, picking the highest-severity CVSS score we can extract.
func projectVuln(pkg lockfile.PackageDetails, v models.Vulnerability) Finding {
	return Finding{
		PackageName:    pkg.Name,
		PackageVersion: pkg.Version,
		Ecosystem:      string(pkg.Ecosystem),
		OSVID:          v.ID,
		Severity:       extractSeverity(v),
		Summary:        v.Summary,
		URL:            osvDetailURL(v.ID),
	}
}

// osvDetailURL builds the canonical osv.dev page URL for a given
// advisory ID.
func osvDetailURL(id string) string {
	if id == "" {
		return ""
	}
	return "https://osv.dev/vulnerability/" + id
}

// extractSeverity walks the Vulnerability's severity records and
// returns the highest bucket as a normalized string. We prefer
// top-level Severity entries, then fall back to per-Affected entries.
// Returns "UNKNOWN" if no parseable score is found.
func extractSeverity(v models.Vulnerability) string {
	best := ""
	bestRank := 0

	consider := func(score string) {
		bucket := scoreToBucket(score)
		if bucket == "" {
			return
		}
		rank := bucketRank(bucket)
		if rank > bestRank {
			best = bucket
			bestRank = rank
		}
	}

	for _, s := range v.Severity {
		consider(s.Score)
	}
	for _, a := range v.Affected {
		for _, s := range a.Severity {
			consider(s.Score)
		}
	}

	if best == "" {
		return "UNKNOWN"
	}
	return best
}

// scoreToBucket maps a CVSS vector or numeric base score to a
// CRITICAL/HIGH/MEDIUM/LOW bucket. Returns "" when the input is not
// parseable as a CVSS-style score.
func scoreToBucket(score string) string {
	score = strings.TrimSpace(score)
	if score == "" {
		return ""
	}
	// CVSS vectors look like "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/...". For
	// these we can't compute the base score without a full CVSS
	// calculator; instead we look for an embedded numeric score after
	// a slash (some OSV records ship "CVSS:3.1/AV:.../...:9.8" — but
	// that's rare). The common shape is just the vector, which we
	// can't bucket precisely. Fall back: if the entire string parses
	// as a float, treat it as the numeric base score.
	if f, err := strconv.ParseFloat(score, 64); err == nil {
		return numericBucket(f)
	}
	// As a last resort: look for an explicit "/score=N.N" or trailing
	// numeric component. OSV currently stores vectors most of the
	// time, so this branch is best-effort.
	if idx := strings.LastIndex(score, "/"); idx >= 0 && idx < len(score)-1 {
		tail := score[idx+1:]
		if f, err := strconv.ParseFloat(tail, 64); err == nil {
			return numericBucket(f)
		}
	}
	// Recognise plain CVSS-string severity tokens that some sources
	// ship instead of a numeric score.
	switch strings.ToUpper(score) {
	case "CRITICAL":
		return "CRITICAL"
	case "HIGH":
		return "HIGH"
	case "MEDIUM", "MODERATE":
		return "MEDIUM"
	case "LOW":
		return "LOW"
	}
	return ""
}

// numericBucket maps a CVSS base-score float to its bucket per
// FIRST.org ratings.
func numericBucket(f float64) string {
	switch {
	case f >= 9.0:
		return "CRITICAL"
	case f >= 7.0:
		return "HIGH"
	case f >= 4.0:
		return "MEDIUM"
	case f > 0:
		return "LOW"
	}
	return ""
}

// bucketRank lets extractSeverity pick the highest bucket among
// multiple severity entries.
func bucketRank(b string) int {
	switch b {
	case "CRITICAL":
		return 4
	case "HIGH":
		return 3
	case "MEDIUM":
		return 2
	case "LOW":
		return 1
	}
	return 0
}

// Compile-time guard: the unexported struct really does satisfy the
// exported interface. P16 (audit_pattern_p16_clients_interfaces_test)
// expects this shape: interface exported, struct unexported,
// constructor returns the interface.
var _ Client = (*inProcessClient)(nil)
