// Package codeartifact: in-process client backed by aws-sdk-go-v2.
//
// Construction is via NewInProcess(ctx, db) — the SystemConfig handle
// supplies the domain / domain-owner / region overrides, and
// config.LoadDefaultConfig walks the standard AWS credential chain
// (env vars → ~/.aws/credentials → ~/.aws/sso/cache/ → EC2 IMDS) on
// every call. Force does NOT cache tokens; the SDK reads fresh creds
// per request, so an `umt artifacts` re-auth is picked up immediately.
//
// caClient is the minimal AWS SDK surface this package needs. Defining
// it as an interface (rather than calling the concrete client direct)
// lets the unit tests stub the SDK at the API boundary — no real AWS
// calls in CI. Pattern P16 still holds because the interface lives
// inside this package and the production type embedding is the
// concrete *codeartifact.Client.
package codeartifact

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awsca "github.com/aws/aws-sdk-go-v2/service/codeartifact"
	"github.com/aws/aws-sdk-go-v2/service/codeartifact/types"

	"force-orchestrator/internal/store"
)

// awsCAAPI is the subset of the AWS SDK's *codeartifact.Client used by
// the in-process backing. Stubbed in unit tests.
type awsCAAPI interface {
	DescribePackageVersion(ctx context.Context, input *awsca.DescribePackageVersionInput, opts ...func(*awsca.Options)) (*awsca.DescribePackageVersionOutput, error)
	ListPackages(ctx context.Context, input *awsca.ListPackagesInput, opts ...func(*awsca.Options)) (*awsca.ListPackagesOutput, error)
	DescribeDomain(ctx context.Context, input *awsca.DescribeDomainInput, opts ...func(*awsca.Options)) (*awsca.DescribeDomainOutput, error)
}

// inProcessClient is the production Client backing. Per Pattern P16,
// the type is unexported; callers obtain it through NewInProcess.
type inProcessClient struct {
	api         awsCAAPI
	domain      string
	domainOwner string
	region      string
}

// NewInProcess returns a Client backed by AWS CodeArtifact via the
// default credential chain. Domain / owner / region come from
// SystemConfig (with hardcoded defaults that match Upstart prod).
//
// Returns an error only when the AWS SDK fails to load its
// configuration (e.g. malformed region in env). Token expiry is NOT
// detected here — the SDK lazy-resolves credentials at the first API
// call, so an expired token surfaces inside DescribePackageVersion /
// ListPackages / Health as ErrTokenExpired.
func NewInProcess(ctx context.Context, db *sql.DB) (Client, error) {
	domain := DefaultDomain
	domainOwner := DefaultDomainOwner
	region := DefaultRegion

	if db != nil {
		if v := store.GetConfig(db, ConfigKeyDomain, ""); v != "" {
			domain = v
		}
		if v := store.GetConfig(db, ConfigKeyDomainOwner, ""); v != "" {
			domainOwner = v
		}
		if v := store.GetConfig(db, ConfigKeyRegion, ""); v != "" {
			region = v
		}
	}

	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("%w: LoadDefaultConfig: %v", ErrConfig, err)
	}
	api := awsca.NewFromConfig(cfg)

	return &inProcessClient{
		api:         api,
		domain:      domain,
		domainOwner: domainOwner,
		region:      region,
	}, nil
}

// newInProcessFromAPI is the test-only constructor that injects a
// pre-built awsCAAPI stub. Kept package-private so production code
// cannot accidentally bypass the credential chain.
func newInProcessFromAPI(api awsCAAPI, domain, owner, region string) Client {
	if domain == "" {
		domain = DefaultDomain
	}
	if owner == "" {
		owner = DefaultDomainOwner
	}
	if region == "" {
		region = DefaultRegion
	}
	return &inProcessClient{api: api, domain: domain, domainOwner: owner, region: region}
}

// awsPackageFormat maps our Ecosystem enum to the SDK's PackageFormat.
func awsPackageFormat(e Ecosystem) (types.PackageFormat, error) {
	switch e {
	case EcosystemPyPI:
		return types.PackageFormatPypi, nil
	case EcosystemNPM:
		return types.PackageFormatNpm, nil
	case EcosystemRubyGems:
		return types.PackageFormatRuby, nil
	case EcosystemMaven:
		return types.PackageFormatMaven, nil
	default:
		return "", ErrUnsupportedEcosystem
	}
}

