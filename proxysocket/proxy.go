package proxysocket

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"proxy_man/mproxy"
	"proxy_man/myminio"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/rs/cors"
)

//go:embed dist
var embeddedFS embed.FS

var distFS fs.FS
var staticFileServer http.Handler
var indexHTML []byte

func init() {
	var err error
	distFS, err = fs.Sub(embeddedFS, "dist")
	if err != nil {
		log.Fatalf("无法加载嵌入的前端资源: %v", err)
	}
	staticFileServer = http.FileServer(http.FS(distFS))
	indexHTML, err = fs.ReadFile(distFS, "index.html")
	if err != nil {
		log.Fatalf("无法读取嵌入的 index.html: %v", err)
	}
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
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
	MitmDetail  bool       // MITM Exchange 详细信息
	writeMu     sync.Mutex // 保护 WebSocket 写操作
}

// WebSocket Hub
type WebSocketHub struct {
	clients sync.Map // *websocket.Conn -> *Subscription
	proxy   *mproxy.CoreHttpServer
}

var hub *WebSocketHub

type WebsocketServer struct {
	Proxy  *mproxy.CoreHttpServer
	Addr   string
	Secret string
}

// 启动控制服务器
func (ws *WebsocketServer) StartControlServer(cm *mproxy.ConfigManager, router *mproxy.Router) bool {
	hub = &WebSocketHub{proxy: ws.Proxy}
	mux := http.NewServeMux()
	mux.HandleFunc("/start", ws.loginHandler(ws.handleWebSocket))
	mux.HandleFunc("/api/storage/download", myminio.HandleDownload) // MinIO 下载 API
	mux.HandleFunc("/api/config", ws.handleConfig(cm, router))      // 配置管理 API
	mux.HandleFunc("/", handleStaticFiles)                          // 静态文件服务 + SPA fallback

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
	hub.StartMitmDetailPusher()

	var err error
	go func() {
		err = http.ListenAndServe(ws.Addr, corsMiddleware.Handler(mux))
	}()
	// 通道有两种架构，非阻塞和阻塞通道，这里需要阻塞通道
	if err != nil {
		log.Printf("Socket Server failed to start: %v", err)
		return false
	}
	return true
}

func (ws *WebsocketServer) loginHandler(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if ws.Secret != "" {
			token := r.URL.Query().Get("token")
			if token != ws.Secret {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next(w, r)
	}
}

func (ws *WebsocketServer) handleWebSocket(w http.ResponseWriter, r *http.Request) {
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
				ws.Proxy.MarkConnectionClosed(key.(int64))
				return true
			})
		case "closeConnection":
			idFloat, ok := msg["id"].(float64)
			if !ok {
				break
			}
			id := int64(idFloat)
			// 先关闭所有子连接（对子连接调用时 Range 不会匹配到任何项）
			hub.proxy.Connections.Range(func(key, value any) bool {
				info := value.(*mproxy.ConnectionInfo)
				if info.ParentSess == id {
					hub.proxy.CloseAndRemoveConnection(info.Session)
				}
				return true
			})
			// 关闭目标连接自身
			hub.proxy.CloseAndRemoveConnection(id)
		}
	}
}

// handleConfig 处理代理配置的 GET/POST 请求
func (ws *WebsocketServer) handleConfig(cm *mproxy.ConfigManager, router *mproxy.Router) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case "GET":
			cfg := cm.GetConfig()
			json.NewEncoder(w).Encode(cfg)

		case "POST":
			var updated mproxy.ServerConfig
			if err := json.NewDecoder(r.Body).Decode(&updated); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if err := cm.UpdateConfig(&updated); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			// 热重载路由
			if updated.RouteEnable {
				router.ReloadFromConfig(&updated)
			}
			json.NewEncoder(w).Encode(map[string]string{"status": "ok"})

		default:
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

// handleStaticFiles 提供嵌入的前端静态文件，支持 Vue Router History 模式
func handleStaticFiles(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/")
	if path == "" {
		path = "index.html"
	}

	// 尝试打开静态文件
	f, err := distFS.Open(path)
	if err == nil {
		f.Close()
		staticFileServer.ServeHTTP(w, r)
		return
	}

	// 文件不存在 → 返回 index.html，由 Vue Router 接管
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(indexHTML)
}
