# tasmonaut - common developer tasks.
APP     := tasmonaut
PKG     := ./cmd/tasmonaut
BIN     := bin/$(APP)
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: help
help: ## Show this help.
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  %-10s %s\n", $$1, $$2}'

.PHONY: build
build: ## Build a local binary into bin/.
	CGO_ENABLED=0 go build -mod=vendor -trimpath -ldflags="$(LDFLAGS)" -o $(BIN) $(PKG)

.PHONY: run
run: build ## Run locally against config.yaml (copy config.example.yaml first). No auth: keep it on localhost.
	TASMONAUT_LISTEN=127.0.0.1:8080 $(BIN) -config config.yaml

.PHONY: test
test: ## Run tests.
	go test -mod=vendor ./... -count=1

.PHONY: vet
vet: ## Run go vet and gofmt check.
	go vet -mod=vendor ./...
	@unformatted=$$(gofmt -l $$(git ls-files '*.go')); if [ -n "$$unformatted" ]; then echo "gofmt needed on:"; echo "$$unformatted"; exit 1; fi

.PHONY: tidy
tidy: ## Sync go.mod, go.sum and vendor/.
	go mod tidy
	go mod vendor

.PHONY: clean
clean: ## Remove build artifacts.
	rm -rf bin/
