// Package codeartifact defines the client interface for AWS
// CodeArtifact lookups used by the D5 SUPPLY-* ISB rules.
//
// The interface is small and matches the operations the supply-chain
// rules need: per-package metadata (SUPPLY-001/003/004), allowlist
// enumeration (SUPPLY-002), and a domain health probe used by the
// supply-token-recheck dog and ConvoyReview gate.
//
// Pattern P16
// (internal/audittools/audit_pattern_p16_clients_interfaces_test.go)
// enforces that production agent code references the Client interface
// only — never a concrete struct type. Construction is via the
// exported NewInProcess factory; future siblings (gRPC, mock) follow
// the same shape.
//
// Implementations live as siblings:
//   - inprocess.go — backed by aws-sdk-go-v2 + the default credential
//     chain (config.LoadDefaultConfig). Picks up SSO / env / instance-
//     profile creds in that order. Used in production.
//   - mock.go (test-only) — interface stub used by unit tests so the
//     suite never makes a real AWS call.
//
// Auth model (per docs/roadmap.md § D5 "Auth model"): Force does not
// cache tokens. The SDK reads `~/.aws/credentials`, `~/.aws/sso/cache/`,
// and env vars on every call; when the operator's `umt artifacts`
// session expires, all auth-class errors must be wrapped as
// ErrTokenExpired so the SUPPLY rules' deferral path fires correctly.
package codeartifact

import (
	"context"
	"errors"
	"time"
)

// Ecosystem is the package-format enum the SUPPLY rules pass. Values
// map 1-1 to the CodeArtifact `format` field (PyPI, npm, RubyGems,
// Maven). The generic-binary "grpc-generic-prod" format is excluded
// from supply-chain checks (out of scope per roadmap line 1503) —
// callers passing it receive ErrUnsupportedEcosystem.
type Ecosystem string

const (
	EcosystemPyPI     Ecosystem = "pypi"
	EcosystemNPM      Ecosystem = "npm"
	EcosystemRubyGems Ecosystem = "rubygems"
	EcosystemMaven    Ecosystem = "maven"
)

// PackageVersionInfo is the per-version metadata SUPPLY-001/003/004
// need. Source: CodeArtifact's DescribePackageVersion response. Fields
// only carry what the rules actually consume — the full SDK response
// is not surfaced through the interface so future implementations
// (gRPC, shared) stay easy to satisfy.
type PackageVersionInfo struct {
	Ecosystem      Ecosystem
	Name           string    // canonical package name
	Version        string    // exact version string queried
	License        string    // SPDX-style ID when CodeArtifact preserves it; '' when missing
	PublishedAt    time.Time // upstream publish time; zero when CodeArtifact does not surface it
	Status         string    // 'Published' | 'Unfinished' | 'Unlisted' | 'Archived' | 'Disposed' | ...
	Origin         string    // raw publishing origin string (best-effort)
	HomePage       string    // optional metadata
}

// Package is one row of ListPackages output. Used by SUPPLY-002 to
// build the typosquat allowlist from "packages the org has actually
// pulled."
type Package struct {
	Ecosystem Ecosystem
	Name      string
	Namespace string // Maven groupId / npm scope; '' otherwise
}

// Client is the contract between agents and the CodeArtifact service.
type Client interface {
	// DescribePackageVersion returns metadata for a single (ecosystem,
	// name, version) triple. Returns ErrPackageNotFound when the
	// upstream returns ResourceNotFoundException (the package or
	// version is unknown to the registry). Returns ErrTokenExpired on
	// any auth-class error (NoCredentialProviders, ExpiredToken,
	// SSOTokenExpired, AccessDenied/InvalidAccessKeyId, etc.). Returns
	// ErrTransient on throttling / 5xx so callers can choose to retry
	// or skip.
	DescribePackageVersion(ctx context.Context, ecosystem Ecosystem, name, version string) (PackageVersionInfo, error)

	// ListPackages enumerates the names of every package the
	// organisation has ever pulled into the per-ecosystem repository.
	// Pages are walked internally; the returned slice is the full
	// flattened result. Used by the supply-allowlist-refresh dog
	// (P4) — call cost scales with org footprint, so callers that
	// cache should refresh once per day at most.
	ListPackages(ctx context.Context, ecosystem Ecosystem) ([]Package, error)

	// Health is the domain-level reachability probe. The supply-token
	// -recheck dog (every 30 min) calls this; on success it walks the
	// SecurityFindings table for `disposition='token_expired'` rows
	// and replays them. Implementations call DescribeDomain under the
	// hood — cheap, no per-package overhead.
	Health(ctx context.Context) error
}

// Sentinel errors. Callers compare with errors.Is.
var (
	// ErrTokenExpired wraps any AWS auth-class error. The SUPPLY-*
	// rules map this to a SecurityFindings row with
	// disposition='token_expired' (deferral path) and pass through in
	// advise mode rather than blocking the commit.
	ErrTokenExpired = errors.New("codeartifact: AWS token expired or missing — operator must run `umt artifacts`")

	// ErrPackageNotFound wraps ResourceNotFoundException. SUPPLY-001
	// (hallucinated package) maps this to a block-severity finding;
	// other rules treat it as an inconclusive lookup.
	ErrPackageNotFound = errors.New("codeartifact: package or version not found in CodeArtifact")

	// ErrTransient wraps throttling and 5xx errors. Callers may retry
	// (with backoff) or skip; not a token-expired condition.
	ErrTransient = errors.New("codeartifact: transient error (throttle / 5xx)")

	// ErrUnsupportedEcosystem is returned when the caller passes an
	// ecosystem the client does not handle. The grpc-generic-prod
	// repository is excluded from supply-chain checks (different
	// threat model) per docs/roadmap.md line 1503.
	ErrUnsupportedEcosystem = errors.New("codeartifact: unsupported ecosystem (grpc-generic-prod is out of scope)")

	// ErrConfig is returned when SystemConfig defaults / overrides are
	// malformed (e.g. an unparseable region). Distinct from
	// ErrTokenExpired so callers don't blame the operator's SSO flow.
	ErrConfig = errors.New("codeartifact: configuration error")
)

// SystemConfig keys consumed by NewInProcess. Defaults baked into the
// constructor so a fresh DB without explicit config still resolves to
// Upstart's prod domain. Overrides land via store.SetConfig.
const (
	ConfigKeyDomain      = "codeartifact_domain"        // default: code-artifacts-prod
	ConfigKeyDomainOwner = "codeartifact_domain_owner"  // default: 801997600626
	ConfigKeyRegion      = "codeartifact_region"        // default: us-east-1
)

// Defaults exported for the constructor + testing. Mirror the values
// in docs/roadmap.md § D5 "Registry layer".
const (
	DefaultDomain      = "code-artifacts-prod"
	DefaultDomainOwner = "801997600626"
	DefaultRegion      = "us-east-1"
)

// EcosystemRepository returns the per-ecosystem repository name as
// configured in the Upstart CodeArtifact domain. Mirrors the table in
// docs/roadmap.md (pypi-prod / npm-prod / rubygems-prod / maven-prod).
// Returns ErrUnsupportedEcosystem for any unknown value.
func EcosystemRepository(e Ecosystem) (string, error) {
	switch e {
	case EcosystemPyPI:
		return "pypi-prod", nil
	case EcosystemNPM:
		return "npm-prod", nil
	case EcosystemRubyGems:
		return "rubygems-prod", nil
	case EcosystemMaven:
		return "maven-prod", nil
	default:
		return "", ErrUnsupportedEcosystem
	}
}
