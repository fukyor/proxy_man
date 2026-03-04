# HTTPS MITM 统一重构方案（v2）

## Context

`mproxy/https.go` 中 `ConnectHTTPMitm`（行437-674）和 `ConnectMitm`（行676-904）两个分支代码重复度约 70%，共 ~470 行。核心差异仅在于：

- HTTP MITM 使用手动双工转发（`req.Write` + `http.ReadResponse`），无路由能力
- HTTPS MITM 使用 `ctxt.RoundTrip`，Transport 自动处理连接池和 100-continue

参考 `https_new.go`（goproxy 标准实现）的首字节嗅探方案，将两个分支合并为一个，统一使用 `RoundTrip`，同时引入 `RouterRoundTripper` 使所有 MITM 流量都经过路由规则。

---

## 五问评估结论

| 问题                | 结论                                                         |
| ------------------- | ------------------------------------------------------------ |
| **1. 业务兼容度**   | **完全兼容**。Pcontext/TrafficCounter/Connections.Store/StartCapture/Exchange 挂载点在两分支中完全对称，合并后不丢失任何挂载 |
| **2. 代理模型统一** | **可实现**。RouterRoundTripper 使 HTTP MITM 也获得路由能力。`ConnectWithReqDial` 保留给隧道模式，不废弃 |
| **3. 协议嗅探**     | **可行**。`Peek(1)` + `0x16` 检测 TLS Handshake，彻底取代端口 80 硬编码判断 |
| **4. HTTP/2 裁剪**  | **安全**。项目已设 `AllowHTTP2=false`，无需 PRI 魔数检测和 H2Transport |
| **5. 重构侵入性**   | **可控**。3 个文件改动，净减 ~240 行代码                     |

---

## plan.md 审查问题

| #     | 问题                                                         | 严重度   | 修复方案                                                     |
| ----- | ------------------------------------------------------------ | -------- | ------------------------------------------------------------ |
| 1     | requestOk 缺少 `bodyModified` 检测，filterResponse 修改 Body 后 Content-Length 不匹配 | 严重     | 保留 ConnectMitm 的 origBody/bodyModified + chunked 编码逻辑 |
| 2     | 缺少 RFC7230 Body 抑制（HEAD/1xx/204/304 不应写 Body）       | 中       | 保留 ConnectMitm 行858-864 的判断                            |
| ~~3~~ | ~~100-continue 死锁~~                                        | ~~撤销~~ | **不需要特殊处理**。Go 的 `http.Transport.RoundTrip` 完全自动处理 100-continue。当前 ConnectMitm 分支就是直接调用 `ctxt.RoundTrip(req)` 且无任何 100-continue 代码，生产环境完全正常 |
| 4     | Pcontext 未继承 `topctx.UserData` 和 `topctx.RoundTripper`   | 低       | 按 ConnectMitm 模式继承                                      |
| 5     | 不必要的 `go func()`                                         | 低       | 同步执行，与当前两个分支行为一致                             |
| 6     | 缺少 `resp.Close` / `Connection: close` 检查                 | 低       | 保留两分支都有的关闭检查                                     |
| 7     | `resp.Request.Method` 在 filterRequest 短路响应时可能 nil    | 低       | 改用闭包作用域内的 `req.Method`                              |

---

## 架构说明：for 循环的必要性

plan.md 中的 `for !reqReader.IsEOF()` 循环**不是在管理 proxy→target 的连接复用**（那由 `http.Transport` 连接池自动处理），而是在处理 **client→proxy MITM 隧道上的 HTTP/1.1 Keep-Alive 请求流**：

```
客户端 ──CONNECT──→ 代理（建立隧道）
        ←── 200 OK ──

// 同一隧道内，客户端发送多个顺序请求：
客户端 ──GET /path1──→ 代理 ──RoundTrip──→ 目标（Transport 自动管理连接池）
        ←── 响应1 ──
客户端 ──GET /path2──→ 代理 ──RoundTrip──→ 目标（可能复用同一连接或新建）
        ←── 响应2 ──
// ... 直到客户端发送 FIN (EOF)
```

- `for` 循环 = 从客户端隧道中逐个读取请求（**必须手动，因为是裸 TCP/TLS 连接**）
- `RoundTrip` = 向目标服务器发送请求（**Transport 自动管理连接池，无需手动**）
- `https_new.go` 参考实现也使用完全相同的 for 循环模式（行251-385）

---

## 实施步骤

### 步骤 1：添加嗅探辅助结构（`mproxy/https.go` 顶部）

在 import 块之后、`ConnectActionSelecter` 之前插入：

