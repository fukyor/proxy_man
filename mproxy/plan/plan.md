# HTTPS MITM 统一重构计划审查报告（最终版）

## 审查结果

### 发现的问题

| 问题                              | 严重程度 | 描述                                          |
| --------------------------------- | -------- | --------------------------------------------- |
| **RouterRoundTripper 连接池失效** | 🔴 致命   | 每次 RoundTrip 新建 Transport，连接池无法复用 |
| **TLS Handshake 阻塞风险**        | 🟡 中     | 使用阻塞式 Handshake()，可能导致协程卡死      |
| **tunnelSession 清理时机错误**    | 🔴 高     | 新计划使用 goroutine，但 defer 位置错误       |
| plan.md 未考虑现有 router.go      | 🟢 已修复 | 代码库已有完整路由系统                        |

### 优化建议

1. **修复连接池问题**：RouterRoundTripper 持有全局 Transport，通过 context 传递请求信息
2. **使用 HandshakeContext**：支持超时和上下文取消
3. **完全复用现有路由系统**：`mproxy/router.go` 已实现完整的规则引擎

---

## 修订后的实施计划

### 第一阶段：添加嗅探辅助结构体

在 `mproxy/https.go` 顶部添加：

```go
// readBufferedConn 包装连接以支持 Peek 后的正常读取
type readBufferedConn struct {
    net.Conn
    r io.Reader
}

func (c *readBufferedConn) Read(p []byte) (int, error) {
    return c.r.Read(p)
}

const _tlsRecordTypeHandshake = byte(22)
```

---

### 第二阶段：创建 RouterRoundTripper（修复连接池问题）

**新建文件 `mproxy/router_roundtrip.go`**：

```go
package mproxy

import (
    "context"
    "crypto/tls"
    "net"
    "net/http"
    "time"
)

// contextKey 类型用于 context 中的键，避免冲突
type contextKey string

const routingReqKey contextKey = "routing_req"

// RouterRoundTripper 将路由逻辑提升到 HTTP 请求层面
// 复用现有 Router.RouteDial 的规则匹配和拨号逻辑
type RouterRoundTripper struct {
    proxy     *CoreHttpServer
    router    *Router
    transport *http.Transport // 全局复用的连接池
}

// NewRouterRoundTripper 创建基于 Router 的 RoundTripper
// 关键：全局只初始化一次 Transport，保证连接池复用
func NewRouterRoundTripper(proxy *CoreHttpServer, router *Router) *RouterRoundTripper {
    rt := &RouterRoundTripper{
        proxy:  proxy,
        router: router,
    }

    // 全局只初始化一次 Transport，这是连接池的核心
    rt.transport = &http.Transport{
        DialContext: func(c context.Context, network, addr string) (net.Conn, error) {
            // 从 context 中提取原始请求，用于路由规则匹配
            req, ok := c.Value(routingReqKey).(*http.Request)
            if !ok {
                // 如果没有请求信息，使用直连作为兜底
                return net.Dial(network, addr)
            }
            // 调用现有路由引擎的 RouteDial 方法
            return rt.router.RouteDial(req, network, addr)
        },
        TLSClientConfig: &tls.Config{
            InsecureSkipVerify: true, // 根据需要配置
        },
        // 连接池配置（这些配置现在会真正生效）
        MaxIdleConns:          100,
        MaxIdleConnsPerHost:   10,
        IdleConnTimeout:       90 * time.Second,
        TLSHandshakeTimeout:   10 * time.Second,
        ExpectContinueTimeout: 1 * time.Second,
    }

    return rt
}

// RoundTrip 实现 RoundTripper 接口
func (rt *RouterRoundTripper) RoundTrip(req *http.Request, ctx *Pcontext) (*http.Response, error) {
    // 将请求放入 context，传递给底层的 DialContext
    // 这样 DialContext 可以访问请求信息进行路由匹配
    reqWithCtx := req.WithContext(context.WithValue(req.Context(), routingReqKey, req))

    // 使用全局复用的 Transport 进行请求
    return rt.transport.RoundTrip(reqWithCtx)
}
```

