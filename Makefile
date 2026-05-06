# CRM — Fase 0 Makefile (SIN-62208).
# Targets: up, down, logs, test, test-integration, lint, migrate-up,
#          migrate-down, seed-stg, smoke-alert.

SHELL := /bin/bash
COMPOSE_DIR := deploy/compose
COMPOSE := docker compose --project-directory $(COMPOSE_DIR) -f $(COMPOSE_DIR)/compose.yml
GO ?= go

# Integration test coverage threshold (SIN-62277 acceptance criterion).
# The webhook stack + its adapters must clear this bar measured across
# unit + integration runs combined.
ITEST_COVER_PKGS := github.com/pericles-luz/crm/internal/webhook,github.com/pericles-luz/crm/internal/adapter/store/postgres,github.com/pericles-luz/crm/internal/adapter/channel/meta
ITEST_COVER_THRESHOLD ?= 85.0

.DEFAULT_GOAL := help

.PHONY: help up down logs test test-integration test-integration-cover \
        lint migrate-up migrate-down seed-stg smoke-alert

help: ## Show available targets
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z0-9_-]+:.*## / {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

up: ## Bring up the local stack (Postgres, Redis, NATS, MinIO, Caddy, app)
	@if [ ! -f $(COMPOSE_DIR)/.env ]; then \
		echo "missing $(COMPOSE_DIR)/.env — copy $(COMPOSE_DIR)/.env.example and fill secrets"; \
		exit 1; \
	fi
	$(COMPOSE) up -d --wait

down: ## Stop the stack and remove orphaned containers (volumes preserved)
	$(COMPOSE) down --remove-orphans

logs: ## Tail logs from every service
	$(COMPOSE) logs -f --tail=200

test: ## Run Go test suite with coverage
	$(GO) test ./... -race -count=1 -cover

test-integration: ## Run webhook integration suite (Postgres real, build tag `integration`)
	@if [ -z "$$TEST_POSTGRES_DSN" ]; then \
		echo "test-integration: TEST_POSTGRES_DSN not set; falling back to testcontainers (requires Docker)."; \
	fi
	$(GO) test -tags integration -count=1 -timeout 300s \
		./internal/webhook/integration/...

test-integration-cover: ## Run unit + integration with combined coverage and enforce >=$(ITEST_COVER_THRESHOLD)%
	@if [ -z "$$TEST_POSTGRES_DSN" ]; then \
		echo "test-integration-cover: TEST_POSTGRES_DSN not set; falling back to testcontainers (requires Docker)."; \
	fi
	$(GO) test -count=1 -coverpkg=$(ITEST_COVER_PKGS) \
		-coverprofile=coverage.unit.out \
		./internal/webhook/... ./internal/adapter/...
	$(GO) test -tags integration -count=1 -timeout 300s \
		-coverpkg=$(ITEST_COVER_PKGS) \
		-coverprofile=coverage.itest.out \
		./internal/webhook/integration/...
	@{ \
		head -1 coverage.unit.out; \
		tail -n +2 coverage.unit.out; \
		tail -n +2 coverage.itest.out; \
	} > coverage.combined.out
	@pct=$$($(GO) tool cover -func=coverage.combined.out | awk '/^total:/ {sub("%","",$$3); print $$3}'); \
		echo "combined coverage (webhook+adapters): $${pct}%"; \
		awk -v p="$$pct" -v t="$(ITEST_COVER_THRESHOLD)" \
			'BEGIN { exit !(p+0 >= t+0) }' || { \
			echo "coverage $${pct}% below $(ITEST_COVER_THRESHOLD)% threshold (SIN-62277 quality bar)"; \
			exit 1; \
		}

lint: ## Run go vet (staticcheck wired in PR8)
	$(GO) vet ./...

migrate-up: ## Apply DB migrations (wired in PR2 with goose)
	@echo "migrate-up: stub — PR2 (SIN Fase 0) wires goose against postgres service"

migrate-down: ## Rollback the most recent DB migration (wired in PR2)
	@echo "migrate-down: stub — PR2 (SIN Fase 0) wires goose against postgres service"

seed-stg: ## Seed staging fixtures (wired in PR2)
	@echo "seed-stg: stub — PR2 (SIN Fase 0) provides staging seed"

smoke-alert: ## Inject a synthetic alert into Slack #alerts (wired in PR10)
	@echo "smoke-alert: stub — PR10 (SIN Fase 0) implements Slack injection"
