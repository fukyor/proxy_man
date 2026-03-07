# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 项目概述

**proxy_man** 是基于 Go 语言的高性能 HTTP/HTTPS 代理服务器，基于 `github.com/elazarl/goproxy` v1.7.2 扩展，支持 MITM（中间人）模式、WebSocket、流量统计、MinIO 存储等功能。

## 常用命令

```bash
# 构建
go build -o proxy_man main.go

# 运行（默认端口 8080）
./proxy_man -v

# 自定义端口
./proxy_man -addr :8888 -v
```

**WebSocket 控制接口**：`ws://localhost:8000/start?token=123`

---

## 核心架构

**责任链模式**：请求/响应通过过滤器链处理，核心入口在 `CoreHttpServer.ServeHTTP`。

```
ServeHTTP → 判断 HTTP/HTTPS
    HTTP  → MyHttpHandle → filterRequest → RoundTrip → filterResponse
    HTTPS → MyHttpsHandle → Connect 处理 → MITM 或隧道模式
```

**关键文件**：
- `mproxy/core_proxy.go` - 主结构体，管理会话、连接、过滤器链
- `mproxy/http.go` - HTTP 处理（普通 + MITM 引擎模式）
- `mproxy/https.go` - HTTPS/CONNECT 处理
- `mproxy/router.go` - 路由引擎，规则代理
- `mproxy/router_roundtrip.go` - 路由感知的 RoundTripper
- `mproxy/hooks.go` - Hook 机制和条件过滤器
- `mproxy/ctxt.go` - `Pcontext` 请求上下文
- `proxysocket/hub.go` - WebSocket 实时推送中心

---

## Hook 机制

**请求 Hook** - 修改请求或提前返回：
```go
proxy.HookOnReq().DoFunc(func(req *http.Request, ctx *mproxy.Pcontext) (*http.Request, *http.Response) {
    return req, nil // 继续转发
    // return resp, nil // 直接返回响应
})
```

**CONNECT Hook** - 控制 HTTPS 连接策略：
```go
proxy.HookOnReq().DoConnectFunc(func(host string, ctx *mproxy.Pcontext) (*mproxy.ConnectAction, string) {
    return mproxy.MitmConnect, host       // HTTPS MITM
    // return mproxy.HTTPMitmConnect, host  // HTTP MITM
    // return mproxy.OkConnect, host         // 隧道透传
})
```

**条件过滤器**：
```go
proxy.HookOnReq(mproxy.UrlHook("/api/user")).DoFunc(...)
proxy.HookOnResp(mproxy.ContentTypeHook("application/json")).DoFunc(...)
```

---

## 路由模块

实现规则代理功能，根据域名/IP 将请求分发到不同的出站节点（二级代理）。

**触发条件**：`RouteEnable=true`

**核心组件**：
- `OutboundDialer` 接口 - 出站拨号器（`DirectDialer`、`HttpProxyDialer`）
- `Router` 路由引擎 - 管理拨号器和规则，按优先级匹配
- `RouterRoundTripper` - 使 MITM 流量也经过路由

**规则构建**：
```go
DomainSuffixRule("twitter.com", "x.com")    // 域名后缀
DomainKeywordRule("youtube", "google")      // 域名关键词
IPRule("127.0.0.1")                         // IP 精确
```

**两种路由调用链**：

| 转发模式 | 路由方式 | 调用链 |
|----------|----------|--------|
| 隧道透传 | `ConnectWithReqDial` | `connectDial()` → `proxy.ConnectWithReqDial()` → `router.RouteDial()` |
| MITM 模式（3种） | `RouterRoundTripper` | `filterRequest` → `ctx.RoundTripper` → `routerRT.RoundTrip()` → `transport.RoundTrip()` → `router.RouteDial()` |

**关键位置**：
- `mproxy/https.go:152-159` - `connectDial()` 中的路由逻辑
- `mproxy/router_roundtrip.go:26-32` - `RouterRoundTripper.DialContext` 调用路由
- `mproxy/actions.go:205-214` - `AddRouter()` 设置两种路由