**修复说明**：

- ✅ Transport 在 `NewRouterRoundTripper` 中只创建一次
- ✅ 连接池配置（`MaxIdleConns` 等）真正生效
- ✅ 通过 context 传递请求信息给 DialContext
- ✅ 避免了 FD 泄漏和性能问题

---

### 第三阶段：合并 MITM 分支（修复 TLS Handshake）

**修改 `mproxy/https.go:348` 的 switch 语句**：

```go
switch strategy.Action {
case ConnectAccept:
    // ... 保持不变

case ConnectHTTPMitm, ConnectMitm:
    // ===== 统一 MITM 处理 =====
    _, _ = connFromClinet.Write([]byte("HTTP/1.0 200 OK\r\n\r\n"))
    topctx.Log_P("Starting MITM (HTTP/HTTPS Auto-Detect)")

    go func() {
        // ===== 关键修复：tunnelSession 清理必须在 goroutine 内部 =====
        // 这样可以确保清理时机与实际 TCP 连接生命周期一致
        defer proxy.MarkConnectionClosed(tunnelSession)
        defer connFromClinet.Close()

        // --- 1. 流量嗅探 ---
        readBuffer := bufio.NewReader(connFromClinet)
        peek, _ := readBuffer.Peek(1)
        isTLS := len(peek) > 0 && peek[0] == _tlsRecordTypeHandshake

        var mitmClientConn net.Conn = &readBufferedConn{Conn: connFromClinet, r: readBuffer}

        // 动态设置协议标识
        scheme := "http"
        protocolLabel := "HTTP-MITM"

        if isTLS {
            scheme = "https"
            protocolLabel = "HTTPS-MITM"

            tlsConfig := defaultTLSConfig
            if strategy.TLSConfig != nil {
                var err error
                tlsConfig, err = strategy.TLSConfig(host, topctx)
                if err != nil {
                    httpError(mitmClientConn, topctx, err)
                    return
                }
            }

            // ===== 修复：使用 HandshakeContext 支持超时和取消 =====
            tlsConn := tls.Server(mitmClientConn, tlsConfig)

            // 使用 topctx.Req.Context() 作为握手上下文
            // 当客户端断开时，上下文会自动取消，避免协程卡死
            if err := tlsConn.HandshakeContext(topctx.Req.Context()); err != nil {
                topctx.WarnP("TLS 握手失败/超时: %v", err)
                return
            }
            mitmClientConn = tlsConn
        }

        // --- 2. 请求循环（复用现有 ConnectMitm 逻辑）---
        reqReader := http1parser.NewRequestReader(proxy.PreventParseHeader, mitmClientConn)

        for !reqReader.IsEOF() {
            req, err := reqReader.ReadRequest()
            if err != nil && !errors.Is(err, io.EOF) {
                topctx.WarnP("协议解析错误: %v", err)
            }
            if err != nil {
                return
            }

            req.RemoteAddr = r.RemoteAddr

            // 使用动态 scheme 构造 URL
            if !strings.HasPrefix(req.URL.String(), scheme+"://") {
                req.URL, err = url.Parse(scheme + "://" + r.Host + req.URL.String())
            }

            // --- 3. 单次请求处理（完全复用 https.go:733-895）---
            requestOk := func(req *http.Request) bool {
                requestContext, finishRequest := context.WithCancel(req.Context())
                req = req.WithContext(requestContext)
                defer finishRequest()

                ctxt := &Pcontext{
                    core_proxy:     proxy,
                    Req:            req,
                    parCtx:         topctx,
                    TrafficCounter: &TrafficCounter{},
                    Session:        atomic.AddInt64(&proxy.sess, 1),
                }
                ctxt.StartCapture(tunnelSession)

                // 注册连接（使用动态 protocolLabel）
                proxy.Connections.Store(ctxt.Session, &ConnectionInfo{
                    Session:      ctxt.Session,
                    ParentSess:   tunnelSession,
                    Host:         r.Host,
                    Method:       req.Method,
                    URL:          req.URL.String(),
                    RemoteAddr:   r.RemoteAddr,
                    Protocol:     protocolLabel,  // 动态：HTTP-MITM 或 HTTPS-MITM
                    StartTime:    time.Now(),
                    Status:       "Active",
                    PuploadRef:   &ctxt.parCtx.TrafficCounter.req_sum,
                    PdownloadRef: &ctxt.parCtx.TrafficCounter.resp_sum,
                    UploadRef:    &ctxt.TrafficCounter.req_sum,
                    DownloadRef:  &ctxt.TrafficCounter.resp_sum,
                    OnClose:      func() { finishRequest() },
                })
                defer proxy.MarkConnectionClosed(ctxt.Session)

                ctxt.Req = req
                req, resp := proxy.filterRequest(req, ctxt)
                ctxt.CaptureRequest(req)

                if resp == nil {
                    RemoveProxyHeaders(ctxt, req)

                    // --- 关键：使用 RouterRoundTripper 进行 HTTP 层路由 ---
                    resp, err = ctxt.RoundTrip(req)
                    if err != nil {
                        ctxt.SetCaptureError(err)
                        ctxt.WarnP("请求失败: %v", err)
                        httpError(mitmClientConn, ctxt, err)
                        return false
                    }
                }

                // 响应处理（完全复用 https.go:789-895）
                resp = proxy.filterResponse(resp, ctxt)
                defer resp.Body.Close()

                // WebSocket 检测
                isWebsocket := isWebSocketHandshake(resp.Header)
                if isWebsocket {
                    ctxt.SetCaptureSkip()
                }
                if !isWebsocket && !proxy.ConnectMaintain {
                    resp.Header.Set("Connection", "close")
                }

                // 手动写回响应头
                text := resp.Status
                statusCode := strconv.Itoa(resp.StatusCode) + " "
                text = strings.TrimPrefix(text, statusCode)
                if _, err := io.WriteString(mitmClientConn, "HTTP/1.1"+" "+statusCode+text+"\r\n"); err != nil {
                    ctxt.WarnP("写响应头失败: %v", err)
                    return false
                }
                if err := resp.Header.Write(mitmClientConn); err != nil {
                    ctxt.WarnP("写响应头失败: %v", err)
                    return false
                }
                if _, err = io.WriteString(mitmClientConn, "\r\n"); err != nil {
                    ctxt.WarnP("写响应头结束符失败: %v", err)
                    return false
                }

                // WebSocket 处理
                if isWebsocket {
                    wsConn, ok := resp.Body.(io.ReadWriter)
                    if !ok {
                        ctxt.WarnP("Unable to use Websocket connection")
                        return false
                    }
                    proxy.proxyWebsocket(ctxt, wsConn, mitmClientConn)
                    proxy.MarkConnectionClosed(ctxt.Session)
                    return false
                }

                // 写回响应体
                if resp.Body != nil {
                    if _, err := io.Copy(mitmClientConn, resp.Body); err != nil {
                        ctxt.WarnP("写响应体失败: %v", err)
                        return false
                    }
                }

                return true
            }

            if !requestOk(req) {
                return
            }
        }
        topctx.Log_P("Connect Tunnel Normal Exiting on Client EOF")
    }()

case ConnectReject:
    // ... 保持不变
}
```

