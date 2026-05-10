.PHONY: build build-all build-linux-amd64 build-linux-arm64 test integration smoke lint clean

GO ?= go
GOFLAGS ?= -trimpath
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS ?= -s -w -X main.version=$(VERSION)

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

test:
	$(GO) test ./...

integration:
	$(GO) test -tags=integration ./test/integration/...

smoke:
	./test/smoke/run.sh

lint:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt:"; echo "$$out"; exit 1; fi
	$(GO) vet ./...

clean:
	rm -rf bin/ zpinit-linux-* zpctl-linux-* checksums.txt release-body.md