---

## 五大转发模式

| 模式 | 触发条件 | 协议类型 | 连接层级 | 内容可读 |
|------|----------|----------|----------|----------|
| 1. 普通 HTTP | 非 CONNECT | `HTTP` | 单层 | 是 |
| 2. 隧道透传 | `ConnectAccept` | `HTTPS-Tunnel` | 父隧道 | 否 |
| 3. HTTPS MITM | `ConnectMitm` | `HTTPS-MITM` | 父隧道+子连接 | 是 |
| 4. HTTP MITM (CONNECT) | `ConnectHTTPMitm` | `HTTP-MITM` | 父隧道+子连接 | 是 |
| 5. HTTP MITM 引擎 | `HttpMitmNoTunnel=true` | `HTTP-MITM` | 虚拟隧道+子连接 | 是 |

### 1. 普通 HTTP 转发
**位置**：`mproxy/http.go:15-168` - `MyHttpHandle`

**流程**：创建 Pcontext → filterRequest → RoundTrip → filterResponse → 写回响应

**路由**：`ctx.RoundTripper` → `RouterRoundTripper.RoundTrip` → `router.RouteDial`

### 2. 隧道透传模式
**位置**：`mproxy/https.go:346-428` - `ConnectAccept` 动作

**流程**：回复 200 → `connectDial()` → 双向 io.Copy 转发

**路由**：`connectDial()` → `proxy.ConnectWithReqDial` → `router.RouteDial` (`https.go:152-159`)

**特点**：无法解密，支持 HTTP/2、WebSocket

### 3. HTTPS MITM 模式
**位置**：`mproxy/https.go:672-900` - `ConnectMitm` 动作

**流程**：TLS 握手 → RequestReader 循环 → filterRequest → `ctxt.RoundTrip()` → 手动写回响应

**路由**：`ctxt.RoundTrip()` → `ctx.RoundTripper` → `RouterRoundTripper`

### 4. HTTP MITM 模式（CONNECT 触发）
**位置**：`mproxy/https.go:433-670` - `ConnectHTTPMitm` 动作

**流程**：200 OK → RequestReader 循环 → filterRequest → `ctxt.RoundTrip()` → 手动写回

**路由**：`ctxt.RoundTrip()` → `ctx.RoundTripper` → `RouterRoundTripper`

**特点**：复用 TCP 连接（Keep-Alive），支持分块编码

### 5. HTTP MITM 引擎模式（http.go 特有）
**位置**：`mproxy/http.go:170-415` - `myHttpHandleWithEngine`

**触发条件**：`HttpMitmNoTunnel=true`

**流程**：Hijack → 虚拟隧道 → RequestReader 循环 → filterRequest → `ctxt.RoundTrip()` → 手动写回

**路由**：子 Pcontext 继承 `topctx.RoundTripper` → `RouterRoundTripper`

---

## 流量统计模块

**装饰器模式**包装 `io.ReadCloser` 实现零侵入计数。

**TrafficCounter** (`mproxy/https_traffic.go`):
```go
type TrafficCounter struct {
    req_header, req_body, resp_header, resp_body int64
    req_sum, resp_sum, total int64
}
```

**全局计数器**：`GlobalTrafficUp`、`GlobalTrafficDown`（atomic.Int64）

**工作原理**：
1. 请求阶段：计算请求头大小，包装 `req.Body` 为 `reqBodyReader`
2. 响应阶段：计算响应头大小，包装 `resp.Body` 为 `respBodyReader`，设置 onClose 回调
3. MITM 模式：子连接流量累加到父隧道（通过 `Pcounter` 引用）

---

## 连接统计模块

**ConnectionInfo** (`mproxy/connections.go`):
```go
type ConnectionInfo struct {
    Session, ParentSess int64       // 会话 ID、父连接 ID
    Host, Method, URL, RemoteAddr string
    Protocol string                // HTTP/HTTPS-Tunnel/HTTPS-MITM/HTTP-MITM
    StartTime, EndTime time.Time
    Status string                   // "Active" / "Closed"
    UploadRef, DownloadRef *int64   // 流量计数器引用
    PuploadRef, PdownloadRef *int64 // 父隧道流量引用
}
```