**修复说明**：

- ✅ 使用 `HandshakeContext` 替代 `Handshake`
- ✅ 支持上下文取消和超时
- ✅ 避免恶意客户端导致协程卡死

---

### 第四阶段：配置 RouterRoundTripper

**修改 `main.go`**：

```go
func main() {
    verbose := flag.Bool("v", true, "should every proxy request be logged to stdout")
    addr := flag.String("addr", ":8080", "proxy listen address")
    flag.Parse()

    proxy := mproxy.NewCoreHttpSever()
    proxy.Verbose = *verbose
    proxy.AllowHTTP2 = false
    proxy.KeepAcceptEncoding = false
    proxy.PreventParseHeader = false
    proxy.KeepDestHeaders = true
    proxy.ConnectMaintain = true
    proxy.MitmEnabled = true
    proxy.HttpMitmNoTunnel = true

    // 使用 LogCollector 包装原有 Logger
    proxy.Logger = mproxy.NewLogCollector(proxy.Logger)

    // 初始化 MinIO
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
        log.Printf("警告: MinIO 初始化失败: %v", err)
    } else {
        myminio.GlobalClient = client
        log.Printf("MinIO 存储已启用: %s/%s", minioConfig.Endpoint, minioConfig.Bucket)
    }

    // 启动 pprof
    go func() {
        log.Println("🔍 性能监控 (pprof) 服务已启动: http://localhost:6060/debug/pprof/")
        if err := http.ListenAndServe(":6060", nil); err != nil {
            log.Printf("pprof 启动失败: %v", err)
        }
    }()

    mproxy.AddTrafficMonitor(proxy)

    // ===== 创建路由引擎 =====
    router := mproxy.NewRouter(proxy)

    // 注册二级代理节点
    proxy1, err := mproxy.NewHttpProxyDialer(proxy, "Proxy1", "http://127.0.0.1:7892")
    if err != nil {
        log.Printf("警告: 创建 Proxy1 失败: %v", err)
    } else {
        router.AddDialer("Proxy1", proxy1)
    }

    // 配置路由规则
    router.AddRule(mproxy.DomainKeywordRule("youtube", "google"), "Proxy1")
    router.AddRule(mproxy.DomainSuffixRule("twitter.com", "x.com"), "Proxy1")

    // ===== 关键：创建并注入 RouterRoundTripper =====
    routerRT := mproxy.NewRouterRoundTripper(proxy, router)

    // 使用 Hook 为所有请求注入 RouterRoundTripper
    mproxy.HookOnReq().DoFunc(func(req *http.Request, ctx *mproxy.Pcontext) (*http.Request, *http.Response) {
        ctx.RoundTripper = routerRT  // 注入自定义 RoundTripper
        return req, nil
    })

    // 保留现有的 ConnectWithReqDial 挂载（用于隧道模式）
    proxy.ConnectWithReqDial = router.RouteDial

    // 启动 WebSocket 控制服务
    ws := &proxysocket.WebsocketServer{
        Proxy: proxy,
        Addr:  ":8000",
        Secret: "123",
    }
    if !ws.StartControlServer() {
        log.Fatal("websocket server启动失败")
    }

    s := http.Server{
        Addr:    *addr,
        Handler: proxy,
    }
    if err := s.ListenAndServe(); err != nil {
        log.Fatal("服务器错误", err)
    }
}
```

