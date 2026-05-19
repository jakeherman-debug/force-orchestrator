// Package proto provides a ProviderExtractor for .proto files.
// It extracts service/rpc definitions as grpc_rpc rows and event-like
// message types as proto_event rows. Accuracy target: ≥ 99%.
package proto

import (
	"bufio"
	"bytes"
	"fmt"
	"regexp"
	"strings"

	"force-orchestrator/internal/store"
)

const (
	rpcKind       = "grpc_rpc"
	eventKind     = "proto_event"
	extractorName = "proto-service"
)

// Extractor implements apiextract.ProviderExtractor for .proto files.
// Because a .proto file can define both RPCs and events, the interface's
// single Kind() is "grpc_rpc" (primary output); event rows are also emitted
// from Extract() with kind = "proto_event".
type Extractor struct{}

// Kind returns "grpc_rpc".
func (e *Extractor) Kind() string { return rpcKind }

// ExtractorName returns "proto-service".
func (e *Extractor) ExtractorName() string { return extractorName }

// Extract parses a .proto file and returns CrossRepoAPI rows for both
// service RPCs (kind=grpc_rpc) and event messages (kind=proto_event).
func (e *Extractor) Extract(repoName, filePath string, content []byte) ([]store.CrossRepoAPI, error) {
	return parse(repoName, filePath, content)
}

var (
	// package foo.bar;
	rePackage = regexp.MustCompile(`^\s*package\s+([\w.]+)\s*;`)

	// service FooService {
	reService = regexp.MustCompile(`^\s*service\s+(\w+)\s*\{?`)

	// rpc GetUser(GetUserRequest) returns (GetUserResponse);
	// rpc GetUser(stream GetUserRequest) returns (stream GetUserResponse);
	reRPC = regexp.MustCompile(`^\s*rpc\s+(\w+)\s*\(`)

	// message UserCreated { — ends in Event, Created, Updated, Deleted
	reMessage    = regexp.MustCompile(`^\s*message\s+(\w+)\s*\{?`)
	reEventSufx  = regexp.MustCompile(`(Event|Created|Updated|Deleted)$`)
)

func parse(repoName, filePath string, content []byte) ([]store.CrossRepoAPI, error) {
	scanner := bufio.NewScanner(bytes.NewReader(content))

	var out []store.CrossRepoAPI
	lineNum := 0

	var pkg string          // current package name
	var currentService string

	for scanner.Scan() {
		lineNum++
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		// Skip comments.
		if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "/*") {
			continue
		}

		if m := rePackage.FindStringSubmatch(trimmed); m != nil {
			pkg = m[1]
			continue
		}

		if m := reService.FindStringSubmatch(trimmed); m != nil {
			currentService = m[1]
			continue
		}

		// "}" closes the current service block.
		if trimmed == "}" {
			currentService = ""
			continue
		}

		if currentService != "" {
			if m := reRPC.FindStringSubmatch(trimmed); m != nil {
				rpcName := m[1]
				identifier := fmt.Sprintf("%s/%s", currentService, rpcName)
				out = append(out, store.CrossRepoAPI{
					RepoName:      repoName,
					APIKind:       rpcKind,
					APIIdentifier: identifier,
					SourceFile:    filePath,
					SourceLine:    lineNum,
					Extractor:     extractorName,
					LastScannedAt: store.NowSQLite(),
				})
				continue
			}
		}

		// Message types that look like events (outside service blocks too).
		if m := reMessage.FindStringSubmatch(trimmed); m != nil {
			msgName := m[1]
			if reEventSufx.MatchString(msgName) {
				var identifier string
				if pkg != "" {
					identifier = pkg + "." + msgName
				} else {
					identifier = msgName
				}
				out = append(out, store.CrossRepoAPI{
					RepoName:      repoName,
					APIKind:       eventKind,
					APIIdentifier: identifier,
					SourceFile:    filePath,
					SourceLine:    lineNum,
					Extractor:     extractorName,
					LastScannedAt: store.NowSQLite(),
				})
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("proto extractor: scan %s: %w", filePath, err)
	}
	return out, nil
}
