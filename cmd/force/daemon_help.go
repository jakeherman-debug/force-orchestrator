package main

// D12 — daemon subcommand help + unknown-flag rejection.
//
// As of fix(cli)/cli-flag-parsing this file is a thin wrapper around
// the generalized helper in cli_flags.go. parseDaemonFlags is kept so
// the existing daemon handlers don't need to change their call sites,
// but it now delegates to parseSubcommandFlags with the "daemon "
// prefix on the name (which the helper detects to route the rendered
// "Usage: force daemon <name>" line through printDaemonSubcommandHelp).
//
// Background: D12 P4 found that `force daemon install --help` silently
// ran the install (writing the launchd plist) because the handler
// ignored unrecognized tokens. Generalization to the rest of the CLI
// surface is documented in cli_flags.go.

import (
	"flag"
	"fmt"
	"io"
)

// flagDoc describes one flag for help-text rendering. Shared between
// the daemon-specific printer below and the generic printer in
// cli_flags.go.
type flagDoc struct {
	Name string // e.g. "--dry-run" or "--binary <path>"
	Desc string // one-line description
}

// printDaemonSubcommandHelp writes the standard help block to w with
// the "Usage: force daemon <name>" prefix. Used by:
//
//   - the per-subcommand --help handler (via parseDaemonFlags →
//     parseSubcommandFlags's daemon-aware branch), AND
//   - the `force daemon help <sub>` surface in dispatchDaemon.
//
// Kept as a separate function (rather than collapsed into
// printSubcommandHelp) so the daemon family's "force daemon <sub>"
// prefix is preserved verbatim — daemon-help-test.go asserts on the
// exact string.
func printDaemonSubcommandHelp(w io.Writer, name, desc string, flags []flagDoc, examples []string) {
	fmt.Fprintf(w, "Usage: force daemon %s [flags]\n\n", name)
	if desc != "" {
		fmt.Fprintf(w, "%s\n\n", desc)
	}
	if len(flags) > 0 {
		fmt.Fprintln(w, "Flags:")
		width := 0
		for _, f := range flags {
			if len(f.Name) > width {
				width = len(f.Name)
			}
		}
		if width < 18 {
			width = 18
		}
		for _, f := range flags {
			fmt.Fprintf(w, "  %-*s  %s\n", width, f.Name, f.Desc)
		}
		fmt.Fprintln(w)
	}
	if len(examples) > 0 {
		fmt.Fprintln(w, "Examples:")
		for _, ex := range examples {
			fmt.Fprintf(w, "  %s\n", ex)
		}
	}
}

// parseDaemonFlags wraps parseSubcommandFlags with the "daemon "
// name prefix so the rendered usage stays "Usage: force daemon
// <name>" — preserving daemon_help_test.go's expectations.
//
// Behavior is identical to the pre-generalization parseDaemonFlags:
//
//  1. --help / -h prints help to stdout, returns helpRequested=true.
//  2. Unknown flags print usage to stderr, return parseErr.
//
// Daemon handlers MUST keep calling this (rather than parseSubcommandFlags
// directly) so the AST audit Pattern P_CLIFlagParsing's daemon-family
// allowlist + the existing daemon_help_test.go assertions both keep
// holding without further updates.
func parseDaemonFlags(fs *flag.FlagSet, args []string, name, desc string, flagDocs []flagDoc, examples []string) (bool, error) {
	return parseSubcommandFlags(fs, args, "daemon "+name, desc, flagDocs, examples)
}
