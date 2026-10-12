BINARY   := um-status-gateway
IMAGE    ?= ghcr.io/hashed-albaham/mikrotik-um-gateway
TAG      ?= latest
PLATFORMS ?= linux/amd64,linux/arm64,linux/arm/v7
GOFLAGS  := -trimpath -buildvcs=false
LDFLAGS  := -s -w

.PHONY: help build test vet fmt tidy run image image-multiarch size rss clean

help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "%-16s %s\n", $$1, $$2}'

build: ## Build a static local binary into bin/
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags="$(LDFLAGS)" -o bin/$(BINARY) .

test: ## Run the test suite with the race detector
	go test -race -count=1 ./...

vet: ## Run the Go static checks
	go vet ./...

fmt: ## Format the sources
	gofmt -l -w .

tidy: ## Verify that the module graph is clean
	go mod tidy

run: build ## Run locally against UM_HOST
	UM_HOST=$(UM_HOST) ./bin/$(BINARY)

image: ## Build a single-architecture image
	docker build -t $(IMAGE):$(TAG) .

image-multiarch: ## Build and push linux/amd64, linux/arm64 and linux/arm/v7
	docker buildx build --platform $(PLATFORMS) -t $(IMAGE):$(TAG) --push .

size: build ## Report the stripped binary size
	@ls -l bin/$(BINARY) | awk '{ printf "binary: %.2f MiB (%s bytes)\n", $$5/1048576, $$5 }'

rss: build ## Measure gateway RAM usage against the bundled mock portal
	./scripts/measure-memory.sh

clean: ## Remove build output
	rm -rf bin
