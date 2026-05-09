package main

import (
	"crypto/tls"
	"fmt"
	"log"
	"net/http"
	"os"
	//_ "net/http/pprof"
	"proxy_man/mproxy"
	"proxy_man/proxysocket"
	// "net/http/httputil"
)

func main() {
	proxy := mproxy.NewCoreHttpSever()

	// 1. 初始化配置（显式赋值，无副作用）
	configPath := os.Getenv("PROXY_MAN_CONFIG_PATH")
	if configPath == "" {
		log.Fatal("未设置 PROXY_MAN_CONFIG_PATH，Docker 部署必须显式指定持久化配置文件路径")
	}
	proxy.Config = mproxy.NewConfigManager(configPath)
	cfg := proxy.Config.GetConfig()

	// 2. 日志收集器
	proxy.Logger = mproxy.NewLogCollector(proxy.Logger)

	// 3. MinIO
	mproxy.InitMinio(proxy)

	// 4. pprof
	// go func() {
	// 	log.Println("🔍 性能监控 (pprof) 服务已启动: http://localhost:6060/debug/pprof/")
	// 	if err := http.ListenAndServe(":6060", nil); err != nil {
	// 		log.Printf("pprof 启动失败: %v", err)
	// 	}
	// }()

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
	httpsTLSConfig, err := mproxy.BuildProxyListenerTLSConfig(cfg)
	if err != nil {
		log.Fatal("生成 HTTPS 监听证书失败", err)
	}

	httpServer := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.Port),
		Handler: proxy,
	}

	httpsServer := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.HTTPSPort),
		Handler: proxy,
		// CONNECT 隧道依赖 Hijack，8443 监听必须固定为 HTTP/1.1。
		TLSNextProto: map[string]func(*http.Server, *tls.Conn, http.Handler){},
		TLSConfig:    httpsTLSConfig,
	}

	errCh := make(chan error, 2)

	go func() {
		log.Printf("HTTP 代理服务已启动: 127.0.0.1:%d", cfg.Port)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- fmt.Errorf("HTTP 代理服务器错误: %w", err)
		}
	}()

	go func() {
		log.Printf("HTTPS 代理服务已启动: 127.0.0.1:%d", cfg.HTTPSPort)
		if err := httpsServer.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
			errCh <- fmt.Errorf("HTTPS 代理服务器错误: %w", err)
		}
	}()

	if err := <-errCh; err != nil {
		log.Fatal(err)
	}
}
