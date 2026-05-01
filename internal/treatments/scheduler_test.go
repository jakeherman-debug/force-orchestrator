package treatments

import (
	"reflect"
	"testing"
)

// TestConflictsWith_DisjointFactors_False — two factorial experiments
// whose factor sets don't intersect can run on the same unit. This is
// the orthogonal-overlap green path (paired-runs.md § Orthogonal
// dimension invariant).
func TestConflictsWith_DisjointFactors_False(t *testing.T) {
	a := &ExperimentDescriptor{ID: 1, Kind: "factorial", Factors: []string{"prompt"}}
	b := &ExperimentDescriptor{ID: 2, Kind: "factorial", Factors: []string{"rules"}}
	if ConflictsWith(a, b) {
		t.Errorf("disjoint factor sets {prompt} vs {rules} should NOT conflict")
	}
	if ConflictsWith(b, a) {
		t.Errorf("conflict not symmetric")
	}
}

// TestConflictsWith_SharedFactor_True — overlapping factor names
// trigger the canonical factorial-overlap conflict rule.
func TestConflictsWith_SharedFactor_True(t *testing.T) {
	a := &ExperimentDescriptor{ID: 1, Kind: "factorial", Factors: []string{"prompt", "rules"}}
	b := &ExperimentDescriptor{ID: 2, Kind: "factorial", Factors: []string{"prompt"}}
	if !ConflictsWith(a, b) {
		t.Errorf("shared factor 'prompt' should conflict")
	}
}

// TestConflictsWith_SharedPromptTemplate_True — two single-treatment
// experiments touching the same prompt slot on the same agent
// confound each other.
func TestConflictsWith_SharedPromptTemplate_True(t *testing.T) {
	a := &ExperimentDescriptor{
		ID:                 1,
		Kind:               "single",
		SubjectAgent:       "captain",
		PromptTemplateRefs: []string{"captain/default@HEAD"},
	}
	b := &ExperimentDescriptor{
		ID:                 2,
		Kind:               "single",
		SubjectAgent:       "captain",
		PromptTemplateRefs: []string{"captain/default@HEAD"},
	}
	if !ConflictsWith(a, b) {
		t.Errorf("same agent + same prompt_template_ref should conflict")
	}
}

// TestConflictsWith_SharedMetric_True — two experiments both declaring
// the same primary metric compete for the same scoring channel.
func TestConflictsWith_SharedMetric_True(t *testing.T) {
	a := &ExperimentDescriptor{
		ID:                 1,
		Kind:               "single",
		SubjectAgent:       "captain",
		PromptTemplateRefs: []string{"captain/default@HEAD"},
		PrimaryMetric:      "approval_rate",
	}
	b := &ExperimentDescriptor{
		ID:                 2,
		Kind:               "single",
		SubjectAgent:       "council",
		PromptTemplateRefs: []string{"council/default@HEAD"},
		PrimaryMetric:      "approval_rate",
	}
	if !ConflictsWith(a, b) {
		t.Errorf("shared primary metric 'approval_rate' should conflict")
	}
}

// TestConflictsWith_DisjointSingleTreatments_False — two
// single-treatment experiments on the same agent but touching
// different prompts and tracking different metrics do not conflict.
func TestConflictsWith_DisjointSingleTreatments_False(t *testing.T) {
	a := &ExperimentDescriptor{
		ID:                 1,
		Kind:               "single",
		SubjectAgent:       "captain",
		PromptTemplateRefs: []string{"captain/promptA@HEAD"},
		PrimaryMetric:      "approval_rate",
	}
	b := &ExperimentDescriptor{
		ID:                 2,
		Kind:               "single",
		SubjectAgent:       "captain",
		PromptTemplateRefs: []string{"captain/promptB@HEAD"},
		PrimaryMetric:      "rework_count",
	}
	if ConflictsWith(a, b) {
		t.Errorf("different prompts + different metrics should NOT conflict")
	}
}

// TestSelectOrthogonal_TwoNonConflictingPicksBoth — both candidates
// are orthogonal; both selected.
func TestSelectOrthogonal_TwoNonConflictingPicksBoth(t *testing.T) {
	a := &ExperimentDescriptor{ID: 1, Kind: "factorial", Factors: []string{"prompt"}}
	b := &ExperimentDescriptor{ID: 2, Kind: "factorial", Factors: []string{"rules"}}
	got := SelectOrthogonalEnrollments("task", 42, []*ExperimentDescriptor{a, b})
	if len(got) != 2 {
		t.Fatalf("expected 2 selected, got %d", len(got))
	}
	if got[0].ID != 1 || got[1].ID != 2 {
		t.Errorf("expected [1, 2], got [%d, %d]", got[0].ID, got[1].ID)
	}
}

