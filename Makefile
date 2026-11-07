# Syncr v2 build tooling. Run `make help` for targets.

GO            ?= go
BUF           ?= buf
# sqlc runs via `go run` at a pinned version (keep in sync with SQLC_VERSION
# in .github/workflows/ci.yml); override SQLC to use an installed binary.
SQLC_VERSION  ?= v1.31.1
SQLC          ?= $(GO) run github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION)
GOLANGCI_LINT ?= golangci-lint

BIN_DIR   ?= bin
BINARIES  := syncr-controller syncr-worker

# Release builds pass VERSION/COMMIT/BUILD_TIME explicitly; local builds
# derive them from git. Only v* tags count as versions.
VERSION    ?= $(shell git describe --tags --match 'v*' --dirty 2>/dev/null || echo dev)
COMMIT     ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

VERSION_PKG := github.com/Zeptiny/Syncr/internal/platform/version
LDFLAGS := -s -w \
	-X $(VERSION_PKG).version=$(VERSION) \
	-X $(VERSION_PKG).commit=$(COMMIT) \
	-X $(VERSION_PKG).buildTime=$(BUILD_TIME)

# Cross-compile with e.g. `make build GOOS=windows GOARCH=amd64`.
GOOS   ?= $(shell $(GO) env GOOS)
GOARCH ?= $(shell $(GO) env GOARCH)
EXE    := $(if $(filter windows,$(GOOS)),.exe,)

# Pinned rclone/restic used by integration tests. `make tools` downloads them
# (checksum-verified) into TOOLS_DIR. To use other binaries instead, set
# SYNCR_TEST_RCLONE and SYNCR_TEST_RESTIC (environment or make arguments);
# they must report the pinned versions, see `syncr-worker tools status`.
TOOLS_DIR ?= $(CURDIR)/.cache/tools

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z_-]+:.*## / {printf "  %-18s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: generate
generate: ## Regenerate protobuf/Connect (Go and TypeScript) and sqlc code (committed under gen/, web/src/gen/ and internal/adapters/postgres/db/)
	npm ci --prefix web
	$(BUF) generate
	$(SQLC) generate
	$(GO) generate ./...

.PHONY: lint
lint: ## Run buf lint and golangci-lint (includes depguard layering)
	$(BUF) lint
	$(GOLANGCI_LINT) run ./...

.PHONY: test
test: ## Run unit tests
	$(GO) test ./...

.PHONY: test-race
test-race: ## Run unit tests with the race detector (needs cgo)
	CGO_ENABLED=1 $(GO) test -race ./...

.PHONY: tools
tools: ## Fetch the pinned rclone and restic into .cache/tools (TOOLS_DIR)
	$(GO) run ./cmd/syncr-worker tools fetch --dir "$(TOOLS_DIR)"

# Integration tests need the pinned tools and, for the MinIO tests, Docker.
# Setting both SYNCR_TEST_RCLONE and SYNCR_TEST_RESTIC skips the fetch;
# otherwise missing ones come from `tools fetch`, whose last output column is
# the binary's path.
.PHONY: test-integration
test-integration: ## Run integration tests against the pinned tools (fetched like `make tools`)
	@set -e; \
	rclone='$(SYNCR_TEST_RCLONE)'; restic='$(SYNCR_TEST_RESTIC)'; \
	if [ -z "$$rclone" ] || [ -z "$$restic" ]; then \
		out=$$($(GO) run ./cmd/syncr-worker tools fetch --dir "$(TOOLS_DIR)"); \
		echo "$$out"; \
		[ -n "$$rclone" ] || rclone=$$(echo "$$out" | sed -n 's/^rclone  *[^ ]*  *[^ ]*  *[^ ]*  *//p'); \
		[ -n "$$restic" ] || restic=$$(echo "$$out" | sed -n 's/^restic  *[^ ]*  *[^ ]*  *[^ ]*  *//p'); \
	fi; \
	echo "SYNCR_TEST_RCLONE=$$rclone"; echo "SYNCR_TEST_RESTIC=$$restic"; \
	SYNCR_TEST_RCLONE="$$rclone" SYNCR_TEST_RESTIC="$$restic" $(GO) test -tags integration -count=1 ./...

.PHONY: build
build: ## Build both binaries into $(BIN_DIR)/ (set GOOS/GOARCH to cross-compile)
	@mkdir -p $(BIN_DIR)
	@for bin in $(BINARIES); do \
		echo "building $(BIN_DIR)/$$bin$(EXE) ($(GOOS)/$(GOARCH), $(VERSION))"; \
		CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) $(GO) build -trimpath \
			-ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$$bin$(EXE) ./cmd/$$bin || exit 1; \
	done

.PHONY: tidy
tidy: ## Tidy go.mod/go.sum
	$(GO) mod tidy

.PHONY: dev-up
dev-up: ## Start the dev stack (Postgres, MinIO, SFTP)
	docker compose -f deploy/docker-compose.dev.yml up -d --wait

.PHONY: dev-down
dev-down: ## Stop the dev stack and delete its volumes
	docker compose -f deploy/docker-compose.dev.yml down -v

.PHONY: clean
clean: ## Remove build output
	rm -rf $(BIN_DIR)