```go
type readBufferedConn struct {
    net.Conn
    r io.Reader
}

func (c *readBufferedConn) Read(p []byte) (int, error) {
    return c.r.Read(p)
}

const _tlsRecordTypeHandshake = byte(22)
```

### 步骤 2：新建 `mproxy/router_roundtrip.go`

```go
package mproxy

import (
    "context"
    "crypto/tls"
    "net"
    "net/http"
    "time"
)

type contextKey string
const routingReqKey contextKey = "routing_req"

type RouterRoundTripper struct {
    proxy     *CoreHttpServer
    router    *Router
    transport *http.Transport // 全局唯一，连接池生效
}

func NewRouterRoundTripper(proxy *CoreHttpServer, router *Router) *RouterRoundTripper {
    rt := &RouterRoundTripper{proxy: proxy, router: router}
    rt.transport = &http.Transport{
        DialContext: func(c context.Context, network, addr string) (net.Conn, error) {
            req, ok := c.Value(routingReqKey).(*http.Request)
            if !ok {
                return net.Dial(network, addr) // 兜底直连
            }
            return rt.router.RouteDial(req, network, addr)
        },
        TLSClientConfig:       &tls.Config{InsecureSkipVerify: true},
        MaxIdleConns:          100,
        MaxIdleConnsPerHost:   10,
        IdleConnTimeout:       90 * time.Second,
        TLSHandshakeTimeout:   10 * time.Second,
        ExpectContinueTimeout: 1 * time.Second,
    }
    return rt
}

// 实现 mproxy.RoundTripper 接口（ctxt.go:36-38）
func (rt *RouterRoundTripper) RoundTrip(req *http.Request, ctx *Pcontext) (*http.Response, error) {
    reqWithCtx := req.WithContext(context.WithValue(req.Context(), routingReqKey, req))
    return rt.transport.RoundTrip(reqWithCtx)
}
```

### 步骤 3：替换 `mproxy/https.go` 的两个 MITM case

**删除**：行437-904（`case ConnectHTTPMitm:` 到 `case ConnectMitm:` 结尾）

**替换为统一分支**（以 ConnectMitm 为基础，融入嗅探 + 修复全部问题）：

