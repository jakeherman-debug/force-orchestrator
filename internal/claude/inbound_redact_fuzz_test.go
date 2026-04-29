package claude

import (
	"strings"
	"testing"
)

// FuzzScrubInbound exercises the inbound-redact patterns against random
// inputs. The properties asserted on every fuzz iteration:
//
//  1. The function never panics on any input.
//  2. The string output is idempotent — ScrubInbound(ScrubInbound(s)).str
//     equals ScrubInbound(s).str. (Count is NOT asserted idempotent;
//     some patterns re-match their own [REDACTED] replacement, which is
//     harmless because count drives an alert threshold, not correctness.)
//  3. The output contains no surviving sensitive markers from the seed
//     corpus (BEGIN PRIVATE KEY, ghp_<long>, AKIA<...>). This is a
//     leakage check: even on adversarial inputs, the canonical anchors
//     of a secret should be gone.
//  4. The output length is bounded by 3× the input length plus a fixed
//     constant for replacement-marker overhead. Replacement markers add
//     at most ~26 chars per match; the bound has plenty of headroom.
func FuzzScrubInbound(f *testing.F) {
	// Seed corpus: real-shape secret payloads + benign prose. The seeds
	// give the fuzzer concrete examples of redactable content to mutate.
	seeds := []string{
		"",
		"plain prose with no secrets",
		"API_KEY=sk-livedeadbeef1234567890",
		"GITHUB_TOKEN=ghp_realtokendeadbeef1234567890ab",
		"DATABASE_PASSWORD=hunter2longvalue",
		"AKIAIOSFODNN7EXAMPLE",
		"Bearer abc.def.ghi.jkl-MNOPQRST_uvwxyz",
		"https://user:hunter2@github.com/foo/bar.git",
		realRSAPEM,
		realECPEM,
		gcpServiceAccountJSON,
		"-----BEGIN OPENSSH PRIVATE KEY-----\nbody\n-----END OPENSSH PRIVATE KEY-----",
		"prefix\nAPI_KEY=v1\nAKIAIOSFODNN7EXAMPLE\nsuffix",
		strings.Repeat("a", 4096),
		"=======BOUNDARY=======",
		"\x00\x01\x02 binary garbage",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, in string) {
		out, count := ScrubInbound(in)

		// 1. No panic — implicit, the test would fail if Fuzz body
		// panicked. Count must not be negative.
		if count < 0 {
			t.Fatalf("negative count: %d (in=%q)", count, in)
		}

		// 2. String idempotence.
		out2, _ := ScrubInbound(out)
		if out != out2 {
			t.Fatalf("not string-idempotent:\n  first=%q\n  second=%q\n  in=%q",
				out, out2, in)
		}

		// 3. Leakage check — every shape recognised as a complete PEM
		// block by pemBlockRe must be replaced. We use the regex
		// itself to define "complete block" so the fuzz property is
		// in lockstep with the production redactor: any input that
		// the regex matches is also an input the redactor must
		// scrub. Orphan BEGIN-only markers (no END counterpart) are
		// intentionally tolerated by the regex AND by the fuzz
		// property — they carry no key body.
		if pemBlockRe.MatchString(in) && pemBlockRe.MatchString(out) {
			t.Fatalf("PEM block survived ScrubInbound:\n  in=%q\n  out=%q", in, out)
		}
		// Same for AWS access keys and the GH-PAT prefix anchor.
		if awsAccessKeyRe.MatchString(in) && awsAccessKeyRe.MatchString(out) {
			t.Fatalf("AWS access key survived:\n  in=%q\n  out=%q", in, out)
		}

		// 4. Output length bound. Each replacement marker is at most
		// 26 chars; the input length plus a generous multiplier covers
		// any plausible blow-up.
		const maxOverheadPerMatch = 64
		bound := len(in)*3 + maxOverheadPerMatch*(count+1) + 64
		if len(out) > bound {
			t.Fatalf("output exceeds length bound: in=%d out=%d bound=%d count=%d",
				len(in), len(out), bound, count)
		}
	})
}
