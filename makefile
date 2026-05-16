# ==============================================================================
# 变量定义
# ==============================================================================
# 路径配置
FRONTEND_DIR := ../proxyui
PROXY_SOCKET := proxysocket

# Go 编译参数
LDFLAGS := -w -s

# ==============================================================================
# 伪目标声明 (防止和同名文件冲突)
# ==============================================================================
.PHONY: all build-ui copy-dist build-win build-linux clean

# ...前面的变量定义保持不变...

all: build-win build-linux
	@echo ""
	@echo "====== All platforms built successfully! ======"

build-ui:
	@echo "[1/3] Building Vue Frontend..."
	cd $(FRONTEND_DIR) && npm run build

copy-dist: build-ui
	@echo "[2/3] Copying dist to Go directory..."
	rm -rf $(PROXY_SOCKET)/dist
	cp -r $(FRONTEND_DIR)/dist $(PROXY_SOCKET)/

build-win: copy-dist
	@echo "[3/3] Compiling Windows version: proxy_man_win.exe ..."
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags="$(LDFLAGS)" -o win.exe main.go

build-linux: copy-dist
	@echo "[3/3] Compiling Linux version: proxy_man_linux ..."
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="$(LDFLAGS)" -o linux main.go

clean:
	@echo "清理历史构建产物..."
	rm -rf $(PROXY_SOCKET)/dist
	rm -f win.exe linux