// TestSelectOrthogonal_TwoConflictingPicksOne — overlapping factors;
// lowest id wins, the other is skipped.
func TestSelectOrthogonal_TwoConflictingPicksOne(t *testing.T) {
	a := &ExperimentDescriptor{ID: 1, Kind: "factorial", Factors: []string{"prompt"}}
	b := &ExperimentDescriptor{ID: 2, Kind: "factorial", Factors: []string{"prompt", "rules"}}
	got := SelectOrthogonalEnrollments("task", 42, []*ExperimentDescriptor{a, b})
	if len(got) != 1 {
		t.Fatalf("expected 1 selected, got %d", len(got))
	}
	if got[0].ID != 1 {
		t.Errorf("expected lowest-id (1) to win tie-break, got %d", got[0].ID)
	}
}

// TestSelectOrthogonal_DenseConflict_PicksMaximal — A,B both touch
// "prompt"; C,D both touch "rules". Greedy id-order selection picks
// {A, C} — the maximal orthogonal subset.
func TestSelectOrthogonal_DenseConflict_PicksMaximal(t *testing.T) {
	a := &ExperimentDescriptor{ID: 1, Kind: "factorial", Factors: []string{"prompt"}}
	b := &ExperimentDescriptor{ID: 2, Kind: "factorial", Factors: []string{"prompt"}}
	c := &ExperimentDescriptor{ID: 3, Kind: "factorial", Factors: []string{"rules"}}
	d := &ExperimentDescriptor{ID: 4, Kind: "factorial", Factors: []string{"rules"}}
	got := SelectOrthogonalEnrollments("task", 1, []*ExperimentDescriptor{a, b, c, d})
	if len(got) != 2 {
		t.Fatalf("expected 2 selected, got %d", len(got))
	}
	if got[0].ID != 1 || got[1].ID != 3 {
		t.Errorf("expected [1, 3], got [%d, %d]", got[0].ID, got[1].ID)
	}
}

// TestSelectOrthogonal_DeterministicAcrossRuns — the load-bearing
// property for sticky assignment under retries (paired-runs.md
// § Sticky task retries). Same input → same output.
func TestSelectOrthogonal_DeterministicAcrossRuns(t *testing.T) {
	candidates := []*ExperimentDescriptor{
		{ID: 3, Kind: "factorial", Factors: []string{"rules"}},
		{ID: 1, Kind: "factorial", Factors: []string{"prompt"}},
		{ID: 5, Kind: "factorial", Factors: []string{"prompt"}},
		{ID: 2, Kind: "factorial", Factors: []string{"model"}},
	}
	first := SelectOrthogonalEnrollments("task", 100, candidates)
	for i := 0; i < 10; i++ {
		again := SelectOrthogonalEnrollments("task", 100, candidates)
		if !sameIDOrder(first, again) {
			t.Fatalf("non-deterministic selection: first=%v, again=%v", idsOf(first), idsOf(again))
		}
	}
	// Deterministic shape we expect: lowest-id wins → 1 (prompt), then
	// 2 (model), then 3 (rules); 5 conflicts with 1 on "prompt".
	want := []int{1, 2, 3}
	if !reflect.DeepEqual(idsOf(first), want) {
		t.Errorf("selection ids: got %v, want %v", idsOf(first), want)
	}
}

// TestParseFactors_CanonicalAndBareShapes — the parser accepts both
// the documented `[{name, levels}]` shape and the convenience bare
// string array.
func TestParseFactors_CanonicalAndBareShapes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"empty array", "[]", nil},
		{"canonical", `[{"name":"prompt","levels":["A","B"]},{"name":"rules","levels":["on","off"]}]`, []string{"prompt", "rules"}},
		{"bare", `["prompt","rules"]`, []string{"prompt", "rules"}},
		{"unparseable", `not json`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseFactors(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseFactors(%q): got %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func sameIDOrder(a, b []*ExperimentDescriptor) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID {
			return false
		}
	}
	return true
}

func idsOf(xs []*ExperimentDescriptor) []int {
	out := make([]int, len(xs))
	for i, x := range xs {
		out[i] = x.ID
	}
	return out
}
