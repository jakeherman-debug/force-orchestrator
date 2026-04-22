package agents

import (
	"strings"
	"testing"
)

// ── parseLibrarianOutput ────────────────────────────────────────────────────

func TestParseLibrarianOutput_ValidJSON(t *testing.T) {
	raw := `{"summary":"Added JWT auth middleware to the api handler.","tags":["authentication","jwt","middleware","auth"]}`
	summary, tagsCSV := parseLibrarianOutput(raw)
	if summary != "Added JWT auth middleware to the api handler." {
		t.Errorf("summary mismatch: %q", summary)
	}
	if !strings.Contains(tagsCSV, "authentication") || !strings.Contains(tagsCSV, "jwt") {
		t.Errorf("tags missing expected values: %q", tagsCSV)
	}
	// Tags should be comma-separated.
	if !strings.Contains(tagsCSV, ", ") {
		t.Errorf("expected comma-separated tags, got %q", tagsCSV)
	}
}

func TestParseLibrarianOutput_JSONWithSurroundingProse(t *testing.T) {
	// Claude sometimes wraps output in prose despite the prompt; ExtractJSON handles it.
	raw := "Here's the memory:\n" +
		`{"summary":"Fix session store.","tags":["session","store","bugfix"]}` +
		"\n\nLet me know if you need more."
	summary, tagsCSV := parseLibrarianOutput(raw)
	if summary != "Fix session store." {
		t.Errorf("summary extraction: %q", summary)
	}
	if !strings.Contains(tagsCSV, "session") {
		t.Errorf("tags missing: %q", tagsCSV)
	}
}

func TestParseLibrarianOutput_MalformedFallsBackToRaw(t *testing.T) {
	// Not valid JSON at all — the whole output becomes the summary, no tags.
	raw := "Just a plain prose summary from Claude, no structure at all."
	summary, tagsCSV := parseLibrarianOutput(raw)
	if summary != raw {
		t.Errorf("plain prose should become the summary as-is: %q", summary)
	}
	if tagsCSV != "" {
		t.Errorf("no tags expected for malformed output, got %q", tagsCSV)
	}
}

func TestParseLibrarianOutput_EmptyTagsAllowed(t *testing.T) {
	raw := `{"summary":"Minor fix.","tags":[]}`
	summary, tagsCSV := parseLibrarianOutput(raw)
	if summary != "Minor fix." {
		t.Errorf("summary: %q", summary)
	}
	if tagsCSV != "" {
		t.Errorf("empty tags should produce empty CSV, got %q", tagsCSV)
	}
}

func TestParseLibrarianOutput_DedupesAndNormalizesTags(t *testing.T) {
	raw := `{"summary":"s","tags":["Auth","auth","  JWT  ","jwt","",""]}`
	_, tagsCSV := parseLibrarianOutput(raw)
	// "Auth" and "auth" should dedupe to "auth"; "JWT" and "jwt" to "jwt";
	// empties dropped.
	parts := strings.Split(tagsCSV, ", ")
	if len(parts) != 2 {
		t.Errorf("expected 2 unique tags, got %d: %q", len(parts), tagsCSV)
	}
	for _, p := range parts {
		if p != "auth" && p != "jwt" {
			t.Errorf("unexpected tag %q (should be lowercased and trimmed)", p)
		}
	}
}

func TestParseLibrarianOutput_CapsTagCount(t *testing.T) {
	// More than 8 tags should be capped at 8.
	raw := `{"summary":"s","tags":["a","b","c","d","e","f","g","h","i","j","k"]}`
	_, tagsCSV := parseLibrarianOutput(raw)
	parts := strings.Split(tagsCSV, ", ")
	if len(parts) != 8 {
		t.Errorf("tag count should be capped at 8, got %d", len(parts))
	}
}
