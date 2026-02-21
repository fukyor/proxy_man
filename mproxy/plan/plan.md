# HTTP 代理中 WebSocket 支持方案

## 问题分析

### 当前状态对比

| 处理逻辑       | HTTP 代理 (`http.go`)            | HTTPS MITM (`https.go`)                |
| -------------- | -------------------------------- | -------------------------------------- |
| WebSocket 检测 | ❌ 无检测逻辑                     | ✅ `isWebSocketHandshake(resp.Header)`  |
| 捕获跳过标记   | ❌ 未调用                         | ✅ `ctxt.SetCaptureSkip()`              |
| 客户端连接     | `http.ResponseWriter` (单向写入) | `connFromClinet`, `tlsConn` (原始 TCP) |
| 数据转发       | ❌ `io.Copy(w, resp.Body)` 单向   | ✅ `proxyWebsocket()` 双向转发          |

### HTTPS MITM 中的处理流程 (`https.go:717-732`)

```go
if isWebsocket {
    ctxt.Log_P("Response looks like websocket upgrade.")
    // Go 的 http.Response.Body 在 101 响应后会实现 io.ReadWriter
    wsConn, ok := resp.Body.(io.ReadWriter)
    proxy.proxyWebsocket(ctxt, wsConn, tlsConn)  // 双向转发！
    return false
}
```

### HTTP 代理的根本问题

**`http.ResponseWriter` 无法支持 WebSocket**：

- `w` 只能**写入**响应，不能**读取**客户端数据
- `io.Copy(w, resp.Body)` 只能复制：服务器 → 客户端
- WebSocket 需要双向通信，客户端发送的帧无法被读取

**WebSocket 握手后的通信路径**：

```
客户端 ←WebSocket 帧→ 代理 ←WebSocket 帧→ 服务器
   ↑                         ↓
   └───────── 必须双向转发 ─────────┘
```

**当前方案的致命缺陷**：

```go
// 用户修改后的方案
if isWebsocket {
    ctxt.SetCaptureSkip()  // 仅标记跳过捕获
}
// ... 后续 io.Copy(w, resp.Body) 只能单向复制服务器→客户端
// 客户端发送的数据无法读取，连接半死！
```

## 解决方案

### 方案对比

| 方案                            | 复杂度 | WebSocket 支持 | 推荐度     |
| ------------------------------- | ------ | -------------- | ---------- |
| A. 仅标记跳过捕获               | 低     | ❌ 无法双向通信 | ❌ 不推荐   |
| **B. Hijack + 双向转发**        | 中     | ✅ 完整支持     | ✅ **推荐** |
| C. 禁用 HTTP 代理中的 WebSocket | 低     | ❌ 不支持       | ⚠️ 备选     |

### 推荐方案：Hijack + 双向转发

**核心思路**：复刻 `https.go` 中的处理方式

1. 检测 WebSocket 握手响应
2. Hijack 获取客户端原始连接
3. 手动写入 101 响应头
4. 使用 `proxyWebsocket()` 双向转发

---

## 具体修改

**文件**: `mproxy/http.go`

---

### 修改点 1：修复 DownloadRef 引用错误（第 43 行）

```go
// 当前代码
DownloadRef: &ctxt.TrafficCounter.req_sum,  // ❌ 错误

// 修改为
DownloadRef: &ctxt.TrafficCounter.resp_sum,  // ✅ 正确
```

---

### 修改点 2：添加 WebSocket 检测和处理（第 65 行之后）

**位置**：`filterResponse` 调用之后，替换原有的响应处理逻辑

**修改前**（第 65-118 行）：

```go
resp = proxy.filterResponse(resp, ctxt)

// 在流量统计关闭回调中注销（确保流量统计完成后再删除）
if resp != nil && resp.Body != nil {
    if rBReader, ok := resp.Body.(*respBodyReader); ok {
        // ... onClose 处理
    }
}

if resp == nil {
    // ... 错误处理
    return
}

// 封装响应头
buildHeaders(w.Header(), resp.Header, proxy.KeepDestHeaders)
w.WriteHeader(resp.StatusCode)

var bodyWriter io.Writer = w
// ... SSE/chunked 处理
_, err = io.Copy(bodyWriter, resp.Body)
```

**修改后**：

