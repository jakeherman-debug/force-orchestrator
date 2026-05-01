package shadow

import (
	"context"
	"strings"
	"testing"
)

func TestCISuppress_ShouldSuppressPush(t *testing.T) {
	cases := []struct {
		name string
		sess *ShadowSession
		want bool
	}{
		{"nil session = real arm = no suppress", nil, false},
		{"empty WorktreePath = no shadow setup = no suppress",
			&ShadowSession{ExperimentID: 1, RunID: 1}, false},
		{"populated session = shadow run = suppress",
			&ShadowSession{ExperimentID: 1, RunID: 1, WorktreePath: "/tmp/shadow"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ShouldSuppressPush(c.sess); got != c.want {
				t.Fatalf("ShouldSuppressPush: got %v want %v", got, c.want)
			}
		})
	}
}

func TestCISuppress_SuppressPush_RewritesToLocalBranch(t *testing.T) {
	sess := &ShadowSession{ExperimentID: 7, RunID: 42, WorktreePath: "/tmp/shadow"}
	out := SuppressPush(context.Background(), sess, "origin main")
	if !out.Suppressed {
		t.Fatalf("Suppressed must be true for shadow runs")
	}
	if out.RewrittenBranch != "shadow-exp-7-run-42" {
		t.Fatalf("RewrittenBranch mismatch: %q", out.RewrittenBranch)
	}
	if !strings.Contains(out.Reason, "run 42") {
		t.Fatalf("Reason must include run id for forensics: %q", out.Reason)
	}
}

func TestCISuppress_SuppressPush_NilSessIsZeroOutcome(t *testing.T) {
	out := SuppressPush(context.Background(), nil, "origin main")
	if out.Suppressed {
		t.Fatalf("nil session must produce zero PushOutcome")
	}
}

func TestCISuppress_IsShadowGhWrite_Reads(t *testing.T) {
	reads := [][]string{
		{"pr", "view", "42"},
		{"pr", "checks", "42"},
		{"pr", "list"},
		{"pr", "diff", "42"},
		{"issue", "view", "5"},
		{"api", "/repos/foo/bar"},
		{"api", "/repos/foo/bar", "-q", ".name"},
		{"auth", "status"},
		{"version"},
		{"run", "list"},
		{"run", "view", "42"},
		{"workflow", "list"},
	}
	for _, r := range reads {
		if IsShadowGhWrite(r) {
			t.Errorf("IsShadowGhWrite(%v) = true, want false (it's a read)", r)
		}
	}
}

func TestCISuppress_IsShadowGhWrite_Writes(t *testing.T) {
	writes := [][]string{
		{"pr", "create", "--title", "x"},
		{"pr", "merge", "42"},
		{"pr", "close", "42"},
		{"pr", "comment", "42", "--body", "hi"},
		{"issue", "create"},
		{"issue", "close", "5"},
		{"api", "/repos/foo/bar/dispatches", "-X", "POST"},
		{"api", "/x", "--method", "DELETE"},
		{"api", "/x", "--method=PATCH"},
		{"release", "create"},
		{"workflow", "run"},
	}
	for _, w := range writes {
		if !IsShadowGhWrite(w) {
			t.Errorf("IsShadowGhWrite(%v) = false, want true (it's a write)", w)
		}
	}
}

func TestCISuppress_IsShadowGhWrite_UnknownDefaultsToWrite(t *testing.T) {
	// Conservative-by-default: unknown verb is treated as a write so a
	// new gh feature can't bypass shadow suppression.
	if !IsShadowGhWrite([]string{"newverb", "doathing"}) {
		t.Fatalf("unknown gh verb must default to write")
	}
}

func TestCISuppress_IsShadowGhWrite_Empty(t *testing.T) {
	if IsShadowGhWrite(nil) {
		t.Fatalf("empty args must be treated as no-op (false)")
	}
	if IsShadowGhWrite([]string{}) {
		t.Fatalf("empty args must be treated as no-op (false)")
	}
}
