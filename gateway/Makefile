VERSION ?= $(shell git describe --tags --always 2>/dev/null || echo dev)

.PHONY: build test vet run

build:
	go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/aif ./cmd/aif

test:
	go test -race ./...

vet:
	go vet ./...

run: build
	./bin/aif serve -config aif.yaml
