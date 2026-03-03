# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 项目概述

**proxy_man** 是一个基于 Go 语言实现的高性能 HTTP/HTTPS 代理服务器，基于 `github.com/elazarl/goproxy` v1.7.2 扩展，支持 MITM（中间人）模式、WebSocket、流量统计、MinIO 存储等功能。

## 常用命令

### 构建和运行
```bash
# 构建主程序
go build -o proxy_man main.go

# 运行代理服务器（默认端口 8080，开启详细日志）
./proxy_man -v

# 自定义端口
./proxy_man -addr :8888 -v

# 关闭详细日志
./proxy_man
```

### pprof 性能分析
服务器启动后自动在 6060 端口提供 pprof 服务：
```
http://localhost:6060/debug/pprof/
```

### WebSocket 控制接口
- WebSocket 连接：`ws://localhost:8000/start?token=123`
- MinIO 下载 API：`http://localhost:8000/api/storage/download?key=<object_key>`

---

## 核心架构

### 责任链模式

项目使用责任链模式实现请求/响应过滤，核心入口在 `CoreHttpServer.ServeHTTP`：

```
ServeHTTP (判断 HTTP/HTTPS)
    ↓
HTTP 请求 → MyHttpHandle → filterRequest (reqHandlers) → RoundTrip → filterResponse (respHandlers) → io.Copy
HTTPS 请求 → MyHttpsHandle → Connect 处理 → MITM 或隧道模式
```

**关键代码位置**：`mproxy/core_proxy.go:91-111`

```go
// 请求过滤责任链
func (proxy *CoreHttpServer) filterRequest(r *http.Request, ctx *Pcontext) (req *http.Request, resp *http.Response) {
    req = r
    for _, h := range proxy.reqHandlers {
        req, resp = h.Handle(req, ctx)
        if resp != nil { break } // 提前返回响应
    }
    return
}

// 响应过滤责任链
func (proxy *CoreHttpServer) filterResponse(respOrig *http.Response, ctx *Pcontext) (resp *http.Response) {
    resp = respOrig
    for _, h := range proxy.respHandlers {
        ctx.Resp = resp
        resp = h.Handle(resp, ctx)
    }
    return
}
```

### 关键组件

| 文件 | 职责 |
|------|------|
| `mproxy/core_proxy.go` | `CoreHttpServer` 主结构体，管理全局会话计数器、连接存储、过滤器链 |
| `mproxy/http.go` | HTTP 请求处理逻辑（普通模式 + HTTP-MITM 引擎模式） |
| `mproxy/https.go` | HTTPS/CONNECT 处理逻辑（约 910 行），包含 4 种转发模式 |
| `mproxy/hooks.go` | Hook 机制和条件过滤器（`ReqCondition`, `RespCondition`） |
| `mproxy/actions.go` | Handler 接口定义和便捷函数 |
| `mproxy/ctxt.go` | `Pcontext` 请求上下文，存储请求/响应状态和流量计数器 |
| `mproxy/websocket.go` | WebSocket 支持和连接劫持 |
| `mproxy/connections.go` | 连接统计模块 |
| `mproxy/https_traffic.go` | HTTP/MITM 流量统计模块 |
| `mproxy/tunnel_traffic.go` | 隧道模式流量统计 |
| `mproxy/logs.go` | 日志统计模块 |
| `mproxy/mitm_exchange.go` | MITM Exchange 捕获和推送 |
| `proxysocket/hub.go` | WebSocket 实时推送中心 |
| `myminio/` | MinIO 对象存储模块 |
| `signer/` | 动态证书生成（MITM 模式） |
| `http1parser/` | HTTP/1 协议解析器 |

---

## Hook 机制

Hook 机制是代理的核心扩展点，允许在请求/响应生命周期的各个阶段注入自定义逻辑。

### 请求 Hook

修改请求或提前返回响应：

