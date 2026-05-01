// Package scanners wraps the vendored Go security libraries (gosec,
// gitleaks) as in-process callable functions so ISB rules can call
// them like any other deterministic check — no subprocess shell-out,
// no JSON parsing of CLI output (per the all-Go decision in D4 Phase
// 2 scope).
package scanners

import (
	"context"
	"fmt"
	"sync"

	gldetect "github.com/zricethezav/gitleaks/v8/detect"
	glreport "github.com/zricethezav/gitleaks/v8/report"
)

// Finding is the scanner-neutral shape that ISB rules consume. The
// scanner output (gitleaks's report.Finding, gosec's issue.Issue) is
// projected into this shape so the rule body never needs to know
// which library produced the hit.
type Finding struct {
	RuleID    string // scanner-side rule id (e.g. "github-pat" for gitleaks)
	Path      string
	Line      int
	Message   string
	Secret    string // optional: the matched secret (gitleaks)
	StartCol  int
	EndCol    int
}

// gitleaksDetectorOnce ensures the default-config detector is built
// exactly once per process. The detector is heavy (compiles all
// gitleaks rules) so we cache it. Concurrency is fine: DetectString is
// safe for concurrent use.
var (
	gitleaksDetectorOnce sync.Once
	gitleaksDetector     *gldetect.Detector
	gitleaksDetectorErr  error
)

func loadGitleaksDetector() (*gldetect.Detector, error) {
	gitleaksDetectorOnce.Do(func() {
		d, err := gldetect.NewDetectorDefaultConfig()
		if err != nil {
			gitleaksDetectorErr = fmt.Errorf("gitleaks default config: %w", err)
			return
		}
		gitleaksDetector = d
	})
	return gitleaksDetector, gitleaksDetectorErr
}

// RunGitleaks scans the in-memory file content for secret patterns
// (GitHub PATs, AWS keys, basic-auth URLs, etc.) using gitleaks's
// default rule set. Returns a slice of Findings projected into the
// scanner-neutral shape.
//
// ctx is accepted for shape parity with other scanners and for future
// timeout-respecting variants — gitleaks's DetectString is in-memory
// and synchronous, so cancellation is a no-op today.
//
// inputs is keyed path → source. We project gitleaks's report.Finding
// list back per-file by passing one path at a time; gitleaks's
// DetectString operates on raw content without a filename, so we
// stitch the path back in from the input map.
func RunGitleaks(ctx context.Context, inputs map[string]string) ([]Finding, error) {
	_ = ctx // future cancellation point
	if len(inputs) == 0 {
		return nil, nil
	}
	det, err := loadGitleaksDetector()
	if err != nil {
		return nil, err
	}
	var out []Finding
	for path, source := range inputs {
		hits := det.DetectString(source)
		for _, h := range hits {
			out = append(out, projectGitleaksFinding(path, h))
		}
	}
	return out, nil
}

func projectGitleaksFinding(path string, h glreport.Finding) Finding {
	return Finding{
		RuleID:   h.RuleID,
		Path:     path,
		Line:     h.StartLine,
		Message:  h.Description,
		Secret:   h.Secret,
		StartCol: h.StartColumn,
		EndCol:   h.EndColumn,
	}
}
