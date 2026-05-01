package rules

import "testing"

// TestISB008_Red_PromptConcatNoSentinel — claude.CallFoo("base " + body)
// without <user_content> sentinels triggers a finding.
func TestISB008_Red_PromptConcatNoSentinel(t *testing.T) {
	src := `package x
type ClaudeAPI struct{}
var claude ClaudeAPI
func (ClaudeAPI) CallWithTranscript(prompt string) string { return "" }
func F(body string) string {
	return claude.CallWithTranscript("Summarize this: " + body)
}
`
	out := runRule(t, isb008{}, "internal/foo/p.go", src)
	assertHasFinding(t, out, "ISB-008", "")
}

// TestISB008_Green_WrappedInUserContent — same shape but the literal
// includes `<user_content>` passes.
func TestISB008_Green_WrappedInUserContent(t *testing.T) {
	src := `package x
type ClaudeAPI struct{}
var claude ClaudeAPI
func (ClaudeAPI) CallWithTranscript(prompt string) string { return "" }
func F(body string) string {
	return claude.CallWithTranscript("Summarize this: <user_content>" + body + "</user_content>")
}
`
	out := runRule(t, isb008{}, "internal/foo/p.go", src)
	assertNoFindings(t, out)
}

// TestISB008_Green_NotAClaudeCall — same concat shape but not on a
// claude.Call site.
func TestISB008_Green_NotAClaudeCall(t *testing.T) {
	src := `package x
import "fmt"
func F(body string) string {
	return fmt.Sprint("Summarize this: " + body)
}
`
	out := runRule(t, isb008{}, "internal/foo/p.go", src)
	assertNoFindings(t, out)
}
