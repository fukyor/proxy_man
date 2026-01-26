package main

import (
	"flag"
	"log"
	"net/http"
	"proxy_man/mproxy"
	"proxy_man/proxysocket"
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
	proxy.PreventParseHeader = false
	proxy.KeepDestHeaders = false

	// 使用 LogCollector 包装原有 Logger
	proxy.Logger = mproxy.NewLogCollector(proxy.Logger)

	// mproxy.PrintReqHeader(proxy)
	// mproxy.PrintRespHeader(proxy)
	mproxy.AddTrafficMonitor(proxy)
	//mproxy.StatusChange(proxy)
	//mproxy.HttpMitmMode(proxy)
	mproxy.HttpsMitmMode(proxy)


	// 启动 WebSocket 控制服务
	if !proxysocket.StartControlServer(proxy, ":8000", "123") {
		log.Fatal("websocket server启动失败")
	}

	s := http.Server{
		Addr: *addr,
		Handler: proxy,
	}
	if err := s.ListenAndServe(); err != nil{
		log.Fatal("服务器错误", err)
	}

}