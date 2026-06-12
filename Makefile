BINARY    := host-local-evict
MODULE    := github.com/warroyo/host-local-evict
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS   := -s -w -X main.version=$(VERSION)
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

.PHONY: build build-all clean release test vet

## build: build for the current platform
build:
	go build -ldflags="$(LDFLAGS)" -o $(BINARY) ./cmd

## build-all: cross-compile for all platforms into dist/
build-all:
	@mkdir -p dist
	$(foreach PLATFORM,$(PLATFORMS), \
		GOOS=$(word 1,$(subst /, ,$(PLATFORM))) \
		GOARCH=$(word 2,$(subst /, ,$(PLATFORM))) \
		CGO_ENABLED=0 \
		go build -ldflags="$(LDFLAGS)" \
			-o dist/$(BINARY)-$(word 1,$(subst /, ,$(PLATFORM)))-$(word 2,$(subst /, ,$(PLATFORM))) \
			./cmd ;)

## vet: run go vet
vet:
	go vet ./...

## test: run tests
test:
	go test ./...

## release: tag and push a release (usage: make release VERSION=v0.2.0)
release:
	@if [ -z "$(VERSION)" ] || [ "$(VERSION)" = "dev" ]; then \
		echo "Usage: make release VERSION=v0.x.y"; exit 1; \
	fi
	@if ! echo "$(VERSION)" | grep -qE '^v[0-9]+\.[0-9]+\.[0-9]+'; then \
		echo "VERSION must be semver (e.g. v0.2.0)"; exit 1; \
	fi
	@if [ -n "$$(git status --porcelain)" ]; then \
		echo "Working tree is dirty — commit or stash changes first"; exit 1; \
	fi
	git tag $(VERSION)
	git push origin $(VERSION)
	@echo "Tagged and pushed $(VERSION). Watch the release at:"
	@echo "  https://github.com/warroyo/host-local-evict/actions"

## clean: remove build artifacts
clean:
	rm -f $(BINARY)
	rm -rf dist/

## help: list available targets
help:
	@grep -E '^## ' Makefile | sed 's/^## //' | column -t -s ':'
