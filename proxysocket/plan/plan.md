# 前端 UI 配置代理路由功能 - 实施计划

## 背景

用户希望通过前端 UI 界面配置后端代理的路由规则，实现按域名/IP/关键词将流量分发到不同的出站节点。

## 审查发现

### 现有代码结构分析

#### 后端 (proxy_man)

- **配置管理**：`mproxy/config.go` 已实现 `ConfigManager`，支持配置的读写和持久化
- **路由引擎**：`mproxy/router.go` 实现了基于拨号器的路由系统
- **控制服务器**：`proxysocket/proxy.go` 的 `StartControlServer` 已注册 WebSocket 和 MinIO 下载 API
- **未实现**：`/api/config` HTTP API 端点

#### 前端 (proxyui)

- **Vue 3 + Vite + Pinia** 架构
- **WebSocket Store**：已实现完善的发布-订阅模式
- **现有路由**：`/dashboard/overview`、`/dashboard/connections`、`/dashboard/logs`、`/dashboard/mitm`
- **缺失**：配置管理页面和 HTTP API 调用能力

### 发现的关键问题

#### 问题 1：配置结构不兼容 (严重)

- `ServerConfig.Routes` 使用 `RouteRule` 结构（Type、Value、Action）
- `Router.Rules` 使用 `RoutingRule` 结构（ReqCondition、Target拨号器名称）
- 两者无法直接对接，需要转换层

#### 问题 2：Action 枚举语义不清 (严重)

- 原计划定义：`Proxy`、`Direct`、`Block`
- 实际路由系统使用拨号器名称：`"clash"`、`"Direct"`
- `Proxy` 无法映射到具体拨号器

#### 问题 3：规则构建函数需要同步 (中等)

- 现有：`DomainSuffixRule()`、`DomainKeywordRule()`、`IPRule()`
- 每个函数接受可变参数，合并为一条规则
- UI 需要逐条添加规则，结构不匹配

#### 问题 4：前端缺少 HTTP 请求能力 (中等)

- 现有 `api.js` 仅提供 WebSocket 连接
- 需要添加 HTTP POST 支持

#### 问题 5：路由热重载线程安全问题 (严重) - 新增

- 每次保存配置会调用 `AddRule`，导致规则越积越多
- 需要实现 `ClearRules()` 方法清理旧规则
- 重载时必须持有写锁，防止请求匹配到空路由表

#### 问题 6：默认代理节点选择不明确 (中等) - 新增

- 当前 `getDefaultProxyNode` 逻辑模糊（取第一个启用的节点）
- 需要在 `ProxyNode` 中添加 `IsDefault` 字段明确指定

#### 问题 7：前端配置加载时序问题 (低) - 新增

- 当前计划在页面加载时调用 `fetchConfig()`
- 但路由守卫要求 WebSocket 连接后才能进入 dashboard
- 应在登录成功后立即加载配置，独立于 WebSocket 连接

## 修订后的实施计划

---

### 阶段一：后端基础架构

#### 步骤 1.1：扩展配置结构

**文件**：`mproxy/config.go`

在 `ServerConfig` 中添加代理节点配置：

```go
type ProxyNode struct {
    Name      string `json:"name"`      // 节点名称，如 "clash"
    URL       string `json:"url"`       // 代理地址，如 "http://127.0.0.1:7892"
    Enable    bool   `json:"enable"`    // 是否启用
    IsDefault bool   `json:"isDefault"` // 是否为默认代理节点（新增）
}

type ServerConfig struct {
    // ... 现有字段 ...

    // 代理节点配置
    ProxyNodes []ProxyNode `json:"proxyNodes"`

    // 路由配置
    RouteEnable   bool        `json:"routeEnable"`
    DefaultAction string      `json:"defaultAction"` // "direct" | "proxy" | "block"
    Routes        []RouteRule `json:"routes"`
}
```

#### 步骤 1.2：扩展 Router 结构（线程安全改进）

**文件**：`mproxy/router.go`

添加清理方法，支持热重载：

```go
// ClearRules 清空所有路由规则
func (r *Router) ClearRules() {
    r.mu.Lock()
    defer r.mu.Unlock()
    r.Rules = r.Rules[:0] // 清空切片但保留容量
}

// ClearDialers 清空所有拨号器（保留 Direct）
func (r *Router) ClearDialers() {
    r.mu.Lock()
    defer r.mu.Unlock()
    r.Dialers = make(map[string]OutboundDialer)
}
```

#### 步骤 1.3：实现配置转换层

**文件**：新建 `mproxy/config_loader.go`