```go
proxy.HookOnReq().DoFunc(func(req *http.Request, ctx *mproxy.Pcontext) (*http.Request, *http.Response) {
    // 返回 nil, nil 表示继续转发
    // 返回 resp != nil 表示直接返回该响应
    return req, nil
})
```

### CONNECT Hook（HTTPS 连接策略）

控制 HTTPS 连接行为，决定使用哪种转发模式：

```go
proxy.HookOnReq().DoConnectFunc(func(host string, ctx *mproxy.Pcontext) (*mproxy.ConnectAction, string) {
    return mproxy.MitmConnect, host       // HTTPS MITM 模式
    // return mproxy.HTTPMitmConnect, host  // HTTP MITM 模式
    // return mproxy.OkConnect, host         // 隧道透传模式
})
```

### 条件过滤器

```go
// URL 精确匹配
proxy.HookOnReq(mproxy.UrlHook("/api/user")).DoFunc(...)

// URL 正则匹配
proxy.HookOnReq(mproxy.UrlRegHook(`^https://.*\.com/.*`)).DoFunc(...)

// 响应 Content-Type 匹配
proxy.HookOnResp(mproxy.ContentTypeHook("application/json")).DoFunc(...)

// 双层条件：请求 + 响应
proxy.HookOnResp().OnRespByReq(mproxy.UrlHook("/api")).DoFunc(...)
```

**条件过滤器执行逻辑**（`mproxy/hooks.go:64-98`）：
1. 先检查所有 `reqConds`，任一不满足则跳过
2. 再检查所有 `respConds`，任一不满足则跳过
3. 所有条件满足后执行用户函数

---

## 四大转发模式

### 1. 普通 HTTP 转发

**文件位置**：`mproxy/http.go:17-170`

**流程**：
1. 检查 URL 是否为绝对路径
2. 创建 `Pcontext`，注册连接
3. 执行 `filterRequest`
4. `RoundTrip` 发起请求
5. 执行 `filterResponse`
6. 写回响应头和 Body

**协议类型标识**：`HTTP`

**连接注册位置**：`mproxy/http.go:44-56`

### 2. 隧道透传模式（HTTPS Tunnel）

**文件位置**：`mproxy/https.go:346-428`

**触发条件**：`ConnectAccept` 动作

**流程**：
1. 回复客户端 `200 Connection established`
2. 拨号连接目标服务器
3. 使用 `tunnelTrafficClient` 包装客户端连接进行流量统计
4. 双向 `io.Copy` 转发 TCP 数据流

**协议类型标识**：`HTTPS-Tunnel`

**连接注册位置**：`mproxy/https.go:380-393`

**特点**：
- 无法解密内容，直接透传 TCP 字节流
- 流量统计在 TCP 层进行（nread/nwrite）
- 支持 HTTP/2、WebSocket 等所有协议

### 3. HTTPS MITM 模式

**文件位置**：`mproxy/https.go:672-900`

**触发条件**：`ConnectMitm` 动作

**流程**：
1. 回复客户端 `200 OK`
2. 使用动态生成的 TLS 证书与客户端完成 TLS 握手
3. 循环读取解密后的 HTTP 请求
4. 对每个请求创建独立的子 `Pcontext`（`parCtx` 指向顶层隧道）
5. 执行 `filterRequest` → `RoundTrip` → `filterResponse`
6. 手动写回响应头和 Body
7. 支持 1xx 中间状态响应（100 Continue）

**协议类型标识**：`HTTPS-MITM`（子连接）

**连接注册位置**：
- 隧道层：`mproxy/https.go:295-308`
- 子请求：`mproxy/https.go:734-749`

**证书生成**：`signer/signer.go`，使用内置 CA 证书（`mproxy/tls_cert.go`）

### 4. HTTP MITM 模式

**文件位置**：`mproxy/https.go:433-670`（CONNECT 触发）或 `mproxy/http.go:172-434`（`HttpMitmNoTunnel` 模式）

**触发条件**：`ConnectHTTPMitm` 动作

**流程**：
1. 回复客户端 `200 OK`
2. 使用 `RequestReader` 循环读取 HTTP 请求
3. 对每个请求创建独立的子 `Pcontext`
4. 复用与目标服务器的 TCP 连接（HTTP/1.1 Keep-Alive）
5. 支持 1xx 中间状态响应处理（全双工模式）
6. 手动写回响应头和 Body

**协议类型标识**：`HTTP-MITM`

**连接注册位置**：`mproxy/https.go:480-495`

**特点**：
- 用于 80 端口 CONNECT 请求或纯 HTTP 站点
- 复用目标连接，减少握手开销
- 支持分块编码（Chunked）传输

### 四大模式对比

| 模式 | 触发条件 | 协议类型 | 连接层级 | 流量统计 | 内容可读 |
|------|----------|----------|----------|----------|----------|
| 普通 HTTP | 非 CONNECT 请求 | HTTP | 单层 | req_body/resp_body | 是 |
| 隧道透传 | `ConnectAccept` | HTTPS-Tunnel | 父隧道 | nread/nwrite | 否 |
| HTTPS MITM | `ConnectMitm` | HTTPS-MITM | 父隧道+子连接 | req_body/resp_body | 是 |
| HTTP MITM | `ConnectHTTPMitm` | HTTP-MITM | 父隧道+子连接 | req_body/resp_body | 是 |

---

## 流量统计模块

流量统计通过装饰器模式包装 `io.ReadCloser`，实现零侵入式流量计数。

### 数据结构

**TrafficCounter** (`mproxy/https_traffic.go:15-23`)
```go
type TrafficCounter struct {
    resp_header int64  // 远程->代理 头部大小
    resp_body   int64  // 远程->代理 Body大小
    req_header  int64  // 客户端->代理 头部大小
    req_body    int64  // 客户端->代理 Body大小
    req_sum     int64  // 请求总计 (req_header + req_body)
    resp_sum    int64  // 响应总计 (resp_header + resp_body)
    total       int64  // 总计 (req_sum + resp_sum)
}
```

**全局流量计数器** (`mproxy/https_traffic.go:10-13`)
```go
var (
    GlobalTrafficUp   atomic.Int64  // 累计上行流量（所有请求）
    GlobalTrafficDown atomic.Int64  // 累计下行流量（所有请求）
)
```

### 工作原理

**请求阶段** (`mproxy/actions.go:14-54`)
```go
proxy.HookOnReq().DoFunc(func(req *http.Request, ctx *mproxy.Pcontext) (*http.Request, *http.Response) {
    // 1. 计算请求头大小
    ctx.TrafficCounter.req_header = GetHeaderSize(req, ctx)
    GlobalTrafficUp.Add(ctx.TrafficCounter.req_header)

    // 2. 支持父子连接流量统计（MITM 模式）
    if ctx.parCtx != nil {
        ctx.parCtx.TrafficCounter.req_sum += ctx.TrafficCounter.req_header
    }

    // 3. 包装请求体
    if req.Body != nil {
        req.Body = &reqBodyReader{
            ReadCloser: req.Body,
            counter:    ctx.TrafficCounter,
            Pcounter:   ctx.parCtx != nil ? ctx.parCtx.TrafficCounter : nil,
        }
    }
    return req, nil
})
```

**响应阶段** (`mproxy/actions.go:56-128`)
```go
proxy.HookOnResp().DoFunc(func(resp *http.Response, ctx *mproxy.Pcontext) *http.Response {
    // 1. 计算响应头大小
    ctx.TrafficCounter.resp_header = GetHeaderSize(resp, ctx)
    GlobalTrafficDown.Add(ctx.TrafficCounter.resp_header)

    // 2. 包装响应体，设置 onClose 回调
    if resp.Body != nil {
        resp.Body = &respBodyReader{
            ReadCloser: resp.Body,
            counter:    ctx.TrafficCounter,
            Pcounter:   ctx.parCtx != nil ? ctx.parCtx.TrafficCounter : nil,
            onClose: func() {
                ctx.TrafficCounter.UpdateTotal()
                ctx.Log_P("[流量统计] ...")
                ctx.SendExchange() // 触发 MITM Exchange 发送
            },
        }
    }
    return resp
})
```

### Body 包装器

**reqBodyReader** (`mproxy/https_traffic.go:26-46`)
```go
func (r *reqBodyReader) Read(p []byte) (n int, err error) {
    n, err = r.ReadCloser.Read(p)
    r.counter.req_sum += int64(n)
    r.counter.req_body += int64(n)
    GlobalTrafficUp.Add(int64(n))
    if r.Pcounter != nil {
        r.Pcounter.req_sum += int64(n)  // 累加到父隧道
    }
    return n, err
}
```

**respBodyReader** (`mproxy/https_traffic.go:50-74`)
```go
func (r *respBodyReader) Read(p []byte) (n int, err error) {
    n, err = r.ReadCloser.Read(p)
    r.counter.resp_sum += int64(n)
    r.counter.resp_body += int64(n)
    GlobalTrafficDown.Add(int64(n))
    if r.Pcounter != nil {
        r.Pcounter.resp_sum += int64(n)  // 累加到父隧道
    }
    return n, err
}

