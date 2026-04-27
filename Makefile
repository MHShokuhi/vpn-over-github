# Makefile for vpn-over-github
# Builds both binaries with static linking, no CGO, version injection.

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X main.Version=$(VERSION) -X main.Commit=$(COMMIT) -X main.BuildDate=$(DATE)
GOFLAGS := CGO_ENABLED=0

.PHONY: all build-client build-server build-all build-termux test test-race lint clean fmt tidy help release-all

all: build-all

## build-client: Build the SOCKS5 tunnel client binary
build-client:
	$(GOFLAGS) go build -trimpath -ldflags "$(LDFLAGS)" -o build/gh-tunnel-client ./cmd/client

## build-server: Build the tunnel server binary
build-server:
	$(GOFLAGS) go build -trimpath -ldflags "$(LDFLAGS)" -o build/gh-tunnel-server ./cmd/server

## build-all: Build both client and server binaries
build-all: build-client build-server

## build-termux: Cross-compile linux/arm64 binaries for Termux on Android (ARMv8-A)
build-termux:
	@mkdir -p build
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o build/gh-tunnel-client_termux_arm64 ./cmd/client
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o build/gh-tunnel-server_termux_arm64 ./cmd/server
	@echo "Termux ARM64 binaries built in build/"

## test: Run all unit tests
test:
	$(GOFLAGS) go test ./tests/... -v -count=1

## test-race: Run tests with race detector
test-race:
	$(GOFLAGS) go test ./tests/... -v -race -count=1

## lint: Run golangci-lint
lint:
	golangci-lint run ./...

## fmt: Format all Go source files
fmt:
	gofmt -w .

## tidy: Tidy go.mod and update go.sum
tidy:
	go mod tidy

## clean: Remove built binaries
clean:
	rm -rf build/ dist/

## release-all: Cross-compile all release binaries to dist/
release-all:
	@mkdir -p dist
	CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/gh-tunnel-client_linux_amd64   ./cmd/client
	CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/gh-tunnel-server_linux_amd64   ./cmd/server
	CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/gh-tunnel-client_linux_arm64   ./cmd/client
	CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/gh-tunnel-server_linux_arm64   ./cmd/server
	CGO_ENABLED=0 GOOS=linux   GOARCH=arm GOARM=7 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/gh-tunnel-client_linux_armv7 ./cmd/client
	CGO_ENABLED=0 GOOS=linux   GOARCH=arm GOARM=7 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/gh-tunnel-server_linux_armv7 ./cmd/server
	# Termux on Android (ARMv8-A); the binary is the same as linux/arm64 but
	# named to be discoverable for Termux users.
	cp dist/gh-tunnel-client_linux_arm64 dist/gh-tunnel-client_termux_arm64
	cp dist/gh-tunnel-server_linux_arm64 dist/gh-tunnel-server_termux_arm64
	cp dist/gh-tunnel-client_linux_armv7 dist/gh-tunnel-client_termux_armv7
	cp dist/gh-tunnel-server_linux_armv7 dist/gh-tunnel-server_termux_armv7
	CGO_ENABLED=0 GOOS=darwin  GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/gh-tunnel-client_darwin_amd64  ./cmd/client
	CGO_ENABLED=0 GOOS=darwin  GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/gh-tunnel-server_darwin_amd64  ./cmd/server
	CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/gh-tunnel-client_darwin_arm64  ./cmd/client
	CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/gh-tunnel-server_darwin_arm64  ./cmd/server
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/gh-tunnel-client_windows_amd64.exe ./cmd/client
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/gh-tunnel-server_windows_amd64.exe ./cmd/server
	cd dist && sha256sum * > sha256sums.txt
	@echo "Done. Binaries are in dist/"

help:
	@grep -E '^##' Makefile | sed 's/## /  /'