将 UI 配置转换为路由规则：

```go
// LoadRoutesFromConfig 从 ServerConfig 加载路由规则到 Router
// 注意：调用此函数前必须持有 router.mu 写锁，确保线程安全
func LoadRoutesFromConfig(proxy *CoreHttpServer, router *Router, cfg *ServerConfig) error {
    // 1. 清理旧规则和拨号器（新增，防止规则越积越多）
    router.ClearRules()
    router.ClearDialers()

    // 保留 DirectDialer
    router.AddDialer("Direct", &DirectDialer{})

    // 2. 注册代理节点拨号器
    for _, node := range cfg.ProxyNodes {
        if !node.Enable {
            continue
        }
        dialer, err := NewHttpProxyDialer(proxy, node.Name, node.URL)
        if err != nil {
            proxy.Logger.Printf("WARN: 节点 %s 创建失败: %v", node.Name, err)
            continue
        }
        router.AddDialer(node.Name, dialer)
    }

    // 3. 构建路由规则
    for _, route := range cfg.Routes {
        if !route.Enable {
            continue
        }

        // 根据类型创建条件
        var condition ReqCondition
        switch route.Type {
        case "DomainSuffix":
            condition = DomainSuffixRule(route.Value)
        case "DomainKeyword":
            condition = DomainKeywordRule(route.Value)
        case "IP":
            condition = IPRule(route.Value)
        default:
            proxy.Logger.Printf("WARN: 未知规则类型 %s", route.Type)
            continue
        }

        // 根据行为选择目标
        target := route.Action
        if route.Action == "proxy" {
            // 使用 IsDefault 标记的节点
            target = getDefaultProxyNode(cfg)
            if target == "" {
                proxy.Logger.Printf("WARN: 未设置默认代理节点，回退 Direct")
                target = "Direct"
            }
        }

        router.AddRule(condition, target)
    }

    return nil
}

// getDefaultProxyNode 查找标记为 IsDefault 的代理节点
func getDefaultProxyNode(cfg *ServerConfig) string {
    for _, node := range cfg.ProxyNodes {
        if node.Enable && node.IsDefault {
            return node.Name
        }
    }
    // 如果没有明确标记，返回第一个启用的节点
    for _, node := range cfg.ProxyNodes {
        if node.Enable {
            return node.Name
        }
    }
    return ""
}
```

#### 步骤 1.3：实现配置 API

**文件**：`proxysocket/proxy.go`

在 `StartControlServer` 中添加 HTTP 处理器：

```go
// 在 StartControlServer 中，需要将 router 传入
func (ws *WebsocketServer) StartControlServer(router *mproxy.Router) bool {
    hub = &WebSocketHub{proxy: ws.Proxy}
    mux := http.NewServeMux()
    mux.HandleFunc("/start", ws.loginHandler(ws.handleWebSocket))
    mux.HandleFunc("/api/storage/download", myminio.HandleDownload)
    // 配置 API 无需鉴权（前端路由守卫保护）
    mux.HandleFunc("/api/config", ws.configHandler(cm, router))
    // ...
}
```

**注意**：

- `/api/config` 端点不需要 `loginHandler` 包装（前端有路由守卫）
- `/api/storage/download` 同样不需要鉴权（保持一致）
- `/start` WebSocket 连接仍然需要 `loginHandler` 验证 token

```go
// configHandler 处理配置的读写请求
func (ws *WebsocketServer) configHandler(cm *mproxy.ConfigManager, router *mproxy.Router) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        switch r.Method {
        case "GET":
            cm.mu.RLock()
            json.NewEncoder(w).Encode(cm.Current)
            cm.mu.RUnlock()

        case "POST":
            var updated mproxy.ServerConfig
            if err := json.NewDecoder(r.Body).Decode(&updated); err != nil {
                http.Error(w, err.Error(), http.StatusBadRequest)
                return
            }

            // 1. 更新配置到内存和磁盘
            if err := cm.UpdateFromJSON(updated); err != nil {
                http.Error(w, err.Error(), http.StatusInternalServerError)
                return
            }

            // 2. 热重载路由（关键改进：持有写锁）
            if updated.RouteEnable {
                router.mu.Lock() // 获取写锁
                err := mproxy.LoadRoutesFromConfig(ws.Proxy, router, &updated)
                router.mu.Unlock() // 释放写锁

                if err != nil {
                    ws.Proxy.Logger.Printf("ERROR: 路由重载失败: %v", err)
                    http.Error(w, "路由重载失败: "+err.Error(), http.StatusInternalServerError)
                    return
                }
                ws.Proxy.Logger.Printf("INFO: 路由配置已热重载，共 %d 条规则", len(updated.Routes))
            }

            w.WriteHeader(http.StatusOK)
            json.NewEncoder(w).Encode(map[string]string{"status": "ok"})

        default:
            http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
        }
    }
}
```

