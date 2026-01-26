# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 项目概述

**proxy_man** 是一个基于 Go 语言实现的 HTTP/HTTPS 代理服务器，基于 `github.com/elazarl/goproxy` v1.7.2 扩展，支持 MITM（中间人）模式、WebSocket、流量统计等功能。

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

## 核心架构

### 责任链模式

项目使用责任链模式实现请求/响应过滤：

```
ServeHTTP (判断 HTTP/HTTPS)
    ↓
HTTP 请求 → MyHttpHandle → filterRequest (reqHandlers) → RoundTrip → filterResponse (respHandlers) → io.Copy
HTTPS 请求 → MyHttpsHandle → Connect 处理 → MITM 或隧道模式
```

### 关键组件

| 文件 | 职责 |
|------|------|
| `mproxy/core_proxy.go` | `CoreHttpServer` 主结构体，实现 `http.Handler` 接口，管理全局会话计数器和连接存储 |
| `mproxy/http.go` | HTTP 请求处理逻辑，处理明文 HTTP 请求 |
| `mproxy/https.go` | HTTPS/CONNECT 处理逻辑（约 620 行，核心文件），处理隧道模式和 MITM 模式 |
| `mproxy/hooks.go` | Hook 机制和条件过滤器（`ReqCondition`, `RespCondition`） |
| `mproxy/actions.go` | Handler 接口定义和便捷函数，包含流量监控注册（`AddTrafficMonitor()`） |
| `mproxy/ctxt.go` | `Pcontext` 请求上下文，存储请求/响应状态和流量计数器 |
| `mproxy/websocket.go` | WebSocket 支持和连接劫持 |
| `mproxy/connections.go` | 连接统计模块，定义 `ConnectionInfo` 结构 |
| `mproxy/https_traffic.go` | 流量统计模块，定义 `TrafficCounter`、`reqBodyReader`、`respBodyReader` |
| `mproxy/tunnel_traffic.go` | 隧道模式流量统计，定义 `tunnelTrafficClient` |
| `mproxy/logs.go` | 日志统计模块，定义 `LogCollector`、`LogMessage`、全局 `LogChan` |
| `proxysocket/hub.go` | WebSocket 实时推送中心，推送日志、连接、流量数据 |
| `signer/` | 动态证书生成（MITM 模式） |
| `http1parser/` | HTTP/1 协议解析器 |

### Hook 机制

**请求 Hook**：修改请求或提前返回响应
```go
proxy.HookOnReq().DoFunc(func(req *http.Request, ctx *mproxy.Pcontext) (*http.Request, *http.Response) {
    // 返回 nil, nil 表示继续转发
    // 返回 resp != nil 表示直接返回该响应
    return req, nil
})
```

**CONNECT Hook**：控制 HTTPS 连接行为（MITM/隧道）
```go
proxy.HookOnReq().DoConnectFunc(func(host string, ctx *mproxy.Pcontext) (*ConnectAction, string) {
    return MitmConnect, host    // 使用 MITM 模式
    // return HTTPMitmConnect, host  // 强制 HTTP 模式处理
    // return OkConnect, host  // 普通隧道模式
})
```

### 条件过滤器

```go
// URL 精确匹配
proxy.HookOnReq(UrlHook("/api/user")).DoFunc(...)

// URL 正则匹配
proxy.HookOnReq(UrlRegHook(`^https://.*\.com/.*`)).DoFunc(...)

// 响应 Content-Type 匹配
proxy.HookOnResp(ContentTypeHook("application/json")).DoFunc(...)

