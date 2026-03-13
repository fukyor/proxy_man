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

# 默认目标：只需输入 `make` 就会按顺序执行
all: build-win build-linux
	@echo ""
	@echo "====== 所有平台编译完成！ ======"

# [1/3] 构建 Vue 前端
build-ui:
	@echo "[1/3] 正在构建 Vue 前端..."
	cd $(FRONTEND_DIR) && npm run build

# [2/3] 复制前端产物至 Go 目录 (依赖前端构建)
copy-dist: build-ui
	@echo "[2/3] 正在复制前端产物至 Go 目录..."
	rm -rf $(PROXY_SOCKET)/dist
	cp -r $(FRONTEND_DIR)/dist $(PROXY_SOCKET)/

# [3/3] 编译 Windows 版本 (依赖复制动作)
build-win: copy-dist
	@echo "[3/3] 开始编译 Windows 版本: proxy_man_win.exe ..."
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags="$(LDFLAGS)" -o proxy_man_win.exe main.go

# [3/3] 编译 Linux 版本 (依赖复制动作)
build-linux: copy-dist
	@echo "[3/3] 开始编译 Linux 版本: proxy_man_linux ..."
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="$(LDFLAGS)" -o proxy_man_linux main.go

# 清理命令 (可选)
clean:
	@echo "清理历史构建产物..."
	rm -rf $(PROXY_SOCKET)/dist
	rm -f proxy_man_win.exe proxy_man_linux