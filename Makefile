# CRM — Fase 0 Makefile (SIN-62208).
# Targets: up, down, logs, test, lint, migrate-up, migrate-down, seed-stg, smoke-alert.

SHELL := /bin/bash
COMPOSE_DIR := deploy/compose
COMPOSE := docker compose --project-directory $(COMPOSE_DIR) -f $(COMPOSE_DIR)/compose.yml
GO ?= go

.DEFAULT_GOAL := help

.PHONY: help up down logs test lint jstest e2e e2e-fixtures migrate-up migrate-down seed-stg smoke-alert

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

lint: ## Run go vet (staticcheck wired in PR8)
	$(GO) vet ./...

jstest: ## Run JS unit tests for the embedded upload helpers (SIN-62258)
	node --test --experimental-test-coverage internal/adapter/web/upload/static/upload.test.js

e2e: ## Run browser E2E tests for the upload form (SIN-62270, requires Chrome/Chromium)
	@command -v google-chrome >/dev/null 2>&1 || command -v chromium >/dev/null 2>&1 || command -v chromium-browser >/dev/null 2>&1 || { \
		echo "make e2e: no chrome/chromium binary on PATH — see docs/e2e.md"; \
		exit 1; \
	}
	$(GO) test -tags=e2e -count=1 -timeout=120s ./internal/e2e/...

e2e-fixtures: ## Regenerate the SIN-62270 upload-form fixture bytes (PNG/EXE/SVG)
	$(GO) run ./internal/adapter/web/upload/static/testdata/gen_fixtures.go

migrate-up: ## Apply DB migrations (wired in PR2 with goose)
	@echo "migrate-up: stub — PR2 (SIN Fase 0) wires goose against postgres service"

migrate-down: ## Rollback the most recent DB migration (wired in PR2)
	@echo "migrate-down: stub — PR2 (SIN Fase 0) wires goose against postgres service"

seed-stg: ## Seed staging fixtures (wired in PR2)
	@echo "seed-stg: stub — PR2 (SIN Fase 0) provides staging seed"

smoke-alert: ## Inject a synthetic alert into Slack #alerts (wired in PR10)
	@echo "smoke-alert: stub — PR10 (SIN Fase 0) implements Slack injection"
