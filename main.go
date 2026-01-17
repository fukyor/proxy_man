package main

import (
	"flag"
	"log"
	"net/http"
	"proxy_man/mproxy"
	"net/http/httptrace"
	// "net/http/httputil"
	// "fmt"
)

func main() {
	verbose := flag.Bool("v", true, "should every proxy request be logged to stdout")
	addr := flag.String("addr", ":8080", "proxy listen address")
	flag.Parse()
	proxy := mproxy.NewCoreHttpSever()
	proxy.Verbose = *verbose
	proxy.AllowHTTP2 = false
	proxy.KeepHeader = false  // 不保留代理头部
	mproxy.PrintReqHeader(proxy)
	mproxy.PrintRespHeader(proxy)
	mproxy.AddTrafficMonitor(proxy)
	//mproxy.StatusChange(proxy)
	mproxy.MitmMode(proxy)

	// 注册一个请求钩子来注入 httptrace
    proxy.HookOnReq().DoFunc(func(req *http.Request, ctx *mproxy.Pcontext) (*http.Request, *http.Response) {
        // 定义 Trace 钩子
        trace := &httptrace.ClientTrace{
            // 当成功获取到连接时（无论是新建还是复用）调用
            GotConn: func(connInfo httptrace.GotConnInfo) {
                remoteAddr := connInfo.Conn.RemoteAddr()
                if connInfo.Reused {
                    ctx.Log_P("[RoundTrip] 复用连接 IP: %s", remoteAddr)
                } else {
                    ctx.Log_P("[RoundTrip] 新建连接 IP: %s", remoteAddr)
                }
            },
        }
        ctxTrace := httptrace.WithClientTrace(req.Context(), trace)
        return req.WithContext(ctxTrace), nil
    })

	s := http.Server{
		Addr: *addr,
		Handler: proxy,
	}
	if err := s.ListenAndServe(); err != nil{
		log.Fatal("服务器错误", err)
	}
	
}