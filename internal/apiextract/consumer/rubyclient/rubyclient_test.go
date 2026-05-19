package rubyclient_test

import (
	"os"
	"testing"

	"force-orchestrator/internal/apiextract/consumer/rubyclient"
)

func TestExtract_HappyPath(t *testing.T) {
	content, err := os.ReadFile("../../testdata/ruby-consumer/http_client.rb")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	ext := rubyclient.New()
	deps, err := ext.Extract("ruby-repo", "http_client.rb", content)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	if len(deps) < 4 {
		t.Errorf("expected >=4 deps, got %d", len(deps))
		for _, d := range deps {
			t.Logf("  line %d  kind=%s  id=%q", d.ConsumerLine, d.CallKind, d.APIIdentifier)
		}
	}

	for _, d := range deps {
		if d.ProviderAPIID != 0 {
			t.Errorf("expected ProviderAPIID=0, got %d", d.ProviderAPIID)
		}
	}
}

func TestExtract_URLStripping(t *testing.T) {
	ext := rubyclient.New()
	src := []byte(`HTTParty.get('https://api.example.com/users')`)
	deps, err := ext.Extract("repo", "f.rb", src)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 1 {
		t.Fatalf("expected 1 dep, got %d", len(deps))
	}
	d := deps[0]
	if d.APIIdentifier != "GET /users" {
		t.Errorf("want 'GET /users', got %q", d.APIIdentifier)
	}
	if d.CallKind != "httparty" {
		t.Errorf("want call_kind=httparty, got %s", d.CallKind)
	}
}

func TestExtract_HTTPartyPost(t *testing.T) {
	ext := rubyclient.New()
	src := []byte(`HTTParty.post('/api/users', body: params)`)
	deps, err := ext.Extract("repo", "f.rb", src)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 1 {
		t.Fatalf("expected 1 dep, got %d", len(deps))
	}
	if deps[0].APIIdentifier != "POST /api/users" {
		t.Errorf("want 'POST /api/users', got %q", deps[0].APIIdentifier)
	}
}

func TestExtract_FaradayGet(t *testing.T) {
	ext := rubyclient.New()
	src := []byte(`Faraday.get('/api/users')`)
	deps, err := ext.Extract("repo", "f.rb", src)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 1 {
		t.Fatalf("expected 1 dep, got %d", len(deps))
	}
	if deps[0].CallKind != "faraday" {
		t.Errorf("want call_kind=faraday, got %s", deps[0].CallKind)
	}
	if deps[0].APIIdentifier != "GET /api/users" {
		t.Errorf("want 'GET /api/users', got %q", deps[0].APIIdentifier)
	}
}

func TestExtract_ConnGet(t *testing.T) {
	ext := rubyclient.New()
	src := []byte(`conn.get('/api/users/profile')`)
	deps, err := ext.Extract("repo", "f.rb", src)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 1 {
		t.Fatalf("expected 1 dep, got %d", len(deps))
	}
	if deps[0].CallKind != "faraday" {
		t.Errorf("want call_kind=faraday, got %s", deps[0].CallKind)
	}
}

func TestExtract_NetHTTP(t *testing.T) {
	ext := rubyclient.New()
	src := []byte(`Net::HTTP.get(URI('https://api.example.com/users'))`)
	deps, err := ext.Extract("repo", "f.rb", src)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 1 {
		t.Fatalf("expected 1 dep, got %d", len(deps))
	}
	if deps[0].CallKind != "net-http" {
		t.Errorf("want call_kind=net-http, got %s", deps[0].CallKind)
	}
	if deps[0].APIIdentifier != "GET /users" {
		t.Errorf("want 'GET /users', got %q", deps[0].APIIdentifier)
	}
}

func TestExtract_RestClient(t *testing.T) {
	ext := rubyclient.New()
	src := []byte(`RestClient.get('https://api.example.com/settings')`)
	deps, err := ext.Extract("repo", "f.rb", src)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 1 {
		t.Fatalf("expected 1 dep, got %d", len(deps))
	}
	if deps[0].CallKind != "rest-client" {
		t.Errorf("want call_kind=rest-client, got %s", deps[0].CallKind)
	}
}

func TestExtract_EmptyFile(t *testing.T) {
	ext := rubyclient.New()
	deps, err := ext.Extract("repo", "empty.rb", []byte{})
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 0 {
		t.Errorf("expected 0 deps for empty file, got %d", len(deps))
	}
}

func TestExtract_NoMatch(t *testing.T) {
	ext := rubyclient.New()
	src := []byte(`def nothing; puts "hello"; end`)
	deps, err := ext.Extract("repo", "f.rb", src)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 0 {
		t.Errorf("expected 0 deps, got %d", len(deps))
	}
}

func TestExtract_Idempotent(t *testing.T) {
	ext := rubyclient.New()
	src := []byte("HTTParty.get('/api/users')\nFaraday.get('/api/orders')\n")
	deps1, _ := ext.Extract("repo", "f.rb", src)
	deps2, _ := ext.Extract("repo", "f.rb", src)
	if len(deps1) != len(deps2) {
		t.Errorf("idempotency: first=%d second=%d", len(deps1), len(deps2))
	}
	for i := range deps1 {
		if deps1[i].APIIdentifier != deps2[i].APIIdentifier {
			t.Errorf("idempotency mismatch at dep[%d]", i)
		}
	}
}

func TestSupportedCallKinds(t *testing.T) {
	kinds := rubyclient.New().SupportedCallKinds()
	kindSet := make(map[string]bool)
	for _, k := range kinds {
		kindSet[k] = true
	}
	for _, want := range []string{"httparty", "faraday", "net-http", "rest-client"} {
		if !kindSet[want] {
			t.Errorf("missing call_kind %q", want)
		}
	}
}
