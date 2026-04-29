// Command force-bash-guard is the astromech Bash tool gatekeeper.
//
// Astromechs invoke Bash via the Claude CLI; the actual command line passes
// through this binary first. force-bash-guard parses the command (including
// compound forms via &&, ||, ;, |), evaluates each segment against an
// allowlist/denylist, and exits 0 when the entire command is safe to run.
// A non-zero exit code means the wrapping shell shim must NOT execute the
// real command — that is the security boundary.
//
// Exit codes:
//
//	0  the command is safe; caller may exec it
//	1  the command (or a compound segment) is denied
//	2  the input is malformed or unparsable
//
// Allowlist + denylist are hardcoded in this file. Adding or relaxing
// entries requires operator review (see CLAUDE.md "Astromech Bash
// boundary"); LLM-driven fleet automation MUST NOT edit this file.
package main

import (
	"bufio"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// Version stamp logged into bash.log so an audit can correlate a guarded
// command back to a known binary build.
const Version = "v0.1.0-D2-T1-3"

// allowedPrograms is the closed set of executables an astromech may run.
// A command whose first token is not in this map is denied — there is no
// regex escape hatch and no "warn but allow" path. When new tooling is
// genuinely needed, add the program here AND update the deny rules below
// if the tool has destructive flags.
var allowedPrograms = map[string]struct{}{
	// Source control / GitHub / Go toolchain.
	"git": {}, "gh": {}, "go": {}, "gofmt": {},
	// Package managers (popular ecosystems).
	"npm": {}, "yarn": {}, "pnpm": {}, "cargo": {}, "bun": {}, "deno": {},
	// Test runners / builders.
	"pytest": {}, "make": {}, "rustc": {}, "rustfmt": {},
	"jest": {}, "vitest": {}, "mocha": {}, "phpunit": {}, "rspec": {},
	// Read-only file inspection.
	"ls": {}, "cat": {}, "grep": {}, "rg": {}, "head": {}, "tail": {},
	"wc": {}, "diff": {}, "cmp": {}, "find": {},
	// Lightweight transforms.
	"awk": {}, "sed": {}, "jq": {}, "yq": {},
	// chmod is allowlisted but per-program rules below reject the
	// destructive shapes (recursive, world-writable symbolic, …).
	"chmod": {},
	// kill is allowlisted but per-program rules reject `kill -9 1`.
	"kill": {},
	// Net fetchers (host-allowlisted at evaluation time).
	"curl": {}, "wget": {},
	// Allow `echo` and `true`/`false` so simple shim probes don't spuriously
	// reject. These are pure read-effect.
	"echo": {}, "true": {}, "false": {},
}

// deniedPrograms is checked AFTER the allowlist; an entry here always
// rejects regardless of the allowlist. Useful for tools that look benign
// but have destructive defaults (sudo / su / dd / etc.).
var deniedPrograms = map[string]struct{}{
	"sudo": {}, "su": {}, "doas": {},
	"dd": {}, "mkfs": {}, "shutdown": {}, "reboot": {}, "halt": {}, "poweroff": {},
	"chown": {}, "passwd": {},
}

// pathDenylist lists prefix patterns that, when written-or-read, are
// always rejected. Matching is performed AFTER `..` and symlink
// resolution so `rm /../../etc/hosts` doesn't escape.
var pathDenylist = []string{
	"/etc/", "/var/", "/usr/", "/bin/", "/sbin/", "/boot/", "/dev/",
}

// readDenylist guards inbound credential reads regardless of the
// program. Matched against any argument literal that resolves to one of
// these paths.
var readDenylist = []string{
	".ssh", ".aws", ".config/gh/hosts.yml",
}

// systemConfigCurlHostsKey is the SystemConfig row that backs the
// per-fleet curl/wget host allowlist. Operator-tunable; defaults to
// empty (curl/wget reject every host until the operator populates the
// list).
const systemConfigCurlHostsKey = "bash_guard_curl_hosts"

// systemConfigLogMaxBytesKey caps the per-session bash.log so a runaway
// astromech can't fill the disk.
const systemConfigLogMaxBytesKey = "bash_guard_log_max_bytes"

const defaultLogMaxBytes int64 = 10 * 1024 * 1024 // 10 MiB

// validation returns the rejection reason for a command, or "" if the
// command is permitted. Compound commands are split and evaluated
// segment by segment; ANY rejected segment fails the whole command.
type validation struct {
	allowed bool
	reason  string
}

// guardConfig wires the binary's runtime knobs. Backed by SystemConfig
// when --db is supplied; otherwise defaults are used.
type guardConfig struct {
	curlHosts   []string
	logMaxBytes int64
	logPath     string
}

func main() {
	dbPath := flag.String("db", "", "path to holocron.db (for SystemConfig lookups)")
	logPathFlag := flag.String("log", "", "override bash.log path; if empty we derive from CWD or fall back to /tmp/force-bash-guard.log")
	flag.Parse()

	cfg := loadConfig(*dbPath, *logPathFlag)

	cmdLine, srcLabel, err := readCommandInput(flag.Args())
	if err != nil {
		fmt.Fprintf(os.Stderr, "force-bash-guard: %v\n", err)
		os.Exit(2)
	}
	cmdLine = strings.TrimSpace(cmdLine)
	if cmdLine == "" {
		// Empty input is a parse error — the wrapper should never call us
		// with no command. Refuse loudly so a regression in the shim is
		// visible in bash.log.
		fmt.Fprintf(os.Stderr, "force-bash-guard: empty command input (source=%s)\n", srcLabel)
		writeLogEntry(cfg, "rejected", "(empty)", "empty command input")
		os.Exit(2)
	}

	v := evaluateCompound(cmdLine, cfg)
	if v.allowed {
		writeLogEntry(cfg, "allowed", cmdLine, "")
		os.Exit(0)
	}
	writeLogEntry(cfg, "rejected", cmdLine, v.reason)
	fmt.Fprintf(os.Stderr, "force-bash-guard: REJECTED — %s\n", v.reason)
	os.Exit(1)
}

// readCommandInput reads the command line either from argv (preferred,
// "force-bash-guard <command...>") or from stdin (one line per command).
// A multi-line stdin input is folded into a single semicolon-separated
// command so each line is evaluated as its own segment.
func readCommandInput(args []string) (string, string, error) {
	if len(args) > 0 {
		return strings.Join(args, " "), "argv", nil
	}
	stat, err := os.Stdin.Stat()
	if err != nil {
		return "", "stdin", fmt.Errorf("stat stdin: %w", err)
	}
	if (stat.Mode() & os.ModeCharDevice) != 0 {
		// Interactive terminal with no argv — refuse rather than block.
		return "", "stdin", errors.New("no command on argv and stdin is a TTY")
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var lines []string
	for scanner.Scan() {
		l := strings.TrimSpace(scanner.Text())
		if l == "" {
			continue
		}
		lines = append(lines, l)
	}
	if scanErr := scanner.Err(); scanErr != nil {
		return "", "stdin", fmt.Errorf("scan stdin: %w", scanErr)
	}
	return strings.Join(lines, " ; "), "stdin", nil
}

// evaluateCompound splits cmdLine on shell separators (&& || ; |) and
// evaluates each segment. Compound rejection short-circuits.
func evaluateCompound(cmdLine string, cfg guardConfig) validation {
	segments, err := splitCompound(cmdLine)
	if err != nil {
		return validation{false, fmt.Sprintf("parse error: %v", err)}
	}
	for _, seg := range segments {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		v := evaluateSegment(seg, cfg)
		if !v.allowed {
			return validation{false, fmt.Sprintf("segment %q: %s", truncForLog(seg, 200), v.reason)}
		}
	}
	return validation{allowed: true}
}

// splitCompound performs a quote-aware split on the shell separators
// `&&`, `||`, `;`, and `|`. Backticks and `$()` substitutions are NOT
// recursed into — if a segment contains either, we deny the whole
// segment in evaluateSegment because process-substitution is an attack
// vector that bypasses our argv-based denylist (you can't reliably
// guess what `$(...)` will expand to without running it).
func splitCompound(s string) ([]string, error) {
	var (
		out      []string
		buf      strings.Builder
		inSingle bool
		inDouble bool
		i        int
	)
	flush := func() {
		if buf.Len() > 0 {
			out = append(out, buf.String())
			buf.Reset()
		}
	}
	for i < len(s) {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s):
			// Preserve the escape for downstream tokenization.
			buf.WriteByte(c)
			buf.WriteByte(s[i+1])
			i += 2
			continue
		case c == '\'' && !inDouble:
			inSingle = !inSingle
			buf.WriteByte(c)
		case c == '"' && !inSingle:
			inDouble = !inDouble
			buf.WriteByte(c)
		case inSingle || inDouble:
			buf.WriteByte(c)
		case (c == '&' && i+1 < len(s) && s[i+1] == '&') ||
			(c == '|' && i+1 < len(s) && s[i+1] == '|'):
			flush()
			i += 2
			continue
		case c == ';' || c == '|' || c == '\n':
			flush()
		default:
			buf.WriteByte(c)
		}
		i++
	}
	if inSingle || inDouble {
		return nil, fmt.Errorf("unterminated quote")
	}
	flush()
	return out, nil
}

