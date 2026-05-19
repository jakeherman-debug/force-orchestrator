package grpcclient_test

import (
	"os"
	"testing"

	"force-orchestrator/internal/apiextract/consumer/grpcclient"
)

func TestExtract_GoHappyPath(t *testing.T) {
	content, err := os.ReadFile("../../testdata/grpc-consumer/user_client.go")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	ext := grpcclient.New()
	deps, err := ext.Extract("grpc-repo", "user_client.go", content)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	if len(deps) < 2 {
		t.Errorf("expected >=2 deps, got %d", len(deps))
		for _, d := range deps {
			t.Logf("  line %d  kind=%s  id=%q", d.ConsumerLine, d.CallKind, d.APIIdentifier)
		}
	}

	for _, d := range deps {
		if d.CallKind != "grpc-client" {
			t.Errorf("want call_kind=grpc-client, got %s", d.CallKind)
		}
		if d.ProviderAPIID != 0 {
			t.Errorf("expected ProviderAPIID=0, got %d", d.ProviderAPIID)
		}
	}
}

func TestExtract_GoServiceAndMethod(t *testing.T) {
	ext := grpcclient.New()
	src := []byte(`
		client := pb.NewUserServiceClient(conn)
		resp, err := client.GetUser(ctx, &pb.GetUserRequest{Id: id})
	`)
	deps, err := ext.Extract("repo", "client.go", src)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) < 1 {
		t.Fatalf("expected >=1 dep, got %d", len(deps))
	}
	found := false
	for _, d := range deps {
		if d.APIIdentifier == "UserService/GetUser" {
			found = true
		}
	}
	if !found {
		t.Errorf("did not find UserService/GetUser; got:")
		for _, d := range deps {
			t.Logf("  %q", d.APIIdentifier)
		}
	}
}

func TestExtract_JavaStubAndMethod(t *testing.T) {
	ext := grpcclient.New()
	src := []byte(`
		UserServiceGrpc.UserServiceBlockingStub stub = UserServiceGrpc.newBlockingStub(channel);
		stub.getUser(request);
	`)
	deps, err := ext.Extract("repo", "Client.java", src)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) < 1 {
		t.Fatalf("expected >=1 dep, got %d", len(deps))
	}
	found := false
	for _, d := range deps {
		if d.APIIdentifier == "UserService/GetUser" {
			found = true
		}
	}
	if !found {
		t.Errorf("did not find UserService/GetUser; got:")
		for _, d := range deps {
			t.Logf("  %q", d.APIIdentifier)
		}
	}
}

func TestExtract_EmptyFile(t *testing.T) {
	ext := grpcclient.New()
	deps, err := ext.Extract("repo", "empty.go", []byte{})
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 0 {
		t.Errorf("expected 0 deps, got %d", len(deps))
	}
}

func TestExtract_NoGRPCCode(t *testing.T) {
	ext := grpcclient.New()
	src := []byte(`package main
func main() { println("hello") }`)
	deps, err := ext.Extract("repo", "main.go", src)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 0 {
		t.Errorf("expected 0 deps for non-gRPC file, got %d", len(deps))
	}
}

func TestExtract_Idempotent(t *testing.T) {
	ext := grpcclient.New()
	src := []byte(`
		client := pb.NewUserServiceClient(conn)
		resp, err := client.GetUser(ctx, &pb.GetUserRequest{})
	`)
	deps1, _ := ext.Extract("repo", "f.go", src)
	deps2, _ := ext.Extract("repo", "f.go", src)
	if len(deps1) != len(deps2) {
		t.Errorf("idempotency: first=%d second=%d", len(deps1), len(deps2))
	}
	for i := range deps1 {
		if deps1[i].APIIdentifier != deps2[i].APIIdentifier {
			t.Errorf("idempotency mismatch at dep[%d]", i)
		}
	}
}

func TestExtract_GRPCCallKind(t *testing.T) {
	ext := grpcclient.New()
	src := []byte(`
		client := pb.NewOrderServiceClient(conn)
		client.CreateOrder(ctx, req)
	`)
	deps, err := ext.Extract("repo", "f.go", src)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range deps {
		if d.CallKind != "grpc-client" {
			t.Errorf("want grpc-client, got %s", d.CallKind)
		}
	}
}

func TestSupportedCallKinds(t *testing.T) {
	kinds := grpcclient.New().SupportedCallKinds()
	if len(kinds) != 1 || kinds[0] != "grpc-client" {
		t.Errorf("want [grpc-client], got %v", kinds)
	}
}
