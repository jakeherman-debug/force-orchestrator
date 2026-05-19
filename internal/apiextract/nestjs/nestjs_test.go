package nestjs_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"force-orchestrator/internal/apiextract/nestjs"
	"force-orchestrator/internal/store"
)

// testdataDir returns the path to internal/apiextract/testdata relative to
// this test file's location.
func testdataDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "testdata")
}

// expectedRow is the minimal shape we compare from expected_nestjs.json.
type expectedRow struct {
	APIIdentifier string `json:"APIIdentifier"`
	SourceLine    int    `json:"SourceLine"`
	Extractor     string `json:"Extractor"`
}

func TestNestJSExtract_Fixture(t *testing.T) {
	td := testdataDir(t)
	fixturePath := filepath.Join(td, "nestjs-app", "user.controller.ts")
	expectedPath := filepath.Join(td, "expected_nestjs.json")

	content, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	var want []expectedRow
	expBytes, err := os.ReadFile(expectedPath)
	if err != nil {
		t.Fatalf("read expected: %v", err)
	}
	if err := json.Unmarshal(expBytes, &want); err != nil {
		t.Fatalf("parse expected: %v", err)
	}

	ext := nestjs.Extractor{}
	got, err := ext.Extract("test-repo", fixturePath, content)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	// Build a map from APIIdentifier for fast lookup.
	gotByID := make(map[string]store.CrossRepoAPI, len(got))
	for _, r := range got {
		gotByID[r.APIIdentifier] = r
	}

	matched := 0
	for _, w := range want {
		r, ok := gotByID[w.APIIdentifier]
		if !ok {
			t.Errorf("missing route %q", w.APIIdentifier)
			continue
		}
		if r.SourceLine != w.SourceLine {
			t.Errorf("route %q: SourceLine = %d, want %d", w.APIIdentifier, r.SourceLine, w.SourceLine)
		}
		if r.Extractor != w.Extractor {
			t.Errorf("route %q: Extractor = %q, want %q", w.APIIdentifier, r.Extractor, w.Extractor)
		}
		matched++
	}

	// Accuracy check: matched / expected ≥ 0.85.
	const minAccuracy = 0.85
	accuracy := float64(matched) / float64(len(want))
	if accuracy < minAccuracy {
		t.Errorf("accuracy %.2f < %.2f (%d/%d routes matched)",
			accuracy, minAccuracy, matched, len(want))
	}
	t.Logf("NestJS extractor: %d routes extracted, %d/%d expected matched (accuracy %.0f%%)",
		len(got), matched, len(want), accuracy*100)
}

func TestNestJSExtract_Metadata(t *testing.T) {
	ext := nestjs.Extractor{}
	if got := ext.Kind(); got != "http_route" {
		t.Errorf("Kind() = %q, want %q", got, "http_route")
	}
	if got := ext.ExtractorName(); got != "nestjs-decorator" {
		t.Errorf("ExtractorName() = %q, want %q", got, "nestjs-decorator")
	}
}

func TestNestJSExtract_Empty(t *testing.T) {
	ext := nestjs.Extractor{}
	got, err := ext.Extract("repo", "empty.ts", nil)
	if err != nil {
		t.Fatalf("Extract(nil): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Extract(nil): got %d rows, want 0", len(got))
	}
}

func TestNestJSExtract_CombinesPaths(t *testing.T) {
	src := []byte(`
@Controller('api')
export class Ctrl {
  @Get('items/:id')
  find() {}
}
`)
	ext := nestjs.Extractor{}
	got, err := ext.Extract("repo", "ctrl.ts", src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("no routes returned")
	}
	want := "GET /api/items/:id"
	if got[0].APIIdentifier != want {
		t.Errorf("APIIdentifier = %q, want %q", got[0].APIIdentifier, want)
	}
}

func TestNestJSExtract_EmptyControllerPath(t *testing.T) {
	src := []byte(`
@Controller()
export class Ctrl {
  @Get('status')
  check() {}
}
`)
	ext := nestjs.Extractor{}
	got, err := ext.Extract("repo", "ctrl.ts", src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("no routes returned")
	}
	want := "GET /status"
	if got[0].APIIdentifier != want {
		t.Errorf("APIIdentifier = %q, want %q", got[0].APIIdentifier, want)
	}
}

func TestNestJSExtract_EmptyMethodPath(t *testing.T) {
	src := []byte(`
@Controller('health')
export class HealthCtrl {
  @Get()
  check() {}
}
`)
	ext := nestjs.Extractor{}
	got, err := ext.Extract("repo", "ctrl.ts", src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("no routes returned")
	}
	want := "GET /health"
	if got[0].APIIdentifier != want {
		t.Errorf("APIIdentifier = %q, want %q", got[0].APIIdentifier, want)
	}
}

func TestNestJSExtract_CurlyParamNormalization(t *testing.T) {
	// NestJS typically uses :param form but some codebases use {param}.
	src := []byte(`
@Controller('v1')
export class Ctrl {
  @Get('items/{id}')
  find() {}
}
`)
	ext := nestjs.Extractor{}
	got, err := ext.Extract("repo", "ctrl.ts", src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("no routes returned")
	}
	// {id} should be normalized to :id via store.NormalizeAPIPath
	want := "GET /v1/items/:id"
	if got[0].APIIdentifier != want {
		t.Errorf("APIIdentifier = %q, want %q", got[0].APIIdentifier, want)
	}
}

func TestNestJSExtract_MultipleControllers(t *testing.T) {
	src := []byte(`
@Controller('users')
export class UserCtrl {
  @Get(':id')
  find() {}
}

@Controller('posts')
export class PostCtrl {
  @Post()
  create() {}
}
`)
	ext := nestjs.Extractor{}
	got, err := ext.Extract("repo", "ctrl.ts", src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	byID := make(map[string]bool)
	for _, r := range got {
		byID[r.APIIdentifier] = true
	}
	for _, want := range []string{"GET /users/:id", "POST /posts"} {
		if !byID[want] {
			t.Errorf("missing route %q; got %v", want, got)
		}
	}
}

func TestNestJSExtract_Idempotent(t *testing.T) {
	td := testdataDir(t)
	content, err := os.ReadFile(filepath.Join(td, "nestjs-app", "user.controller.ts"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	ext := nestjs.Extractor{}
	got1, _ := ext.Extract("repo", "f.ts", content)
	got2, _ := ext.Extract("repo", "f.ts", content)
	if len(got1) != len(got2) {
		t.Errorf("idempotent: first=%d, second=%d", len(got1), len(got2))
	}
}
