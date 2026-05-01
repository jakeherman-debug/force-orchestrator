// D3 fix-loop iter 2 (slice ζ.3) — `force proposed-features --help`
// must exit 0, not 1.
//
// Pre-fix the verb dispatcher fell through to the `default:` arm of
// the verb switch and printed "unknown verb: --help" to stderr,
// returning 1. Tab-completion / man-page tooling that probes
// `<command> --help` for help text would then surface a spurious
// failure even though the help banner printed sensibly.
//
// Post-fix the dispatcher short-circuits on `--help`, `-h`, `help`,
// and the empty-args case, prints the usage banner to stdout, and
// returns 0. This test pins all four shapes.

package main

import (
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

func TestProposedFeatures_HelpExitCode(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	cases := []struct {
		name string
		args []string
	}{
		{"--help", []string{"--help"}},
		{"-h", []string{"-h"}},
		{"help verb", []string{"help"}},
		{"empty args", []string{}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var rc int
			out := captureOutput(func() {
				rc = cmdProposedFeatures(db, c.args)
			})
			if rc != 0 {
				t.Fatalf("cmdProposedFeatures(%v) returned exit code %d; want 0", c.args, rc)
			}
			if !strings.Contains(out, "Usage: force proposed-features") {
				t.Errorf("expected usage banner in output; got: %s", out)
			}
			// Must surface every legal verb so tab-completion has the
			// canonical list (tooling parses help banners).
			for _, verb := range []string{"list", "suppress", "score", "promote"} {
				if !strings.Contains(out, verb) {
					t.Errorf("usage banner missing %q verb; got: %s", verb, out)
				}
			}
		})
	}
}

func TestProposedFeatures_UnknownVerbStillExitsNonZero(t *testing.T) {
	// Idempotence guard: the --help short-circuit must NOT swallow
	// genuinely-unknown verbs. A bogus subcommand still exits 1 so
	// scripts that depend on the failure code keep working.
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	rc := cmdProposedFeatures(db, []string{"bogus-verb-not-a-real-command"})
	if rc == 0 {
		t.Fatalf("expected non-zero exit for unknown verb; got 0")
	}
}
