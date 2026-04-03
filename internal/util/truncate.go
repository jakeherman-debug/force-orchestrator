package util

// TruncateStr truncates s to at most n bytes, appending "…" when truncated.
func TruncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