// evaluateSegment validates a single (non-compound) command segment.
func evaluateSegment(seg string, cfg guardConfig) validation {
	// Reject process substitution and command substitution outright —
	// they hide nested commands behind expansion that we cannot resolve
	// statically.
	if strings.Contains(seg, "$(") {
		return validation{false, "command substitution $(...) is not permitted"}
	}
	if strings.Contains(seg, "<(") || strings.Contains(seg, ">(") {
		return validation{false, "process substitution <(...) / >(...) is not permitted"}
	}
	if strings.Contains(seg, "`") {
		return validation{false, "backtick substitution is not permitted"}
	}
	// Reject the canonical fork-bomb pattern explicitly.
	if strings.Contains(seg, ":(){") {
		return validation{false, "fork-bomb pattern detected"}
	}

	tokens, err := tokenize(seg)
	if err != nil {
		return validation{false, fmt.Sprintf("tokenize: %v", err)}
	}
	if len(tokens) == 0 {
		return validation{false, "empty segment"}
	}

	// Strip leading env-var assignments (FOO=bar baz). Keep this
	// minimal — we don't allow BASH_ENV / ENV / PROMPT_COMMAND to be
	// set inline.
	idx := 0
	for idx < len(tokens) && isEnvAssignment(tokens[idx]) {
		name := strings.SplitN(tokens[idx], "=", 2)[0]
		if name == "BASH_ENV" || name == "ENV" || name == "PROMPT_COMMAND" {
			return validation{false, fmt.Sprintf("inline env var %q is not permitted", name)}
		}
		idx++
	}
	if idx >= len(tokens) {
		return validation{false, "no program after env assignments"}
	}
	prog := stripPath(tokens[idx])
	args := tokens[idx+1:]

	if _, denied := deniedPrograms[prog]; denied {
		return validation{false, fmt.Sprintf("program %q is on the denylist", prog)}
	}
	if _, ok := allowedPrograms[prog]; !ok {
		return validation{false, fmt.Sprintf("program %q is not on the allowlist", prog)}
	}

	// Per-program rules.
	if v := perProgramRules(prog, args, cfg); !v.allowed {
		return v
	}

	// Path denylist sweep against every literal arg. We check BOTH the
	// post-`..`-resolution path (catches `/../../etc/hosts` traversal) AND
	// the post-symlink-resolution path (catches macOS's /etc → /private/etc
	// indirection). Either match denies.
	for _, a := range args {
		for _, candidate := range candidatePaths(a) {
			for _, prefix := range pathDenylist {
				if strings.HasPrefix(candidate, prefix) {
					return validation{false, fmt.Sprintf("argument %q resolves under %s (denied)", a, prefix)}
				}
			}
			for _, hint := range readDenylist {
				if strings.Contains(candidate, hint) {
					return validation{false, fmt.Sprintf("argument %q resolves to a credential-bearing path (%s)", a, hint)}
				}
			}
		}
	}

	return validation{allowed: true}
}

