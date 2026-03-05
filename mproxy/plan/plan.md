# 修复普通 HTTP 代理及消除 MitmEnabled 的不当耦合 - 修订计划

## 一、错误报告分析

### Bug 描述

1. **Panic/内存溢出错误**：在 `MyHttpHandle`（普通 HTTP GET 代理流程）中，当 `MitmEnabled=true` 时，下游 hook（`actions.go`）尝试访问 `ctx.exchangeCapture.reqBodyCapture` 和 `ctx.exchangeCapture.respBodyCapture`，由于 `exchangeCapture` 为 `nil` 导致 **nil pointer dereference panic**。

2. **错误捕获问题**：在 `HttpMitmNoTunnel=false` 时（普通代理模式），HTTP 请求不应该进行 MITM 记录，但当前代码逻辑存在混淆。

### 根本原因

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                          请求处理流程对比                                    │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                             │
│  【普通 HTTP 代理】MyHttpHandle (HttpMitmNoTunnel=false)                     │
│  ┌─────────────────────────────────────────────────────────────────────┐   │
│  │ 1. 创建 Pcontext，但 **没有调用 StartCapture()**                      │   │
│  │    → exchangeCapture = nil                                           │   │
│  │                                                                       │   │
│  │ 2. filterRequest() → AddTrafficMonitor Hook (actions.go:44-51)       │   │
│  │    ┌─────────────────────────────────────────────────────────────┐   │   │
│  │    │ if ctx.core_proxy.MitmEnabled {  ← 只判断全局开关             │   │   │
│  │    │     ctx.exchangeCapture.reqBodyCapture = ...  ← 💥 PANIC!     │   │   │
│  │    │ }                                                             │   │   │
│  │    └─────────────────────────────────────────────────────────────┘   │   │
│  │                                                                       │   │
│  │ 3. filterResponse() → AddTrafficMonitor Hook (actions.go:119-126)    │   │   │
│  │    ┌─────────────────────────────────────────────────────────────┐   │   │
│  │    │ if ctx.core_proxy.MitmEnabled {  ← 只判断全局开关             │   │   │
│  │    │     ctx.exchangeCapture.respBodyCapture = ...  ← 💥 PANIC!    │   │   │
│  │    │ }                                                             │   │   │
│  │    └─────────────────────────────────────────────────────────────┘   │   │
│  └─────────────────────────────────────────────────────────────────────┘   │
│                                                                             │
│  【HTTP MITM 引擎模式】myHttpHandleWithEngine (HttpMitmNoTunnel=true)         │
│  ┌─────────────────────────────────────────────────────────────────────┐   │
│  │ 1. 调用 StartCapture(tunnelSession)  ← 初始化 exchangeCapture       │   │
│  │    → exchangeCapture ≠ nil                                           │   │
│  │                                                                       │   │
│  │ 2. filterRequest() → AddTrafficMonitor Hook                          │   │   │
│  │    ┌─────────────────────────────────────────────────────────────┐   │   │
│  │    │ if ctx.core_proxy.MitmEnabled {  ← 全局开关开启               │   │   │
│  │    │     ctx.exchangeCapture.reqBodyCapture = ...  ← ✅ 正常       │   │   │
│  │    │ }                                                             │   │   │
│  │    └─────────────────────────────────────────────────────────────┘   │   │
│  └─────────────────────────────────────────────────────────────────────┘   │
│                                                                             │
└─────────────────────────────────────────────────────────────────────────────┘
```

**核心矛盾**：

- `actions.go` **只检查** `ctx.core_proxy.MitmEnabled`，**没有检查** `exchangeCapture` 是否为 `nil`
- 当 `MitmEnabled=true` 但 `exchangeCapture=nil` 时（普通 HTTP 请求），直接访问字段触发 panic
- 需要**双重检查**：既检查全局开关（业务逻辑控制），又检查对象是否初始化（防御性编程）

### 配置开关说明

| 配置项             | 作用域       | 说明                                                |
| ------------------ | ------------ | --------------------------------------------------- |
| `MitmEnabled`      | **全局开关** | 控制所有 MITM 相关行为（MinIO 上传、Exchange 发送） |
| `HttpMitmNoTunnel` | **局部开关** | 仅控制 http.go 中的 MITM 引擎模式是否启用           |

---

## 二、修订后的实施计划

### 修改 1：修复 actions.go 的判断逻辑（关键修复）

**文件**：`mproxy/actions.go`

**问题**：当前代码（第 44-51 行和第 119-126 行）只检查 `MitmEnabled`，直接访问 `exchangeCapture` 字段而不检查其是否为 `nil`。

**原代码（错误）**：

```go
// 请求体处理 - 第 44-51 行
if ctx.core_proxy.MitmEnabled {
    contentType := req.Header.Get("Content-Type")
    captReader := myminio.BuildBodyReader(trafficReader, ctx.Session, "req", contentType, req.ContentLength)
    ctx.exchangeCapture.reqBodyCapture = captReader.Capture  // ← nil panic 风险
    req.Body = captReader
} else {
    req.Body = trafficReader
}