**关键改进**：

- 添加 `router` 参数传入
- 调用 `LoadRoutesFromConfig` 前获取 `router.mu.Lock()`
- 完成后立即 `Unlock()`，最小化锁持有时间

#### 步骤 1.4：关联 ConfigManager 到 CoreHttpServer

**文件**：`mproxy/core_proxy.go`

在 `CoreHttpServer` 中添加 `ConfigManager` 字段：

```go
type CoreHttpServer struct {
    // ... 现有字段 ...
    ConfigMgr *ConfigManager
}
```

---

### 阶段二：前端实现

#### 步骤 2.1：扩展 API 层

**文件**：`src/api/api.js`

添加 HTTP 请求方法：

```js
// 获取配置（无需鉴权，前端有路由守卫保护）
export async function fetchConfig(apiUrl) {
  const response = await fetch(`${apiUrl}/api/config`)
  if (!response.ok) throw new Error('获取配置失败')
  return response.json()
}

// 更新配置
export async function updateConfig(apiUrl, config) {
  const response = await fetch(`${apiUrl}/api/config`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(config)
  })
  if (!response.ok) throw new Error('更新配置失败')
  return response.json()
}
```

#### 步骤 2.2：扩展 WebSocket Store

**文件**：`src/stores/websocket.js`

添加配置管理状态和方法：

```js
// 新增状态
const config = ref(null)

// 新增方法
async function fetchConfig() {
  const response = await fetch(apiUrl.value + '/api/config')
  config.value = await response.json()
  return config.value
}

async function updateConfig(newConfig) {
  const response = await fetch(apiUrl.value + '/api/config', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(newConfig)
  })
  if (response.ok) {
    config.value = newConfig
  }
  return response.json()
}
```

#### 步骤 2.3：创建路由配置页面

**文件**：新建 `src/views/RouteConfig.vue`

实现以下功能：

1. 顶部：全局路由开关 + 默认行为下拉框
2. 中部：添加规则表单（类型、值、行为、备注）
3. 主体：规则列表（表格形式，支持启用/禁用、删除）

#### 步骤 2.4：添加路由

**文件**：`src/router/index.js`

```js
import RouteConfig from '../views/RouteConfig.vue'

// 在 dashboard children 中添加
{
  path: 'route-config',
  name: 'route-config',
  component: RouteConfig,
}
```

**文件**：`src/views/dashboard.vue`

在侧边栏添加导航项：

```vue
<RouterLink to="/dashboard/route-config" class="nav-item">
  <svg>...</svg>
  <span>路由配置</span>
</RouterLink>
```

---

### 阶段三：UI/UX 细节

#### 组件设计

**开关组件**：复用现有的开关样式

**规则表格**：

```
| 启用 | 类型 | 值 | 行为 | 备注 | 操作 |
|------|------|-----|------|------|------|
| [✓] | DomainSuffix | twitter.com | Proxy | Twitter推持 | 🗑️ |
| [  ] | DomainKeyword | google | Direct | 谷歌直连 | 🗑️ |
```

**交互流程**：

1. **登录成功后立即加载配置**（关键改进）
   - 在登录成功回调中调用 `wsStore.fetchConfig()`
   - 不依赖 WebSocket 连接状态
   - 配置数据存储在 `wsStore.config` 中
2. 页面加载时从 `wsStore.config` 读取配置（如果已有）
3. 用户修改规则后，本地 `routes` 数组更新
4. 点击"保存"按钮时调用 `updateConfig()` 提交到后端
5. 保存成功后显示成功提示并更新本地缓存

**数据加载时序改进**：

```
登录 → 立即 fetchConfig()（并行）→ WebSocket 连接（并行）
              ↓                              ↓
         config.value ←─────────── isConnected
```

这样配置加载不阻塞页面渲染，用户体验更好。

---

## 关键文件清单

### 后端

- `mproxy/config.go` - 配置结构定义（添加 `ProxyNode.IsDefault`）
- `mproxy/router.go` - 添加 `ClearRules()` 和 `ClearDialers()` 方法
- `mproxy/config_loader.go` - **新建** 配置加载逻辑（线程安全）
- `proxysocket/proxy.go` - 添加 `/api/config` API 端点
- `mproxy/core_proxy.go` - 关联 ConfigManager（可选）
- `main.go` - 调整 `StartControlServer` 调用顺序

