package rules

import "testing"

func TestBOS010_Red(t *testing.T) {
	src := `
package agents
func SendMail(from, to, subject, body string, taskID int, t string) {}
func emit(body string) {
	SendMail("agent", "operator", "subject", body, 1, "alert")
}
`
	out := runRule(t, bos010{}, "internal/agents/example.go", src)
	assertHasFinding(t, out, "BOS-010", "SendMail")
}

func TestBOS010_Green_Wrapped(t *testing.T) {
	src := `
package agents
func RedactSecrets(s string) string { return s }
func SendMail(from, to, subject, body string, taskID int, t string) {}
func emit(body string) {
	SendMail("agent", "operator", "subject", RedactSecrets(body), 1, "alert")
}
`
	out := runRule(t, bos010{}, "internal/agents/example.go", src)
	assertNoFindings(t, out)
}

// Pure-literal calls have nothing to redact; not flagged.
func TestBOS010_PureLiteral(t *testing.T) {
	src := `
package agents
func SendMail(from, to, subject, body string, taskID int, t string) {}
func emit() {
	SendMail("agent", "operator", "subject", "all-literal body", 1, "alert")
}
`
	out := runRule(t, bos010{}, "internal/agents/example.go", src)
	assertNoFindings(t, out)
}