// 响应体处理 - 第 119-126 行
if ctx.core_proxy.MitmEnabled {
    contentType := resp.Header.Get("Content-Type")
    captReader := myminio.BuildBodyReader(trafficReader, ctx.Session, "resp", contentType, resp.ContentLength)
    ctx.exchangeCapture.respBodyCapture = captReader.Capture  // ← nil panic 风险
    resp.Body = captReader
} else {
    resp.Body = trafficReader
}
```

**修复方案**：使用**双重检查** `ctx.exchangeCapture != nil && ctx.core_proxy.MitmEnabled`。

```go
// 请求体处理 - 修改后
if ctx.exchangeCapture != nil && ctx.core_proxy.MitmEnabled {
    // 双重检查：对象已初始化 AND 全局开关开启
    contentType := req.Header.Get("Content-Type")
    captReader := myminio.BuildBodyReader(trafficReader, ctx.Session, "req", contentType, req.ContentLength)
    ctx.exchangeCapture.reqBodyCapture = captReader.Capture
    req.Body = captReader
} else {
    // 只使用流量统计层
    req.Body = trafficReader
}

// 响应体处理 - 修改后
if ctx.exchangeCapture != nil && ctx.core_proxy.MitmEnabled {
    // 双重检查：对象已初始化 AND 全局开关开启
    contentType := resp.Header.Get("Content-Type")
    captReader := myminio.BuildBodyReader(trafficReader, ctx.Session, "resp", contentType, resp.ContentLength)
    ctx.exchangeCapture.respBodyCapture = captReader.Capture
    resp.Body = captReader
} else {
    // 只使用流量统计层
    resp.Body = trafficReader
}
```

**修复理由**：

1. **防止 panic**：`exchangeCapture != nil` 检查确保只在对象已初始化时才访问其字段
2. **保留全局控制**：`MitmEnabled == true` 确保全局开关仍然有效，不被架空
3. **防御性编程**：即使逻辑上 `MitmEnabled=true` 时应该有 `exchangeCapture`，也加上了 nil 检查以防御意外情况

---

### 修改 2：保留 mitm_exchange.go 的全局开关检查（防御性设计）

**文件**：`mproxy/mitm_exchange.go`

**分析**：`SendExchange()` 函数（第 104-107 行）开头的 `MitmEnabled` 检查应该**保留**。

**当前代码（保持不变）**：

```go
func (ctx *Pcontext) SendExchange() {
    if !ctx.core_proxy.MitmEnabled {
        return  // ← 全局开关关闭时，直接返回，不发送任何 Exchange
    }
    cap := ctx.exchangeCapture
    if cap == nil || cap.skipSend || cap.sent {
        return
    }
    // ... 发送 Exchange
}
```

**保持不变的理由**：

1. **早期退出优化**：全局开关关闭时立即返回，避免不必要的处理
2. **防御性保护**：即使 `exchangeCapture` 被错误初始化（非预期情况），也能被全局开关拦截
3. **语义清晰**：全局开关是最高优先级的控制，应该最先检查
4. **符合原 plan.md 的意图**：原计划也提到 MitmEnabled 是"总开关"

---

### 修改 3：确认 MyHttpHandle 不需要修改

**文件**：`mproxy/http.go` 的 `MyHttpHandle` 函数（第 15-169 行）

**分析**：

- 当前代码**没有调用** `StartCapture()`、`CaptureRequest()` 或 `SetCaptureError()`
- 这是**正确的行为**，因为普通 HTTP 代理不应该进行 MITM 捕获
- 修复 `actions.go` 后，由于 `exchangeCapture` 为 `nil`，`MitmEnabled` 的判断会短路，不会访问 `exchangeCapture` 字段

**确认**：无需修改 `MyHttpHandle`，当前行为是正确的。

---

## 三、各场景下的行为分析

### 场景 1：普通 HTTP 请求（HttpMitmNoTunnel=false）

| 配置 | exchangeCapture | MitmEnabled | 判断结果             | 行为               |
| ---- | --------------- | ----------- | -------------------- | ------------------ |
| 任意 | `nil`           | `false`     | 短路（`nil && ...`） | 只使用流量统计层 ✅ |
| 任意 | `nil`           | `true`      | 短路（`nil && ...`） | 只使用流量统计层 ✅ |

**结论**：由于 `exchangeCapture` 为 `nil`，无论 `MitmEnabled` 是什么值，都会短路，**不会触发 panic**。

### 场景 2：HTTP MITM 引擎模式（HttpMitmNoTunnel=true）

| 配置 | exchangeCapture | MitmEnabled | 判断结果                  | 行为               |
| ---- | --------------- | ----------- | ------------------------- | ------------------ |
| 正常 | 非 `nil`        | `true`      | `true && true` = `true`   | MinIO 捕获 ✅       |
| 正常 | 非 `nil`        | `false`     | `true && false` = `false` | 只使用流量统计层 ✅ |

**结论**：只有在 `exchangeCapture` 已初始化 **且** `MitmEnabled=true` 时才进行 MinIO 捕获。

### 场景 3：HTTPS MITM 模式

| 配置 | exchangeCapture | MitmEnabled | 判断结果                  | 行为               |
| ---- | --------------- | ----------- | ------------------------- | ------------------ |
| 正常 | 非 `nil`        | `true`      | `true && true` = `true`   | MinIO 捕获 ✅       |
| 正常 | 非 `nil`        | `false`     | `true && false` = `false` | 只使用流量统计层 ✅ |

**结论**：与 HTTP MITM 引擎模式一致。

---

## 四、验证计划

### 测试场景 1：普通 HTTP 请求（不触发捕获）

**配置**：`HttpMitmNoTunnel=false`, `MitmEnabled=true`（**这是之前 panic 的场景**）

**测试命令**：

```bash
curl -x http://localhost:8080 http://baidu.com
```

**预期结果**：

- ✅ 请求正常转发，**无 panic**
- ✅ 前端不会收到此请求的 MITM Exchange
- ✅ MinIO 不会存储此请求的 Body

### 测试场景 2：HTTP MITM 引擎模式（触发捕获）

**配置**：`HttpMitmNoTunnel=true`, `MitmEnabled=true`

**测试命令**：

```bash
curl -x http://localhost:8080 http://baidu.com
```

**预期结果**：

- ✅ 请求正常转发，无 panic
- ✅ 前端收到 MITM Exchange
- ✅ MinIO 存储请求/响应 Body

### 测试场景 3：全局开关关闭时的 MITM 引擎模式

**配置**：`HttpMitmNoTunnel=true`, `MitmEnabled=false`

**测试命令**：

```bash
curl -x http://localhost:8080 http://baidu.com
```

**预期结果**：

- ✅ 请求正常转发（使用 MITM 引擎解密，但不进行 MinIO 存储）
- ✅ 前端**不会**收到 MITM Exchange（被 `SendExchange()` 的全局开关拦截）
- ✅ MinIO 不会存储请求/响应 Body

### 测试场景 4：HTTPS MITM 模式

**配置**：`MitmEnabled=true`

**测试命令**：

```bash
curl -x http://localhost:8080 https://baidu.com
```

**预期结果**：

- ✅ 请求正常转发，无 panic
- ✅ 前端收到 MITM Exchange
- ✅ MinIO 存储请求/响应 Body

---

## 五、关键文件清单

| 文件                      | 修改类型         | 行号范围          | 说明                                           |
| ------------------------- | ---------------- | ----------------- | ---------------------------------------------- |
| `mproxy/actions.go`       | **修改**         | 第 44-51 行       | 请求体处理：添加 `exchangeCapture != nil` 检查 |
| `mproxy/actions.go`       | **修改**         | 第 119-126 行     | 响应体处理：添加 `exchangeCapture != nil` 检查 |
| `mproxy/mitm_exchange.go` | **保持不变**     | 第 105-107 行     | 保留全局开关检查（防御性设计）                 |
| `mproxy/http.go`          | **确认无需修改** | MyHttpHandle 函数 | 当前行为正确                                   |
| `mproxy/https.go`         | **无需修改**     | HTTPS MITM 流程   | 已调用 StartCapture()                          |
| `mproxy/ctxt.go`          | **无需修改**     | Pcontext 定义     | exchangeCapture 字段定义                       |

---

## 六、风险评估

| 修改项                        | 风险等级 | 影响范围                           |
| ----------------------------- | -------- | ---------------------------------- |
| actions.go 添加 nil 检查      | **极低** | 纯粹添加防御性检查，不改变现有逻辑 |
| mitm_exchange.go 保持全局开关 | **无**   | 不修改现有代码                     |

**总体风险：极低**

---

## 七、设计原则

修复后的代码遵循以下设计原则：

1. **双重检查模式**：
   - `exchangeCapture != nil` → 防御性编程，防止 nil pointer panic
   - `MitmEnabled == true` → 业务逻辑控制，保留全局开关功能

2. **短路求值优化**：
   - 当 `exchangeCapture == nil` 时，`&&` 运算符会短路，不会评估 `MitmEnabled`
   - 普通 HTTP 请求（`exchangeCapture == nil`）的性能影响最小

3. **防御性设计**：
   - `SendExchange()` 开头的 `MitmEnabled` 检查提供最后一道防线
   - 即使逻辑上应该有 `exchangeCapture`，也加上 nil 检查

4. **向后兼容**：
   - 不影响现有的 MITM 流程（HTTPS MITM、HTTP MITM 引擎模式）
   - 全局开关 `MitmEnabled` 的控制能力完全保留

---

## 八、代码变更总结

### actions.go - 请求体处理（第 44-51 行）

```diff
  // 第二层：MinIO 捕获（仅 MITM 开启时执行）
