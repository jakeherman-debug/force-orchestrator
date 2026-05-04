// D7 — ShipGate / ConfirmPhaseRequired manifest-extension tests.
//
// These guard the manifest fields that authorize PromotionAuthor's
// promotion gate. The fields are the load-bearing seam for the D7
// roadmap directive that the ship gate requires BOTH quality-hold
// AND cost-drop (paired-runs.md § Anti-cheat directives + roadmap
// line 2090–2093). A regression that drops the persistence would let
// a future PromotionAuthor mint a proposal on cost-alone.
//
// Each test exercises a real-shape AuthorFromManifest → DB write →
// LoadShipGate read-back, and a parse of one actual on-disk D7 YAML
// from experiments/E7-*-*/manifest.yaml.

package experiments

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const d7ManifestWithShipGate = `
name: d7-shakedown
hypothesis: Haiku holds quality on captain.
subject_agent: captain
assignment_unit: task
stakes_tier: medium
analysis_framework_version: "2026-04-29"
min_practical_effect: 0.05

confirm_phase_required: true

treatments:
  - arm_label: control
    prompt_template_ref: captain/default@HEAD
    model: claude-sonnet-4-7
    target_cell_weight: 0.5
  - arm_label: treatment
    prompt_template_ref: captain/default@HEAD
    model: claude-haiku-4-5-20251001
    target_cell_weight: 0.5

metrics:
  - metric_name: captain_rejection_rate
    metric_version: "2026-04-23"
    direction: lower_is_better
    is_primary: true

ship_gate:
  quality: "P(haiku captain_rejection_rate <= control + 0.05) > 0.95"
  cost: "haiku captain_cost_per_call < 0.4 * control captain_cost_per_call"
`

func TestAuthorFromYAML_PersistsShipGate(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	id, err := AuthorFromBytes(ctx, db, []byte(d7ManifestWithShipGate))
	if err != nil {
		t.Fatalf("AuthorFromBytes: %v", err)
	}
	gate, confirm, err := LoadShipGate(ctx, db, id)
	if err != nil {
		t.Fatalf("LoadShipGate: %v", err)
	}
	if !confirm {
		t.Errorf("confirm_phase_required: got false, want true")
	}
	if gate == nil {
		t.Fatalf("ShipGate: got nil — expected populated")
	}
	if !strings.Contains(gate.Quality, "captain_rejection_rate") {
		t.Errorf("ShipGate.Quality: got %q — does not reference primary metric", gate.Quality)
	}
	if !strings.Contains(gate.Cost, "0.4") {
		t.Errorf("ShipGate.Cost: got %q — does not encode the < 0.4 × control invariant", gate.Cost)
	}
}

// TestAuthorFromYAML_NoShipGate_NoPersistence — when neither ship_gate
// nor confirm_phase_required is set, no SystemConfig row lands. This
// guards against accidentally minting an empty-but-present gate that
// PromotionAuthor would then evaluate against.
func TestAuthorFromYAML_NoShipGate_NoPersistence(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	id, err := AuthorFromBytes(ctx, db, []byte(minimalManifest))
	if err != nil {
		t.Fatalf("AuthorFromBytes: %v", err)
	}
	gate, confirm, err := LoadShipGate(ctx, db, id)
	if err != nil {
		t.Fatalf("LoadShipGate: %v", err)
	}
	if confirm {
		t.Errorf("confirm_phase_required: got true, want false (manifest unset)")
	}
	if gate != nil {
		t.Errorf("ShipGate: got %+v, want nil (manifest unset)", gate)
	}
}