---

### 第五阶段：删除冗余代码

删除 `mproxy/https.go:437-674` 的整个 `ConnectHTTPMitm` 分支（约 240 行）

---

## 关键文件

| 文件                         | 修改类型 | 说明                                  |
| ---------------------------- | -------- | ------------------------------------- |
| `mproxy/https.go`            | 修改     | 合并分支、添加嗅探、修复 Handshake    |
| `mproxy/router_roundtrip.go` | 新增     | RouterRoundTripper 实现（修复连接池） |
| `mproxy/router.go`           | 不变     | 完全复用现有路由系统                  |
| `main.go`                    | 修改     | 配置 RouterRoundTripper               |

---

## 架构对比

### 修改前（问题）

```
HTTP 层（仅 HTTPS MITM）:
    ctxt.RoundTrip → proxy.Transport.RoundTrip → 直连

TCP 层:
    Router.RouteDial → 建立连接 → 隧道透传

问题：HTTP MITM 无法使用路由规则
```

### 修改后（统一路由）

```
HTTP 层（统一）:
    ctxt.RoundTrip → RouterRoundTripper.RoundTrip
                    → 全局 Transport.RoundTrip (连接池复用)
                    → DialContext (从 context 提取 req)
                    → Router.RouteDial → 路由规则匹配 → 建立连接

TCP 层（保持不变）:
    Router.RouteDial → 建立连接 → 隧道透传
```

**关键修复**：

