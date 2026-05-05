.PHONY: build test integration smoke lint clean

GO ?= go
GOFLAGS ?= -trimpath
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS ?= -s -w -X main.version=$(VERSION)

export CGO_ENABLED := 0

build:
	$(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o bin/zpinit ./cmd/zpinit
	$(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o bin/zpctl  ./cmd/zpctl

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
	rm -rf bin/
