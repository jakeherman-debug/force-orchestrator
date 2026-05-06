BINARY  := force
TAGS    := sqlite_fts5
GOFLAGS := -tags $(TAGS)

.PHONY: build build-bash-guard test cover clean help smoke fuzz test-audit hooks-install render-rules render-rules-check protect-db unprotect-db install-snapshots uninstall-snapshots db-status docs-broken-links docs-orphan-check docs-architecture docs-check

build:
	go build $(GOFLAGS) -o $(BINARY) ./cmd/force/

# make build-bash-guard — compile the astromech Bash-tool gatekeeper.
# Output goes to ./bin/force-bash-guard so the astromech wiring code
# can reference a stable on-disk path. Operator action only — D2 T1-3.
build-bash-guard:
	@mkdir -p bin
	go build $(GOFLAGS) -o bin/force-bash-guard ./cmd/force-bash-guard/

test:
	go test $(GOFLAGS) -timeout 600s ./...

cover:
	go test $(GOFLAGS) -timeout 600s -coverprofile=cover.out ./...
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
	for pkg in internal/git internal/store internal/agents internal/claude cmd/force-bash-guard; do \
		for fn in $$(go test $(GOFLAGS) -list 'Fuzz.*' ./$$pkg | grep '^Fuzz'); do \
			echo "==> $$pkg $$fn"; \
			go test $(GOFLAGS) -run='^$$' -fuzz="^$$fn$$" -fuzztime=30s ./$$pkg || exit $$?; \
		done; \
	done

# make test-audit — fails if any AUDIT-NNN skip marker survives in the suite.
# Runs the guard as a real Go test so CI picks it up uniformly.
test-audit:
	go test $(GOFLAGS) -timeout 60s -run '^TestNoAuditSkipMarkersRemain$$' -count=1 ./internal/audittools

# make docs-broken-links — D13 P3 drift detector. Walks every *.md
# in the repo and verifies every relative-path Markdown link
# `[text](path/to/file.md#anchor)` resolves: file exists, anchor
# (if any) matches an H1/H2/H3 heading in the target. External
# http(s):// links are skipped. Runs in `make test` automatically;
# this target is the operator-friendly alias.
docs-broken-links:
	go test $(GOFLAGS) -timeout 60s -run '^TestPatternP_DocsBrokenLinks$$' -count=1 ./internal/audittools/...

# make docs-orphan-check — D13 P3 drift detector. Asserts every
# *.md under docs/{agents,subsystems,patterns}/ is linked from a
# sibling README.md or from docs/README.md. Files that exist on
# disk but are not reachable from an index are "orphans" and the
# test fails.
docs-orphan-check:
	go test $(GOFLAGS) -timeout 60s -run '^TestPatternP_DocsOrphan$$' -count=1 ./internal/audittools/...

# make docs-architecture — D13 P3 consolidated structural-invariant
# test (H2 section floor on authored docs, auto-rendered exemption
# honored, docs/README.md links to every category mini-index).
docs-architecture:
	go test $(GOFLAGS) -timeout 60s -run '^TestPatternP_DocsArchitecture$$' -count=1 ./internal/audittools/...

# make docs-check — full docs gate. Runs all three D13 P3 drift
# detectors plus the four P1 structural guards (README cap, index
# stubs, metadata blocks). Used by the pre-commit hook
# scripts/pre-commit/docs-check.sh and the D13 verifier.
docs-check: docs-broken-links docs-orphan-check docs-architecture
	go test $(GOFLAGS) -timeout 60s -count=1 \
		-run '^(TestReadmeSizeUnder200Lines|TestDocsIndexExists|TestDocsSubdirsHaveIndex|TestMetadataBlockOnAllNewDocs)$$' \
		./internal/audittools/...

# make hooks-install — opt-in installer for the local pre-commit gate.
# Symlinks scripts/pre-commit/forceignore-check.sh into .git/hooks/pre-commit.
# Operator-invoked only — Force chunked development never auto-installs
# this; it would alter the operator's git environment without consent.
hooks-install:
	bash scripts/install-hooks.sh

# make render-rules — regenerate CLAUDE.md / FIX-LOG.md / docs/* from the
# FleetRules table (D3 Phase 1). Idempotent: writes only when content
# changed. Runs against an in-memory DB (bootstrap-then-render) so this
# target works without a live holocron.db.
render-rules: build
	./$(BINARY) render-rules