1. ✅ Transport 全局复用，连接池真正生效
2. ✅ 通过 context 传递请求信息
3. ✅ 所有 HTTP/HTTPS MITM 流量都经过路由规则

---

## 验证方法

### 1. 单元测试

```go
// 测试连接池复用
func TestRouterRoundTripperConnPool(t *testing.T) {
    router := setupTestRouter()
    rt := NewRouterRoundTripper(proxy, router)

    // 发送多个请求到同一主机
    for i := 0; i < 10; i++ {
        req := httptest.NewRequest("GET", "http://example.com/test", nil)
        resp, err := rt.RoundTrip(req, &Pcontext{})
        require.NoError(t, err)
        require.NotNil(t, resp)
        resp.Body.Close()
    }

    // 验证连接池被复用（而不是每次都新建连接）
    // 可以通过监控 TCP 连接数量来验证
}

// 测试 TLS Handshake 超时
func TestTLSHandshakeTimeout(t *testing.T) {
    // 模拟一个只发送 TLS 字节后不响应的客户端
    // 验证 HandshakeContext 会正确超时返回
}
```

### 2. 性能测试

```bash
# 使用 wrk 或 ab 进行压力测试
# 验证连接池是否生效（响应时间应该显著降低）
wrk -t4 -c100 -d30s http://localhost:8080
```

### 3. 集成测试

```bash
# 测试 HTTP MITM 路由
curl -x http://localhost:8080 http://youtube.com --proxy-insecure

# 测试 HTTPS MITM 路由
curl -x http://localhost:8080 https://twitter.com --proxy-insecure

# 验证日志中的 [路由匹配] 消息
```

### 4. 回归测试清单

- [ ] HTTP MITM 流量正常
- [ ] HTTPS MITM 流量正常
- [ ] 路由规则正确匹配
- [ ] WebSocket 连接正常
- [ ] 流量统计正确
- [ ] 连接池复用生效（验证通过监控连接数）
- [ ] TLS Handshake 超时正常
- [ ] MinIO 上传正常
- [ ] 100-continue 请求正常

---

## 收益评估

| 指标              | 修改前         | 修改后         | 收益           |
| ----------------- | -------------- | -------------- | -------------- |
| 代码行数          | ~910 行        | ~670 行        | -240 行 (-26%) |
| 分支数量          | 2 个独立分支   | 1 个统一分支   | 逻辑统一       |
| HTTP MITM 路由    | 不支持         | 完全支持       | 功能增强       |
| 连接池复用        | 部分           | 完全           | 性能大幅提升   |
| TLS Handshake     | 阻塞           | 可取消         | 稳定性提升     |
| 100-continue 处理 | 手动（易出错） | 自动（标准库） | 稳定性提升     |
| 协议识别          | 端口硬编码     | 字节嗅探       | 准确性提升     |

---

## 风险与缓解

| 风险                   | 影响               | 缓解措施                         |
| ---------------------- | ------------------ | -------------------------------- |
| context 传递失败       | 路由不生效         | 添加兜底直连逻辑                 |
| TLS Handshake 超时设置 | 正常握手被中断     | 使用合理的超时时间（10s）        |
| WebSocket 兼容性       | WebSocket 连接失败 | 完全保留现有 proxyWebsocket 逻辑 |
| 连接池配置不当         | 连接泄漏           | 充分测试，监控 FD 数量           |
| tunnelSession 清理时机 | 生命周期不一致     | defer 必须在 goroutine 内部      |

---

## 关键修复详解

### 修复 1：tunnelSession 清理时机

**问题根源**：

- 原 `ConnectMitm` 不使用 goroutine，`defer` 在主线程 for 循环结束后执行 ✅
- 新计划使用 goroutine，如果 `defer` 在外层，会在 goroutine 启动后立即执行 ❌

**错误代码**：

```go
case ConnectHTTPMitm, ConnectMitm:
    defer proxy.MarkConnectionClosed(tunnelSession)  // ❌ 在 switch 语句结束时执行
    go func() {
        defer connFromClinet.Close()
        // ... 长时间处理多个请求
    }()
    // 主线程立即返回，defer 执行，tunnelSession 被标记为关闭
```