```go
case ConnectHTTPMitm, ConnectMitm:
    _, _ = connFromClinet.Write([]byte("HTTP/1.0 200 OK\r\n\r\n"))
    topctx.Log_P("MITM 模式启动, 协议自动嗅探")

    defer proxy.MarkConnectionClosed(tunnelSession)

    // --- 1. 首字节嗅探 ---
    readBuffer := bufio.NewReader(connFromClinet)
    peek, _ := readBuffer.Peek(1)
    isTLS := len(peek) > 0 && peek[0] == _tlsRecordTypeHandshake

    var mitmClientConn net.Conn = &readBufferedConn{Conn: connFromClinet, r: readBuffer}
    defer mitmClientConn.Close()

    scheme := "http"
    protocolLabel := "HTTP-MITM"

    // --- 2. TLS 握手（仅 TLS 流量） ---
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

        tlsConn := tls.Server(mitmClientConn, tlsConfig)
        if err := tlsConn.HandshakeContext(topctx.Req.Context()); err != nil {
            topctx.WarnP("TLS 握手失败 Cannot handshake client %v %v", r.Host, err)
            return
        }
        mitmClientConn = tlsConn
    }

    // --- 3. 请求循环（从客户端隧道读取 HTTP/1.1 Keep-Alive 请求流）---
    // 注：for 循环是读取客户端请求的标准模式（https_new.go 同样使用）
    // proxy→target 的连接复用由 Transport 连接池自动管理
    reqReader := http1parser.NewRequestReader(proxy.PreventParseHeader, mitmClientConn)

    for !reqReader.IsEOF() {
        req, err := reqReader.ReadRequest()

        ctxt := &Pcontext{
            Req:            req,
            Session:        atomic.AddInt64(&proxy.sess, 1),
            core_proxy:     proxy,
            parCtx:         topctx,
            UserData:       topctx.UserData,      // [修复4]
            RoundTripper:   topctx.RoundTripper,   // [修复4]
            TrafficCounter: &TrafficCounter{},
        }
        ctxt.StartCapture(tunnelSession)

        if err != nil && !errors.Is(err, io.EOF) {
            ctxt.WarnP("协议解析错误 Cannot read request from client %v %v", r.Host, err)
        }
        if err != nil {
            return
        }

        req.RemoteAddr = r.RemoteAddr
        ctxt.Log_P("client ip: %v, request Host %v", r.RemoteAddr, r.Host)

        if !strings.HasPrefix(req.URL.String(), scheme+"://") {
            req.URL, err = url.Parse(scheme + "://" + r.Host + req.URL.String())
        }

        requestOk := func(req *http.Request) bool {
            requestContext, finishRequest := context.WithCancel(req.Context())
            req = req.WithContext(requestContext)
            defer finishRequest()

            proxy.Connections.Store(ctxt.Session, &ConnectionInfo{
                Session:      ctxt.Session,
                ParentSess:   tunnelSession,
                Host:         r.Host,
                Method:       req.Method,
                URL:          req.URL.String(),
                RemoteAddr:   r.RemoteAddr,
                Protocol:     protocolLabel,
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
                if err != nil {
                    ctxt.SetCaptureError(err)
                    if req.URL != nil {
                        ctxt.WarnP("Illegal URL %s", scheme+"://"+r.Host+req.URL.Path)
                    } else {
                        ctxt.WarnP("Illegal URL %s", scheme+"://"+r.Host)
                    }
                    return false
                }

                RemoveProxyHeaders(ctxt, req)

                // 100-continue 由 Transport 自动处理，无需特殊代码
                resp, err = func() (*http.Response, error) {
                    defer req.Body.Close()
                    return ctxt.RoundTrip(req)
                }()
                if err != nil {
                    ctxt.SetCaptureError(err)
                    ctxt.WarnP("Cannot read response from mitm'd server %v", err)
                    return false
                }
                ctxt.Log_P("resp %v", resp.Status)
            }

            // [修复1] bodyModified 检测
            origBody := resp.Body
            resp = proxy.filterResponse(resp, ctxt)
            bodyModified := resp.Body != origBody
            defer resp.Body.Close()

            // WebSocket 检测
            isWebsocket := isWebSocketHandshake(resp.Header)
            if isWebsocket {
                ctxt.SetCaptureSkip()
            }

            // [修复2] RFC7230 头部处理（用 req.Method 替代 resp.Request.Method，修复7）
            if isWebsocket || req.Method == http.MethodHead {
                // HEAD 请求不修改 Content-Length
            } else if (resp.StatusCode >= 100 && resp.StatusCode < 200) ||
                resp.StatusCode == http.StatusNoContent {
                resp.Header.Del("Content-Length")
            } else if bodyModified {
                resp.Header.Del("Content-Length")
                resp.Header.Set("Transfer-Encoding", "chunked")
            }

            if !isWebsocket && !proxy.ConnectMaintain {
                resp.Header.Set("Connection", "close")
            }

            // 手动写回响应头
            text := resp.Status
            statusCode := strconv.Itoa(resp.StatusCode) + " "
            text = strings.TrimPrefix(text, statusCode)
            if _, err := io.WriteString(mitmClientConn, "HTTP/1.1"+" "+statusCode+text+"\r\n"); err != nil {
                ctxt.WarnP("Cannot write response HTTP status from mitm'd client: %v", err)
                return false
            }
            if err := resp.Header.Write(mitmClientConn); err != nil {
                ctxt.WarnP("Cannot write response header from mitm'd client: %v", err)
                return false
            }
            if _, err = io.WriteString(mitmClientConn, "\r\n"); err != nil {
                ctxt.WarnP("Cannot write response header end from mitm'd client: %v", err)
                return false
            }

            // WebSocket 处理
            if isWebsocket {
                ctxt.Log_P("Response looks like websocket upgrade.")
                wsConn, ok := resp.Body.(io.ReadWriter)
                if !ok {
                    ctxt.WarnP("Unable to use Websocket connection")
                    return false
                }
                proxy.proxyWebsocket(ctxt, wsConn, mitmClientConn)
                proxy.MarkConnectionClosed(ctxt.Session)
                return false
            }

            // [修复2] RFC7230 Body 写回
            if req.Method == http.MethodHead ||
                (resp.StatusCode >= 100 && resp.StatusCode < 200) ||
                resp.StatusCode == http.StatusNoContent ||
                resp.StatusCode == http.StatusNotModified {
                // RFC7230: 这些情况不写 Body
            } else if bodyModified {
                // [修复1] filterResponse 修改了 Body，使用 chunked 编码
                chunked := newChunkedWriter(mitmClientConn)
                if _, err := io.Copy(chunked, resp.Body); err != nil {
                    ctxt.WarnP("Cannot write response body: %v", err)
                    return false
                }
                if err := chunked.Close(); err != nil {
                    ctxt.WarnP("Cannot write chunked EOF: %v", err)
                    return false
                }
                if _, err = io.WriteString(mitmClientConn, "\r\n"); err != nil {
                    ctxt.WarnP("Cannot write chunked trailer: %v", err)
                    return false
                }
            } else {
                if _, err := io.Copy(mitmClientConn, resp.Body); err != nil {
                    ctxt.WarnP("Cannot write response body: %v", err)
                    return false
                }
            }

            // [修复6] Connection: close 检查
            if resp.Close || strings.EqualFold(resp.Header.Get("Connection"), "close") {
                ctxt.WarnP("收到服务器close响应, client->proxy->target连接关闭")
                return false
            }

            return true
        }

        if !requestOk(req) {
            return
        }
    }
    topctx.Log_P("Connect Tunnel Normal Exiting on Client EOF")
```

