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
# vacuum lints the OpenAPI contract (ticket 6.6); run via `go run` like sqlc.
VACUUM_VERSION := v0.30.0
# promtool validates & unit-tests the alert rules (ticket 7.5); run via the
# compose stack's exact Prometheus image. Keep in sync with docker-compose.yml.
PROMETHEUS_IMAGE := prom/prometheus:v3.5.0
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

.PHONY: openapi-lint
openapi-lint: ## Validate & lint the OpenAPI contract (api/openapi.yaml); warnings fail
	go run github.com/daveshanley/vacuum@$(VACUUM_VERSION) lint \
		--no-banner --fail-severity warn \
		-r api/vacuum.ruleset.yaml api/openapi.yaml

.PHONY: obs-lint
obs-lint: ## Validate the Prometheus alert rules and run their promtool unit tests (ticket 7.5)
	docker run --rm --entrypoint promtool \
		-v $(CURDIR)/deploy/observability:/obs:ro \
		$(PROMETHEUS_IMAGE) check rules /obs/prometheus-rules.yml
	docker run --rm --entrypoint promtool \
		-v $(CURDIR)/deploy/observability:/obs:ro \
		$(PROMETHEUS_IMAGE) test rules /obs/prometheus-rules.test.yml

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

.PHONY: test-chaos-long
test-chaos-long: ## Run the sustained chaos suite in long mode (override with AGENTLOOM_CHAOS_DURATION=10m)
	AGENTLOOM_CHAOS_DURATION=$${AGENTLOOM_CHAOS_DURATION:-5m} \
		go test -race -tags integration -run TestSustainedChaos -count=1 -timeout 20m -v ./test/crash

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

.PHONY: up-app
up-app: ## Boot the full stack (stores + migrate + api + 2 workers) and wait until healthy
	$(COMPOSE) --profile app up -d --build --wait

.PHONY: up-obs
up-obs: ## Boot the full stack plus Prometheus/Grafana/Jaeger, with OTel export on (ticket 7.1)
	AGENTLOOM_OBS_OTEL_ENABLED=true $(COMPOSE) --profile app --profile obs up -d --build --wait

.PHONY: down
down: ## Stop the dev stack, app services included (data volumes are kept)
	$(COMPOSE) --profile app --profile obs down

.PHONY: demo-crash
demo-crash: ## SIGKILL a worker mid-run against compose and watch the run recover (docs/demos/crash-recovery.md)
	bash scripts/demo-crash.sh

.PHONY: smoke-metrics
smoke-metrics: ## Boot app+obs, drive a workload, and assert every 7.2 metric is visible in Prometheus
	bash scripts/metrics-smoke.sh

.PHONY: smoke-dashboards
smoke-dashboards: ## Boot app+obs, drive a chaos-grade workload (incl. a worker SIGKILL), assert every dashboard panel is non-empty and test-fire an alert (ticket 7.5)
	bash scripts/dashboard-smoke.sh

.PHONY: smoke-trace
smoke-trace: ## Boot app+obs, run a retrying fan-out, and assert one Jaeger trace spans 2 workers with a retry link (ticket 7.3)
	bash scripts/trace-smoke.sh

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
	$(COMPOSE) --profile app --profile obs down -v --remove-orphans

.PHONY: tools
tools: ## Install pinned golangci-lint into ./bin if missing or outdated
	@$(GOLANGCI_LINT) version 2>/dev/null | grep -qF "$(patsubst v%,%,$(GOLANGCI_LINT_VERSION))" || \
		curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/HEAD/install.sh | sh -s -- -b $(BIN_DIR) $(GOLANGCI_LINT_VERSION)
