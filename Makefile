BINARY  := force
TAGS    := sqlite_fts5
GOFLAGS := -tags $(TAGS)

.PHONY: build test cover clean help

build:
	go build $(GOFLAGS) -o $(BINARY) ./cmd/force/

test:
	go test $(GOFLAGS) -timeout 300s ./...

cover:
	go test $(GOFLAGS) -timeout 300s -coverprofile=cover.out ./...
	go tool cover -func=cover.out | tail -1

clean:
	rm -f $(BINARY) cover.out

help:
	@echo "make build   — compile the force binary (with FTS5)"
	@echo "make test    — run all tests (with FTS5)"
	@echo "make cover   — run tests and print coverage summary"
	@echo "make clean   — remove build artifacts"
