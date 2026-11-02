.PHONY: build build-all build-linux-amd64 build-linux-arm64 test test-race integration integration-race coverage lint vuln check clean

GO ?= go
GOFLAGS ?= -trimpath
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS ?= -s -w -X main.version=$(VERSION)

# Pinned tool versions. Both run via `go run pkg@version`, which uses
# the module cache only and never touches go.mod (the only approved
# dependency stays BurntSushi/toml). Bump deliberately; staticcheck
# must support the Go version in go.mod.
STATICCHECK_VERSION ?= v0.8.2
GOVULNCHECK_VERSION ?= v1.9.0

# Where `make coverage` writes its report.
COVDIR ?= cover

export CGO_ENABLED := 0

build:
	$(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o bin/zpinit ./cmd/zpinit
	$(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o bin/zpctl  ./cmd/zpctl

# Release artifacts. Filenames must match the Dockerfile COPY paths and
# the release workflow's `files:` list.
build-all: build-linux-amd64 build-linux-arm64

build-linux-amd64:
	GOOS=linux GOARCH=amd64 $(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o zpinit-linux-amd64 ./cmd/zpinit
	GOOS=linux GOARCH=amd64 $(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o zpctl-linux-amd64  ./cmd/zpctl

build-linux-arm64:
	GOOS=linux GOARCH=arm64 $(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o zpinit-linux-arm64 ./cmd/zpinit
	GOOS=linux GOARCH=arm64 $(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o zpctl-linux-arm64  ./cmd/zpctl

# Fast dev loop: cached, no race detector.
test:
	$(GO) test ./...

# What CI runs. -race needs cgo; -shuffle catches order-dependent
# tests; -count=1 defeats the cache so a flake cannot hide behind a
# cached pass.
test-race:
	CGO_ENABLED=1 $(GO) test -race -shuffle=on -count=1 ./...

integration:
	$(GO) test -tags=integration ./test/integration/...

integration-race:
	CGO_ENABLED=1 $(GO) test -race -shuffle=on -count=1 -tags=integration ./test/integration/...

# Merged unit + integration coverage. Unit test binaries write binary
# coverage data via -test.gocoverdir; the integration TestMain builds
# instrumented zpinit/zpctl binaries when ZPINIT_TEST_COVERDIR is set
# and points their GOCOVERDIR at the same directory, so one covdata
# merge yields the real end-to-end figure.
coverage:
	rm -rf $(COVDIR) && mkdir -p $(COVDIR)/data
	CGO_ENABLED=1 $(GO) test -race -count=1 -cover -coverpkg=./... ./... -args -test.gocoverdir=$(abspath $(COVDIR)/data)
	ZPINIT_TEST_COVERDIR=$(abspath $(COVDIR)/data) $(GO) test -count=1 -tags=integration ./test/integration/...
	$(GO) tool covdata textfmt -i=$(COVDIR)/data -o $(COVDIR)/coverage.out
	$(GO) tool cover -html=$(COVDIR)/coverage.out -o $(COVDIR)/coverage.html
	@echo "per package:"
	@$(GO) tool covdata percent -i=$(COVDIR)/data
	@$(GO) tool cover -func=$(COVDIR)/coverage.out | tail -1

lint:
	@out=$$(gofmt -s -l .); if [ -n "$$out" ]; then echo "gofmt -s:"; echo "$$out"; exit 1; fi
	$(GO) vet ./...
	$(GO) vet -tags=integration ./test/integration/...
	$(GO) run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) ./...
	$(GO) run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) -tags=integration ./test/integration/...

vuln:
	$(GO) run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

# Everything CI checks, in one command.
check: lint vuln test-race integration-race

clean:
	rm -rf bin/ $(COVDIR)/ zpinit-linux-* zpctl-linux-* checksums.txt release-body.md