func (r *respBodyReader) Close() error {
    if r.onClose != nil {
        r.onClose()  // 触发回调，输出统计结果
        r.onClose = nil
    }
    return r.ReadCloser.Close()
}
```

### 隧道模式流量统计

**tunnelTrafficClient** (`mproxy/tunnel_traffic.go:8-47`)
```go
func (r *tunnelTrafficClient) Read(p []byte) (n int, err error) {
    n, err = r.halfClosable.Read(p)
    r.nread += int64(n)
    GlobalTrafficUp.Add(int64(n))  // 客户端连接读取 = 上行流量
    return n, err
}

func (w *tunnelTrafficClient) Write(p []byte) (n int, err error) {
    n, err = w.halfClosable.Write(p)
    w.nwrite += int64(n)
    GlobalTrafficDown.Add(int64(n))  // 客户端连接写入 = 下行流量
    return n, err
}
```

**统计输出位置**：`mproxy/https.go:395`（通过 `tunnelMonitor` 注册回调）

---

## 连接统计模块

连接统计模块追踪所有活跃代理连接，支持层级显示和实时推送。

### 数据结构

**ConnectionInfo** (`mproxy/connections.go:5-21`)
```go
type ConnectionInfo struct {
    Session     int64     `json:"id"`        // 会话ID（唯一标识）
    ParentSess  int64     `json:"parentId"`  // 父连接Session（MITM 子连接指向父隧道）
    Host        string    `json:"host"`      // 目标主机
    Method      string    `json:"method"`    // HTTP方法
    URL         string    `json:"url"`       // 完整URL
    RemoteAddr  string    `json:"remote"`    // 客户端远程地址
    Protocol    string    `json:"protocol"`  // 协议类型：HTTP/HTTPS-Tunnel/HTTPS-MITM/HTTP-MITM
    StartTime   time.Time `json:"startTime"` // 连接开始时间
    Status      string    `json:"status"`    // "Active" 或 "Closed"
    EndTime     time.Time `json:"endTime"`   // 连接关闭时间
    UploadRef   *int64    `json:"-"`         // 上行流量计数器引用
    DownloadRef *int64    `json:"-"`         // 下行流量计数器引用
    PuploadRef  *int64    `json:"-"`         // 父隧道上行引用（MITM 模式）
    PdownloadRef *int64    `json:"-"`        // 父隧道下行引用（MITM 模式）
    OnClose     func()    `json:"-"`         // 关闭回调
}
```

**连接存储** (`mproxy/core_proxy.go:42`)
```go
type CoreHttpServer struct {
    Connections sync.Map  // 键：Session int64，值：*ConnectionInfo
    sess        int64     // 会话ID计数器（原子递增）
    // ...
}
```

### 会话ID生成

```go
atomic.AddInt64(&proxy.sess, 1)
```

### 连接层级关系

```
顶层隧道 (Session=1, ParentSess=0)
  ├── 子请求 1 (Session=2, ParentSess=1)  - HTTPS-MITM
  ├── 子请求 2 (Session=3, ParentSess=1)  - HTTPS-MITM
  └── 子请求 3 (Session=4, ParentSess=1)  - HTTPS-MITM
