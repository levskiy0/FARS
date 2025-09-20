VERSION ?= $(shell git describe --tags --dirty --always 2>/dev/null || echo dev)
BINARY ?= fars
CONFIG ?= ./config.yaml
LD_FLAGS := -X fars/internal/version.Version=$(VERSION)

.PHONY: all build run test fmt clean

all: build

build:
	go build -ldflags "$(LD_FLAGS)" -o $(BINARY) ./cmd/fars-server

run: build
	./$(BINARY) serve --config $(CONFIG)

test:
	GOCACHE=$(CURDIR)/.gocache go test ./...

fmt:
	gofmt -w ./cmd ./internal ./pkg ./tests

clean:
	rm -f $(BINARY)