- if ctx.core_proxy.MitmEnabled {
+ if ctx.exchangeCapture != nil && ctx.core_proxy.MitmEnabled {
      contentType := req.Header.Get("Content-Type")
      captReader := myminio.BuildBodyReader(trafficReader, ctx.Session, "req", contentType, req.ContentLength)
      ctx.exchangeCapture.reqBodyCapture = captReader.Capture
      req.Body = captReader
  } else {
      req.Body = trafficReader
  }
```

### actions.go - 响应体处理（第 119-126 行）

```diff
  // 第二层：MinIO 捕获（仅 MITM 开启时执行）
- if ctx.core_proxy.MitmEnabled {
+ if ctx.exchangeCapture != nil && ctx.core_proxy.MitmEnabled {
      contentType := resp.Header.Get("Content-Type")
      captReader := myminio.BuildBodyReader(trafficReader, ctx.Session, "resp", contentType, resp.ContentLength)
      ctx.exchangeCapture.respBodyCapture = captReader.Capture
      resp.Body = captReader
  } else {
      resp.Body = trafficReader
  }
```

### mitm_exchange.go - SendExchange()（保持不变）

```go
// 保持第 105-107 行的全局开关检查
func (ctx *Pcontext) SendExchange() {
    if !ctx.core_proxy.MitmEnabled {
        return  // ← 保留此防御性检查
    }
    cap := ctx.exchangeCapture
    if cap == nil || cap.skipSend || cap.sent {
        return
    }
    // ... 后续代码保持不变
}
```