```

- **单层连接**：普通 HTTP 转发，`ParentSess = 0`
- **两层连接**：MITM 模式，父隧道 + 子请求

### 连接注册位置

| 场景 | 文件位置 | 协议类型 |
|------|----------|----------|
| HTTP 请求 | `http.go:44-56` | `HTTP` |
| HTTPS 隧道模式 | `https.go:295-308` | `TUNNEL` + `HTTPS-Tunnel` |
| HTTPS MITM | `https.go:734-749` | `HTTPS-MITM`（子连接） |
| HTTP MITM | `https.go:480-495` | `HTTP-MITM`（子连接） |

### 连接清理机制

1. **墓碑标记**：连接关闭时标记为 `Closed` 状态，保留 2 秒用于 UI 显示
2. **物理删除**：超过保留时间后从 `sync.Map` 中删除
3. **清理代码**：`proxysocket/hub.go:137-140`

---

## 日志统计模块

日志统计提供统一的日志记录和实时推送功能。

### 数据结构

**LogMessage** (`mproxy/logs.go:16-21`)
```go
type LogMessage struct {
    Level   string    `json:"level"`   // 日志级别: INFO/WARN/ERROR/DEBUG
    Session int64     `json:"session"` // 会话ID（取低16位）
    Message string    `json:"message"` // 日志内容
    Time    time.Time `json:"time"`    // 时间戳
}
```

**LogCollector** (`mproxy/logs.go:27-33`)
```go
type LogCollector struct {
    Underlying Logger  // 原始Logger（输出到stderr）
}
```

**全局日志通道** (`mproxy/logs.go:24`)
```go
var LogChan = make(chan LogMessage, 1000)  // 缓冲通道，避免阻塞
```

### 日志格式

```
[XXX] LEVEL: 消息内容
```

- `[XXX]`: 3位会话ID（Session 取低16位）
- `LEVEL`: INFO/WARN/ERROR/DEBUG

### 日志记录机制

```go
func (l *LogCollector) Printf(format string, v ...any) {
    msg := fmt.Sprintf(format, v...)
    level, session, payload := ParseLogMessage(msg)

    // 非阻塞发送到 Channel
    select {
    case LogChan <- LogMessage{Level: level, Session: session, Message: payload, Time: time.Now()}:
    default:
        // Channel 满时丢弃，避免阻塞主流程
    }

    // 同时输出到原始 Logger
    l.Underlying.Printf(format, v...)
}
```

### 上下文日志方法

```go
// 公有日志方法 - 受 Verbose 控制 (ctxt.go:59-63)
func (ctx *Pcontext) Log_P(msg string, argv ...any) {
    if ctx.core_proxy.Verbose == true {
        ctx.printf("INFO: "+msg, argv...)
    }
}