```go
resp = proxy.filterResponse(resp, ctxt)

// 检测 WebSocket 握手
if resp != nil && isWebSocketHandshake(resp.Header) {
    ctxt.Log_P("检测到 WebSocket 握手响应")
    ctxt.SetCaptureSkip() // 跳过 minio 捕获

    // Hijack 获取客户端连接
    hj, ok := w.(http.Hijacker)
    if !ok {
        ctxt.WarnP("ResponseWriter 不支持 Hijack，无法处理 WebSocket")
        http.Error(w, "WebSocket not supported", http.StatusInternalServerError)
        return
    }
    clientConn, _, err := hj.Hijack()
    if err != nil {
        ctxt.WarnP("Hijack 失败: %v", err)
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }
    defer clientConn.Close()

    // 手动写入 101 响应头
    text := resp.Status
    statusCode := strconv.Itoa(resp.StatusCode) + " "
    text = strings.TrimPrefix(text, statusCode)
    if _, err := io.WriteString(clientConn, "HTTP/1.1 "+statusCode+text+"\r\n"); err != nil {
        ctxt.WarnP("写入 WebSocket 响应状态失败: %v", err)
        return
    }
    if err := resp.Header.Write(clientConn); err != nil {
        ctxt.WarnP("写入 WebSocket 响应头失败: %v", err)
        return
    }
    if _, err := io.WriteString(clientConn, "\r\n"); err != nil {
        ctxt.WarnP("写入 WebSocket 响应头结束符失败: %v", err)
        return
    }

    // 获取服务器端双向连接
    wsConn, ok := resp.Body.(io.ReadWriter)
    if !ok {
        ctxt.WarnP("resp.Body 不支持 io.ReadWriter")
        return
    }

    // 双向转发 WebSocket 数据
    ctxt.Log_P("开始 WebSocket 双向转发")
    proxy.proxyWebsocket(ctxt, wsConn, clientConn)

    // 连接关闭后清理
    proxy.MarkConnectionClosed(ctxt.Session)
    return
}

// 非 WebSocket 的原有处理逻辑
// ... 保持原有代码不变
```

---

### 代码修改汇总

| 行号   | 修改类型     | 说明                                           |
| ------ | ------------ | ---------------------------------------------- |
| 43     | 修复 bug     | `DownloadRef` 引用从 `req_sum` 改为 `resp_sum` |
| 65+    | 新增逻辑     | 添加 WebSocket 检测和 hijack 处理              |
| import | 可能需要添加 | `strconv` 包（如果未导入）                     |

---

### 需要添加的 import

检查 `http.go` 的 import 部分，确保包含：

```go
import (
    "io"
    "net/http"
    "sync/atomic"
    "strings"
    "time"
    "context"
    "strconv"  // ← 确保有这个
)
```

---

### 关键实现细节

**1. Hijack 的必要性**

```go
// http.ResponseWriter 只能写入，不能读取
w.Write(data)  // ✅ 可以
w.Read(data)   // ❌ 不存在这个方法

// WebSocket 需要双向通信
clientConn, _, err := hj.Hijack()  // 获取原始 TCP 连接
io.Copy(clientConn, wsConn)        // 服务器 → 客户端
io.Copy(wsConn, clientConn)        // 客户端 → 服务器
```

**2. proxyWebsocket 双向转发**

```go
// websocket.go:40-56
func (proxy *CoreHttpServer) proxyWebsocket(ctx *Pcontext, remoteConn io.ReadWriter, proxyClient io.ReadWriter) {
    waitChan := make(chan struct{}, 2)
    go func() {
        _ = copyOrWarn(ctx, remoteConn, proxyClient)  // 服务器 → 客户端
        waitChan <- struct{}{}
    }()
    go func() {
        _ = copyOrWarn(ctx, proxyClient, remoteConn)  // 客户端 → 服务器
        waitChan <- struct{}{}
    }()
    <-waitChan  // 等待任一方向关闭
}
```

**3. 101 响应的手动写入**

```go
// 标准的 w.WriteHeader(resp.StatusCode) 会设置 HTTP 状态
// 但 WebSocket 需要完整的 101 Switching Protocols 响应
// 必须手动写入完整响应
```

---

## 验证测试

### 测试步骤

1. **编译并启动代理服务器**

   ```bash
   go build -o proxy_man main.go
   ./proxy_man -v -addr :8080
   ```

2. **配置客户端使用 HTTP 代理**

3. **测试 WebSocket 连接**

   **方法 A - 浏览器控制台**：

   ```javascript
   // 访问任意网站后，在浏览器控制台执行
   const ws = new WebSocket('ws://echo.websocket.org');
   ws.onopen = () => console.log('✅ WebSocket 已连接');
   ws.onmessage = (e) => console.log('📨 收到消息:', e.data);
   ws.onerror = (e) => console.error('❌ WebSocket 错误:', e);
   ws.onclose = (e) => console.log('🔌 WebSocket 已关闭', e);
   ws.send('🚀 测试消息');
   ```

   **方法 B - 在线测试工具**：

   - 访问 `https://www.websocket.org/echo.html`
   - 配置浏览器使用代理后连接测试

   **方法 C - 本地测试**：

   ```bash
   # 终端 1：启动本地 WebSocket 服务器
   npx wscat -l 9000
   
   # 终端 2：通过代理连接（配置代理环境变量）
   export HTTP_PROXY=http://127.0.0.1:8080
   npx wscat -ws://localhost:9000
   ```

4. **观察日志输出**

   ```bash
   # 期望看到以下日志序列：
   [XXX] INFO: Sending request GET ws://echo.websocket.org/...
   [XXX] INFO: 检测到 WebSocket 握手响应        ← 新增的日志
   [XXX] INFO: 开始 WebSocket 双向转发            ← 新增的日志
   # （连接保持期间无日志，直到关闭）
   ```