### 前端

- `src/api/api.js` - HTTP 请求方法
- `src/stores/websocket.js` - 配置状态管理
- `src/views/RouteConfig.vue` - **新建** 路由配置页面
- `src/router/index.js` - 添加路由
- `src/views/dashboard.vue` - 添加导航项

---

## 验证方法

### 后端验证

```bash
# 1. 启动服务
go run main.go

# 2. 获取配置（无需 token）
curl http://localhost:8000/api/config

# 3. 更新配置
curl -X POST http://localhost:8000/api/config \
  -H "Content-Type: application/json" \
  -d '{"routeEnable":true,"routes":[...]}'

# 4. 并发测试（验证线程安全）
for i in {1..10}; do
  curl -X POST http://localhost:8000/api/config \
    -H "Content-Type: application/json" \
    -d '{"routeEnable":true,"routes":[...]}' &
done
wait
```

### 线程安全验证（新增）

1. 启动代理服务器
2. 使用 `ab` 或 `wrk` 进行并发配置更新
3. 同时发送真实流量请求
4. 检查日志，确认没有请求匹配到空路由表
5. 验证规则数量正确（不会越积越多）

### 前端验证

1. 访问 `/dashboard/route-config` 页面
2. 添加一条新规则（如：DomainSuffix + twitter.com + Proxy）
3. 点击保存
4. 验证后端配置文件已更新
5. 测试规则是否生效（访问 twitter.com 查看日志中的路由匹配信息）

---

## 设计决策说明

### 1. 为什么 Action 使用 "proxy" 而非具体节点名？

- 简化用户操作：用户只需选择"走代理"，无需关心具体是哪个节点
- 系统自动选择：后端使用第一个启用的代理节点
- 扩展性：未来可增加"节点选择"高级功能

### 2. 为什么使用 HTTP API 而非 WebSocket？

- RESTful 语义更清晰（GET 获取、POST 更新）
- 配置更新是低频操作，不需要实时推送
- 可复用现有的 HTTP 工具（curl、Postman）

### 3. 配置更新后是否需要重启服务？

- 不需要：`ConfigManager.Update()` 直接更新内存配置
- 路由规则需要重新构建（在 `configHandler` 中处理）
- 配置和 WebSocket 连接解耦，互不影响

---

## 关键改进总结（基于 plan_add.md 分析）

### A. 路由热重载线程安全问题 ✅

**问题**：每次保存配置会调用 `AddRule`，导致规则越积越多

**解决方案**：

1. 在 `Router` 中添加 `ClearRules()` 和 `ClearDialers()` 方法
2. `LoadRoutesFromConfig` 开始时调用清理方法
3. 重载全程持有 `router.mu.Lock()` 写锁
4. 最小化锁持有时间

**风险**：锁持有期间会阻塞路由匹配请求
**缓解**：重载操作很快（<10ms），影响可忽略

### B. 默认代理节点选择逻辑 ✅

**问题**：`getDefaultProxyNode` 取第一个启用的节点，语义不清晰

**解决方案**：

1. 在 `ProxyNode` 中添加 `IsDefault bool` 字段
2. UI 提供单选框让用户选择默认节点
3. 优先使用 `IsDefault=true` 的节点
4. 如果没有标记，才回退到第一个启用的节点

**用户体验**：可以明确知道"走代理"会使用哪个节点

### C. 前端数据加载时序优化 ✅

**问题**：原计划在页面加载时调用 `fetchConfig()`，但依赖 WebSocket 连接

**解决方案**：

1. 登录成功后立即调用 `fetchConfig()`（不等待 WebSocket）
2. 配置数据存储在 `wsStore.config` 中
3. 各页面直接从 `wsStore.config` 读取
4. WebSocket 连接和配置加载并行进行

**效果**：首屏加载更快，用户体验更好

### 4. 热重载线程安全如何保证？（新增）

- 使用 `router.mu.Lock()` 保护整个重载过程
- 先 `ClearRules()` 清空旧规则，再添加新规则
- 最小化锁持有时间，避免阻塞正常请求

### 5. 为什么在 ProxyNode 中添加 IsDefault 字段？（新增）

- 明确指定默认代理节点，避免歧义
- UI 可以提供单选框让用户选择默认节点
- 当规则 Action 为 "proxy" 时，使用此节点

### 6. 为什么登录后立即加载配置？（新增）

- 配置是静态数据，不需要 WebSocket 实时推送
- HTTP GET 请求更快速，首屏体验更好
- 配置和 WebSocket 连接解耦，互不影响