package main

import (
	"fmt"
	"log"
	"net/http"
	_ "net/http/pprof"
	"proxy_man/mproxy"
	"proxy_man/myminio"
	"proxy_man/proxysocket"
	// "net/http/httputil"
)

func main() {
	proxy := mproxy.NewCoreHttpSever()

	// 初始化配置管理器
	cm := mproxy.NewConfigManager("config.json")
	cfg := cm.GetConfig()
	proxy.Config = cm

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
		log.Printf("警告: MinIO 初始化失败: %v，Body 捕获功能将被禁用", err)
		return
	} else {
		myminio.GlobalClient = client
		log.Printf("MinIO 存储已启用: %s/%s", minioConfig.Endpoint, minioConfig.Bucket)
	}

	// 我们监听 6060 端口，传 nil 表示使用默认的 DefaultServeMux (里面已经有了 pprof)
	go func() {
		log.Println("🔍 性能监控 (pprof) 服务已启动: http://localhost:6060/debug/pprof/")
		if err := http.ListenAndServe(":6060", nil); err != nil {
			log.Printf("pprof 启动失败: %v", err)
		}
	}()

	// mproxy.PrintReqHeader(proxy)
	// mproxy.PrintRespHeader(proxy)
	mproxy.AddTrafficMonitor(proxy)
	router := mproxy.AddRouter(proxy, cm)
	//mproxy.StatusChange(proxy)
	//mproxy.HttpMitmMode(proxy)
	//mproxy.HttpsMitmMode(proxy)

	// 启动 WebSocket 控制服务
	ws := &proxysocket.WebsocketServer{
		Proxy:  proxy,
		Addr:   ":8000",
		Secret: "123",
	}
	if ws.StartControlServer(cm, router) {
		log.Println("websocket server 已启动: 127.0.0.1:8000")
	}else {
		log.Fatal("websocket server启动失败")
	}

	s := http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.Port),
		Handler: proxy,
	}
	if err := s.ListenAndServe(); err != nil {
		log.Fatal("服务器错误", err)
	}else {
		log.Println("proxy_man server 已启动: 127.0.0.1:8000")
	}

}