5. **验证连接信息和流量统计**（WebSocket Hub 推送）

   - 连接应出现在活跃连接列表
   - 协议显示为 "HTTP"
   - 流量统计应正常更新

### 预期结果

| 测试项                    | 修改前           | 修改后                             |
| ------------------------- | ---------------- | ---------------------------------- |
| WebSocket 握手            | ❌ 日志无提示     | ✅ 显示 "检测到 WebSocket 握手响应" |
| 连接建立                  | ⚠️ 可能建立但半死 | ✅ 正常建立并保持                   |
| 消息传输（客户端→服务器） | ❌ 无法发送       | ✅ 正常发送                         |
| 消息传输（服务器→客户端） | ⚠️ 可能接收       | ✅ 正常接收                         |
| 连接保持                  | ⚠️ 不稳定         | ✅ 稳定保持                         |
| 流量捕获                  | ⚠️ 可能误捕获     | ✅ 跳过 Exchange 捕获               |

### 关键验证点

**双向通信测试**：

```javascript
// 发送消息
ws.send('Hello from client');
// 应该能收到服务器的回显消息
ws.onmessage = (e) => console.log('收到:', e.data);
```

**持久连接测试**：

```javascript
// 连接应保持打开，不会立即关闭
setTimeout(() => {
    console.log('连接状态:', ws.readyState); // 应该是 1 (OPEN)
}, 5000);
```

### 常见问题排查

| 问题                            | 可能原因                       | 解决方法                            |
| ------------------------------- | ------------------------------ | ----------------------------------- |
| WebSocket 连接立即关闭          | Hijack 失败或响应头写入错误    | 检查 `hj.Hijack()` 返回值           |
| 只能接收不能发送                | 未使用 proxyWebsocket 双向转发 | 确认调用了 `proxy.proxyWebsocket()` |
| 编译错误 `strconv 未导入`       | 缺少 import                    | 添加 `import "strconv"`             |
| 日志显示 "Hijack not supported" | ResponseWriter 不支持 Hijack   | 检查是否使用了支持的 HTTP 服务器    |

---

## 关键文件

| 文件             | 修改内容                                           | 修改行数 |
| ---------------- | -------------------------------------------------- | -------- |
| `mproxy/http.go` | 添加 WebSocket 检测、hijack 处理、修复 DownloadRef | ~60 行   |

---

## 依赖函数和类型（已存在，无需修改）

| 函数/类型                  | 位置                     | 说明                          |
| -------------------------- | ------------------------ | ----------------------------- |
| `isWebSocketHandshake()`   | `websocket.go:21-24`     | 检测 WebSocket 握手           |
| `SetCaptureSkip()`         | `mitm_exchange.go:88-89` | 标记跳过 Exchange 捕获        |
| `proxy.proxyWebsocket()`   | `websocket.go:40-56`     | 双向转发 WebSocket 数据       |
| `proxy.hijackConnection()` | `websocket.go:26-38`     | Hijack 获取客户端连接         |
| `http.Hijacker`            | 标准库                   | ResponseWriter 的 Hijack 接口 |
| `io.ReadWriter`            | 标准库                   | 101 响应后 Body 实现的接口    |

---

## 与 HTTPS MITM 的对比

| 方面           | HTTPS MITM (`https.go`) | HTTP 代理 (`http.go` 修改后) |
| -------------- | ----------------------- | ---------------------------- |
| 客户端连接类型 | `*tls.Conn` (已 TLS)    | `net.Conn` (原始 TCP)        |
| Hijack 时机    | CONNECT 阶段            | WebSocket 检测后             |
| 响应写入方式   | `tlsConn.Write()`       | `clientConn.Write()`         |
| 双向转发       | ✅ `proxyWebsocket()`    | ✅ `proxyWebsocket()`         |
| 连接关闭       | `return false`          | `return`                     |

---

## 实现注意事项

1. **Hijack 后的响应头写入**：必须手动写入完整的 HTTP 响应，不能使用 `w.WriteHeader()`

2. **连接清理**：确保在 WebSocket 连接关闭后调用 `proxy.MarkConnectionClosed()`

3. **错误处理**：Hijack 失败时需要返回适当的 HTTP 错误响应

4. **defer clientConn.Close()**：确保连接最终被关闭，避免资源泄露

---

## 代码实现逻辑图

```
HTTP 请求到达
    ↓
filterRequest (可选修改请求)
    ↓
RoundTrip (发起请求)
    ↓
filterResponse (可选修改响应)
    ↓
检测 WebSocket 握手？
    ├─ 是 → Hijack 客户端连接
    │       ↓
    │   手动写入 101 响应头
    │       ↓
    │   proxyWebsocket 双向转发
    │       ↓
    │   连接关闭，清理资源
    │       ↓
    │   return
    │
    └─ 否 → 正常 HTTP 响应处理
            ↓
        buildHeaders + w.WriteHeader
            ↓
        io.Copy(bodyWriter, resp.Body)
            ↓
        连接关闭
```