package rules

import "testing"

// TestISB010_Red_LLMResponseUnmarshalNoDisallow — json.Unmarshal in a
// function named "ParseClaudeResponse" without DisallowUnknownFields
// triggers a finding.
func TestISB010_Red_LLMResponseUnmarshalNoDisallow(t *testing.T) {
	src := `package x
import "encoding/json"
type R struct{ X string }
func ParseClaudeResponse(data []byte) (R, error) {
	var r R
	err := json.Unmarshal(data, &r)
	return r, err
}
`
	out := runRule(t, isb010{}, "internal/foo/p.go", src)
	assertHasFinding(t, out, "ISB-010", "")
}

// TestISB010_Red_AgentsPathTriggersHeuristic — same shape but in
// internal/agents/ triggers regardless of function name.
func TestISB010_Red_AgentsPathTriggersHeuristic(t *testing.T) {
	src := `package agents
import "encoding/json"
type R struct{ X string }
func parse(data []byte) (R, error) {
	var r R
	err := json.Unmarshal(data, &r)
	return r, err
}
`
	out := runRule(t, isb010{}, "internal/agents/parse.go", src)
	assertHasFinding(t, out, "ISB-010", "")
}

// TestISB010_Green_DisallowUnknownFieldsPresent — same shape but the
// function uses json.NewDecoder + DisallowUnknownFields.
func TestISB010_Green_DisallowUnknownFieldsPresent(t *testing.T) {
	src := `package x
import (
	"bytes"
	"encoding/json"
)
type R struct{ X string }
func ParseClaudeResponse(data []byte) (R, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var r R
	err := dec.Decode(&r)
	return r, err
}
`
	out := runRule(t, isb010{}, "internal/foo/p.go", src)
	assertNoFindings(t, out)
}
