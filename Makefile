# Thin wrappers over the commands CI runs, so the two cannot drift apart.
# Everything here works with a plain Go toolchain; only `integration` needs Docker.

GOLANGCI_LINT_VERSION ?= v2.14.0
VERSION               ?= dev
COMPOSE               := docker compose -f tests/integration/docker-compose.yaml

.PHONY: all build test lint vet fmt integration integration-up integration-down image tools clean

all: build vet lint test

build:
	go build ./...

# The race detector is not optional here: the pool's whole job is to serialise
# access to sessions that are not safe for concurrent use.
test:
	go test -race -coverprofile=coverage.out ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

lint: tools
	$(shell go env GOPATH)/bin/golangci-lint run

tools:
	@command -v $(shell go env GOPATH)/bin/golangci-lint >/dev/null 2>&1 || \
		go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

integration: integration-up
	go test -tags=integration -timeout 10m ./tests/integration/...

integration-up:
	$(COMPOSE) up -d --build --wait

integration-down:
	$(COMPOSE) down -v

image:
	docker build --build-arg VERSION=$(VERSION) -t dapr-ftp-binding:$(VERSION) .

clean:
	rm -f coverage.out
	rm -rf bin dist