// 警告日志 (ctxt.go:55-57)
func (ctx *Pcontext) WarnP(msg string, argv ...any) {
    ctx.printf("WARN: "+msg, argv...)
}
```

### 日志级别过滤

```go
var logLevels = map[string]int{"DEBUG": 0, "INFO": 1, "WARN": 2, "ERROR": 3}

func shouldSendLog(msgLevel, clientLevel string) bool {
    return logLevels[msgLevel] >= logLevels[clientLevel]
}
```

### 日志推送

**批量推送机制** (`proxysocket/hub.go:173-203`)：
- 每 300ms 或批量达到 200 条时推送
- 支持客户端级别过滤
- 使用 JSON 编码，禁用 HTML 转义

---

## MinIO 模块

MinIO 模块提供请求/响应 Body 的对象存储功能，仅在 `MitmEnabled=true` 时生效。

### 配置结构

**Config** (`myminio/config.go:12-20`)
```go
type Config struct {
    Endpoint        string  // MinIO 服务器地址
    AccessKeyID     string  // 访问密钥
    SecretAccessKey string  // 秘密密钥
    UseSSL          bool    // 是否使用 SSL
    Bucket          string  // 存储桶名称
    Enabled         bool    // 是否启用
}
```

**全局客户端** (`myminio/config.go:28-29`)
```go
var GlobalClient *Client
```

### Body 捕获机制

**BodyCapture** (`myminio/storage_api.go:14-21`)
```go
type BodyCapture struct {
    ObjectKey   string // MinIO 对象 Key
    Size        int64  // 上传后的实际大小
    Uploaded    bool   // 是否成功上传
    ContentType string // 内容类型
    Error       error  // 上传过程中的错误
}
```

**bodyCaptReader** (`myminio/storage_api.go:24-31`)
```go
type bodyCaptReader struct {
    inner         io.ReadCloser  // 内层 Reader（流量统计层）
    pipeWriter    *io.PipeWriter // 用于向上传协程传输数据
    Capture       *BodyCapture   // 捕获状态
    doneCh        chan struct{}  // 上传完成信号
    skipUpload    bool           // 是否跳过捕获
    contentLength int64          // HTTP Content-Length
}
```

### 上传策略

**已知大小**（contentLength >= 0）：直接流式上传
```go
info, err := GlobalClient.PutObjectWithSize(ctx, key, pr, contentLength, contentType)
```

**未知大小**（contentLength < 0）：通过临时文件上传
1. 写入临时文件 `myminio/tmp/upload-*.tmp`
2. 获取实际文件大小
3. 使用已知大小上传到 MinIO
4. 清理临时文件

**原因**：避免大文件分片过多（S3 标准最多 10000 个分片）

### 对象 Key 格式

```
mitm-data/YYYY-MM-DD/SessionID/bodyType
```

示例：`mitm-data/2026-02-04/10086/req`

### 跳过捕获的内容类型

```go
skipTypes := []string{
    "text/event-stream",         // SSE
    "websocket",                 // WebSocket
    "multipart/x-mixed-replace", // 流式响应
}
```

### 与流量统计的集成

**双层包装** (`mproxy/actions.go:36-51`)
```
原始 req.Body
    ↓
