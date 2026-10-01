# Vinx 助手构建
# make test           单元测试
# make vet            静态检查
# make build          本机平台 dist/vinx-assistant
# make cross          dist/vinx-assistant-linux-amd64、dist/vinx-assistant-linux-arm64
# make offline-build  只用 local.mk 提供的工具链、不联网（GOPROXY=off）也能编译；验证不依赖全局 Go 环境

# 本机私有的 Go 环境（工具链、模块缓存、代理）写在 local.mk，不入库；没有时用 PATH 里的 go
-include local.mk

GO      ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

export CGO_ENABLED = 0

.PHONY: test vet build cross offline-build clean

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

build:
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o dist/vinx-assistant ./cmd/vinx-assistant

cross:
	GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o dist/vinx-assistant-linux-amd64 ./cmd/vinx-assistant
	GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o dist/vinx-assistant-linux-arm64 ./cmd/vinx-assistant

offline-build:
	env -i HOME=$(HOME) PATH=/usr/bin:/bin GOENV=off GOPROXY=off $(MAKE) --no-print-directory build

clean:
	rm -rf dist
