package javaclient_test

import (
	"os"
	"testing"

	"force-orchestrator/internal/apiextract/consumer/javaclient"
)

func TestExtract_HappyPath(t *testing.T) {
	content, err := os.ReadFile("../../testdata/java-consumer/ApiClient.java")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	ext := javaclient.New()
	deps, err := ext.Extract("java-repo", "ApiClient.java", content)
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

func TestExtract_RestTemplateGetURL(t *testing.T) {
	ext := javaclient.New()
	src := []byte(`restTemplate.getForObject("https://api.example.com/users/{id}", User.class, id)`)
	deps, err := ext.Extract("repo", "f.java", src)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 1 {
		t.Fatalf("expected 1 dep, got %d", len(deps))
	}
	d := deps[0]
	if d.CallKind != "rest-template" {
		t.Errorf("want call_kind=rest-template, got %s", d.CallKind)
	}
	// {id} should be normalized to :id
	if d.APIIdentifier != "GET /users/:id" {
		t.Errorf("want 'GET /users/:id', got %q", d.APIIdentifier)
	}
}

func TestExtract_RestTemplatePost(t *testing.T) {
	ext := javaclient.New()
	src := []byte(`restTemplate.postForObject("/api/users", request, User.class)`)
	deps, err := ext.Extract("repo", "f.java", src)
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

func TestExtract_URLStripping(t *testing.T) {
	ext := javaclient.New()
	src := []byte(`restTemplate.getForObject("https://api.example.com/orders", Object.class)`)
	deps, err := ext.Extract("repo", "f.java", src)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 1 {
		t.Fatalf("expected 1 dep, got %d", len(deps))
	}
	if deps[0].APIIdentifier != "GET /orders" {
		t.Errorf("want 'GET /orders', got %q", deps[0].APIIdentifier)
	}
}

func TestExtract_OkHTTP(t *testing.T) {
	ext := javaclient.New()
	src := []byte(`.url("https://api.example.com/profile")`)
	deps, err := ext.Extract("repo", "f.java", src)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 1 {
		t.Fatalf("expected 1 dep, got %d", len(deps))
	}
	if deps[0].CallKind != "okhttp" {
		t.Errorf("want call_kind=okhttp, got %s", deps[0].CallKind)
	}
	if deps[0].APIIdentifier != "/profile" {
		t.Errorf("want '/profile', got %q", deps[0].APIIdentifier)
	}
}

func TestExtract_RetrofitAnnotations(t *testing.T) {
	ext := javaclient.New()
	src := []byte(`
		@GET("/api/users/{id}")
		@POST("/api/users")
		@DELETE("/api/users/{id}")
	`)
	deps, err := ext.Extract("repo", "f.java", src)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 3 {
		t.Fatalf("expected 3 retrofit deps, got %d", len(deps))
	}
	// First annotation: GET /api/users/:id
	if deps[0].CallKind != "retrofit" {
		t.Errorf("want call_kind=retrofit, got %s", deps[0].CallKind)
	}
	if deps[0].APIIdentifier != "GET /api/users/:id" {
		t.Errorf("want 'GET /api/users/:id', got %q", deps[0].APIIdentifier)
	}
}

func TestExtract_PathParamNormalization(t *testing.T) {
	ext := javaclient.New()
	src := []byte(`@GET("/api/users/{id}/orders/{orderId}")`)
	deps, err := ext.Extract("repo", "f.java", src)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 1 {
		t.Fatalf("expected 1 dep, got %d", len(deps))
	}
	if deps[0].APIIdentifier != "GET /api/users/:id/orders/:orderId" {
		t.Errorf("got %q", deps[0].APIIdentifier)
	}
}

func TestExtract_EmptyFile(t *testing.T) {
	ext := javaclient.New()
	deps, err := ext.Extract("repo", "Empty.java", []byte{})
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 0 {
		t.Errorf("expected 0 deps, got %d", len(deps))
	}
}

func TestExtract_NoMatch(t *testing.T) {
	ext := javaclient.New()
	src := []byte(`public class Foo { public void bar() { System.out.println("hi"); } }`)
	deps, err := ext.Extract("repo", "Foo.java", src)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 0 {
		t.Errorf("expected 0 deps, got %d", len(deps))
	}
}

func TestExtract_Idempotent(t *testing.T) {
	ext := javaclient.New()
	src := []byte(`restTemplate.getForObject("/api/users", Object.class)`)
	deps1, _ := ext.Extract("repo", "f.java", src)
	deps2, _ := ext.Extract("repo", "f.java", src)
	if len(deps1) != len(deps2) {
		t.Errorf("idempotency: first=%d second=%d", len(deps1), len(deps2))
	}
}

func TestSupportedCallKinds(t *testing.T) {
	kinds := javaclient.New().SupportedCallKinds()
	kindSet := make(map[string]bool)
	for _, k := range kinds {
		kindSet[k] = true
	}
	for _, want := range []string{"rest-template", "okhttp", "retrofit"} {
		if !kindSet[want] {
			t.Errorf("missing call_kind %q", want)
		}
	}
}