// 双层条件：请求 + 响应
proxy.HookOnResp().OnRespByReq(UrlHook("/api")).DoFunc(...)
```

## 流量统计模块

流量统计模块是代理服务器的核心监控功能，通过包装 `io.ReadCloser` 实现零侵入式流量计数。

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

流量统计通过装饰器模式包装原始的 `req.Body` 和 `resp.Body`，在每次 `Read()` 调用时累加字节数。

#### 请求阶段（actions.go:27-48）

```go
proxy.HookOnReq().DoFunc(func(req *http.Request, ctx *Pcontext) (*http.Request, *http.Response) {
    // 1. 计算请求头大小
    ctx.TrafficCounter.req_header = GetHeaderSize(req, ctx)
    GlobalTrafficUp.Add(ctx.TrafficCounter.req_header)

    // 2. 包装请求体
    if req.Body != nil {
        req.Body = &reqBodyReader{
            ReadCloser: req.Body,
            counter:    ctx.TrafficCounter,
        }
    }
    return req, nil
})
```

#### 响应阶段（actions.go:50-72）

```go
proxy.HookOnResp().DoFunc(func(resp *http.Response, ctx *Pcontext) *http.Response {
    // 1. 更新请求汇总
    ctx.TrafficCounter.UpdateReqSum()

    // 2. 计算响应头大小
    ctx.TrafficCounter.resp_header = GetHeaderSize(resp, ctx)
    GlobalTrafficDown.Add(ctx.TrafficCounter.resp_header)

    // 3. 包装响应体，设置 onClose 回调
    if resp.Body != nil {
        resp.Body = &respBodyReader{
            ReadCloser: resp.Body,
            counter:    ctx.TrafficCounter,
            onClose: func() {
                ctx.TrafficCounter.UpdateRespSum()
                ctx.TrafficCounter.UpdateTotal()
                // 输出流量统计日志
                ctx.Log_P("[流量统计] 上行: %d | 下行: %d | 总计: %d | %s | %s | %s",
                    ctx.TrafficCounter.req_sum, ctx.TrafficCounter.resp_sum, ctx.TrafficCounter.total,
                    ctx.Req.Method, ctx.Req.URL.String(), resp.Status)
            },
        }
    }
    return resp
})
```

### Body 包装器实现

**reqBodyReader** (`mproxy/https_traffic.go:26-41`)
```go
type reqBodyReader struct {
    io.ReadCloser  // 嵌入原始 req.Body
    counter *TrafficCounter
    onClose func()
}

func (r *reqBodyReader) Read(p []byte) (n int, err error) {
    n, err = r.ReadCloser.Read(p)
    r.counter.req_body += int64(n)
    GlobalTrafficUp.Add(int64(n))  // 实时累加全局上行
    return n, err
}
```

**respBodyReader** (`mproxy/https_traffic.go:45-64`)
```go
type respBodyReader struct {
    io.ReadCloser  // 嵌入原始 resp.Body
    counter *TrafficCounter
    onClose func()  // 关闭时的回调函数，用于输出统计结果
}

func (r *respBodyReader) Read(p []byte) (n int, err error) {
    n, err = r.ReadCloser.Read(p)
    r.counter.resp_body += int64(n)
    GlobalTrafficDown.Add(int64(n))  // 实时累加全局下行
    return n, err
}

func (r *respBodyReader) Close() error {
    if r.onClose != nil {
        r.onClose()  // 触发回调，输出流量统计
        r.onClose = nil
    }
    return r.ReadCloser.Close()
}
```

### 隧道模式流量统计

隧道模式下无法解密内容，直接在 TCP 层统计字节流：

**tunnelTrafficClient** (`mproxy/tunnel_traffic.go:8-45`)
```go
type tunnelTrafficClient struct {
    halfClosable  // 嵌入原始连接
    nread   int64
    nwrite  int64
    onUpdate func()
}

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

隧道模式的统计输出位置：`mproxy/https.go:269-277`

### 三种流量统计模式