// candidatePaths returns the set of resolved forms we evaluate against
// the path denylist. Three forms are produced:
//   - the lexically-cleaned absolute path (no symlink eval) — catches
//     `..`-based traversals
//   - the symlink-resolved path — catches macOS /etc → /private/etc
//   - the prefix-stripped post-eval path — catches indirection like
//     /private/etc/passwd by also testing /etc/passwd
func candidatePaths(arg string) []string {
	if arg == "" {
		return nil
	}
	expanded := expandHome(arg)
	abs := expanded
	if !filepath.IsAbs(abs) {
		cwd, err := os.Getwd()
		if err != nil {
			cwd = "/"
		}
		abs = filepath.Join(cwd, abs)
	}
	clean := filepath.Clean(abs)
	out := []string{clean}
	if resolved, err := filepath.EvalSymlinks(clean); err == nil && resolved != clean {
		out = append(out, resolved)
		// Strip a leading /private prefix (macOS) so the canonical /etc
		// form is also tested even when symlink eval landed under /private.
		if strings.HasPrefix(resolved, "/private/") {
			out = append(out, strings.TrimPrefix(resolved, "/private"))
		}
	}
	return out
}

// perProgramRules covers the per-tool deny rules (rm targets, sed -i,
// chmod recursive, find -exec, kill -9 1, curl host-allowlist, etc.).
func perProgramRules(prog string, args []string, cfg guardConfig) validation {
	switch prog {
	case "rm":
		return validation{false, "rm is not on the allowlist (use git rm or operator action)"}
	case "sed":
		for i, a := range args {
			if a == "-i" || strings.HasPrefix(a, "-i") {
				// Allow `-i ''` only if explicitly that exact form? The
				// prompt says "non-in-place ONLY — reject -i" so we reject
				// any -i variant.
				_ = i
				return validation{false, "sed -i (in-place) is not permitted"}
			}
		}
	case "find":
		for _, a := range args {
			if a == "-exec" || a == "-execdir" || a == "-delete" || a == "-ok" || a == "-okdir" {
				return validation{false, fmt.Sprintf("find %s has path-effect; not permitted", a)}
			}
		}
	case "chmod":
		// Only allow numeric-mode-on-existing-file. Any flag beginning
		// with `-R` (recursive) or any +x/g+w-style recursive symbolic
		// mode that touches groups/world-writable is rejected.
		for _, a := range args {
			if a == "-R" || a == "--recursive" {
				return validation{false, "chmod recursive is not permitted"}
			}
			if strings.ContainsAny(a, "g+wo+w") {
				// Heuristic: reject world-writable additions. A bare numeric
				// like `644` or `755` doesn't trip this.
				if !isNumericMode(a) && (strings.Contains(a, "o+w") || strings.Contains(a, "+w")) {
					return validation{false, fmt.Sprintf("chmod symbolic mode %q is not permitted", a)}
				}
			}
		}
	case "kill":
		for _, a := range args {
			if a == "-9" {
				// Reject only when the target is pid 1.
				for _, b := range args {
					if b == "1" {
						return validation{false, "kill -9 of pid 1 is not permitted"}
					}
				}
			}
		}
	case "curl", "wget":
		for _, a := range args {
			if !strings.HasPrefix(a, "http://") && !strings.HasPrefix(a, "https://") {
				continue
			}
			host := extractHost(a)
			if host == "" {
				return validation{false, fmt.Sprintf("%s URL %q has no parseable host", prog, a)}
			}
			if !hostAllowed(host, cfg.curlHosts) {
				return validation{false, fmt.Sprintf("%s host %q is not in bash_guard_curl_hosts allowlist", prog, host)}
			}
		}
	}
	return validation{allowed: true}
}

