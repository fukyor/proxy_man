# 单文件可执行程序部署方案 - 审查后修订计划

## 审查结果

### 发现的问题

#### 问题 1：embed 指令路径不匹配（严重程度：高）

- **问题描述**：计划中建议 `//go:embed public/dist/*`，但这个路径是相对于 `proxysocket` 包的，而不是项目根目录
- **影响**：如果 `public/dist/` 放在 `proxy_man` 根目录下，embed 指令将无法找到文件
- **解决方案**：将 `dist` 文件夹直接放在 `proxysocket` 包内（`proxysocket/dist/`），并使用 `//go:embed dist/*`

#### 问题 2：计划代码存在占位符（严重程度：中）

- **问题描述**：计划第 84 行存在 `[拦截回退逻辑代码...]` 占位符，缺少完整的 Vue Router History 模式支持实现
- **影响**：无法直接按计划实施，需要补充完整的 SPA fallback 逻辑
- **解决方案**：提供完整的静态文件服务实现代码

#### 问题 3：路由优先级说明不完整（严重程度：低）

- **问题描述**：计划提到了"最长前缀匹配优先"，但未明确说明 Go < 1.21 版本的行为差异
- **影响**：在旧版本 Go 中，`/` 路径可能不会匹配所有路径
- **解决方案**：确保项目使用 Go 1.21+，或使用兼容的处理方式

### 优化建议

1. **自动化脚本建议**：使用 PowerShell 脚本而非批处理，提供更好的跨平台支持
2. **开发模式优化**：添加环境变量控制，在开发模式下跳过 embed 文件服务
3. **错误处理增强**：添加 embed 文件加载失败的降级处理

---

## 修订后的实施计划

### 第一步：修改 `proxysocket/proxy.go`

在 `proxysocket` 包内添加 `embed` 支持，并重写 `StartControlServer` 函数：

```go
package proxysocket

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"proxy_man/mproxy"
	"proxy_man/myminio"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/rs/cors"
)

//go:embed dist
var embeddedFS embed.FS
```

**关键修改点**：

- 将 `dist` 文件夹放在 `proxysocket/` 目录下（与 `proxy.go` 同级）
- `//go:embed dist` 指令会嵌入整个 dist 目录

### 第二步：重写 `StartControlServer` 函数

完整实现静态文件服务和 Vue Router History 模式支持：

```go
func (ws *WebsocketServer) StartControlServer(cm *mproxy.ConfigManager, router *mproxy.Router) bool {
	hub = &WebSocketHub{proxy: ws.Proxy}
	mux := http.NewServeMux()

	// 1. 高优先级路由：API 和 WebSocket
	mux.HandleFunc("/start", ws.loginHandler(ws.handleWebSocket))
	mux.HandleFunc("/api/storage/download", myminio.HandleDownload)
	mux.HandleFunc("/api/config", ws.handleConfig(cm, router))

	// 2. 静态文件服务（兜底处理所有其他请求）
	mux.HandleFunc("/", ws.handleStaticFiles)

	corsMiddleware := cors.New(cors.Options{
		AllowedOrigins:   []string{"*"},
		AllowedMethods:   []string{"GET", "POST", "OPTIONS"},
		AllowedHeaders:   []string{"*"},
		AllowCredentials: true,
	})

	// 启动推送服务
	hub.StartTrafficPusher()
	hub.StartConnectionPusher()
	hub.StartLogPusher()
	hub.StartMitmDetailPusher()

	go func() {
		if err := http.ListenAndServe(ws.Addr, corsMiddleware.Handler(mux)); err != nil {
			log.Printf("Socket Server failed to start: %v", err)
		}
	}()

	return true
}

// handleStaticFiles 处理静态文件请求，支持 Vue Router History 模式
func (ws *WebsocketServer) handleStaticFiles(w http.ResponseWriter, r *http.Request) {
	// 提取嵌入的 dist 子目录
	distFS, err := fs.Sub(embeddedFS, "dist")
	if err != nil {
		http.Error(w, "Frontend assets not available", http.StatusInternalServerError)
		log.Printf("Failed to access embedded FS: %v", err)
		return
	}

	// 尝试直接请求文件
	filePath := strings.TrimPrefix(r.URL.Path, "/")
	if filePath == "" {
		filePath = "index.html"
	}

	// 检查文件是否存在
	if _, err := distFS.Open(filePath); err == nil {
		// 文件存在，直接提供
		http.FileServer(http.FS(distFS)).ServeHTTP(w, r)
		return
	}

	// 文件不存在，返回 index.html（Vue Router History 模式）
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	indexContent, err := distFS.ReadFile("index.html")
	if err != nil {
		http.Error(w, "index.html not found", http.StatusNotFound)
		return
	}
	w.Write(indexContent)
}
```

