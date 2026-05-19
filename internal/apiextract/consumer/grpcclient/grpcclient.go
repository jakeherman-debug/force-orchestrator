// Package grpcclient extracts gRPC stub call sites from Go and Java source
// files. It emits CrossRepoAPIDependency rows with ProviderAPIID = 0 and
// call_kind = "grpc-client" for P6 to resolve.
package grpcclient

import (
	"regexp"
	"strings"

	"force-orchestrator/internal/store"
)

// Extractor implements apiextract.ConsumerExtractor for Go and Java files
// that contain gRPC client code.
type Extractor struct{}

// New returns an initialised Extractor.
func New() *Extractor { return &Extractor{} }

// SupportedCallKinds returns the call_kind values produced by this extractor.
func (e *Extractor) SupportedCallKinds() []string {
	return []string{"grpc-client"}
}

// Compiled regexes for Go gRPC.
var (
	// pb.NewUserServiceClient(conn) — captures "UserService"
	reGoNewClient = regexp.MustCompile(`pb\.New(\w+)Client\s*\(`)

	// client.GetUser(ctx, ...) — captures "GetUser"
	reGoRPCCall = regexp.MustCompile(`(?:client|stub|conn)\s*\.\s*([A-Z]\w+)\s*\(`)

	// Java: UserServiceGrpc.newBlockingStub(channel) — captures "UserService"
	reJavaNewStub = regexp.MustCompile(`(\w+)Grpc\.new\w+Stub\s*\(`)

	// Java: stub.getUser(request) — captures "getUser"
	reJavaRPCCall = regexp.MustCompile(`stub\s*\.\s*([a-z]\w+)\s*\(`)
)

// goResult holds state collected during Go-file scanning.
type goResult struct {
	serviceName string
	calls       []rpcCall
}

type rpcCall struct {
	method string
	lineNo int
}

// Extract parses content and returns dependency rows.
// It detects the language by file extension (via filePath suffix).
func (e *Extractor) Extract(repoName, filePath string, content []byte) ([]store.CrossRepoAPIDependency, error) {
	if strings.HasSuffix(filePath, ".go") {
		return extractGo(repoName, filePath, content), nil
	}
	// Java / Kotlin
	return extractJava(repoName, filePath, content), nil
}

// extractGo handles Go gRPC client files.
func extractGo(repoName, filePath string, content []byte) []store.CrossRepoAPIDependency {
	var deps []store.CrossRepoAPIDependency
	lines := strings.Split(string(content), "\n")

	// First pass: find service name from pb.New*Client(conn).
	serviceName := ""
	for _, line := range lines {
		if m := reGoNewClient.FindStringSubmatch(line); m != nil {
			serviceName = m[1]
			break
		}
	}

	// Second pass: find RPC method calls.
	for i, line := range lines {
		lineNo := i + 1
		if m := reGoRPCCall.FindStringSubmatch(line); m != nil {
			method := m[1]
			// Skip common non-RPC identifiers.
			if isGoBuiltinMethod(method) {
				continue
			}
			identifier := buildGRPCIdentifier(serviceName, method)
			deps = append(deps, makeDep(repoName, filePath, lineNo, "grpc-client", identifier, 1.0))
		}
	}
	return deps
}

// extractJava handles Java/Kotlin gRPC client files.
func extractJava(repoName, filePath string, content []byte) []store.CrossRepoAPIDependency {
	var deps []store.CrossRepoAPIDependency
	lines := strings.Split(string(content), "\n")

	serviceName := ""
	for _, line := range lines {
		if m := reJavaNewStub.FindStringSubmatch(line); m != nil {
			serviceName = m[1]
			break
		}
	}

	for i, line := range lines {
		lineNo := i + 1
		if m := reJavaRPCCall.FindStringSubmatch(line); m != nil {
			method := m[1]
			if isJavaBuiltinMethod(method) {
				continue
			}
			// Convert camelCase method to PascalCase for canonical identifier.
			pascal := strings.ToUpper(method[:1]) + method[1:]
			identifier := buildGRPCIdentifier(serviceName, pascal)
			deps = append(deps, makeDep(repoName, filePath, lineNo, "grpc-client", identifier, 1.0))
		}
	}
	return deps
}

// buildGRPCIdentifier returns "ServiceName/MethodName".
func buildGRPCIdentifier(service, method string) string {
	if service == "" {
		return method
	}
	return service + "/" + method
}

// isGoBuiltinMethod returns true for common Go method names that are not
// gRPC RPC invocations to avoid false positives.
func isGoBuiltinMethod(name string) bool {
	switch name {
	case "Close", "String", "Error", "Err", "Context",
		"WithTimeout", "WithCancel", "WithDeadline",
		"Background", "TODO", "Done":
		return true
	}
	return false
}

// isJavaBuiltinMethod returns true for common Java method names to avoid
// false positives.
func isJavaBuiltinMethod(name string) bool {
	switch name {
	case "toString", "hashCode", "equals", "getClass",
		"notify", "notifyAll", "wait", "clone":
		return true
	}
	return false
}

func makeDep(repoName, filePath string, lineNo int, callKind, apiIdentifier string, conf float64) store.CrossRepoAPIDependency {
	return store.CrossRepoAPIDependency{
		ConsumerRepo:  repoName,
		ConsumerFile:  filePath,
		ConsumerLine:  lineNo,
		ProviderAPIID: 0,
		CallKind:      callKind,
		APIIdentifier: apiIdentifier,
		MatchConf:     conf,
		DiscoveredAt:  store.NowSQLite(),
	}
}
