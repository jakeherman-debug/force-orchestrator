package claude

import (
	"bytes"
	"log"
	"math"
	"strings"
	"testing"
)

// floatNear is a tolerance helper — float multiplication isn't bit-exact
// across compilers / archs, but we expect ~$0 / ~$0.05 / etc. ranges.
func floatNear(a, b, eps float64) bool {
	return math.Abs(a-b) < eps
}

// TestPricingKnownModels exercises the canonical price-table entries.
func TestPricingKnownModels(t *testing.T) {
	t.Cleanup(ResetPriceTableForTest)

	cases := []struct {
		name      string
		model     string
		tokensIn  int
		tokensOut int
		want      float64
	}{
		{"opus 4-5: 1M in + 1M out = 15+75", "claude-opus-4-5", 1_000_000, 1_000_000, 90.0},
		{"sonnet 4-5: 1M in + 1M out = 3+15", "claude-sonnet-4-5", 1_000_000, 1_000_000, 18.0},
		{"haiku 4-5: 1M in + 1M out = 1+5", "claude-haiku-4-5", 1_000_000, 1_000_000, 6.0},
		{"sonnet 1k in + 1k out (small)", "claude-sonnet-4-5", 1_000, 1_000, 0.018},
		{"opus zero tokens = $0", "claude-opus-4-5", 0, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CostUSD(tc.model, tc.tokensIn, tc.tokensOut)
			if !floatNear(got, tc.want, 1e-9) {
				t.Errorf("CostUSD(%q, %d, %d) = %v; want %v",
					tc.model, tc.tokensIn, tc.tokensOut, got, tc.want)
			}
		})
	}
}

// TestPricingNormalisation asserts that case, "anthropic/" prefix, and
// trailing "-YYYYMMDD" date stamps all resolve to the same table entry.
func TestPricingNormalisation(t *testing.T) {
	t.Cleanup(ResetPriceTableForTest)

	want := CostUSD("claude-opus-4-5", 1_000_000, 0)
	for _, raw := range []string{
		"Claude-Opus-4-5",
		"CLAUDE-OPUS-4-5",
		"anthropic/claude-opus-4-5",
		"anthropic/CLAUDE-OPUS-4-5",
		"claude-opus-4-5-20250414",
		" claude-opus-4-5 ",
	} {
		got := CostUSD(raw, 1_000_000, 0)
		if !floatNear(got, want, 1e-9) {
			t.Errorf("normalised %q: got %v want %v", raw, got, want)
		}
	}
}

// TestPricingUnknownModelReturnsZeroAndLogsOnce asserts that an unknown
// model id never crashes the cost path and only logs once per process.
func TestPricingUnknownModelReturnsZeroAndLogsOnce(t *testing.T) {
	t.Cleanup(ResetPriceTableForTest)

	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })

	got := CostUSD("future-model-from-2030", 1_000_000, 1_000_000)
	if got != 0 {
		t.Errorf("CostUSD(unknown) = %v; want 0", got)
	}
	// First call logs.
	first := buf.String()
	if !strings.Contains(first, "unknown model") {
		t.Errorf("expected 'unknown model' in log on first call; got %q", first)
	}
	// Second call must NOT re-log (chatty CLI shouldn't spam operator).
	buf.Reset()
	_ = CostUSD("future-model-from-2030", 1_000_000, 1_000_000)
	if buf.Len() != 0 {
		t.Errorf("expected no second log line; got %q", buf.String())
	}
}

// TestPricingUpdatePriceTable asserts the operator-controlled override path
// changes future lookups and is reset by ResetPriceTableForTest.
func TestPricingUpdatePriceTable(t *testing.T) {
	t.Cleanup(ResetPriceTableForTest)

	// Set a deterministic price for a fictional model.
	UpdatePriceTable("claude-test-1-0", 2.0, 10.0)
	got := CostUSD("claude-test-1-0", 1_000_000, 1_000_000)
	if !floatNear(got, 12.0, 1e-9) {
		t.Errorf("after UpdatePriceTable: got %v want 12.0", got)
	}
	// Override the existing opus entry so we can assert the update writes
	// to the same key the lookup uses.
	UpdatePriceTable("claude-opus-4-5", 1.0, 1.0)
	got = CostUSD("claude-opus-4-5", 1_000_000, 1_000_000)
	if !floatNear(got, 2.0, 1e-9) {
		t.Errorf("after override: got %v want 2.0", got)
	}
	// Reset and re-check the canonical price reasserts itself.
	ResetPriceTableForTest()
	got = CostUSD("claude-opus-4-5", 1_000_000, 1_000_000)
	if !floatNear(got, 90.0, 1e-9) {
		t.Errorf("after reset: got %v want 90.0", got)
	}
}

// TestPricingNegativeTokensClamp makes sure a defensive negative-token
// input doesn't produce a negative cost.
func TestPricingNegativeTokensClamp(t *testing.T) {
	t.Cleanup(ResetPriceTableForTest)

	got := CostUSD("claude-opus-4-5", -1, -1)
	if got != 0 {
		t.Errorf("CostUSD(negative) = %v; want 0", got)
	}
}
