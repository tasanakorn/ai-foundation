# Monorepo entry points. Each part also works on its own from its folder.

.PHONY: dev test build build-gateway build-web check

dev:            ## gateway (auto rebuild) + web (HMR) + optional llama-server
	python scripts/dev.py

test:           ## all tests and type checks
	$(MAKE) -C gateway vet test
	cd web && pnpm typecheck

build: build-web build-gateway

build-gateway:
	$(MAKE) -C gateway build

build-web:
	cd web && pnpm install --frozen-lockfile && pnpm build
