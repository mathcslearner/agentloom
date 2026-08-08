# Keep in sync with the golangci-lint-action version in .github/workflows/ci.yml.
GOLANGCI_LINT_VERSION := v2.12.2
BIN_DIR := $(CURDIR)/bin
GOLANGCI_LINT := $(BIN_DIR)/golangci-lint

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

.PHONY: fmt
fmt: tools ## Format code (gofumpt + gci) and tidy go.mod
	$(GOLANGCI_LINT) fmt ./...
	go mod tidy

.PHONY: lint
lint: tools ## Run golangci-lint
	$(GOLANGCI_LINT) run ./...

.PHONY: test
test: ## Run unit tests with the race detector
	go test -race ./...

.PHONY: test-integration
test-integration: ## Run integration tests (requires the Docker Compose stack; see ticket 2.2)
	@if [ -n "$$(find test -name '*.go' -print -quit)" ]; then \
		go test -race -tags integration ./test/...; \
	else \
		echo "no integration tests yet (suite arrives with ticket 2.2)"; \
	fi

.PHONY: tools
tools: ## Install pinned golangci-lint into ./bin if missing or outdated
	@$(GOLANGCI_LINT) version 2>/dev/null | grep -qF "$(patsubst v%,%,$(GOLANGCI_LINT_VERSION))" || \
		curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/HEAD/install.sh | sh -s -- -b $(BIN_DIR) $(GOLANGCI_LINT_VERSION)
