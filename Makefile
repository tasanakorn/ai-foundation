# Monorepo entry points. Each part also works on its own from its folder.

COMPOSE := docker compose -f deploy/local/compose.yaml $(if $(wildcard .env),--env-file .env)

.PHONY: dev test build build-gateway build-web \
        services-up services-down services-reset services-ps services-logs psql

dev:            ## Postgres + NATS (Docker) + gateway (auto rebuild) + web (HMR) + optional llama-server
	python scripts/dev.py

test:           ## all tests and type checks
	$(MAKE) -C gateway vet test
	cd web && pnpm typecheck

build: build-web build-gateway

build-gateway:
	$(MAKE) -C gateway build

build-web:
	cd web && pnpm install --frozen-lockfile && pnpm build

# ---- local backing services (deploy/local/compose.yaml) ----

services-up:    ## start Postgres + NATS, wait until healthy
	$(COMPOSE) up -d --wait

services-down:  ## stop, keep data
	$(COMPOSE) down

services-reset: ## stop and delete all service data
	$(COMPOSE) down -v

services-ps:
	$(COMPOSE) ps

services-logs:
	$(COMPOSE) logs -f

psql:           ## open psql in the Postgres container
	$(COMPOSE) exec postgres sh -c 'psql -U "$$POSTGRES_USER" -d "$$POSTGRES_DB"'