// awsPackageFormatToEcosystem inverts awsPackageFormat for the
// ListPackages return path. Defensive: returns "" on unrecognised SDK
// values rather than panicking — the caller filters those out.
func awsPackageFormatToEcosystem(f types.PackageFormat) Ecosystem {
	switch f {
	case types.PackageFormatPypi:
		return EcosystemPyPI
	case types.PackageFormatNpm:
		return EcosystemNPM
	case types.PackageFormatRuby:
		return EcosystemRubyGems
	case types.PackageFormatMaven:
		return EcosystemMaven
	}
	return ""
}

// DescribePackageVersion looks up one (ecosystem, name, version)
// triple. See client.go for the error contract.
func (c *inProcessClient) DescribePackageVersion(ctx context.Context, eco Ecosystem, name, version string) (PackageVersionInfo, error) {
	if name == "" || version == "" {
		return PackageVersionInfo{}, fmt.Errorf("DescribePackageVersion: name and version are required")
	}
	format, err := awsPackageFormat(eco)
	if err != nil {
		return PackageVersionInfo{}, err
	}
	repo, err := EcosystemRepository(eco)
	if err != nil {
		return PackageVersionInfo{}, err
	}

	out, err := c.api.DescribePackageVersion(ctx, &awsca.DescribePackageVersionInput{
		Domain:         aws.String(c.domain),
		DomainOwner:    aws.String(c.domainOwner),
		Repository:     aws.String(repo),
		Format:         format,
		Package:        aws.String(name),
		PackageVersion: aws.String(version),
	})
	if err != nil {
		return PackageVersionInfo{}, MapAWSError("DescribePackageVersion", err)
	}
	if out == nil || out.PackageVersion == nil {
		return PackageVersionInfo{}, fmt.Errorf("DescribePackageVersion: %w", ErrPackageNotFound)
	}

	pv := out.PackageVersion
	info := PackageVersionInfo{
		Ecosystem: eco,
		Name:      derefString(pv.PackageName, name),
		Version:   derefString(pv.Version, version),
		Status:    string(pv.Status),
	}
	if pv.PublishedTime != nil {
		info.PublishedAt = *pv.PublishedTime
	}
	if pv.HomePage != nil {
		info.HomePage = *pv.HomePage
	}
	if pv.Origin != nil && pv.Origin.OriginType != "" {
		info.Origin = string(pv.Origin.OriginType)
	}
	if len(pv.Licenses) > 0 && pv.Licenses[0].Name != nil {
		info.License = *pv.Licenses[0].Name
	}
	return info, nil
}

// ListPackages enumerates every package in the per-ecosystem repo,
// walking pagination internally.
func (c *inProcessClient) ListPackages(ctx context.Context, eco Ecosystem) ([]Package, error) {
	format, err := awsPackageFormat(eco)
	if err != nil {
		return nil, err
	}
	repo, err := EcosystemRepository(eco)
	if err != nil {
		return nil, err
	}

	var (
		out       []Package
		nextToken *string
	)
	// Page-cap is a defensive belt against runaway pagination — a
	// healthy repo is well under 100k packages, so 1000 pages × 1000
	// page size = 1M packages headroom.
	const maxPages = 1000
	for page := 0; page < maxPages; page++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		resp, err := c.api.ListPackages(ctx, &awsca.ListPackagesInput{
			Domain:      aws.String(c.domain),
			DomainOwner: aws.String(c.domainOwner),
			Repository:  aws.String(repo),
			Format:      format,
			NextToken:   nextToken,
		})
		if err != nil {
			return nil, MapAWSError("ListPackages", err)
		}
		for _, pkg := range resp.Packages {
			if pkg.Package == nil {
				continue
			}
			p := Package{
				Ecosystem: awsPackageFormatToEcosystem(pkg.Format),
				Name:      *pkg.Package,
			}
			if pkg.Namespace != nil {
				p.Namespace = *pkg.Namespace
			}
			// Defensive: fall back to caller-supplied ecosystem when
			// the SDK returns an unmapped format value.
			if p.Ecosystem == "" {
				p.Ecosystem = eco
			}
			out = append(out, p)
		}
		if resp.NextToken == nil || *resp.NextToken == "" {
			break
		}
		nextToken = resp.NextToken
	}
	return out, nil
}

// Health probes the domain. Used by the supply-token-recheck dog.
func (c *inProcessClient) Health(ctx context.Context) error {
	_, err := c.api.DescribeDomain(ctx, &awsca.DescribeDomainInput{
		Domain:      aws.String(c.domain),
		DomainOwner: aws.String(c.domainOwner),
	})
	if err != nil {
		return MapAWSError("Health", err)
	}
	return nil
}

func derefString(s *string, def string) string {
	if s == nil {
		return def
	}
	return *s
}