// tokenize splits a single command segment into argv-style tokens
// honoring single and double quotes plus simple backslash escapes.
func tokenize(s string) ([]string, error) {
	var (
		toks      []string
		buf       strings.Builder
		inSingle  bool
		inDouble  bool
		hasOutput bool
		i         int
	)
	for i < len(s) {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s) && !inSingle:
			buf.WriteByte(s[i+1])
			hasOutput = true
			i += 2
			continue
		case c == '\'' && !inDouble:
			inSingle = !inSingle
			hasOutput = true
		case c == '"' && !inSingle:
			inDouble = !inDouble
			hasOutput = true
		case inSingle || inDouble:
			buf.WriteByte(c)
			hasOutput = true
		case c == ' ' || c == '\t':
			if hasOutput {
				toks = append(toks, buf.String())
				buf.Reset()
				hasOutput = false
			}
		default:
			buf.WriteByte(c)
			hasOutput = true
		}
		i++
	}
	if inSingle || inDouble {
		return nil, fmt.Errorf("unterminated quote")
	}
	if hasOutput {
		toks = append(toks, buf.String())
	}
	return toks, nil
}

func isEnvAssignment(tok string) bool {
	if eq := strings.Index(tok, "="); eq > 0 {
		name := tok[:eq]
		for _, c := range name {
			if !(c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')) {
				return false
			}
		}
		return name != ""
	}
	return false
}

// stripPath returns the basename of an argv[0]-style program token. We
// match the allowlist on basename so `/usr/bin/git` and `git` both
// resolve to the "git" entry.
func stripPath(p string) string {
	return filepath.Base(p)
}

