package jsclient_test

import (
	"os"
	"testing"

	"force-orchestrator/internal/apiextract/consumer/jsclient"
)

func TestExtract_HappyPath(t *testing.T) {
	content, err := os.ReadFile("../../testdata/frontend-consumer/api-calls.ts")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	ext := jsclient.New()
	deps, err := ext.Extract("frontend-repo", "api-calls.ts", content)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	if len(deps) < 6 {
		t.Errorf("expected >=6 deps, got %d", len(deps))
		for _, d := range deps {
			t.Logf("  line %d  kind=%s  id=%q  conf=%.1f", d.ConsumerLine, d.CallKind, d.APIIdentifier, d.MatchConf)
		}
	}

	// Verify all ProviderAPIIDs are 0 (P6 resolves).
	for _, d := range deps {
		if d.ProviderAPIID != 0 {
			t.Errorf("expected ProviderAPIID=0, got %d (line %d)", d.ProviderAPIID, d.ConsumerLine)
		}
	}

	// Verify call kinds are only fetch or axios.
	for _, d := range deps {
		if d.CallKind != "fetch" && d.CallKind != "axios" {
			t.Errorf("unexpected call_kind %q at line %d", d.CallKind, d.ConsumerLine)
		}
	}
}

func TestExtract_FetchLiteral(t *testing.T) {
	ext := jsclient.New()
	src := []byte(`const r = fetch('/api/users/123');`)
	deps, err := ext.Extract("repo", "f.ts", src)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 1 {
		t.Fatalf("expected 1 dep, got %d", len(deps))
	}
	d := deps[0]
	if d.CallKind != "fetch" {
		t.Errorf("call_kind: want fetch, got %s", d.CallKind)
	}
	if d.APIIdentifier != "/api/users/123" {
		t.Errorf("api_identifier: want /api/users/123, got %s", d.APIIdentifier)
	}
	if d.MatchConf != 1.0 {
		t.Errorf("match_confidence: want 1.0, got %f", d.MatchConf)
	}
}

func TestExtract_FetchTemplateLiteral(t *testing.T) {
	ext := jsclient.New()
	src := []byte("const r = fetch(`/api/users/${id}`);")
	deps, err := ext.Extract("repo", "f.ts", src)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 1 {
		t.Fatalf("expected 1 dep, got %d", len(deps))
	}
	d := deps[0]
	if d.CallKind != "fetch" {
		t.Errorf("call_kind: want fetch, got %s", d.CallKind)
	}
	if d.APIIdentifier != "/api/users" {
		t.Errorf("api_identifier: want /api/users, got %q", d.APIIdentifier)
	}
	if d.MatchConf != 0.7 {
		t.Errorf("match_confidence: want 0.7, got %f", d.MatchConf)
	}
}

func TestExtract_AxiosMethodLiteral(t *testing.T) {
	ext := jsclient.New()
	src := []byte(`axios.post('/api/users', data);`)
	deps, err := ext.Extract("repo", "f.ts", src)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 1 {
		t.Fatalf("expected 1 dep, got %d", len(deps))
	}
	d := deps[0]
	if d.CallKind != "axios" {
		t.Errorf("call_kind: want axios, got %s", d.CallKind)
	}
	if d.APIIdentifier != "POST /api/users" {
		t.Errorf("api_identifier: want 'POST /api/users', got %q", d.APIIdentifier)
	}
	if d.MatchConf != 1.0 {
		t.Errorf("match_confidence: want 1.0, got %f", d.MatchConf)
	}
}

func TestExtract_AxiosTemplateLiteral(t *testing.T) {
	ext := jsclient.New()
	src := []byte("axios.delete(`/api/users/${id}`);")
	deps, err := ext.Extract("repo", "f.ts", src)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 1 {
		t.Fatalf("expected 1 dep, got %d", len(deps))
	}
	d := deps[0]
	if d.APIIdentifier != "DELETE /api/users" {
		t.Errorf("want 'DELETE /api/users', got %q", d.APIIdentifier)
	}
	if d.MatchConf != 0.7 {
		t.Errorf("want conf=0.7, got %f", d.MatchConf)
	}
}

func TestExtract_AxiosRequestObject(t *testing.T) {
	ext := jsclient.New()
	src := []byte("axios.request({\n  url: '/api/v1/users',\n  method: 'PUT',\n});")
	deps, err := ext.Extract("repo", "f.ts", src)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 1 {
		t.Fatalf("expected 1 dep, got %d", len(deps))
	}
	d := deps[0]
	if d.APIIdentifier != "PUT /api/v1/users" {
		t.Errorf("want 'PUT /api/v1/users', got %q", d.APIIdentifier)
	}
}

func TestExtract_NonHTTPPathIgnored(t *testing.T) {
	ext := jsclient.New()
	src := []byte(`
		const a = fetch('not-a-path');
		const b = fetch('relative/path');
		const c = axios.get('not-a-path');
	`)
	deps, err := ext.Extract("repo", "f.ts", src)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 0 {
		t.Errorf("expected 0 deps for non-HTTP paths, got %d", len(deps))
		for _, d := range deps {
			t.Logf("  %+v", d)
		}
	}
}

func TestExtract_EmptyFile(t *testing.T) {
	ext := jsclient.New()
	deps, err := ext.Extract("repo", "empty.ts", []byte{})
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 0 {
		t.Errorf("expected 0 deps for empty file, got %d", len(deps))
	}
}

func TestExtract_Idempotent(t *testing.T) {
	ext := jsclient.New()
	src := []byte(`fetch('/api/users'); axios.get('/api/orders');`)
	deps1, err := ext.Extract("repo", "f.ts", src)
	if err != nil {
		t.Fatal(err)
	}
	deps2, err := ext.Extract("repo", "f.ts", src)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps1) != len(deps2) {
		t.Errorf("idempotency: first=%d second=%d", len(deps1), len(deps2))
	}
	for i := range deps1 {
		if deps1[i].APIIdentifier != deps2[i].APIIdentifier {
			t.Errorf("idempotency: dep[%d] first=%q second=%q", i, deps1[i].APIIdentifier, deps2[i].APIIdentifier)
		}
	}
}

func TestSupportedCallKinds(t *testing.T) {
	kinds := jsclient.New().SupportedCallKinds()
	kindSet := make(map[string]bool)
	for _, k := range kinds {
		kindSet[k] = true
	}
	for _, want := range []string{"fetch", "axios"} {
		if !kindSet[want] {
			t.Errorf("missing call_kind %q", want)
		}
	}
}
