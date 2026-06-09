# Developer convenience targets. CI does not depend on this Makefile —
# the workflows under .github/workflows/ run their own commands. Targets
# here are documented via the `help` target (the default).

SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

# Auto-load .env if present so `make dev-backend` picks up GitHub App
# credentials etc. without `set -a; source .env; set +a` plumbing.
# .env is gitignored; copy .env.example to get started.
ifneq (,$(wildcard .env))
include .env
export
endif

DATABASE_URL ?= $(or $(FISHHAWKD_DATABASE_URL),postgres://fishhawk:fishhawk@localhost:5432/fishhawk?sslmode=disable)
COVERAGE_THRESHOLD ?= 80

GO_MODULES := $(shell go work edit -json | jq -r '.Use[].DiskPath')

.PHONY: help
help: ## Show this help.
	@awk 'BEGIN {FS = ":.*##"; printf "Usage: make <target>\n\nTargets:\n"} \
		/^[a-zA-Z0-9_-]+:.*##/ {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

# ----- local stack ---------------------------------------------------------

.PHONY: up
up: ## Bring up Postgres + MinIO via docker compose.
	docker compose up -d

.PHONY: down
down: ## Stop the local stack (preserves volumes).
	docker compose down

.PHONY: nuke
nuke: ## Stop the local stack AND drop volumes (destroys data).
	docker compose down -v

.PHONY: k8s-up
k8s-up: ## Build the image + helm-install the chart on Docker-Desktop k8s, gate on /healthz.
	scripts/dev k8s

.PHONY: k8s-down
k8s-down: ## Tear down the local k8s deployment (port-forward + helm uninstall).
	scripts/dev k8s-down

.PHONY: minio-init
minio-init: ## Create the MinIO trace bucket on the local stack (idempotent).
	@docker compose exec -T minio mc alias set local http://localhost:9000 fishhawk fishhawk-dev-secret >/dev/null
	@if docker compose exec -T minio mc ls local/fishhawk-traces >/dev/null 2>&1; then \
		echo "bucket fishhawk-traces already exists; nothing to do"; \
	else \
		docker compose exec -T minio mc mb local/fishhawk-traces; \
	fi

.PHONY: migrate
migrate: ## Apply backend Postgres migrations.
	FISHHAWKD_DATABASE_URL='$(DATABASE_URL)' go run ./backend/cmd/fishhawkd migrate up

.PHONY: migrate-down
migrate-down: ## Roll back the most recent migration.
	FISHHAWKD_DATABASE_URL='$(DATABASE_URL)' go run ./backend/cmd/fishhawkd migrate down

# ----- run -----------------------------------------------------------------

.PHONY: dev-backend
dev-backend: ## Run fishhawkd against the local stack on :8080.
	FISHHAWKD_DATABASE_URL='$(DATABASE_URL)' go run ./backend/cmd/fishhawkd serve

.PHONY: dev-frontend
dev-frontend: ## Run the Web UI dev server on :5173 (proxies /v0 → :8080).
	cd frontend && pnpm install && pnpm dev

.PHONY: validate
validate: ## Validate .fishhawk/workflows.yaml with the CLI.
	go run ./cli/cmd/fishhawk validate ./.fishhawk/workflows.yaml

# ----- build ---------------------------------------------------------------

.PHONY: build
build: build-go build-frontend ## Build every Go module and the Web UI.

.PHONY: build-go
build-go: ## Build every registered Go module in go.work.
	@for m in $(GO_MODULES); do \
		echo ">>> build $$m"; \
		(cd $$m && go build ./...) || exit 1; \
	done

.PHONY: build-frontend
build-frontend: ## Type-check and build the Web UI for production.
	cd frontend && pnpm install && pnpm build

# ----- test ----------------------------------------------------------------

.PHONY: test
test: test-go test-frontend ## Run every Go module's tests and the Web UI tests.

.PHONY: test-go
test-go: ## Run `go test -race ./...` across every module in go.work.
	@for m in $(GO_MODULES); do \
		echo ">>> test $$m"; \
		(cd $$m && go test -race ./...) || exit 1; \
	done

.PHONY: test-frontend
test-frontend: ## Run vitest in the Web UI.
	cd frontend && pnpm install && pnpm test

.PHONY: coverage
coverage: ## Reproduce the CI coverage gate (>= 80%, excluding sqlc-generated db).
	(cd backend && go test -race -coverprofile=coverage.out -covermode=atomic ./...)
	python3 scripts/check-coverage.py --threshold $(COVERAGE_THRESHOLD) --exclude internal/run/db backend/coverage.out

# ----- lint / format -------------------------------------------------------

.PHONY: lint
lint: lint-go lint-frontend ## Run all linters.

.PHONY: lint-go
lint-go: ## Run golangci-lint v2 across every Go module.
	@for m in $(GO_MODULES); do \
		echo ">>> lint $$m"; \
		(cd $$m && golangci-lint run ./...) || exit 1; \
	done

.PHONY: lint-frontend
lint-frontend: ## Run eslint + tsc --noEmit in the Web UI.
	cd frontend && pnpm install && pnpm lint && pnpm typecheck

.PHONY: fmt
fmt: ## Format Go (gofmt) and frontend (prettier) sources.
	@for m in $(GO_MODULES); do \
		(cd $$m && gofmt -w .) || exit 1; \
	done
	cd frontend && pnpm format

# ----- housekeeping --------------------------------------------------------

.PHONY: tidy
tidy: ## Run `go mod tidy` in every module.
	@for m in $(GO_MODULES); do \
		echo ">>> tidy $$m"; \
		(cd $$m && go mod tidy) || exit 1; \
	done

.PHONY: clean
clean: ## Remove build artifacts and coverage profiles.
	rm -f backend/coverage.out
	rm -rf frontend/dist frontend/node_modules/.vite
	@for m in $(GO_MODULES); do \
		(cd $$m && go clean ./...) || exit 1; \
	done
