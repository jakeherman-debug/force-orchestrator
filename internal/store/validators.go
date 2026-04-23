package store

import (
	"fmt"
	"strings"
)

// ── Ingress validators (Fix #9) ──────────────────────────────────────────────
//
// Mirror of git.ValidateRef for the store ingress boundary. Duplicated rather
// than imported because the CLAUDE.md layering rule forbids
// store → internal/git. Keep these rules in lock-step with
// internal/git/validators.go. If you touch one, touch the other.
//
// Use at every DB-write where the value flows downstream into a
// `git` / `gh` shell call:
//
//   - SetBranchName, SetBranchNameTx     — BountyBoard.branch_name
//   - UpsertConvoyAskBranch              — ConvoyAskBranches.ask_branch
//   - SetConvoyAskBranch                 — Convoys.ask_branch
//   - SetRepoRemoteInfo                  — Repositories.remote_url
//                                           + Repositories.default_branch
//
// AddRepo's repo-path input is validated at the CLI layer (cmd/force) where
// the RepoPathOptions{Base: ...} containment check has meaningful context.

// refNameMaxLen mirrors the same constant in internal/git/validators.go.
const refNameMaxLen = 256

// validateRefName enforces the git-check-ref-format(1) subset needed at the
// store boundary. Callers that write a branch / ref name to the DB MUST
// route through this.
//
// The list is deliberately identical to internal/git.ValidateRef so a
// downstream shell call against a passed-through value never sees a
// different semantic.
func validateRefName(name string) error {
	if name == "" {
		return fmt.Errorf("invalid git ref: empty")
	}
	if len(name) > refNameMaxLen {
		return fmt.Errorf("invalid git ref: exceeds %d chars", refNameMaxLen)
	}
	if name[0] == '-' {
		return fmt.Errorf("invalid git ref: leading `-` (flag-injection hazard): %q", name)
	}
	if name[0] == '/' {
		return fmt.Errorf("invalid git ref: leading `/`: %q", name)
	}
	if strings.Contains(name, "..") {
		return fmt.Errorf("invalid git ref: contains `..`: %q", name)
	}
	if name[0] == '.' {
		return fmt.Errorf("invalid git ref: leading `.`: %q", name)
	}
	if name[len(name)-1] == '/' {
		return fmt.Errorf("invalid git ref: trailing `/`: %q", name)
	}
	if name[len(name)-1] == '.' {
		return fmt.Errorf("invalid git ref: trailing `.`: %q", name)
	}
	if strings.HasSuffix(name, ".lock") {
		return fmt.Errorf("invalid git ref: trailing `.lock`: %q", name)
	}
	if strings.Contains(name, "//") {
		return fmt.Errorf("invalid git ref: contains `//`: %q", name)
	}
	if strings.Contains(name, "@{") {
		return fmt.Errorf("invalid git ref: contains `@{`: %q", name)
	}
	if name == "@" {
		return fmt.Errorf("invalid git ref: reserved `@`")
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c < 0x20 || c == 0x7F {
			return fmt.Errorf("invalid git ref: control byte 0x%02x at offset %d", c, i)
		}
		switch c {
		case ' ', '~', '^', ':', '?', '*', '[', '\\':
			return fmt.Errorf("invalid git ref: forbidden character %q at offset %d", c, i)
		}
	}
	return nil
}

// validateRemoteURL is the store-side equivalent of git.ValidateRemoteURL for
// SetRepoRemoteInfo. Focuses on the attack surface that actually reaches
// downstream shell calls:
//   - leading-`-` (flag injection at `gh --repo` / `git clone`)
//   - embedded git-protocol flags (`--upload-pack=`, `--receive-pack=`)
//   - control bytes / newlines (log/shell smuggling)
//
// `file://` is NOT rejected at the store layer because the existing test
// corpus uses `file://` + local-path form as a fake-remote signal (the
// real `git clone` calls use LocalPath, not RemoteURL). The fuller
// reachability check (loopback/RFC1918 IPs, scheme allow-list) lives in
// internal/git.ValidateRemoteURL where cmd/force's AddRepo ingress uses
// it against the actual `git remote get-url origin` output.
func validateRemoteURL(raw string) error {
	if raw == "" {
		return fmt.Errorf("invalid remote URL: empty")
	}
	if raw[0] == '-' {
		return fmt.Errorf("invalid remote URL: leading `-`: %q", raw)
	}
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if c < 0x20 || c == 0x7F {
			return fmt.Errorf("invalid remote URL: control byte 0x%02x at offset %d", c, i)
		}
	}
	low := strings.ToLower(raw)
	if strings.Contains(low, "--upload-pack=") ||
		strings.Contains(low, "--receive-pack=") ||
		strings.Contains(low, "--config=") ||
		strings.Contains(low, "--exec=") {
		return fmt.Errorf("invalid remote URL: embedded git-flag hazard: %q", raw)
	}
	return nil
}
