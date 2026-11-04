# Syncr v2 build tooling. Run `make help` for targets.

GO            ?= go
BUF           ?= buf
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

# Binaries used by integration tests (override to point at pinned tools).
SYNCR_TEST_RCLONE ?= $(shell command -v rclone 2>/dev/null)
SYNCR_TEST_RESTIC ?= $(shell command -v restic 2>/dev/null)

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z_-]+:.*## / {printf "  %-18s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: generate
generate: ## Regenerate protobuf/Connect code (committed under gen/)
	$(BUF) generate
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

.PHONY: test-integration
test-integration: ## Run integration tests against real rclone/restic binaries
	SYNCR_TEST_RCLONE="$(SYNCR_TEST_RCLONE)" SYNCR_TEST_RESTIC="$(SYNCR_TEST_RESTIC)" \
		$(GO) test -tags integration -count=1 ./...

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