reqBodyReader (流量统计)
    ↓
bodyCaptReader (MinIO 捕获)
    ↓
RoundTrip
```

### 下载 API

**端点**：`GET /api/storage/download?key=<object_key>`

**响应**：
```json
{
  "code": 0,
  "message": "success",
  "data": {
    "downloadUrl": "https://...",
    "expiresAt": "2026-02-04T12:00:00Z",
    "filename": "10086_req.bin",
    "size": 1024
  }
}
```

---

## MITM Exchange 模块

MITM Exchange 模块捕获完整的 HTTP 请求/响应信息，并通过 WebSocket 推送给前端。

### 数据结构

**HttpExchange** (`mproxy/mitm_exchange.go:12-22`)
```go
type HttpExchange struct {
    ID        int64            // 唯一递增ID
    SessionID int64            // 会话ID
    ParentID  int64            // 父连接ID（MITM 模式）
    Time      int64            // 请求时间戳
    Request   RequestSnapshot  // 请求快照
    Response  ResponseSnapshot // 响应快照
    Duration  int64            // 耗时（毫秒）
    Error     string           // 错误信息
}
```

**RequestSnapshot** (`mproxy/mitm_exchange.go:24-36`)
```go
type RequestSnapshot struct {
    Method  string              // HTTP 方法
    URL     string              // 完整 URL
    Host    string              // 主机
    Header  map[string][]string // 请求头
    SumSize int64               // 总大小
    BodyKey      string         // MinIO 对象 Key
    BodySize     int64          // Body 实际大小
    BodyUploaded bool           // 是否成功上传
    ContentType  string         // Content-Type
    BodyError    string         // MinIO 上传错误
}
```

### 捕获流程

1. **初始化**：`StartCapture(parentSession)` - 创建 `ExchangeCapture`
2. **请求捕获**：`CaptureRequest(req)` - 保存请求快照
3. **跳过标记**：`SetCaptureSkip()` - WebSocket 跳过捕获
4. **发送 Exchange**：`SendExchange()` - 响应完成后触发

**触发位置**：`respBodyReader.Close()` → `onClose()` → `ctx.SendExchange()`

### 全局通道

```go
var GlobalExchangeChan = make(chan *HttpExchange, 1000)
```

### 推送机制

**批量推送** (`proxysocket/hub.go:272-307`)：
- 每 500ms 或批量达到 100 条时推送
- 消息类型：`mitm_exchange_batch`
- 主题：`mitm_detail`

---

## WebSocket 实时推送

### 推送主题

| 主题 | 内容 | 推送频率 | 触发条件 |
|------|------|----------|----------|
| `traffic` | 全局流量统计 | 每 1 秒 | 定时器 |
| `connections` | 活跃连接列表 | 每 500ms | 定时器 |
| `logs` | 日志消息 | 实时批量 | 有日志时 |
| `mitm_detail` | MITM Exchange | 实时批量 | 有 Exchange 时 |

### 订阅配置

```go
type Subscription struct {
    Traffic     bool   // 流量统计
    Connections bool   // 连接信息
    Logs        bool   // 日志推送
    LogLevel    string // 日志级别过滤
    MitmDetail  bool   // MITM 详细信息
}
```

### 客户端消息格式

**订阅主题**：
```json
{
  "action": "subscribe",
  "topics": ["traffic", "connections", "logs", "mitm_detail"],
  "logLevel": "INFO"
}
```

**关闭所有连接**：
```json
{
  "action": "closeAllConnections"
}
```

### 线程安全

- 使用 `sync.Mutex` 保护 WebSocket 写操作
- 写入失败时自动删除死连接
- 批量推送避免 goroutine 风暴

---

## 请求上下文（Pcontext）

**Pcontext** (`mproxy/ctxt.go:12-31`) 是单个请求的上下文对象，贯穿整个请求生命周期。

### 核心字段

```go
type Pcontext struct {
    core_proxy     *CoreHttpServer        // 代理服务器引用
    Req            *http.Request          // 请求对象
    Resp           *http.Response         // 响应对象
    RoundTripper   RoundTripper           // 自定义 RoundTrip
    UserData       any                    // 用户数据
    Session        int64                  // 会话ID

    parCtx         *Pcontext              // 父上下文（MITM 模式）
    TrafficCounter *TrafficCounter        // HTTP/MITM 流量统计
    tunnelTrafficClient *tunnelTrafficClient  // 隧道流量统计

    Dialer         func(ctx context.Context, network, addr string) (net.Conn, error)
    exchangeCapture *ExchangeCapture      // MITM Exchange 捕获状态
}
```

### 父子上下文关系

**MITM 模式**：
```
顶层隧道 Pcontext (Session=1)
  parCtx = nil
  TrafficCounter 统计整个隧道的流量

  子请求 Pcontext (Session=2)
  parCtx = 顶层隧道 Pcontext
  TrafficCounter 统计单个请求流量
  同时累加到 parCtx.TrafficCounter
