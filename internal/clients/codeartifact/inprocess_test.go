package codeartifact

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsca "github.com/aws/aws-sdk-go-v2/service/codeartifact"
	"github.com/aws/aws-sdk-go-v2/service/codeartifact/types"
	smithy "github.com/aws/smithy-go"
)

// stubAPI is the test-only stand-in for the AWS SDK client. Each
// method returns whatever the test set on the matching field.
type stubAPI struct {
	descOut *awsca.DescribePackageVersionOutput
	descErr error

	listOuts []*awsca.ListPackagesOutput
	listIdx  int
	listErr  error

	healthErr error
}

func (s *stubAPI) DescribePackageVersion(ctx context.Context, in *awsca.DescribePackageVersionInput, _ ...func(*awsca.Options)) (*awsca.DescribePackageVersionOutput, error) {
	if s.descErr != nil {
		return nil, s.descErr
	}
	return s.descOut, nil
}

func (s *stubAPI) ListPackages(ctx context.Context, in *awsca.ListPackagesInput, _ ...func(*awsca.Options)) (*awsca.ListPackagesOutput, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	if s.listIdx >= len(s.listOuts) {
		return &awsca.ListPackagesOutput{}, nil
	}
	out := s.listOuts[s.listIdx]
	s.listIdx++
	return out, nil
}

func (s *stubAPI) DescribeDomain(ctx context.Context, in *awsca.DescribeDomainInput, _ ...func(*awsca.Options)) (*awsca.DescribeDomainOutput, error) {
	if s.healthErr != nil {
		return nil, s.healthErr
	}
	return &awsca.DescribeDomainOutput{}, nil
}

func newTestClient(api awsCAAPI) Client {
	return newInProcessFromAPI(api, "", "", "")
}

func TestDescribePackageVersion_HappyPath(t *testing.T) {
	pubAt := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	api := &stubAPI{
		descOut: &awsca.DescribePackageVersionOutput{
			PackageVersion: &types.PackageVersionDescription{
				PackageName:   aws.String("requests"),
				Version:       aws.String("2.31.0"),
				Status:        types.PackageVersionStatusPublished,
				PublishedTime: &pubAt,
				Licenses: []types.LicenseInfo{
					{Name: aws.String("Apache-2.0")},
				},
				HomePage: aws.String("https://requests.readthedocs.io"),
			},
		},
	}
	c := newTestClient(api)
	got, err := c.DescribePackageVersion(context.Background(), EcosystemPyPI, "requests", "2.31.0")
	if err != nil {
		t.Fatalf("DescribePackageVersion: unexpected error: %v", err)
	}
	if got.Name != "requests" || got.Version != "2.31.0" {
		t.Errorf("name/version mismatch: %+v", got)
	}
	if got.License != "Apache-2.0" {
		t.Errorf("license mismatch: %q", got.License)
	}
	if !got.PublishedAt.Equal(pubAt) {
		t.Errorf("publishedAt mismatch: %v", got.PublishedAt)
	}
	if got.Status != "Published" {
		t.Errorf("status mismatch: %q", got.Status)
	}
}

func TestDescribePackageVersion_NotFound(t *testing.T) {
	api := &stubAPI{
		descErr: &types.ResourceNotFoundException{Message: aws.String("nope")},
	}
	c := newTestClient(api)
	_, err := c.DescribePackageVersion(context.Background(), EcosystemNPM, "totally-fake-package", "1.0.0")
	if !errors.Is(err, ErrPackageNotFound) {
		t.Fatalf("expected ErrPackageNotFound, got: %v", err)
	}
}

func TestDescribePackageVersion_AuthError_TokenExpired(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"AccessDeniedException", &types.AccessDeniedException{Message: aws.String("denied")}},
		{"NoCredentialProviders string", errors.New("operation error CodeArtifact: NoCredentialProviders: no valid providers in chain")},
		{"SSO expired", errors.New("the SSO session has expired and must be refreshed")},
		{"smithy ExpiredToken", &smithy.GenericAPIError{Code: "ExpiredToken", Message: "token expired"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := &stubAPI{descErr: tc.err}
			c := newTestClient(api)
			_, err := c.DescribePackageVersion(context.Background(), EcosystemPyPI, "x", "1.0")
			if !errors.Is(err, ErrTokenExpired) {
				t.Fatalf("expected ErrTokenExpired, got: %v", err)
			}
		})
	}
}

func TestDescribePackageVersion_Throttle_Transient(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"typed ThrottlingException", &types.ThrottlingException{Message: aws.String("slow down")}},
		{"smithy ThrottlingException", &smithy.GenericAPIError{Code: "ThrottlingException", Message: "slow down"}},
		{"smithy 5xx fault", &smithy.GenericAPIError{Code: "InternalFailure", Message: "boom", Fault: smithy.FaultServer}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := &stubAPI{descErr: tc.err}
			c := newTestClient(api)
			_, err := c.DescribePackageVersion(context.Background(), EcosystemPyPI, "x", "1.0")
			if !errors.Is(err, ErrTransient) {
				t.Fatalf("expected ErrTransient, got: %v", err)
			}
		})
	}
}

