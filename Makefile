.PHONY: build test clean lint

BINARY_NAME=repo
BINARY_WINDOWS=$(BINARY_NAME).exe
BINARY_LINUX=$(BINARY_NAME)
BINARY_MACOS=$(BINARY_NAME)

# 构建时注入版本信息
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
BUILD_DATE ?= $(shell date -u '+%Y-%m-%dT%H:%M:%SZ' 2>/dev/null || echo "unknown")
LDFLAGS = -ldflags "-X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(BUILD_DATE)"

build:
	go build $(LDFLAGS) -o bin/$(BINARY_NAME) ./cmd/repo

build-all: build-windows build-linux build-macos

build-windows:
	GOOS=windows GOARCH=amd64 go build $(LDFLAGS) -o bin/$(BINARY_WINDOWS) ./cmd/repo

build-linux:
	GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o bin/$(BINARY_LINUX) ./cmd/repo

build-macos:
	GOOS=darwin GOARCH=amd64 go build $(LDFLAGS) -o bin/$(BINARY_MACOS) ./cmd/repo

test:
	go test -v -race ./...

test-coverage:
	go test -coverprofile=coverage.txt -covermode=atomic -race ./...

vet:
	go vet ./...

lint:
	golangci-lint run

clean:
	rm -rf bin/
	rm -f coverage.txt

run:
	go run ./cmd/repo/main.go