```

### 自定义 RoundTrip

```go
func (ctx *Pcontext) RoundTrip(req *http.Request) (*http.Response, error) {
    if ctx.RoundTripper != nil {
        return ctx.RoundTripper.RoundTrip(req, ctx)
    }
    return ctx.core_proxy.Transport.RoundTrip(req)
}
```

---

## 核心配置项

**CoreHttpServer 配置** (`main.go:19-27`)

| 配置项 | 默认值 | 说明 |
|--------|--------|------|
| `Verbose` | `true` | 是否输出详细日志 |
| `AllowHTTP2` | `false` | 是否允许 HTTP/2 |
| `KeepAcceptEncoding` | `false` | 是否保留 Accept-Encoding 头 |
| `PreventParseHeader` | `false` | 是否保留非标头部 |
| `KeepDestHeaders` | `true` | 是否保留响应头 |
| `ConnectMaintain` | `false` | 是否维持长连接 |
| `MitmEnabled` | `false` | MITM 全局开关 |
| `HttpMitmNoTunnel` | `false` | HTTP 使用 TCP 转发引擎 |

---

## 连接模式选择逻辑

**MITM 开启时的默认策略** (`mproxy/https.go:325-333`)
```go
if proxy.MitmEnabled {
    _, port, _ := net.SplitHostPort(host)
    if port == "80" {
        strategy = HTTPMitmConnect  // 80 端口使用 HTTP MITM
    } else {
        strategy = MitmConnect      // 其他端口使用 HTTPS MITM
    }
}
```

**httpsHandlers 可覆盖默认值**：
```go
proxy.HookOnReq().DoConnectFunc(func(host string, ctx *Pcontext) (*ConnectAction, string) {
    // 自定义逻辑
    return MitmConnect, host
})
```

---

## WebSocket 支持

### 检测

```go
func isWebSocketHandshake(header http.Header) bool {
    return headerContains(header, "Connection", "Upgrade") &&
           headerContains(header, "Upgrade", "websocket")
}
```

### 处理流程

1. 检测 WebSocket 握手响应（`Connection: upgrade`, `Upgrade: websocket`）
2. Hijack 获取原始 TCP 连接
3. 写回 101 响应头
4. 使用 `proxyWebsocket()` 双向转发

**文件位置**：`mproxy/websocket.go`、`mproxy/http.go:79-127`、`mproxy/https.go:835-850`

---

## 常见开发任务

### 添加新的 Hook

```go
// 拦截特定域名的请求
proxy.HookOnReq(mproxy.UrlRegHook(`\.example\.com`)).DoFunc(func(req *http.Request, ctx *mproxy.Pcontext) (*http.Request, *http.Response) {
    // 修改请求
    req.Header.Set("X-Custom-Header", "value")
    return req, nil
})