// resolvePath returns a normalised absolute-or-relative form of p,
// resolving `..` segments and (best-effort) symlinks. An unparseable
// result returns "".
func resolvePath(p string) string {
	if p == "" {
		return ""
	}
	expanded := expandHome(p)
	abs := expanded
	if !filepath.IsAbs(abs) {
		// Make relative paths absolute relative to CWD so `../etc/hosts`
		// resolves into the real `/etc/hosts` denylist hit.
		cwd, err := os.Getwd()
		if err != nil {
			cwd = "/"
		}
		abs = filepath.Join(cwd, abs)
	}
	clean := filepath.Clean(abs)
	if resolved, err := filepath.EvalSymlinks(clean); err == nil {
		clean = resolved
	}
	return clean
}

func expandHome(p string) string {
	if !strings.HasPrefix(p, "~") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

func isNumericMode(a string) bool {
	if a == "" {
		return false
	}
	for _, c := range a {
		if c < '0' || c > '7' {
			return false
		}
	}
	return true
}

func extractHost(rawURL string) string {
	rest := strings.TrimPrefix(rawURL, "https://")
	rest = strings.TrimPrefix(rest, "http://")
	if idx := strings.IndexAny(rest, "/?#"); idx >= 0 {
		rest = rest[:idx]
	}
	if at := strings.Index(rest, "@"); at >= 0 {
		rest = rest[at+1:]
	}
	if colon := strings.Index(rest, ":"); colon >= 0 {
		rest = rest[:colon]
	}
	return strings.ToLower(rest)
}

func hostAllowed(host string, allowed []string) bool {
	host = strings.ToLower(host)
	for _, a := range allowed {
		a = strings.ToLower(strings.TrimSpace(a))
		if a == "" {
			continue
		}
		if a == host || strings.HasSuffix(host, "."+a) {
			return true
		}
	}
	return false
}

// loadConfig reads runtime knobs from SystemConfig (if a DB path was
// supplied) plus environment defaults. Failures fall back to safe
// defaults — empty curl hosts, default log size, log under CWD.
func loadConfig(dbPath, logPathOverride string) guardConfig {
	cfg := guardConfig{logMaxBytes: defaultLogMaxBytes}
	cfg.logPath = resolveLogPath(logPathOverride)
	if dbPath != "" {
		if db, err := sql.Open("sqlite3", dbPath); err == nil {
			defer db.Close()
			cfg.curlHosts = readSystemConfigList(db, systemConfigCurlHostsKey)
			if v := readSystemConfigInt(db, systemConfigLogMaxBytesKey); v > 0 {
				cfg.logMaxBytes = v
			}
		}
	}
	return cfg
}

func resolveLogPath(override string) string {
	if override != "" {
		return override
	}
	cwd, err := os.Getwd()
	if err != nil || cwd == "" {
		return "/tmp/force-bash-guard.log"
	}
	// Detect .force-worktrees/<repo>/<agent>/ pattern; if we're inside
	// one, log next to it; otherwise log into the cwd.
	if idx := strings.Index(cwd, ".force-worktrees"); idx >= 0 {
		return filepath.Join(cwd, "bash.log")
	}
	return filepath.Join(cwd, "bash.log")
}

func readSystemConfigList(db *sql.DB, key string) []string {
	row := db.QueryRow(`SELECT value FROM SystemConfig WHERE key = ?`, key)
	var raw string
	if err := row.Scan(&raw); err != nil {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func readSystemConfigInt(db *sql.DB, key string) int64 {
	row := db.QueryRow(`SELECT value FROM SystemConfig WHERE key = ?`, key)
	var raw string
	if err := row.Scan(&raw); err != nil {
		return 0
	}
	var n int64
	if _, err := fmt.Sscanf(strings.TrimSpace(raw), "%d", &n); err != nil {
		return 0
	}
	return n
}

// writeLogEntry appends one log line to bash.log. Bounded by
// cfg.logMaxBytes — past the cap we silently rotate to bash.log.1.
func writeLogEntry(cfg guardConfig, status, cmd, reason string) {
	if cfg.logPath == "" {
		return
	}
	rotateLogIfNeeded(cfg.logPath, cfg.logMaxBytes)
	f, err := os.OpenFile(cfg.logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	timestamp := time.Now().UTC().Format(time.RFC3339)
	line := fmt.Sprintf("%s\t%s\t%s\t%s\t%s\n",
		timestamp, Version, status, truncForLog(cmd, 800), truncForLog(reason, 400))
	_, _ = f.WriteString(line)
}

func rotateLogIfNeeded(path string, maxBytes int64) {
	if maxBytes <= 0 {
		return
	}
	st, err := os.Stat(path)
	if err != nil {
		return
	}
	if st.Size() < maxBytes {
		return
	}
	_ = os.Rename(path, path+".1")
}

func truncForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...[truncated]"
}