### 第三步：简化版自动化打包流程

考虑到不需要使用复杂的 PowerShell 或 Shell 脚本，因为前端的构建和后端的编译本身就是非常确定的单行命令。我们只需要在 `proxy_man` 根目录下，创建一个最基础的 `build.cmd`（Windows 批处理文件）即可：

**`build.cmd`**:
```cmd
@echo off
echo [1/3] 正在构建 Vue 前端...
cd ..\proxyui
call npm run build

echo [2/3] 正在复制前端产物至 Go 目录...
cd ..\proxy_man
rmdir /s /q proxysocket\dist 2>nul
xcopy /e /i /y ..\proxyui\dist proxysocket\dist

echo [3/3] 开始跨平台静态编译...
set CGO_ENABLED=0

echo. & echo 正在编译 Windows 版本: proxy_man_win.exe ...
set GOOS=windows
set GOARCH=amd64
go build -ldflags="-w -s" -o proxy_man_win.exe main.go

echo. & echo 正在编译 Linux 版本: proxy_man_linux ...
set GOOS=linux
set GOARCH=amd64
go build -ldflags="-w -s -extldflags '-static'" -o proxy_man_linux main.go

echo. & echo 编译完成！可执行文件已生成在当前目录下。
pause
```

这个不到 20 行的批处理脚本使用了最基础的 CMD 命令，没有任何复杂的逻辑判断。双击运行它，它就会自动按照顺序帮你完成前端的生产构建，并将结果嵌入到两个独立的无依赖单文件可执行程序中。

### 第四步：更新 .gitignore

在 `.gitignore` 中添加：

```
# 嵌入的前端构建产物
proxysocket/dist/
```

---

## 关键文件

| 文件路径               | 修改类型 | 说明                                                         |
| ---------------------- | -------- | ------------------------------------------------------------ |
| `proxysocket/proxy.go` | 修改     | 添加 embed 支持，重写 StartControlServer，新增 handleStaticFiles |
| `build.ps1`            | 新建     | Windows 构建脚本                                             |
| `build.sh`             | 新建     | Linux/macOS 构建脚本                                         |
| `.gitignore`           | 修改     | 添加 proxysocket/dist/ 忽略规则                              |

---

## 验证方法

### 开发模式验证（前后端分离）

1. 启动后端：`go run main.go`
2. 启动前端：在 `proxyui` 目录执行 `npm run dev`
3. 访问 `http://localhost:5173` 验证功能

### 生产模式验证（单文件部署）

1. 执行 `./build.ps1` 或 `./build.sh`
2. 运行生成的可执行文件：
   - Windows: `.\proxy_man_win.exe`
   - Linux: `./proxy_man_linux`
3. 访问 `http://localhost:8000` 验证：
   - 首页正常加载
   - 直接访问 `/dashboard/connections` 等 Vue Router 路由正常工作
   - 刷新页面不会 404
   - WebSocket 连接正常
   - API 请求正常

### 跨平台验证

1. 在 Windows 上运行 Linux 构建的文件（通过 WSL）
2. 在 Linux 上运行 Windows 构建的文件（通过 Wine）
3. 确认静态链接成功（无外部 libc 依赖）

---

## 影响范围分析

**修改爆炸半径**：

- d=1（直接受影响）：`main.go:main()` 函数调用 `StartControlServer`
- d=2（间接受影响）：无
- 风险等级：**低**（仅修改控制服务器的路由处理，不影响代理核心功能）

**受影响的执行流程**：

1. `Main → SaveLocked`（启动流程）
2. `Main → DefaultConfig`（配置加载）

**不受影响的功能**：

- 代理服务器（8080 端口）
- MITM 功能
- 路由功能
- MinIO 存储
- WebSocket 推送逻辑