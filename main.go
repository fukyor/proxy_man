package main

import (
	"fmt"
	"log"
	"net/http"
	_ "net/http/pprof"
	"proxy_man/mproxy"
	"proxy_man/proxysocket"
	// "net/http/httputil"
)

func main() {
	proxy := mproxy.NewCoreHttpSever()

	// 1. 初始化配置（显式赋值，无副作用）
	proxy.Config = mproxy.NewConfigManager("config.json")
	cfg := proxy.Config.GetConfig()

	// 2. 日志收集器
	proxy.Logger = mproxy.NewLogCollector(proxy.Logger)

	// 3. MinIO
	mproxy.InitMinio(proxy)

	// 4. pprof
	go func() {
		log.Println("🔍 性能监控 (pprof) 服务已启动: http://localhost:6060/debug/pprof/")
		if err := http.ListenAndServe(":6060", nil); err != nil {
			log.Printf("pprof 启动失败: %v", err)
		}
	}()

	// 5. 路由（无需额外参数）
	mproxy.AddRouter(proxy)

	// 6. 访问控制（需在 AddRouter 之后）
	mproxy.AddAccessControl(proxy)

	// 7. 流量监控（需在 AddAccessControl 之后，确保被拦截请求的 Body 不被 MinIO 包装）
	mproxy.AddTrafficMonitor(proxy)

	// 8. WebSocket 控制服务（无需额外参数）
	ws := &proxysocket.WebsocketServer{
		Proxy:  proxy,
		Addr:   ":8000",
		Secret: "123",
	}
	if ws.StartControlServer() {
		log.Println("websocket server 已启动: 127.0.0.1:8000")
	} else {
		log.Fatal("websocket server启动失败")
	}

	// 9. 代理服务器
	s := http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.Port),
		Handler: proxy,
	}
	if err := s.ListenAndServe(); err != nil {
		log.Fatal("服务器错误", err)
	}
}
