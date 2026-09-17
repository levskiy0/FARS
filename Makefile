VERSION ?= $(shell git describe --tags --dirty --always 2>/dev/null || echo dev)
BINARY ?= fars
CONFIG ?= ./config.yaml
LD_FLAGS := -X fars/internal/version.Version=$(VERSION)
GO := GOCACHE=$(CURDIR)/.gocache go
GO_TEST := $(GO) test

.PHONY: all build run test test-short test-race lint fmt fmt-check clean

all: build

build:
	go build -ldflags "$(LD_FLAGS)" -o $(BINARY) ./cmd/fars-server

run: build
	./$(BINARY) serve --config $(CONFIG)

# Everything, the way CI runs it: race detector on, integration-tagged tests
# included. Without -tags=integration the end-to-end resize tests are skipped
# silently, which is how they went unrun for months.
test:
	$(GO_TEST) -race -tags=integration ./...

test-short:
	$(GO_TEST) ./...

test-race: test

# staticcheck is fetched on demand rather than vendored; it needs network.
lint:
	$(GO) vet ./...
	$(GO) run honnef.co/go/tools/cmd/staticcheck@latest ./...

fmt:
	gofmt -w ./cmd ./internal ./pkg ./tests

fmt-check:
	@test -z "$$(gofmt -l ./cmd ./internal ./pkg ./tests)" || { gofmt -l ./cmd ./internal ./pkg ./tests; exit 1; }

clean:
	rm -f $(BINARY)