// TestE7_OnDiskYAMLs_AllParseAndAuthor walks the on-disk E7 manifests
// the engineering deliverable shipped, parses each via AuthorFromBytes,
// and asserts the round-trip produces:
//   - exactly two arms (control + treatment)
//   - exactly one primary metric
//   - confirm_phase_required: true (anti-cheat directive)
//   - both arms set Model — the treatment arm is the Haiku id, the
//     control arm is non-empty (paired-runs.md treatment-spec
//     reproducibility requires the model frozen at experiment start)
//   - ShipGate present with non-empty Quality + Cost (anti-cheat:
//     promoting on cost alone is forbidden, so the YAML cannot omit
//     either).
func TestE7_OnDiskYAMLs_AllParseAndAuthor(t *testing.T) {
	root := d7RepoRoot(t)
	expDir := filepath.Join(root, "experiments")
	entries, err := os.ReadDir(expDir)
	if err != nil {
		t.Fatalf("read experiments dir: %v", err)
	}
	var e7yamls []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if !strings.HasPrefix(e.Name(), "E7-") {
			continue
		}
		e7yamls = append(e7yamls, filepath.Join(expDir, e.Name(), "manifest.yaml"))
	}
	if len(e7yamls) != 8 {
		t.Fatalf("E7 yaml count: got %d, want 8 (one per subject agent)", len(e7yamls))
	}
	const haikuModelID = "claude-haiku-4-5-20251001"
	for _, path := range e7yamls {
		t.Run(filepath.Base(filepath.Dir(path)), func(t *testing.T) {
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			db := openDB(t)
			ctx := context.Background()
			id, err := AuthorFromBytes(ctx, db, body)
			if err != nil {
				t.Fatalf("AuthorFromBytes %s: %v", path, err)
			}
			// Two arms.
			var armCount int
			db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ExperimentTreatments WHERE experiment_id = ?`, id).Scan(&armCount)
			if armCount != 2 {
				t.Errorf("%s: arm count: got %d, want 2", path, armCount)
			}
			// One primary metric.
			var primaryCount int
			db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ExperimentMetrics WHERE experiment_id = ? AND is_primary = 1`, id).Scan(&primaryCount)
			if primaryCount != 1 {
				t.Errorf("%s: primary metric count: got %d, want 1", path, primaryCount)
			}
			// Treatment arm must pin the haiku id.
			var treatmentModel string
			db.QueryRowContext(ctx, `
				SELECT IFNULL(s.model_identifier,'')
				FROM ExperimentTreatments t
				LEFT JOIN TreatmentSpecs s ON s.id = t.treatment_spec_id
				WHERE t.experiment_id = ? AND t.arm_label = 'treatment'
			`, id).Scan(&treatmentModel)
			if treatmentModel != haikuModelID {
				t.Errorf("%s: treatment arm model: got %q, want %q", path, treatmentModel, haikuModelID)
			}
			// Control arm must pin a non-empty model — reproducibility
			// requires the baseline frozen at start.
			var controlModel string
			db.QueryRowContext(ctx, `
				SELECT IFNULL(s.model_identifier,'')
				FROM ExperimentTreatments t
				LEFT JOIN TreatmentSpecs s ON s.id = t.treatment_spec_id
				WHERE t.experiment_id = ? AND t.arm_label = 'control'
			`, id).Scan(&controlModel)
			if controlModel == "" {
				t.Errorf("%s: control arm model is empty — reproducibility requires the baseline frozen", path)
			}
			// Ship gate + confirm phase: both required by roadmap.
			gate, confirm, err := LoadShipGate(ctx, db, id)
			if err != nil {
				t.Fatalf("%s: LoadShipGate: %v", path, err)
			}
			if !confirm {
				t.Errorf("%s: confirm_phase_required must be true (anti-cheat: roadmap line 2093)", path)
			}
			if gate == nil {
				t.Fatalf("%s: ship_gate must be populated (anti-cheat: gate is the load-bearing seam)", path)
			}
			if strings.TrimSpace(gate.Quality) == "" {
				t.Errorf("%s: ship_gate.quality empty — anti-cheat forbids cost-only promotion", path)
			}
			if strings.TrimSpace(gate.Cost) == "" {
				t.Errorf("%s: ship_gate.cost empty — anti-cheat forbids quality-only promotion", path)
			}
		})
	}
}

func d7RepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller")
	}
	// internal/experiments/<this>.go → ../..
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}