| 模式 | 适用场景 | 统计对象 | 文件位置 |
|------|----------|----------|----------|
| HTTP | 明文 HTTP 请求 | req_body, resp_body | `http.go` + `https_traffic.go` |
| HTTPS-MITM | 中间人解密 HTTPS | req_body, resp_body | `https.go` (ConnectMitm) + `https_traffic.go` |
| HTTPS-Tunnel | 加密隧道透传 | nread, nwrite | `https.go` (ConnectAccept) + `tunnel_traffic.go` |

## MITM 模式

MITM 模式通过动态生成证书实现 HTTPS 内容拦截。相关代码：
- `signer/signer.go`：动态证书生成
- `mproxy/https.go`：CONNECT 请求处理
- `mproxy/tls_cert.go`：内置 CA 证书（`Proxy_ManCa`）

### 连接模式选择
通过 `ConnectAction` 常量控制连接行为：
- `OkConnect`：普通 HTTPS 隧道模式
- `MitmConnect`：TLS MITM 解密模式（需要客户端信任 CA 证书）
- `HTTPMitmConnect`：强制按 HTTP 处理（用于纯 HTTP 站点）
- `ConnectHijack`：连接劫持
- `ConnectProxyAuthHijack`：代理认证劫持


## 连接统计模块

连接统计模块负责追踪所有活跃的代理连接，提供实时连接状态查询和 WebSocket 实时推送功能。

### 数据结构

**ConnectionInfo** (`mproxy/connections.go:5-15`)
```go
type ConnectionInfo struct {
    Session     int64     `json:"id"`        // 会话ID（唯一标识）
    Host        string    `json:"host"`      // 目标主机
    Method      string    `json:"method"`    // HTTP方法（GET/POST等）
    URL         string    `json:"url"`       // 完整URL
    RemoteAddr  string    `json:"remote"`    // 客户端远程地址
    Protocol    string    `json:"protocol"`  // 协议类型：HTTP/HTTPS-Tunnel/HTTPS-MITM
    StartTime   time.Time `json:"startTime"` // 连接开始时间
    UploadRef   *int64    `json:"-"`         // 上行流量计数器引用（内部字段）
    DownloadRef *int64    `json:"-"`         // 下行流量计数器引用（内部字段）
}
```

**连接存储** (`mproxy/core_proxy.go:39`)
```go
type CoreHttpServer struct {
    Connections sync.Map  // 键：Session int64，值：*ConnectionInfo
    sess        int64     // 会话ID计数器（原子递增）
    // ... 其他字段
}
```

### 会话ID生成

```go
// 全局唯一递增ID
atomic.AddInt64(&proxy.sess, 1)
```

### 连接注册位置

| 场景 | 文件位置 | 协议类型 |
|------|----------|----------|
| HTTP 请求 | `http.go:23-33` | `HTTP` |
| HTTPS 隧道模式 | `https.go:256-266` | `HTTPS-Tunnel` |
| HTTPS MITM 模式 | `https.go:513-523` | `HTTPS-MITM` |
| HTTP MITM 模式 | `https.go:352-362` | `HTTP-MITM` |

### 连接清理机制

1. **HTTP 请求完成时** (`http.go:62-71`)：通过 `respBodyReader` 的 `onClose` 回调
2. **HTTPS 隧道关闭时** (`https.go:269-277`)：通过 `tunnelTrafficClient.onUpdate` 回调
3. **MITM 模式** (`https.go:363, 524`)：使用 `defer` 确保请求完成后注销

### 连接生命周期

```
请求到达 → 生成SessionID → 注册连接信息 → 处理请求 → 响应完成 → 注销连接
```

### 实时推送

通过 WebSocket 每 2 秒推送活跃连接列表 (`proxysocket/hub.go:88-125`)：

