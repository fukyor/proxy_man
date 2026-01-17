package mproxy

import(
	"io"
	"net/http"
	"sync/atomic"
	"strings"
)

func (proxy *CoreHttpServer) MyHttpHandle(w http.ResponseWriter, r *http.Request){
	var err error 
	var oriBody io.ReadCloser

	ctxt := &Pcontext{
		core_proxy: proxy,
		Req: r,
		TrafficCounter: &TrafficCounter{},
		Session: atomic.AddInt64(&proxy.sess, 1),
	}
	
	if !r.URL.IsAbs(){
		proxy.DirectHandler.ServeHTTP(w, r)
		return	
	}

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
	//ctxt.Log_P("Copying response to client %v [%d]", resp.Status, resp.StatusCode)

	//不用担心Content-Length被删除的问题，会自动降级为chunked模式，
	if oriBody != resp.Body {
		resp.Header.Del("Content-Length")
	}
	// 封装响应头
	buildHeaders(w.Header(), resp.Header, proxy.KeepCurHeaders)
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
	//ctxt.Log_P("Copied %v bytes to client error=%v", nr, err)


}