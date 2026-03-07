package mproxy

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"proxy_man/http1parser"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

func (proxy *CoreHttpServer) MyHttpHandle(w http.ResponseWriter, r *http.Request) {
	// ========== 新增：TCP 转发引擎模式 ==========
	if proxy.Config.GetConfig().HttpMitmNoTunnel {
		proxy.myHttpHandleWithEngine(w, r)
		return
	}
	// ========== 原有代码保留 ==========
	var err error
	var oriBody io.ReadCloser

	if !r.URL.IsAbs() {
		proxy.DirectHandler.ServeHTTP(w, r)
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	r = r.WithContext(ctx)
	defer cancel() // 确保函数退出时清理 Context，避免内存泄露，context本质也是通道占用内存

	ctxt := &Pcontext{
		core_proxy:     proxy,
		Req:            r,
		TrafficCounter: &TrafficCounter{},
		Session:        atomic.AddInt64(&proxy.sess, 1),
	}

	// 注册连接
	proxy.Connections.Store(ctxt.Session, &ConnectionInfo{
		Session:     ctxt.Session,
		Host:        r.Host,
		Method:      r.Method,
		URL:         r.URL.String(),
		RemoteAddr:  r.RemoteAddr,
		Protocol:    "HTTP",
		StartTime:   time.Now(),
		Status:      "Active",
		UploadRef:   &ctxt.TrafficCounter.req_sum,
		DownloadRef: &ctxt.TrafficCounter.resp_sum,
		OnClose:     func() { cancel() },
	})
	defer proxy.MarkConnectionClosed(ctxt.Session) // 函数退出时标记连接关闭

	r, resp := proxy.filterRequest(r, ctxt)

	if resp == nil {
		RemoveProxyHeaders(ctxt, r)
	}

	resp, err = ctxt.RoundTrip(r) // 发起一次http请求

	if err != nil {
		ctxt.Error = err
	}
	if resp != nil {
		// Body是顶层接口，底层是body结构体。
		// 虽然是浅拷贝，底层全部从同一个socket中读取数据，但是可以当resp.Body重新指向另一个body时，保证原数据不丢失
		oriBody = resp.Body
		defer oriBody.Close() // 和linux一样，关闭socket fd后断开tcp连接
	}

	resp = proxy.filterResponse(resp, ctxt)

	// WebSocket 处理：必须在 filterResponse 之后检测（hook 可能修改 header），
	// 但使用 oriBody 而非 resp.Body，因为 filterResponse 的包装器丢失了 Write 方法
	isWebsocket := resp != nil && isWebSocketHandshake(resp.Header)
	if isWebsocket {
		ctxt.Log_P("检测到 HTTP WebSocket 握手响应")
		ctxt.SetCaptureSkip()

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

		// 手动写入 101 响应头（Hijack 后 w 不可用）
		statusCode := strconv.Itoa(resp.StatusCode) + " "
		text := strings.TrimPrefix(resp.Status, statusCode)
		if _, err := io.WriteString(clientConn, "HTTP/1.1 "+statusCode+text+"\r\n"); err != nil {
			ctxt.WarnP("写入 WebSocket 响应状态失败: %v", err)
			return
		}
		if err := resp.Header.Write(clientConn); err != nil {
			ctxt.WarnP("写入 WebSocket 响应头失败: %v", err)
			return
		}
		if _, err := io.WriteString(clientConn, "\r\n"); err != nil {
			ctxt.WarnP("写入响应头结束符失败: %v", err)
			return
		}

		// 使用 oriBody（原始未包装的 resp.Body），它实现了 io.ReadWriter（Go 1.12+ 101 响应特性）
		// resp.Body 经过 filterResponse 包装后丢失了 Write 方法，不可用
		wsConn, ok := oriBody.(io.ReadWriter)
		if !ok {
			ctxt.WarnP("resp.Body 不支持 io.ReadWriter，无法建立 WebSocket")
			return
		}

		ctxt.Log_P("开始 HTTP WebSocket 双向转发")
		proxy.proxyWebsocket(ctxt, wsConn, clientConn)
		return
	}

	if resp == nil {
		var errorString string
		if ctxt.Error != nil {
			errorString = "error read response " + r.URL.Host + " : " + ctxt.Error.Error()
			ctxt.Log_P(errorString)
			http.Error(w, ctxt.Error.Error(), http.StatusInternalServerError)
		} else {
			errorString = "error read response " + r.URL.Host
			ctxt.Log_P(errorString)
			http.Error(w, errorString, http.StatusInternalServerError)
		}
		return // hanler函数结束后，go会自动释放连接
	}

	//不用担心Content-Length被删除的问题，会自动降级为chunked模式，go服务器自动处理chunked传输
	if oriBody != resp.Body {
		resp.Header.Del("Content-Length")
	}
	// 封装响应头
	if !isWebsocket && !proxy.Config.GetConfig().ConnectMaintain {
		resp.Header.Set("Connection", "close")
	}
	buildHeaders(w.Header(), resp.Header, proxy.Config.GetConfig().KeepDestHeaders)
	w.WriteHeader(resp.StatusCode)

	var bodyWriter io.Writer = w

	// 处理sse和chunker连接。sse每个事件需要立即发送，不能缓冲。chunker数据分块发送，每块立即传输不能缓冲。
	if strings.HasPrefix(w.Header().Get("content-type"), "text/event-stream") ||
		strings.Contains(w.Header().Get("transfer-encoding"), "chunked") {

		bodyWriter = &flushWriter{w: w}
	}

	_, err = io.Copy(bodyWriter, resp.Body)

	// resp.Body.Close()可能关闭新打开的内存或文件
	// oriBody.Close()关闭原始socket，由body中的Close字段避免重复关闭socket报错。
	if err := resp.Body.Close(); err != nil {
		ctxt.WarnP("Can't close response body %v", err)
	}
}

// myHttpHandleWithEngine 使用 TCP 转发引擎处理 HTTP 请求
// 参考 https.go ConnectHTTPMitm 的实现
//
// 结构说明：
// 1. Hijack 连接，定义 processRequest 统一处理函数
// 2. 先处理首个请求（Go 标准库已解析的 r）
// 3. 创建 RequestReader 循环读取后续 Keep-Alive 请求
func (proxy *CoreHttpServer) myHttpHandleWithEngine(w http.ResponseWriter, r *http.Request) {
	// ========== 第一阶段：Hijack 连接 ==========
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "Webserver doesn't support hijacking", http.StatusInternalServerError)
		return
	}
	clientConn, bufrw, err := hj.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer clientConn.Close()

	// 保存原始 RemoteAddr，后续所有请求共用
	remoteAddr := r.RemoteAddr

	// ========== 创建虚拟隧道（与 HTTPS MITM 的顶层隧道对齐） ==========
	topctx := &Pcontext{
		core_proxy:     proxy,
		Req:            r,
		TrafficCounter: &TrafficCounter{},
		Session:        atomic.AddInt64(&proxy.sess, 1),
	}
	tunnelSession := topctx.Session

	proxy.Connections.Store(tunnelSession, &ConnectionInfo{
		Session:     tunnelSession,
		ParentSess:  0,
		Host:        r.Host,
		Method:      "Tcp-Keep-Alive",
		URL:         r.Host,
		RemoteAddr:  remoteAddr,
		Protocol:    "HTTP_MUX",
		StartTime:   time.Now(),
		Status:      "Active",
		UploadRef:   &topctx.TrafficCounter.req_sum,
		DownloadRef: &topctx.TrafficCounter.resp_sum,
		OnClose:     func() { clientConn.Close() },
	})
	defer proxy.MarkConnectionClosed(tunnelSession)
	// ========== 虚拟隧道创建结束 ==========

	// ========== 定义统一的请求处理函数（消除首个请求与后续请求的代码重复） ==========
	processRequest := func(req *http.Request) bool {
		// 每次请求内部创建 context，并在函数结束回收，避免 defer 堆积在外部循环中
		requestContext, finishRequest := context.WithCancel(req.Context())
		req = req.WithContext(requestContext)
		defer finishRequest()

		// URL 确保是绝对路径，移动到上面以便在 Connections.Store 时能获取到完整 URL
		if !req.URL.IsAbs() {
			var urlErr error
			req.URL, urlErr = url.Parse("http://" + req.Host + req.URL.String())
			if urlErr != nil {
				proxy.Logger.Printf("WARN: URL 解析失败: %v", urlErr)
				return false
			}
		}

		ctxt := &Pcontext{
			core_proxy:     proxy,
			Req:            req,
			parCtx:         topctx, // 指向虚拟隧道
			UserData:       topctx.UserData,
			RoundTripper:   topctx.RoundTripper,
			TrafficCounter: &TrafficCounter{},
			Session:        atomic.AddInt64(&proxy.sess, 1),
		}
		ctxt.StartCapture(tunnelSession) // 父隧道 session

		// 注册连接（指向虚拟隧道）
		proxy.Connections.Store(ctxt.Session, &ConnectionInfo{
			Session:      ctxt.Session,
			ParentSess:   tunnelSession, // 指向虚拟隧道
			Host:         req.Host,
			Method:       req.Method,
			URL:          req.URL.String(),
			RemoteAddr:   remoteAddr,
			Protocol:     "HTTP-MITM",
			StartTime:    time.Now(),
			Status:       "Active",
			PuploadRef:   &topctx.TrafficCounter.req_sum,  // 父隧道上行引用
			PdownloadRef: &topctx.TrafficCounter.resp_sum, // 父隧道下行引用
			UploadRef:    &ctxt.TrafficCounter.req_sum,
			DownloadRef:  &ctxt.TrafficCounter.resp_sum,
			OnClose:      func() { finishRequest() },
		})
		defer proxy.MarkConnectionClosed(ctxt.Session)

		req.RemoteAddr = remoteAddr
		ctxt.Log_P("req %v", req.Host)
		ctxt.Req = req

		// 请求过滤（RemoveProxyHeaders 移到 filterRequest 之后，与 https.go 一致）
		req, resp := proxy.filterRequest(req, ctxt)

		ctxt.CaptureRequest(req)

		if resp == nil {
			RemoveProxyHeaders(ctxt, req)
			var err error
			resp, err = func() (*http.Response, error) {
				defer req.Body.Close()
				return ctxt.RoundTrip(req)
			}()
			if err != nil {
				ctxt.SetCaptureError(err)
				ctxt.WarnP("RoundTrip 失败: %v", err)
				httpErrorNoClose(clientConn, ctxt, err)
				return false
			}
			ctxt.Log_P("resp %v", resp.Status)
		}

		// bodyModified 检测：保存原始 Body 用于 WebSocket 和 chunked 判断
		origBody := resp.Body
		resp = proxy.filterResponse(resp, ctxt)
		bodyModified := resp.Body != origBody
		defer resp.Body.Close()

		// WebSocket 检测
		isWebsocket := isWebSocketHandshake(resp.Header)
		if isWebsocket {
			ctxt.SetCaptureSkip()
		}

		// RFC7230 头部处理
		if isWebsocket || req.Method == http.MethodHead {
			// HEAD 请求和 WebSocket 不修改 Content-Length
		} else if (resp.StatusCode >= 100 && resp.StatusCode < 200) ||
			resp.StatusCode == http.StatusNoContent {
			resp.Header.Del("Content-Length")
		} else if bodyModified {
			resp.Header.Del("Content-Length")
			resp.Header.Set("Transfer-Encoding", "chunked")
		}

		if !isWebsocket && !proxy.Config.GetConfig().ConnectMaintain {
			resp.Header.Set("Connection", "close")
		}

		// 手动写回响应头
		statusCode := strconv.Itoa(resp.StatusCode) + " "
		text := strings.TrimPrefix(resp.Status, statusCode)
		if _, err := io.WriteString(clientConn, "HTTP/1.1 "+statusCode+text+"\r\n"); err != nil {
			ctxt.WarnP("写回响应状态失败: %v", err)
			return false
		}
		if err := resp.Header.Write(clientConn); err != nil {
			ctxt.WarnP("写回响应头失败: %v", err)
			return false
		}
		if _, err := io.WriteString(clientConn, "\r\n"); err != nil {
			ctxt.WarnP("写回响应头结束符失败: %v", err)
			return false
		}

		// WebSocket 处理：使用 origBody 获取底层 ReadWriter
		// （filterResponse 的 respBodyReader 包装器不实现 Write 方法）
		if isWebsocket {
			ctxt.Log_P("Response looks like websocket upgrade.")
			wsConn, ok := origBody.(io.ReadWriter)
			if !ok {
				ctxt.WarnP("Unable to use Websocket connection")
				return false
			}
			proxy.proxyWebsocket(ctxt, wsConn, clientConn)
			return false
		}

		// RFC7230 Body 写回
		if req.Method == http.MethodHead ||
			(resp.StatusCode >= 100 && resp.StatusCode < 200) ||
			resp.StatusCode == http.StatusNoContent ||
			resp.StatusCode == http.StatusNotModified {
			// RFC7230: 这些情况不写 Body
		} else if bodyModified {
			chunked := newChunkedWriter(clientConn)
			if _, err := io.Copy(chunked, resp.Body); err != nil {
				ctxt.WarnP("写回 chunked 响应体失败: %v", err)
				return false
			}
			if err := chunked.Close(); err != nil {
				ctxt.WarnP("关闭 chunked writer 失败: %v", err)
				return false
			}
			if _, err := io.WriteString(clientConn, "\r\n"); err != nil {
				ctxt.WarnP("写回 chunked trailer 失败: %v", err)
				return false
			}
		} else {
			if _, err := io.Copy(clientConn, resp.Body); err != nil {
				ctxt.WarnP("写回响应体失败: %v", err)
				return false
			}
		}

		// 检查 Connection: close
		if resp.Close || strings.EqualFold(resp.Header.Get("Connection"), "close") {
			ctxt.WarnP("收到服务器close响应, client->proxy->target连接关闭")
			return false
		}

		return true
	}

	// ========== 第二阶段：处理首个请求（Go 标准库已解析） ==========
	if !processRequest(r) {
		return
	}

	// ========== 第三阶段：循环处理后续 Keep-Alive 请求 ==========
	// 这里就需要特殊处理了。
	// 在有connect请求的情况下，如果client没有收到200 OK不会发送有效数据，所以不用担心bufrw中残留数据问题。
	// 但这里没有connect请求，所以Go标准库第一次读取时会读取到client大量的有效数据并缓存。此时劫持连接后，
	// bufrw中存在大量缓存的有效数据。我们必须把他们一起读出来，所以这里我们创建了NewRequestReaderWithBufio。
	reqReader := http1parser.NewRequestReaderWithBufio(
		proxy.Config.GetConfig().PreventParseHeader,
		clientConn,
		bufrw.Reader,
	)

	// 手动连接复用
	for !reqReader.IsEOF() {
		req, readErr := reqReader.ReadRequest()
		isConnClosed := httpMitmCheckError(readErr)
		// isConnClosed为true表示非代理错误，而是client和target的连接主动关闭问题
		if readErr != nil && !isConnClosed {
			// isConnClosed为false表示非代理协议解析错误，可能数据包损坏或者是https协议
			proxy.Logger.Printf("WARN: http协议解析错误1, 检查请求协议是否为http. parser errror: %+#v", err)
		}
		if readErr != nil {
			return
		}
		if !processRequest(req) { // 同样无需在外层处理context
			return
		}
	}
}