// 修改响应内容
proxy.HookOnResp(mproxy.ContentTypeHook("application/json")).DoFunc(func(resp *http.Response, ctx *mproxy.Pcontext) *http.Response {
    // 修改响应
    resp.Header.Set("X-Proxy", "proxy_man")
    return resp
})
```

### 切换 MITM 模式

```go
// 开启 HTTPS MITM（所有端口）
mproxy.HttpsMitmMode(proxy)

// 开启 HTTP MITM（80 端口）
mproxy.HttpMitmMode(proxy)

// 隧道模式
mproxy.TunnelMode(proxy)
```

### 启用 MinIO 存储

```go
minioConfig := myminio.Config{
    Endpoint:        "127.0.0.1:9000",
    AccessKeyID:     "root",
    SecretAccessKey: "12345678",
    UseSSL:          false,
    Bucket:          "bodydata",
    Enabled:         true,
}
client, err := myminio.NewClient(minioConfig)
if err != nil {
    log.Printf("MinIO 初始化失败: %v", err)
} else {
    myminio.GlobalClient = client
    proxy.MitmEnabled = true  // 必须开启 MITM
}
```

---

## 注意事项

1. **MitmEnabled 依赖**：MinIO 存储、MITM Exchange 推送等功能需要 `MitmEnabled=true`
2. **证书信任**：HTTPS MITM 模式需要客户端信任代理的 CA 证书
3. **墓碑机制**：连接关闭后保留 2 秒，避免 UI 中消失太快
4. **批量推送**：日志和 Exchange 使用批量推送，避免高负载时卡死
5. **父子流量统计**：MITM 模式下，子连接流量会累加到父隧道
