// Package audittools: D15 Phase 1 pattern test stubs — API-Surface Dependency Graph.
//
// These four tests are stubs that will be filled by D15 P2-P6 as extractors,
// consumer-resolvers, and Diplomat integration land. They are registered here
// so the test runner includes them and CI can track their progress.
//
//	P_APIPathNormalized                   — (P2) AST walk: every CrossRepoAPIs
//	                                        row inserted by extractors must carry
//	                                        a NormalizeAPIPath-canonical identifier.
//
//	P_APIExtractorCoverage                — (P3) At least one extractor is registered
//	                                        for each supported api_kind value.
//
//	P_APIConsumerProviderResolverComplete — (P4/P5) For every CrossRepoAPIDependencies
//	                                        row, provider_api_id resolves to an
//	                                        existing CrossRepoAPIs row.
//
//	P_DiplomatAPIConsumerIntegration      — (P6) Diplomat's blast-radius payload
//	                                        includes all active API dependency rows
//	                                        for a changed provider API.
package audittools

import "testing"

// TestPattern_APIPathNormalized is a stub; filled by D15 P2.
func TestPattern_APIPathNormalized(t *testing.T) {
	t.Skip("D15: stub — filled by P2-P6")
}

// TestPattern_APIExtractorCoverage is a stub; filled by D15 P3.
func TestPattern_APIExtractorCoverage(t *testing.T) {
	t.Skip("D15: stub — filled by P2-P6")
}

// TestPattern_APIConsumerProviderResolverComplete is a stub; filled by D15 P4/P5.
func TestPattern_APIConsumerProviderResolverComplete(t *testing.T) {
	t.Skip("D15: stub — filled by P2-P6")
}

// TestPattern_DiplomatAPIConsumerIntegration is a stub; filled by D15 P6.
func TestPattern_DiplomatAPIConsumerIntegration(t *testing.T) {
	t.Skip("D15: stub — filled by P2-P6")
}
