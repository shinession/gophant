# gophant Makefile. Requires GNU make and a Go toolchain (see go.mod for the minimum version).

BINARY  := gophant
PKG     := ./cmd/gophant
BIN_DIR := bin
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

# The binary has no CGo dependency, so it can be cross-compiled and copied to the DB hosts as is.
export CGO_ENABLED := 0

.PHONY: all build build-linux test vet fmt tidy e2e clean

all: vet test build

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY) $(PKG)

build-linux:
	GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY)-linux-amd64 $(PKG)

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

tidy:
	go mod tidy

# End-to-end test: PostgreSQL 16 (s1) + PostgreSQL 18 (s3) in Docker Compose. Needs a running Docker engine.
e2e:
	go test -tags e2e -count=1 -timeout 30m ./test/e2e/...

clean:
	rm -rf $(BIN_DIR)