func TestDescribePackageVersion_UnsupportedEcosystem(t *testing.T) {
	api := &stubAPI{}
	c := newTestClient(api)
	_, err := c.DescribePackageVersion(context.Background(), Ecosystem("generic"), "x", "1.0")
	if !errors.Is(err, ErrUnsupportedEcosystem) {
		t.Fatalf("expected ErrUnsupportedEcosystem, got: %v", err)
	}
}

func TestListPackages_Pagination(t *testing.T) {
	api := &stubAPI{
		listOuts: []*awsca.ListPackagesOutput{
			{
				Packages: []types.PackageSummary{
					{Package: aws.String("react"), Format: types.PackageFormatNpm},
					{Package: aws.String("vue"), Format: types.PackageFormatNpm},
				},
				NextToken: aws.String("page-2"),
			},
			{
				Packages: []types.PackageSummary{
					{Package: aws.String("angular"), Format: types.PackageFormatNpm},
				},
			},
		},
	}
	c := newTestClient(api)
	got, err := c.ListPackages(context.Background(), EcosystemNPM)
	if err != nil {
		t.Fatalf("ListPackages: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 packages across 2 pages, got %d: %+v", len(got), got)
	}
	if got[2].Name != "angular" {
		t.Errorf("expected angular at index 2, got %q", got[2].Name)
	}
}

func TestListPackages_TokenExpired(t *testing.T) {
	api := &stubAPI{listErr: &smithy.GenericAPIError{Code: "ExpiredTokenException", Message: "expired"}}
	c := newTestClient(api)
	_, err := c.ListPackages(context.Background(), EcosystemPyPI)
	if !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("expected ErrTokenExpired, got: %v", err)
	}
}

func TestHealth_OK(t *testing.T) {
	c := newTestClient(&stubAPI{})
	if err := c.Health(context.Background()); err != nil {
		t.Fatalf("Health: %v", err)
	}
}

func TestHealth_TokenExpired(t *testing.T) {
	api := &stubAPI{healthErr: errors.New("the SSO session has expired")}
	c := newTestClient(api)
	err := c.Health(context.Background())
	if !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("expected ErrTokenExpired, got: %v", err)
	}
}

func TestEcosystemRepository(t *testing.T) {
	cases := []struct {
		eco  Ecosystem
		want string
		err  bool
	}{
		{EcosystemPyPI, "pypi-prod", false},
		{EcosystemNPM, "npm-prod", false},
		{EcosystemRubyGems, "rubygems-prod", false},
		{EcosystemMaven, "maven-prod", false},
		{Ecosystem("generic"), "", true},
	}
	for _, tc := range cases {
		got, err := EcosystemRepository(tc.eco)
		if tc.err && err == nil {
			t.Errorf("%q: expected error, got %q", tc.eco, got)
		}
		if !tc.err && got != tc.want {
			t.Errorf("%q: want %q got %q", tc.eco, tc.want, got)
		}
	}
}

// Compile-time guard: ensure the inProcess type satisfies Client. P16
// also enforces this from the audittools side.
func TestInProcessSatisfiesClient(t *testing.T) {
	var _ Client = newInProcessFromAPI(&stubAPI{}, "", "", "")
}

// Sanity: MapAWSError surfaces a non-mapped error verbatim (just
// wrapped) so unknown failures aren't silently classified.
func TestMapAWSError_UnknownPassesThrough(t *testing.T) {
	original := errors.New("some weird unrelated thing")
	mapped := MapAWSError("op", original)
	if mapped == nil {
		t.Fatal("expected non-nil mapped error")
	}
	if errors.Is(mapped, ErrTokenExpired) || errors.Is(mapped, ErrPackageNotFound) || errors.Is(mapped, ErrTransient) {
		t.Errorf("unknown error must not be classified: %v", mapped)
	}
	if !errors.Is(mapped, original) {
		t.Errorf("mapped error must wrap original: %v", mapped)
	}
}

// Smoke: ensure the wrapping uses the op label for operator-debug
// readability ("DescribePackageVersion: ...").
func TestMapAWSError_OpLabelInMessage(t *testing.T) {
	mapped := MapAWSError("DescribePackageVersion", &types.ResourceNotFoundException{Message: aws.String("nope")})
	if mapped == nil {
		t.Fatal("nil")
	}
	if want := "DescribePackageVersion"; !contains(mapped.Error(), want) {
		t.Errorf("expected %q in error message: %s", want, mapped.Error())
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || (len(sub) > 0 && (indexOf(s, sub) >= 0)))
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// Sanity: the test stubs implement awsCAAPI.
var _ awsCAAPI = (*stubAPI)(nil)

// Make sure fmt is used (defensive, suppresses linters when this file
// loses usages over time).
var _ = fmt.Sprintf
