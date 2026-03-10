package mproxy

import (
	//"net"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"proxy_man/myminio"
	"regexp"
	"sync"
	"time"
)

/*
需要作为server实现handler处理请求，也要作为client完成连接。
就和proxylab代理服务器一样，当accept一个socket后，立马就要openclientfd。
然后处理socket数据并通过clientfd转发。
*/
type CoreHttpServer struct {
	Transport          *http.Transport // 作为client端转发请求
	DirectHandler      http.Handler
	reqHandlers        []ReqHandler                                                           // 封装请求过滤器
	respHandlers       []RespHandler                                                          // 封装响应过滤器
	httpsHandlers      []HttpsHandler                                                         // 连接策略选择器
	ConnectDial        func(network string, addr string) (net.Conn, error)                    // 默认二级代理
	ConnectWithReqDial func(req *http.Request, network string, addr string) (net.Conn, error) // 规则代理

	ConnectionErrHandler func(conn io.Writer, ctx *Pcontext, err error)

	Logger      Logger
	Config      *ConfigManager              // 并发安全的配置管理器，所有配置读取通过 Config.GetConfig()
	Router      *Router                     // 路由引擎实例
	MinioConfig *myminio.MinioConfigManager // MinIO 配置管理器
	MinioClient *myminio.Client             // MinIO 客户端实例
	sess        int64                       // 全局日志ID，每来一个请求都加1

	Connections sync.Map // int64 (Session) -> *ConnectionInfo
}

var Port = regexp.MustCompile(`:\d+$`)

type flushWriter struct {
	w io.Writer
}

func (fw flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	if f, ok := fw.w.(http.Flusher); ok {
		// only flush if the Writer implements the Flusher interface.
		f.Flush()
	}

	return n, err
}

// 完成响应头封装，通常keepDestHeaders=false。只有在我们需要在响应中加入自定义字段时才为true。
func buildHeaders(dst, src http.Header, keepDestHeaders bool) {
	if !keepDestHeaders {
		for k := range dst {
			dst.Del(k)
		}
	}
	for k, vs := range src {
		dst[k] = append([]string(nil), vs...)
	}
}

// 移除代理头部
func RemoveProxyHeaders(ctx *Pcontext, r *http.Request) {
	r.RequestURI = "" // RequestURI是服务器字段，proxy作为客户端发送需要清空，由http.roundtrip根据r.Url自动填充
	ctx.Log_P("Sending request %v %v", r.Method, r.URL.String())
	if !ctx.core_proxy.Config.GetConfig().KeepAcceptEncoding {
		r.Header.Del("Accept-Encoding")
	}
	r.Header.Del("Proxy-Connection")
	r.Header.Del("Proxy-Authenticate")
	r.Header.Del("Proxy-Authorization")

	// 如果是websocket握手则需保留"Connection: upgrade" 头部
	if !isWebSocketHandshake(r.Header) {
		r.Header.Del("Connection")
	}
}

// InitMinio 初始化 MinIO 并挂载到 proxy 实例
func InitMinio(proxy *CoreHttpServer) {
	minioCM := myminio.NewMinioConfigManager(
		filepath.Join(GetExecutableDir(), "myminio", "minio.json"),
	)
	proxy.MinioConfig = minioCM

	client, err := myminio.NewClient(minioCM)
	if err != nil {
		log.Fatalf("❌ %v", err)
	}

	proxy.MinioClient = client
	minioCfg := minioCM.GetConfig()
	log.Printf("MinIO 存储已启用: %s/%s", minioCfg.Endpoint, minioCfg.Bucket)
}

/*****************构建责任链request过滤*********************/
func (proxy *CoreHttpServer) filterRequest(r *http.Request, ctx *Pcontext) (req *http.Request, resp *http.Response) {
	req = r
	for _, h := range proxy.reqHandlers {
		// 如果返回req和resp!=nil表示需要执行过滤，如果resp==nil则无需过滤
		req, resp = h.Handle(req, ctx)
		if resp != nil {
			break
		}
	}
	return
}

/*****************构建责任链response过滤*********************/
func (proxy *CoreHttpServer) filterResponse(respOrig *http.Response, ctx *Pcontext) (resp *http.Response) {
	resp = respOrig
	for _, h := range proxy.respHandlers {
		ctx.Resp = resp // 每次在ctx中迭代上一次的处理后的resp
		resp = h.Handle(resp, ctx)
	}
	return
}

func (proxy *CoreHttpServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		//调用https处理器
		proxy.MyHttpsHandle(w, r)
	} else {
		//调用http处理器,
		proxy.MyHttpHandle(w, r)

	}
}

func NewCoreHttpSever() *CoreHttpServer {
	core_proxy := &CoreHttpServer{
		Logger: log.New(os.Stderr, "", log.LstdFlags),
		DirectHandler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			http.Error(w, "非代理请求This is a proxy server. Does not respond to non-proxy requests.", http.StatusInternalServerError)
		}),
	}

	// 设置 Transport（闭包捕获 core_proxy 变量）
	core_proxy.Transport = &http.Transport{
		TLSClientConfig: tlsClientSkipVerify,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			// 运行时检查配置（此时 core_proxy.Config 已在 main.go 中初始化）
			if core_proxy.Config != nil {
				cfg := core_proxy.Config.GetConfig()
				if isSelfLoop(addr, cfg.Port, cfg.PublicIPs) {
					return nil, fmt.Errorf("proxy self-loop detected: target %s matches proxy port %d", addr, cfg.Port)
				}
			}
			return (&net.Dialer{
				Timeout:   6 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext(ctx, network, addr)
		},
		TLSHandshakeTimeout: 6 * time.Second,
		MaxIdleConns:        300,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	}

	return core_proxy
}
