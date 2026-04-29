BINARY  := force
TAGS    := sqlite_fts5
GOFLAGS := -tags $(TAGS)

.PHONY: build test cover clean help smoke fuzz test-audit hooks-install

build:
	go build $(GOFLAGS) -o $(BINARY) ./cmd/force/

test:
	go test $(GOFLAGS) -timeout 300s ./...

cover:
	go test $(GOFLAGS) -timeout 300s -coverprofile=cover.out ./...
	go tool cover -func=cover.out | tail -1

# make smoke — daemon-boot + DB-init + one minimal task cycle. Runs under 30s.
# Covers: InitHolocronDSN (schema creation, all migrations), dashboard /healthz,
# spend-cap guard fires (Fix #1), AssertNotDefaultBranch rejects (Fix #0).
smoke:
	go test $(GOFLAGS) -timeout 30s -run '^(TestSmoke|TestAssertNotDefaultBranch_HardDenylist|TestFix2_Healthz_ServesQuickly|TestAPIStatus_ExposesHourlySpend|TestSpendCap_DefaultsToTwentyFive)$$' -count=1 ./...

# make fuzz — run every Fuzz* target for 30s each with a 0-corpus seed.
# Used post-fix to drive the validator/redaction regexes against
# adversarial inputs; confirms no crash paths remain.
fuzz:
	@set -e; \
	for pkg in internal/git internal/store internal/agents internal/claude; do \
		for fn in $$(go test $(GOFLAGS) -list 'Fuzz.*' ./$$pkg | grep '^Fuzz'); do \
			echo "==> $$pkg $$fn"; \
			go test $(GOFLAGS) -run='^$$' -fuzz="^$$fn$$" -fuzztime=30s ./$$pkg || exit $$?; \
		done; \
	done

# make test-audit — fails if any AUDIT-NNN skip marker survives in the suite.
# Runs the guard as a real Go test so CI picks it up uniformly.
test-audit:
	go test $(GOFLAGS) -timeout 60s -run '^TestNoAuditSkipMarkersRemain$$' -count=1 ./internal/audittools

# make hooks-install — opt-in installer for the local pre-commit gate.
# Symlinks scripts/pre-commit/forceignore-check.sh into .git/hooks/pre-commit.
# Operator-invoked only — Force chunked development never auto-installs
# this; it would alter the operator's git environment without consent.
hooks-install:
	bash scripts/install-hooks.sh

clean:
	rm -f $(BINARY) cover.out

help:
	@echo "make build         — compile the force binary (with FTS5)"
	@echo "make test          — run all tests (with FTS5)"
	@echo "make cover         — run tests and print coverage summary"
	@echo "make smoke         — daemon-boot + minimal task cycle (< 30s)"
	@echo "make fuzz          — run every Fuzz* target for 30s each"
	@echo "make test-audit    — fail if any t.Skip(\"AUDIT-...\") marker remains"
	@echo "make hooks-install — install the .forceignore pre-commit hook (opt-in)"
	@echo "make clean         — remove build artifacts"
