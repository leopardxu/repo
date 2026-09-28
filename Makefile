.PHONY: build test clean lint dist

BINARY_NAME=repo
BINARY_WINDOWS=$(BINARY_NAME).exe
BINARY_LINUX=repo-linux
BINARY_MACOS=repo-mac

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
	rm -rf dist/
	rm -f coverage.txt

run:
	go run ./cmd/repo/main.go

# 发布打包:全平台交叉编译,产出 dist/ 下的 tar.gz(zip)、裸二进制与 checksums.txt
# 产物名不带版本号:配合 releases/latest/download/<name> 固定链接,
# selfupdate 的 REPO_SELFUPDATE_URL 无需随版本改 URL;版本由二进制注入信息提供
# 裸二进制与归档一并上传 release,前者供直接下载/自更新,后者含目录结构
# VERSION 可覆盖,CI 中传 tag 名,如 make dist VERSION=v1.2.3
PLATFORMS = linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64

dist: clean
	@mkdir -p dist
	@rm -f dist/checksums.txt
	@for platform in $(PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; \
		name=$(BINARY_NAME)-$${os}-$${arch}; \
		if [ "$${os}" = "windows" ]; then \
			GOOS=$${os} GOARCH=$${arch} go build $(LDFLAGS) -o dist/$${name}.exe ./cmd/repo || exit 1; \
			(cd dist && zip -q $${name}.zip $${name}.exe) || exit 1; \
			(cd dist && sha256sum $${name}.exe $${name}.zip) >> dist/checksums.txt || exit 1; \
		else \
			GOOS=$${os} GOARCH=$${arch} go build $(LDFLAGS) -o dist/$${name} ./cmd/repo || exit 1; \
			tar -czf dist/$${name}.tar.gz -C dist $${name} || exit 1; \
			(cd dist && sha256sum $${name} $${name}.tar.gz) >> dist/checksums.txt || exit 1; \
		fi; \
	done
	@ls -lh dist/
