package proxysocket

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"log"
	"github.com/gorilla/websocket"
	"github.com/rs/cors"
	"proxy_man/mproxy"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     func(r *http.Request) bool { return true },
	Error: func(w http.ResponseWriter, r *http.Request, status int, reason error) {
		w.Header().Set("Connection", "close")
		w.WriteHeader(status)
		w.Write([]byte("请使用websocket协议"))
		fmt.Println(reason)
	},
}

// 订阅信息
type Subscription struct {
	Traffic     bool
	Connections bool
	Logs        bool
	LogLevel    string
	writeMu     sync.Mutex // 保护 WebSocket 写操作
}

// WebSocket Hub
type WebSocketHub struct {
	clients sync.Map // *websocket.Conn -> *Subscription
	proxy   *mproxy.CoreHttpServer
}

var hub *WebSocketHub

// 启动控制服务器
func StartControlServer(proxy *mproxy.CoreHttpServer, addr string, secret string) bool {
	hub = &WebSocketHub{proxy: proxy}

	mux := http.NewServeMux()
	mux.HandleFunc("/start", loginHandler(secret, handleWebSocket))

	corsMiddleware := cors.New(cors.Options{
		AllowedOrigins:   []string{"*"},
		AllowedMethods:   []string{"GET", "POST", "OPTIONS"},
		AllowedHeaders:   []string{"*"},
		AllowCredentials: true,
	})

	// 启动推送服务
	hub.StartTrafficPusher()
	hub.StartConnectionPusher()
	hub.StartLogPusher()

	var err error
	go func(){
		err = http.ListenAndServe(addr, corsMiddleware.Handler(mux))
	}()
	// 通道有两种架构，非阻塞和阻塞通道，这里需要阻塞通道
	if err != nil {
		log.Printf("Socket Server failed to start: %v", err)
		return false
	}
	return true
}

func loginHandler(secret string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if secret != "" {
			token := r.URL.Query().Get("token")
			if token != secret {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next(w, r)
	}
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		var handshakeErr websocket.HandshakeError
		if errors.As(err, &handshakeErr) {
			fmt.Println("握手失败")
		}
		return
	}
	defer conn.Close()

	// 注册客户端
	sub := &Subscription{LogLevel: "INFO"}
	hub.clients.Store(conn, sub)
	defer hub.clients.Delete(conn)

	// 处理客户端消息
	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			break
		}

		var msg map[string]any
		if err := json.Unmarshal(message, &msg); err != nil {
			continue
		}

		action, _ := msg["action"].(string)
		switch action {
		case "subscribe":
			hub.updateSubscription(sub, msg)
		case "closeAllConnections":
			hub.proxy.Connections.Range(func(key, value any) bool {
				if info, ok := value.(*mproxy.ConnectionInfo); ok && info.OnClose != nil {
					info.OnClose()
				}
				hub.proxy.Connections.Delete(key)
				return true
			})
		}
	}
}