# make render-rules-check — drift detector. Renders to memory and exits 1
# if any auto-generated file disagrees with disk. Used by the pre-commit
# hook (scripts/pre-commit/claude-md-size-check.sh).
render-rules-check: build
	./$(BINARY) render-rules --check

clean:
	rm -f $(BINARY) cover.out
	rm -rf bin/

# make protect-db — apply a macOS ACL (deny delete,delete_child) to
# holocron.db and its WAL/SHM sidecars. SQLite read/write operations
# remain unaffected; only unlink() is blocked. Idempotent: re-running
# detects the existing ACL and short-circuits per file. Files that
# don't exist yet are skipped with a log line — run again after the
# daemon first creates them.
protect-db:
	@for f in holocron.db holocron.db-wal holocron.db-shm; do \
		if [ -f "$$f" ]; then \
			if ls -le "$$f" 2>/dev/null | grep -q "deny delete"; then \
				echo "Already protected: $$f"; \
			else \
				chmod +a "everyone deny delete,delete_child" "$$f" && echo "Protected: $$f"; \
			fi; \
		else \
			echo "Skipping (not present): $$f"; \
		fi; \
	done
	@echo ""
	@echo "Current ACLs on protected files:"
	@ls -le holocron.db* 2>/dev/null | grep -B 1 "deny delete" || echo "(none found — expected if all files are missing)"

# make unprotect-db — reverse of protect-db. Run before legitimate
# maintenance that requires removing holocron.db (e.g. a destructive
# migration rollback that the operator has consciously chosen).
unprotect-db:
	@for f in holocron.db holocron.db-wal holocron.db-shm; do \
		if [ -f "$$f" ]; then \
			chmod -a "everyone deny delete,delete_child" "$$f" 2>/dev/null && echo "Unprotected: $$f" || echo "$$f: no ACL to remove"; \
		fi; \
	done

# make install-snapshots — install hourly WAL-consistent sqlite3 .backup
# snapshots into ~/.force/backups/ via crontab, plus a daily 04:00
# cleanup that prunes snapshots older than 30 days. Idempotent.
install-snapshots:
	@scripts/setup-snapshots.sh

# make uninstall-snapshots — remove the snapshot crontab entries
# installed by install-snapshots. Existing snapshot files in
# ~/.force/backups/ are left untouched.
uninstall-snapshots:
	@scripts/uninstall-snapshots.sh

# make db-status — show holocron.db file ACLs, snapshot crontab entries,
# and the most recent snapshots. Read-only diagnostic.
db-status:
	@echo "holocron.db files:"
	@ls -le holocron.db* 2>/dev/null || echo "  (none present)"
	@echo ""
	@echo "Crontab snapshot entries:"
	@crontab -l 2>/dev/null | grep -A 1 "force-orchestrator" || echo "  (none — run \`make install-snapshots\`)"
	@echo ""
	@echo "Recent snapshots:"
	@ls -lt $$HOME/.force/backups/ 2>/dev/null | head -5 || echo "  (none — backup dir may not exist yet)"

help:
	@echo "make build              — compile the force binary (with FTS5)"
	@echo "make build-bash-guard   — compile the astromech Bash-tool gatekeeper to ./bin/"
	@echo "make test               — run all tests (with FTS5)"
	@echo "make cover              — run tests and print coverage summary"
	@echo "make smoke              — daemon-boot + minimal task cycle (< 30s)"
	@echo "make fuzz               — run every Fuzz* target for 30s each"
	@echo "make test-audit         — fail if any t.Skip(\"AUDIT-...\") marker remains"
	@echo "make hooks-install      — install the .forceignore pre-commit hook (opt-in)"
	@echo "make render-rules       — regenerate CLAUDE.md / FIX-LOG.md / docs/* from FleetRules"
	@echo "make render-rules-check — drift detector (exit 1 if rendered files disagree with disk)"
	@echo "make protect-db         — macOS ACL: deny delete on holocron.db (+ -wal/-shm)"
	@echo "make unprotect-db       — remove the deny-delete ACL"
	@echo "make install-snapshots  — install hourly snapshot cron + daily cleanup"
	@echo "make uninstall-snapshots — remove the snapshot crontab entries"
	@echo "make db-status          — show ACLs, crontab, and recent snapshots"
	@echo "make clean              — remove build artifacts"
