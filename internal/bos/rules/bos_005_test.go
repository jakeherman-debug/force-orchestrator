package rules

import "testing"

func TestBOS005_Red_PushForceNoAssert(t *testing.T) {
	src := `
package git
import "context"
func RunIt(ctx context.Context) {
	LogAndRun(ctx, "push", "--force", "origin", "main")
}
func LogAndRun(ctx context.Context, args ...string) {}
`
	out := runRule(t, bos005{}, "internal/git/git.go", src)
	assertHasFinding(t, out, "BOS-005", "destructive")
}

func TestBOS005_Red_ResetHardNoAssert(t *testing.T) {
	src := `
package git
import "context"
func RunIt(ctx context.Context) {
	LogAndRun(ctx, "reset", "--hard", "HEAD~1")
}
func LogAndRun(ctx context.Context, args ...string) {}
`
	out := runRule(t, bos005{}, "internal/git/git.go", src)
	assertHasFinding(t, out, "BOS-005", "")
}

func TestBOS005_Green_AssertGuards(t *testing.T) {
	src := `
package git
import "context"
func AssertNotDefaultBranch(ctx context.Context, branch string) error { return nil }
func LogAndRun(ctx context.Context, args ...string) {}
func RunIt(ctx context.Context, branch string) {
	if err := AssertNotDefaultBranch(ctx, branch); err != nil { return }
	LogAndRun(ctx, "push", "--force", "origin", branch)
}
`
	out := runRule(t, bos005{}, "internal/git/git.go", src)
	assertNoFindings(t, out)
}

func TestBOS005_NotDestructive(t *testing.T) {
	src := `
package git
import "context"
func LogAndRun(ctx context.Context, args ...string) {}
func RunIt(ctx context.Context) {
	LogAndRun(ctx, "fetch", "origin")
}
`
	out := runRule(t, bos005{}, "internal/git/git.go", src)
	assertNoFindings(t, out)
}
