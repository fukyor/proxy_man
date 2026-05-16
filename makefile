# ==============================================================================
# 变量定义
# ==============================================================================
FRONTEND_DIR := ../proxyui
PROXY_SOCKET := proxysocket
DIST_DIR := $(PROXY_SOCKET)/dist
BIN := proxy_man_linux

GO ?= go
NPM ?= npm
LDFLAGS := -w -s

# ==============================================================================
# WSL/Linux 构建目标
# ==============================================================================
.PHONY: all build build-ui copy-dist run test clean

all: build

build: copy-dist
	@echo "[3/3] 编译 Linux 版本: $(BIN) ..."
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -ldflags="$(LDFLAGS)" -o $(BIN) main.go

build-ui:
	@echo "[1/3] 构建 Vue 前端..."
	cd $(FRONTEND_DIR) && $(NPM) run build

copy-dist: build-ui
	@echo "[2/3] 复制前端产物到 Go 嵌入目录..."
	rm -rf $(DIST_DIR)
	cp -r $(FRONTEND_DIR)/dist $(DIST_DIR)

run: copy-dist
	$(GO) run .

test:
	$(GO) test ./...

clean:
	@echo "清理历史构建产物..."
	rm -rf $(PROXY_SOCKET)/dist
	rm -f win.exe linux
