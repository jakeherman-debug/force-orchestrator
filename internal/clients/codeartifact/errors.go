// Package codeartifact: error mapping. Splits AWS SDK errors into the
// three sentinel categories the SUPPLY rules consume:
//
//   - ErrTokenExpired — credential/auth class. The deferral path fires;
//     the rule writes a SecurityFindings row with disposition='token_
//     expired' and proceeds in advise mode.
//   - ErrPackageNotFound — ResourceNotFoundException. Maps to a block-
//     severity finding for SUPPLY-001; informational for others.
//   - ErrTransient — throttling / 5xx. Caller may retry.
//
// Anything else is returned wrapped as a plain fmt.Errorf — the caller
// must NOT treat unknown errors as auth-class (that would break the
// "no silent token-expired passthroughs" anti-cheat directive in
// docs/roadmap.md § D5).
package codeartifact

import (
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/codeartifact/types"
	"github.com/aws/smithy-go"
)

// authErrorCodes is the set of AWS error codes / shapes the SDK can
// produce when the SSO / role / static-creds chain has expired or the
// token lacks the relevant grant. We err on the side of
// "treat-as-auth" because the SUPPLY rules' worst failure mode is a
// silent passthrough — the deferral path is cheap and reversible.
var authErrorCodes = map[string]bool{
	"AccessDeniedException":     true,
	"UnauthorizedException":     true,
	"InvalidAccessKeyId":        true,
	"ExpiredToken":              true,
	"ExpiredTokenException":     true,
	"SSOTokenExpired":           true,
	"SSOError":                  true,
	"NoCredentialProviders":     true,
	"MissingAuthenticationToken": true,
	"InvalidClientTokenId":      true,
	"SignatureDoesNotMatch":     true,
	"TokenRefreshRequired":      true,
}

// throttleCodes covers the AWS rate-limit codes. Mapping is shared
// across services in the AWS SDK.
var throttleCodes = map[string]bool{
	"ThrottlingException":       true,
	"Throttling":                true,
	"ThrottledException":        true,
	"RequestThrottled":          true,
	"RequestThrottledException": true,
	"TooManyRequestsException":  true,
	"ServiceUnavailableException": true,
}

// MapAWSError converts a raw AWS SDK error to one of our sentinels (or
// passes it through wrapped). Exposed at package level so the
// inprocess implementation and the future gRPC backing can share the
// mapping. Tests assert against this directly.
func MapAWSError(op string, err error) error {
	if err == nil {
		return nil
	}

	// Typed: ResourceNotFoundException → ErrPackageNotFound.
	var notFound *types.ResourceNotFoundException
	if errors.As(err, &notFound) {
		return fmt.Errorf("%s: %w", op, ErrPackageNotFound)
	}

	// Throttling — typed shape from CodeArtifact.
	var throttled *types.ThrottlingException
	if errors.As(err, &throttled) {
		return fmt.Errorf("%s: %w", op, ErrTransient)
	}

	// AccessDeniedException — typed shape from CodeArtifact.
	var denied *types.AccessDeniedException
	if errors.As(err, &denied) {
		return fmt.Errorf("%s: %w", op, ErrTokenExpired)
	}

	// Generic smithy.APIError fallback — covers IAM/STS/SSO error
	// types that the SDK surfaces with their own code strings.
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		code := apiErr.ErrorCode()
		if authErrorCodes[code] {
			return fmt.Errorf("%s: %w", op, ErrTokenExpired)
		}
		if throttleCodes[code] {
			return fmt.Errorf("%s: %w", op, ErrTransient)
		}
		fault := apiErr.ErrorFault()
		if fault == smithy.FaultServer {
			return fmt.Errorf("%s: %w", op, ErrTransient)
		}
	}

	// String-shaped fallbacks. The SSO credential providers and the
	// generic NoCredentialProviders error don't always satisfy
	// smithy.APIError; they appear as plain errors with a stable
	// substring. Keep this list short and conservative.
	msg := err.Error()
	for _, needle := range []string{
		"NoCredentialProviders",
		"failed to refresh cached credentials",
		"the SSO session has expired",
		"sso session token has expired",
		"sso session has expired",
		"unable to load SSO token",
		"InvalidGrantException",
		"could not be refreshed",
		"ExpiredToken",
	} {
		if strings.Contains(msg, needle) {
			return fmt.Errorf("%s: %w", op, ErrTokenExpired)
		}
	}

	return fmt.Errorf("%s: %w", op, err)
}
