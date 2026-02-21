package mproxy

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

func (proxy *CoreHttpServer) MyHttpHandle(w http.ResponseWriter, r *http.Request){
	var err error 
	var oriBody io.ReadCloser

	if !r.URL.IsAbs(){
		proxy.DirectHandler.ServeHTTP(w, r)
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	r = r.WithContext(ctx)
	defer cancel() // 确保函数退出时清理 Context，避免内存泄露，context本质也是通道占用内存

	ctxt := &Pcontext{
		core_proxy: proxy,
		Req: r,
		TrafficCounter: &TrafficCounter{},
		Session: atomic.AddInt64(&proxy.sess, 1),
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

	if resp == nil{
		RemoveProxyHeaders(ctxt, r)
	}

	resp, err = ctxt.RoundTrip(r) // 发起一次http请求
	
	if err != nil {
		ctxt.Error = err
	}
	if resp != nil{
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

	if resp == nil{
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
		return  // hanler函数结束后，go会自动释放连接
	}

	//不用担心Content-Length被删除的问题，会自动降级为chunked模式，go服务器自动处理chunked传输
	if oriBody != resp.Body {
		resp.Header.Del("Content-Length")
	}
	// 封装响应头
	if !isWebsocket && !proxy.ConnectMaintain{
		resp.Header.Set("Connection", "close")
	}
	buildHeaders(w.Header(), resp.Header, proxy.KeepDestHeaders)
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