package scanners

import (
	"context"
	"fmt"
	"io"
	"log"
	"strconv"

	"github.com/securego/gosec/v2"
	"github.com/securego/gosec/v2/issue"
	"github.com/securego/gosec/v2/rules"
)

// RunGosec scans the given Go package paths via gosec's library API.
// packagePaths must be importable Go package directories (relative or
// absolute) — gosec.Analyzer.Process loads them with full type
// checking. Snippet-only inputs (in-memory source without an
// importable parent package) are NOT supported by gosec's library
// API, so the caller (the ISB reviewer) is responsible for resolving
// staged files to their containing package directories.
//
// On any package-load error, RunGosec returns the issues collected so
// far plus a non-nil error wrapping gosec's report. The reviewer
// should treat partial output as advisory (no hard reject on scanner
// failure) and surface the error in the BoSReview-shape audit log.
func RunGosec(ctx context.Context, packagePaths []string) ([]Finding, error) {
	_ = ctx
	if len(packagePaths) == 0 {
		return nil, nil
	}

	// Build a quiet analyzer: silence stdlib logger so the daemon's
	// stdout doesn't get gosec's chatty output. Concurrency=1 keeps
	// the analyzer deterministic across re-runs.
	silent := log.New(io.Discard, "", 0)
	analyzer := gosec.NewAnalyzer(
		gosec.NewConfig(),
		false, // tests=false: skip _test.go files (ISB targets prod code)
		true,  // excludeGenerated
		false, // trackSuppressions
		1,
		silent,
	)
	ruleDefs, ruleSuppressed := rules.Generate(false).RulesInfo()
	analyzer.LoadRules(ruleDefs, ruleSuppressed)

	if err := analyzer.Process(nil, packagePaths...); err != nil {
		issues, _, _ := analyzer.Report()
		return projectGosecIssues(issues), fmt.Errorf("gosec process: %w", err)
	}

	issues, _, _ := analyzer.Report()
	return projectGosecIssues(issues), nil
}

func projectGosecIssues(issues []*issue.Issue) []Finding {
	out := make([]Finding, 0, len(issues))
	for _, iss := range issues {
		line, _ := strconv.Atoi(iss.Line)
		col, _ := strconv.Atoi(iss.Col)
		out = append(out, Finding{
			RuleID:   iss.RuleID,
			Path:     iss.File,
			Line:     line,
			Message:  iss.What,
			StartCol: col,
		})
	}
	return out
}