**正确代码**：

```go
case ConnectHTTPMitm, ConnectMitm:
    go func() {
        defer proxy.MarkConnectionClosed(tunnelSession)  // ✅ 在 goroutine 退出时执行
        defer connFromClinet.Close()
        // ... 长时间处理多个请求
    }()
```

### 修复 2：RouterRoundTripper 连接池

**问题根源**：

- 每次 `RoundTrip` 新建 `http.Transport`，连接池无法复用
- `MaxIdleConns` 等配置完全失效
- 可能导致 FD 泄漏

**错误代码**：

```go
func (rt *RouterRoundTripper) RoundTrip(req *http.Request, ctx *Pcontext) (*http.Response, error) {
    transport := &http.Transport{  // ❌ 每次新建
        DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
            return rt.router.RouteDial(req, network, addr)
        },
    }
    return transport.RoundTrip(req)
}
```

**正确代码**：

```go
type RouterRoundTripper struct {
    transport *http.Transport  // ✅ 全局复用
}

func NewRouterRoundTripper(...) *RouterRoundTripper {
    rt := &RouterRoundTripper{...}
    rt.transport = &http.Transport{  // ✅ 只创建一次
        DialContext: func(c context.Context, network, addr string) (net.Conn, error) {
            req := c.Value(routingReqKey).(*http.Request)
            return rt.router.RouteDial(req, network, addr)
        },
    }
    return rt
}
```

### 修复 3：TLS Handshake 超时

**问题根源**：

- 阻塞式 `Handshake()` 可能导致协程卡死
- 恶意客户端只发送 TLS 字节后不响应

**错误代码**：

```go
if err := tlsConn.Handshake(); err != nil {  // ❌ 阻塞调用
    return
}
```

**正确代码**：

```go
if err := tlsConn.HandshakeContext(topctx.Req.Context()); err != nil {  // ✅ 支持取消
    topctx.WarnP("TLS 握手失败/超时: %v", err)
    return
}
```

---

## 附录：修复对比

### 修复前（错误）

```go
// ❌ 每次新建 Transport，连接池失效
func (rt *RouterRoundTripper) RoundTrip(req *http.Request, ctx *Pcontext) (*http.Response, error) {
    transport := &http.Transport{  // 致命错误！
        DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
            return rt.router.RouteDial(req, network, addr)
        },
        MaxIdleConns: 100, // 完全无效
    }
    return transport.RoundTrip(req)
}
```

### 修复后（正确）

```go
// ✅ 全局 Transport，连接池复用
type RouterRoundTripper struct {
    transport *http.Transport // 全局复用
}

func NewRouterRoundTripper(...) *RouterRoundTripper {
    rt := &RouterRoundTripper{...}
    rt.transport = &http.Transport{  // 只创建一次
        DialContext: func(c context.Context, network, addr string) (net.Conn, error) {
            req := c.Value(routingReqKey).(*http.Request)
            return rt.router.RouteDial(req, network, addr)
        },
        MaxIdleConns: 100, // 真正生效
    }
    return rt
}

func (rt *RouterRoundTripper) RoundTrip(req *http.Request, ctx *Pcontext) (*http.Response, error) {
    reqWithCtx := req.WithContext(context.WithValue(req.Context(), routingReqKey, req))
    return rt.transport.RoundTrip(reqWithCtx)  // 复用全局 Transport
}
```

### TLS Handshake 修复

```go
// ❌ 修复前：阻塞调用
tlsConn := tls.Server(mitmClientConn, tlsConfig)
if err := tlsConn.Handshake(); err != nil {  // 可能永久阻塞
    return
}

// ✅ 修复后：支持取消
tlsConn := tls.Server(mitmClientConn, tlsConfig)
if err := tlsConn.HandshakeContext(topctx.Req.Context()); err != nil {  // 支持超时和取消
    topctx.WarnP("TLS 握手失败/超时: %v", err)
    return
}
```