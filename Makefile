.DEFAULT_GOAL := help

VERSION := $(shell git describe --tags --always --dirty)
COMMIT := $(shell git rev-parse --short HEAD)
DATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
VERSION_PACKAGE := github.com/aryanpappu/patchbot/internal/version
LDFLAGS := -s -w -X $(VERSION_PACKAGE).Version=$(VERSION) -X $(VERSION_PACKAGE).Commit=$(COMMIT) -X $(VERSION_PACKAGE).Date=$(DATE)

.PHONY: help fmt fmt-check vet lint test race build ci clean vendor-vue

help:
	@printf '%s\n' \
		'fmt        Format Go source' \
		'fmt-check  Check Go source formatting' \
		'vet        Run go vet' \
		'lint       Run golangci-lint' \
		'test       Run tests' \
		'race       Run tests with the race detector' \
		'build      Build bin/patchbot with version metadata' \
		'ci         Run formatting, vet, lint, race tests and build' \
		'vendor-vue Download pinned Vue and verify SRI before embedding' \
		'clean      Remove build artifacts'

fmt:
	gofmt -w .

fmt-check:
	@files="$$(gofmt -l .)" || exit 1; if [ -n "$$files" ]; then printf '%s\n' "$$files"; exit 1; fi

vet:
	go vet ./...

lint:
	golangci-lint run

test:
	go test ./...

race:
	go test -race ./...

build:
	mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/patchbot ./cmd/patchbot

ci: fmt-check vet lint race build

vendor-vue:
	go run ./internal/web/cmd/vendor-vue

clean:
	rm -rf bin
