package proto_test

import (
	"encoding/json"
	"os"
	"testing"

	"force-orchestrator/internal/apiextract/proto"
	"force-orchestrator/internal/store"
)

const fixtureFile = "../testdata/proto-app/user_service.proto"
const expectedFile = "../testdata/proto-app/expected_proto.json"
const accuracyThreshold = 0.99

type expectedProto struct {
	RPCs   []string `json:"rpcs"`
	Events []string `json:"events"`
}

func TestProtoExtractor_Fixture(t *testing.T) {
	content, err := os.ReadFile(fixtureFile)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	var expected expectedProto
	expBytes, err := os.ReadFile(expectedFile)
	if err != nil {
		t.Fatalf("read expected: %v", err)
	}
	if err := json.Unmarshal(expBytes, &expected); err != nil {
		t.Fatalf("unmarshal expected: %v", err)
	}

	e := &proto.Extractor{}
	if e.Kind() != "grpc_rpc" {
		t.Errorf("Kind() = %q, want grpc_rpc", e.Kind())
	}
	if e.ExtractorName() != "proto-service" {
		t.Errorf("ExtractorName() = %q, want proto-service", e.ExtractorName())
	}

	apis, err := e.Extract("my-svc", "user_service.proto", content)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	rpcs := make(map[string]bool)
	events := make(map[string]bool)
	for _, a := range apis {
		switch a.APIKind {
		case "grpc_rpc":
			rpcs[a.APIIdentifier] = true
		case "proto_event":
			events[a.APIIdentifier] = true
		default:
			t.Errorf("unexpected APIKind %q for %q", a.APIKind, a.APIIdentifier)
		}
		if a.Extractor != "proto-service" {
			t.Errorf("row %q: Extractor = %q, want proto-service", a.APIIdentifier, a.Extractor)
		}
		if a.SourceLine <= 0 {
			t.Errorf("row %q: SourceLine = %d, want > 0", a.APIIdentifier, a.SourceLine)
		}
	}

	t.Logf("Proto: extracted %d RPCs, %d events", len(rpcs), len(events))

	// Check RPC accuracy.
	rpcMatched := 0
	for _, want := range expected.RPCs {
		if rpcs[want] {
			rpcMatched++
		} else {
			t.Logf("MISSING RPC: %q", want)
		}
	}
	rpcAccuracy := float64(rpcMatched) / float64(len(expected.RPCs))
	t.Logf("RPC accuracy: %d/%d = %.1f%%", rpcMatched, len(expected.RPCs), rpcAccuracy*100)
	if rpcAccuracy < accuracyThreshold {
		t.Errorf("RPC accuracy %.2f < threshold %.2f", rpcAccuracy, accuracyThreshold)
	}

	// Check event accuracy.
	eventMatched := 0
	for _, want := range expected.Events {
		if events[want] {
			eventMatched++
		} else {
			t.Logf("MISSING event: %q", want)
		}
	}
	eventAccuracy := float64(eventMatched) / float64(len(expected.Events))
	t.Logf("Event accuracy: %d/%d = %.1f%%", eventMatched, len(expected.Events), eventAccuracy*100)
	if eventAccuracy < accuracyThreshold {
		t.Errorf("event accuracy %.2f < threshold %.2f", eventAccuracy, accuracyThreshold)
	}
}

// TestProtoExtractor_EmptyFile verifies that an empty file returns no rows.
func TestProtoExtractor_EmptyFile(t *testing.T) {
	e := &proto.Extractor{}
	apis, err := e.Extract("repo", "empty.proto", []byte{})
	if err != nil {
		t.Fatalf("Extract(empty): %v", err)
	}
	if len(apis) != 0 {
		t.Errorf("Extract(empty): got %d rows, want 0", len(apis))
	}
}

// TestProtoExtractor_MultiService verifies multiple services in one file.
func TestProtoExtractor_MultiService(t *testing.T) {
	content := []byte(`
syntax = "proto3";
package billing;

service PaymentService {
  rpc Charge(ChargeRequest) returns (ChargeResponse);
  rpc Refund(RefundRequest) returns (RefundResponse);
}

service InvoiceService {
  rpc GetInvoice(GetInvoiceRequest) returns (GetInvoiceResponse);
}

message PaymentCreated {
  string payment_id = 1;
}
`)
	e := &proto.Extractor{}
	apis, err := e.Extract("billing-svc", "billing.proto", content)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	rpcs := make(map[string]bool)
	events := make(map[string]bool)
	for _, a := range apis {
		if a.APIKind == "grpc_rpc" {
			rpcs[a.APIIdentifier] = true
		} else {
			events[a.APIIdentifier] = true
		}
	}

	wantRPCs := []string{"PaymentService/Charge", "PaymentService/Refund", "InvoiceService/GetInvoice"}
	for _, id := range wantRPCs {
		if !rpcs[id] {
			t.Errorf("missing RPC %q", id)
		}
	}
	if !events["billing.PaymentCreated"] {
		t.Errorf("missing event billing.PaymentCreated")
	}
}

// TestProtoExtractor_EventSuffixes verifies all event suffix patterns.
func TestProtoExtractor_EventSuffixes(t *testing.T) {
	content := []byte(`
syntax = "proto3";
package evts;

message OrderCreated   { string id = 1; }
message OrderUpdated   { string id = 1; }
message OrderDeleted   { string id = 1; }
message OrderEvent     { string id = 1; }
message OrderPayload   { string id = 1; }
`)
	e := &proto.Extractor{}
	apis, err := e.Extract("svc", "order.proto", content)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	events := make(map[string]bool)
	for _, a := range apis {
		if a.APIKind == "proto_event" {
			events[a.APIIdentifier] = true
		}
	}
	wantEvents := []string{
		"evts.OrderCreated",
		"evts.OrderUpdated",
		"evts.OrderDeleted",
		"evts.OrderEvent",
	}
	for _, id := range wantEvents {
		if !events[id] {
			t.Errorf("missing event %q", id)
		}
	}
	// OrderPayload should NOT be an event.
	if events["evts.OrderPayload"] {
		t.Errorf("OrderPayload should not be classified as event")
	}
}

// TestProtoExtractor_RoundTrip verifies Extract → UpsertCrossRepoAPI → ListCrossRepoAPIs.
func TestProtoExtractor_RoundTrip(t *testing.T) {
	content, err := os.ReadFile(fixtureFile)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	e := &proto.Extractor{}
	apis, err := e.Extract("proto-rt-repo", "user_service.proto", content)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(apis) == 0 {
		t.Fatal("Extract: no rows")
	}

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	for i, a := range apis {
		if _, err := store.UpsertCrossRepoAPI(db, a); err != nil {
			t.Fatalf("UpsertCrossRepoAPI[%d]: %v", i, err)
		}
	}
	// Idempotency pass.
	for i, a := range apis {
		if _, err := store.UpsertCrossRepoAPI(db, a); err != nil {
			t.Fatalf("UpsertCrossRepoAPI idempotent[%d]: %v", i, err)
		}
	}

	recovered, err := store.ListCrossRepoAPIs(db, "proto-rt-repo")
	if err != nil {
		t.Fatalf("ListCrossRepoAPIs: %v", err)
	}
	if len(recovered) != len(apis) {
		t.Errorf("round-trip: got %d rows, want %d", len(recovered), len(apis))
	}
}
