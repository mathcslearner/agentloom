# Auto-load .env (the compose override file, see .env.example) so the Go
# tooling run through make — cmd/migrate, the integration-test harness —
# sees the same overrides Docker Compose does.
ifneq (,$(wildcard .env))
include .env
export
endif

# Keep in sync with the golangci-lint-action version in .github/workflows/ci.yml.
GOLANGCI_LINT_VERSION := v2.12.2
# sqlc is run via `go run` so the pin lives here, not in a global install.
SQLC_VERSION := v1.30.0
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

.PHONY: generate
generate: ## Regenerate derived artifacts (workflow definition JSON Schema, sqlc store code)
	go run ./internal/dag/gen -out docs/schema/workflow-definition.v1.json
	go run github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION) generate

.PHONY: test
test: ## Run unit tests with the race detector
	go test -race ./...

.PHONY: test-integration
test-integration: ## Run integration tests (requires the Docker Compose stack: make up)
	go test -race -tags integration ./...

.PHONY: migrate-up
migrate-up: ## Apply all pending schema migrations (AGENTLOOM_POSTGRES_DSN overrides the target)
	go run ./cmd/migrate up

.PHONY: migrate-down
migrate-down: ## Roll back the most recent schema migration (one step)
	go run ./cmd/migrate down

.PHONY: migrate-new
migrate-new: ## Create a new migration pair: make migrate-new name=add_runs_table
	@test -n "$(name)" || { echo "usage: make migrate-new name=<snake_case_name>"; exit 1; }
	go run ./cmd/migrate new $(name)

COMPOSE := docker compose

.PHONY: up
up: ## Boot the dev stack (Postgres + Redis) and wait until healthy
	$(COMPOSE) up -d --wait

.PHONY: down
down: ## Stop the dev stack (data volumes are kept)
	$(COMPOSE) down

.PHONY: psql
psql: ## Open a psql shell inside the running postgres container
	$(COMPOSE) exec postgres sh -c 'exec psql -U "$$POSTGRES_USER" -d "$$POSTGRES_DB"'

.PHONY: redis-cli
redis-cli: ## Open a redis-cli shell inside the running redis container
	$(COMPOSE) exec redis redis-cli

.PHONY: nuke
nuke: ## DESTRUCTIVE: tear down the dev stack AND delete all data volumes (asks first)
	@printf 'This will DESTROY the dev stack and ALL of its data (volumes included).\nType "yes" to continue: '; \
	read -r answer && [ "$$answer" = "yes" ] || { echo "aborted."; exit 1; }
	$(COMPOSE) down -v --remove-orphans

.PHONY: tools
tools: ## Install pinned golangci-lint into ./bin if missing or outdated
	@$(GOLANGCI_LINT) version 2>/dev/null | grep -qF "$(patsubst v%,%,$(GOLANGCI_LINT_VERSION))" || \
		curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/HEAD/install.sh | sh -s -- -b $(BIN_DIR) $(GOLANGCI_LINT_VERSION)