```go
func (h *WebSocketHub) StartConnectionPusher() {
    ticker := time.NewTicker(2 * time.Second)
    for range ticker.C {
        connections := make([]map[string]any, 0)

        h.proxy.Connections.Range(func(key, value any) bool {
            info := value.(*mproxy.ConnectionInfo)
            connData := map[string]any{
                "id":       info.Session,
                "host":     info.Host,
                "method":   info.Method,
                "url":      info.URL,
                "remote":   info.RemoteAddr,
                "protocol": info.Protocol,
            }
            // 通过指针引用实时读取流量
            if info.UploadRef != nil {
                connData["up"] = *info.UploadRef
            }
            if info.DownloadRef != nil {
                connData["down"] = *info.DownloadRef
            }
            connections = append(connections, connData)
            return true
        })

        h.broadcastToTopic("connections", map[string]any{
            "type": "connections",
            "data": connections,
        })
    }
}
```

## 日志统计模块

日志统计模块提供统一的日志记录和推送功能，支持级别过滤和实时 WebSocket 推送。

### 数据结构

**LogMessage** (`mproxy/logs.go:16-21`)
```go
type LogMessage struct {
    Level   string    `json:"level"`   // 日志级别: INFO/WARN/ERROR/DEBUG
    Session int64     `json:"session"` // 会话ID（标识单个请求）
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

**全局日志通道** (`mproxy/logs.go:11-12`)
```go
var LogChan = make(chan LogMessage, 1000)  // 缓冲通道，避免阻塞
```

### 日志格式

```
[XXX] LEVEL: 消息内容
```

- `[XXX]`: 3位会话ID（取低16位）
- `LEVEL`: INFO/WARN/ERROR/DEBUG
- `消息内容`: 实际日志信息

### 日志记录机制

```go
func (l *LogCollector) Printf(format string, v ...any) {
    msg := fmt.Sprintf(format, v...)

    // 非阻塞发送到 Channel
    select {
    case LogChan <- LogMessage{...}:
    default:
        // Channel 满时丢弃，避免阻塞主流程
    }

    // 同时输出到原始 Logger
    l.Underlying.Printf(format, v...)
}
```

### 上下文日志方法

```go
// 公有日志方法 - 受 Verbose 控制 (ctxt.go:57-61)
func (ctx *Pcontext) Log_P(msg string, argv ...any) {
    if ctx.core_proxy.Verbose == true {
        ctx.printf("INFO: "+msg, argv...)
    }
}

// 警告日志 (ctxt.go:47-49)
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

```go
func (h *WebSocketHub) StartLogPusher() {
    go func() {
        for msg := range mproxy.LogChan {  // 从全局 LogChan 读取
            h.broadcastLog(msg)
        }
    }()
}
```

### 日志类型

| 类型 | 输出位置 | 示例 |
|------|----------|------|
| 请求日志 | `http.go`, `https.go` | `Sending request GET https://...` |
| 响应日志 | `https.go` | `resp 200 OK` |
| 流量日志 | `actions.go`, `https.go` | `[流量统计] 上行: 1024 | 下行: 2048 | ...` |
| 错误日志 | 各处 | `Error dialing to ...` |
| 连接日志 | 注册到 Connections map | - |

## WebSocket 支持

WebSocket 支持通过 `websocket.go` 实现：
- 检测 WebSocket 握手：`isWebSocketHandshake()` 检查 `Connection: upgrade` 和 `Upgrade: websocket` 头
- 连接劫持：`hijackConnection()` 获取原始 TCP 连接
- 双向转发：`proxyWebsocket()` 实现客户端和服务端之间的数据双向转发

## WebSocket 实时推送

WebSocket Hub (`proxysocket/hub.go`) 提供三种实时数据推送：

| 主题 | 内容 | 推送频率 |
|------|------|----------|
| `connections` | 活跃连接列表 | 每 2 秒 |
| `logs` | 日志消息 | 实时（有日志时） |
| `traffic` | 全局流量统计 | 每 1 秒 |

客户端订阅配置：

```go
type Subscription struct {
    Traffic     bool      // 流量统计
    Connections bool      // 连接信息
    Logs        bool      // 日志推送
    LogLevel    string    // 日志级别过滤
}
```


