SHELL = /bin/bash
GOPATH := $(shell go env GOPATH | tr '\\' '/')
GOEXE := $(shell go env GOEXE)
GORELEASER := $(GOPATH)/bin/goreleaser$(GOEXE)
GOOS ?= linux
GOARCH ?= amd64
VERSION := $(shell git describe --tags --always --dirty)
COMMIT := $(shell git rev-parse HEAD)
DATE := $(shell git log -1 --format=%cI)

all: build

$(GORELEASER):
	go install github.com/goreleaser/goreleaser@v1.6.3

# NB --snapshot is required because this repository has no git tags.
build: $(GORELEASER)
	$(GORELEASER) build --snapshot --skip-validate --rm-dist

# build without goreleaser. it creates the same dist directory layout.
build-local:
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -trimpath \
		-ldflags "-s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)" \
		-o dist/gitlab-source-link-proxy_$(GOOS)_$(GOARCH)/gitlab-source-link-proxy .

release-snapshot: $(GORELEASER)
	$(GORELEASER) release --snapshot --skip-publish --rm-dist

release: $(GORELEASER)
	$(GORELEASER) release --rm-dist

clean:
	rm -rf dist

.PHONY: all build build-local release-snapshot release clean