**连接层级关系**：
```
顶层隧道 (Session=1, ParentSess=0)
  ├── 子请求 1 (Session=2, ParentSess=1)
  ├── 子请求 2 (Session=3, ParentSess=1)
```

**清理机制**：墓碑标记 `Closed` 状态，保留 2 秒后物理删除

---

## 日志统计模块

**LogMessage** (`mproxy/logs.go`):
```go
type LogMessage struct {
    Level   string    // INFO/WARN/ERROR/DEBUG
    Session int64     // 会话 ID（取低 16 位）
    Message string
    Time    time.Time
}
```

**日志格式**：`[XXX] LEVEL: 消息内容`

**上下文日志方法**：
```go
ctx.Log_P(msg, argv...)  // 受 Verbose 控制
ctx.WarnP(msg, argv...)  // 警告日志
```

**推送机制**：批量推送（每 300ms 或 200 条），支持级别过滤

---

## MinIO 模块

仅在 `MitmEnabled=true` 时生效，捕获请求/响应 Body 存储。

**Config** (`myminio/config.go`):
```go
type Config struct {
    Endpoint, AccessKeyID, SecretAccessKey, Bucket string
    UseSSL, Enabled bool
}
```

**对象 Key 格式**：`mitm-data/YYYY-MM-DD/SessionID/bodyType`

**跳过捕获**：`text/event-stream`、`websocket`、`multipart/x-mixed-replace`

**上传策略**：
- 已知大小（contentLength >= 0）：直接流式上传
- 未知大小：通过临时文件上传（避免分片过多）

**双层包装**：`req.Body` → `reqBodyReader`（流量） → `bodyCaptReader`（MinIO）

---

## WebSocket 实时推送

**推送主题**：

| 主题 | 内容 | 频率 |
|------|------|------|
| `traffic` | 全局流量统计 | 每 1 秒 |
| `connections` | 活跃连接列表 | 每 500ms |
| `logs` | 日志消息 | 实时批量 |
| `mitm_detail` | MITM Exchange | 实时批量 |

**客户端订阅**：
```json
{"action": "subscribe", "topics": ["traffic", "connections", "logs", "mitm_detail"], "logLevel": "INFO"}
```

**WebSocket 支持**：检测握手响应（`Connection: upgrade`, `Upgrade: websocket`）→ Hijack → `proxyWebsocket()` 双向转发

---

## 核心配置项

| 配置项 | 默认值 | 说明 |
|--------|--------|------|
| `Verbose` | `true` | 详细日志 |
| `MitmEnabled` | `false` | MITM 全局开关 |
| `HttpMitmNoTunnel` | `false` | HTTP 使用 TCP 转发引擎 |
| `RouteEnable` | `false` | 启用路由规则代理 |
| `ConnectMaintain` | `false` | 维持长连接 |
| `KeepDestHeaders` | `true` | 保留响应头 |

---

## MITM Exchange 模块

捕获完整 HTTP 请求/响应信息，WebSocket 推送。

**HttpExchange** (`mproxy/mitm_exchange.go`):
```go
type HttpExchange struct {
    ID, SessionID, ParentID, Time int64
    Request   RequestSnapshot
    Response  ResponseSnapshot
    Duration  int64
    Error     string
}
```

**流程**：`StartCapture()` → `CaptureRequest()` → `SetCaptureSkip()`（WebSocket 跳过）→ `SendExchange()`（响应完成触发）

**推送机制**：批量推送（每 500ms 或 100 条），消息类型 `mitm_exchange_batch`

---

## 注意事项

1. **MitmEnabled 依赖**：MinIO、MITM Exchange 需要开启
2. **证书信任**：HTTPS MITM 需要客户端信任代理 CA 证书
3. **墓碑机制**：连接关闭后保留 2 秒
4. **批量推送**：避免高负载时卡死
5. **父子流量**：MITM 子连接流量累加到父隧道
