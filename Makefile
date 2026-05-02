# CRM — Fase 0 Makefile (SIN-62208).
# Targets: up, down, logs, test, lint, migrate-up, migrate-down, seed-stg, smoke-alert.

SHELL := /bin/bash
COMPOSE_DIR := deploy/compose
COMPOSE := docker compose --project-directory $(COMPOSE_DIR) -f $(COMPOSE_DIR)/compose.yml
GO ?= go

.DEFAULT_GOAL := help

.PHONY: help up down logs test lint lint-aicache migrate-up migrate-down seed-stg smoke-alert

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

lint-aicache: ## Run the SIN-62236 aicache analyzer over internal/ai/ as a vet tool
	$(GO) build -o ./bin/aicache ./cmd/aicache
	$(GO) vet -vettool=$(CURDIR)/bin/aicache ./internal/ai/...

migrate-up: ## Apply DB migrations (wired in PR2 with goose)
	@echo "migrate-up: stub — PR2 (SIN Fase 0) wires goose against postgres service"

migrate-down: ## Rollback the most recent DB migration (wired in PR2)
	@echo "migrate-down: stub — PR2 (SIN Fase 0) wires goose against postgres service"

seed-stg: ## Seed staging fixtures (wired in PR2)
	@echo "seed-stg: stub — PR2 (SIN Fase 0) provides staging seed"

smoke-alert: ## Inject a synthetic alert into Slack #alerts (wired in PR10)
	@echo "smoke-alert: stub — PR10 (SIN Fase 0) implements Slack injection"
