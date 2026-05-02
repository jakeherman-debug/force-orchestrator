// Package spdx is a deterministic SPDX-license-id detector. Given the
// raw bytes of a LICENSE / LICENSE.md / LICENSE.txt file, returns the
// SPDX ID for the ~10 most common open-source licenses, or "Unknown"
// when nothing matches.
//
// Approach: each canonical license has a unique fingerprint phrase
// (e.g. "Apache License" + "Version 2.0", "Permission is hereby
// granted, free of charge" for MIT). We match on lowercased,
// whitespace-collapsed input — same shape go-license-detector and
// licensee use under the hood, hand-written for the small set we
// need. SUPPLY-004 (license compatibility) only consumes a small
// matrix, so coverage of the long tail is low-value.
//
// Out-of-scope: dual-license headers, custom license files, partial
// matches. Returns "Unknown" so the caller can advise-mode + log for
// operator review (per docs/roadmap.md § D5 anti-cheat: "no LLM
// decides licenses").
package spdx

import (
	"path/filepath"
	"regexp"
	"strings"
)

// Unknown is the sentinel SPDX id when no canonical license matches.
const Unknown = "Unknown"

// Detect returns the SPDX id for the supplied LICENSE-file body.
// Empty input returns Unknown (NOT an error — empty LICENSE file is a
// real condition we surface for operator review).
func Detect(content []byte) string {
	if len(content) == 0 {
		return Unknown
	}
	norm := normalize(string(content))
	for _, m := range matchers {
		if m.match(norm) {
			return m.spdx
		}
	}
	return Unknown
}

// IsLicenseFilename returns true if `path`'s basename matches the
// LICENSE-family naming convention (LICENSE, LICENSE.md, LICENSE.txt,
// COPYING, COPYING.md, with case-insensitive matching). Used by the
// AddRepo wiring to decide which file to feed Detect.
func IsLicenseFilename(path string) bool {
	base := strings.ToUpper(filepath.Base(path))
	switch base {
	case "LICENSE", "LICENSE.MD", "LICENSE.TXT", "LICENSE.RST",
		"COPYING", "COPYING.MD", "COPYING.TXT",
		"UNLICENSE", "UNLICENSE.MD":
		return true
	}
	return false
}

// matcher is one canonical-license signature.
type matcher struct {
	spdx     string
	required []string // ALL must appear (post-normalize)
}

func (m matcher) match(norm string) bool {
	for _, r := range m.required {
		if !strings.Contains(norm, r) {
			return false
		}
	}
	return true
}

// matchers covers the ~10 most common SPDX ids. Order matters when
// multiple licenses share signature substrings — list the more-
// specific id first so it claims the match. (e.g. AGPL is checked
// before GPL.)
var matchers = []matcher{
	// Apache-2.0
	{
		spdx:     "Apache-2.0",
		required: []string{"apache license", "version 2.0"},
	},
	// MPL-2.0 — checked before GPL (shares "you must" phrasing).
	{
		spdx:     "MPL-2.0",
		required: []string{"mozilla public license", "version 2.0"},
	},
	// MPL-1.1 fallback
	{
		spdx:     "MPL-1.1",
		required: []string{"mozilla public license", "version 1.1"},
	},
	// AGPL-3.0 — checked before GPL-3.0.
	{
		spdx:     "AGPL-3.0",
		required: []string{"gnu affero general public license", "version 3"},
	},
	// LGPL-3.0 — checked before GPL-3.0.
	{
		spdx:     "LGPL-3.0",
		required: []string{"gnu lesser general public license", "version 3"},
	},
	// LGPL-2.1
	{
		spdx:     "LGPL-2.1",
		required: []string{"gnu lesser general public license", "version 2.1"},
	},
	// GPL-3.0
	{
		spdx:     "GPL-3.0",
		required: []string{"gnu general public license", "version 3"},
	},
	// GPL-2.0
	{
		spdx:     "GPL-2.0",
		required: []string{"gnu general public license", "version 2"},
	},
	// BSD-3-Clause
	{
		spdx:     "BSD-3-Clause",
		required: []string{"redistribution and use in source and binary forms",
			"neither the name"},
	},
	// BSD-2-Clause
	{
		spdx:     "BSD-2-Clause",
		required: []string{"redistribution and use in source and binary forms",
			"this list of conditions"},
	},
	// MIT — checked after BSD (BSD shares some phrasing).
	{
		spdx:     "MIT",
		required: []string{"permission is hereby granted, free of charge",
			"the software is provided"},
	},
	// ISC
	{
		spdx:     "ISC",
		required: []string{"permission to use, copy, modify",
			"isc"},
	},
	// Unlicense
	{
		spdx:     "Unlicense",
		required: []string{"this is free and unencumbered software released into the public domain"},
	},
	// CC0-1.0
	{
		spdx:     "CC0-1.0",
		required: []string{"creative commons", "cc0"},
	},
}

// whitespaceRe collapses any run of whitespace into a single space.
var whitespaceRe = regexp.MustCompile(`\s+`)

// normalize lowercases and collapses whitespace so format-only
// differences in canonical license text don't break detection.
func normalize(s string) string {
	s = strings.ToLower(s)
	s = whitespaceRe.ReplaceAllString(s, " ")
	return s
}