**统一后消除的差异**：

| 原差异                                                    | 统一方案                                                     |
| --------------------------------------------------------- | ------------------------------------------------------------ |
| HTTP: `req.Write`+`ReadResponse` / HTTPS: `RoundTrip`     | 统一用 `ctxt.RoundTrip`，Transport 自动管理连接池和 100-continue |
| HTTP: 手动 100-continue 全双工                            | 不需要，RoundTrip 完全自动处理                               |
| HTTP: `connFromClinet` / HTTPS: `tlsConn`                 | 统一变量 `mitmClientConn`（Peek 后可能被 TLS 包装）          |
| HTTP: Protocol="HTTP-MITM" / HTTPS: Protocol="HTTPS-MITM" | 变量 `protocolLabel`，由嗅探结果决定                         |
| HTTP: 无 WebSocket / HTTPS: 有 WebSocket                  | 统一支持 WebSocket                                           |
| HTTP: `reqDoneCh` 等 MinIO / HTTPS: 无                    | RoundTrip 中 `req.Body.Close()` 同步等待，无需额外 channel   |

### 步骤 4：修改 `main.go`

在 `proxy.ConnectWithReqDial = router.RouteDial` 之后添加：

```go
routerRT := mproxy.NewRouterRoundTripper(proxy, router)
proxy.HookOnReq().DoFunc(func(req *http.Request, ctx *mproxy.Pcontext) (*http.Request, *http.Response) {
    if ctx.RoundTripper == nil {
        ctx.RoundTripper = routerRT
    }
    return req, nil
})
```

---

## 关键文件

| 文件                         | 改动类型 | 说明                                              |
| ---------------------------- | -------- | ------------------------------------------------- |
| `mproxy/https.go`            | 修改     | 删除两个 case（~470行），替换为统一分支（~180行） |
| `mproxy/router_roundtrip.go` | 新建     | RouterRoundTripper 实现（~45行）                  |
| `main.go`                    | 修改     | 添加 RouterRoundTripper 注入（~5行）              |

**不改动**：`mproxy/router.go`、`mproxy/ctxt.go`、`mproxy/actions.go`、`mproxy/http.go`

---

## 不在本轮范围

- `http.go:myHttpHandleWithEngine`（TCP 引擎模式）仍使用手动双工，后续可独立优化
- `ConnectWithReqDial` 不废弃，隧道模式和引擎模式仍需要
- `httpMitmCheckError` 函数不删除，`http.go` 仍在使用

---

## 验证方法

```bash
# 1. 编译
go build -o proxy_man.exe main.go

# 2. HTTP MITM（嗅探为 HTTP）
curl -x http://localhost:8080 http://httpbin.org/get

# 3. HTTPS MITM（嗅探为 TLS）
curl -x http://localhost:8080 https://httpbin.org/get --proxy-insecure -k

# 4. 路由验证（日志应有 [路由匹配]）
curl -x http://localhost:8080 https://www.youtube.com --proxy-insecure -k

# 5. 100-continue（RoundTrip 自动处理）
curl -x http://localhost:8080 https://httpbin.org/post -H "Expect: 100-continue" -d "test" -k

# 6. WebSocket — 订阅 mitm_detail 主题验证 Exchange 推送
# 7. MinIO — 验证 bodyKey/bodyUploaded 字段和下载 API
```

**回归清单**：

- [ ] HTTP/HTTPS MITM 流量正常
- [ ] 非标端口嗅探正确（如 8443 发 TLS、8080 发 HTTP）
- [ ] 路由规则在两种 MITM 模式下均生效
- [ ] 隧道模式（ConnectAccept）不受影响
- [ ] WebSocket 连接正常
- [ ] 流量统计正确（父子连接累加）
- [ ] 连接注册/注销/Protocol 显示正确
- [ ] MinIO Body 捕获和 Exchange 推送正常
- [ ] 100-continue 请求正常（Transport 自动处理）
- [ ] Connection: close 后正确断开
- [ ] HEAD/204/304 不写 Body
- [ ] bodyModified 时使用 chunked 编码