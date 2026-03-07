package mproxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"proxy_man/http1parser"
	"proxy_man/signer"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// readBufferedConn 用于首字节嗅探后将已读字节"放回"连接
type readBufferedConn struct {
	net.Conn
	r io.Reader
}

func (c *readBufferedConn) Read(p []byte) (int, error) {
	return c.r.Read(p)
}

// TLS Record 类型：Handshake = 22 (0x16)
const _tlsRecordTypeHandshake = byte(22)

type ConnectActionSelecter int

var _errorRespMaxLength int64 = 500

const (
	ConnectAccept = iota
	ConnectReject
	ConnectMitm
	ConnectHTTPMitm
)

var (
	OkConnect       = &ConnectAction{Action: ConnectAccept, TLSConfig: TLSConfigFromCA(&Proxy_ManCa)}
	HTTPMitmConnect = &ConnectAction{Action: ConnectHTTPMitm, TLSConfig: TLSConfigFromCA(&Proxy_ManCa)}
	MitmConnect     = &ConnectAction{Action: ConnectMitm, TLSConfig: TLSConfigFromCA(&Proxy_ManCa)}
)

type ConnectAction struct {
	Action    ConnectActionSelecter
	Hijack    func(req *http.Request, client net.Conn, ctx *Pcontext)
	TLSConfig func(host string, ctx *Pcontext) (*tls.Config, error)
}

// 这是处理tcp连接的核心接口和函数，把他们单独抽象为一个接口
type halfClosable interface {
	net.Conn
	CloseWrite() error
	CloseRead() error
}

var _ halfClosable = (*net.TCPConn)(nil)

func httpMitmCheckError(err error) (isConnClosed bool) {
	isConnClosed = errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed)
	// net.OpError 表示网络层面的错误，通常意味着连接已被对方关闭
	if err != nil && !isConnClosed {
		var netErr *net.OpError
		if errors.As(err, &netErr) {
			// 网络操作错误通常表示连接关闭，不打印警告
			isConnClosed = true
		}
	}
	return
}

func stripPort(s string) string {
	var ix int
	if strings.Contains(s, "[") && strings.Contains(s, "]") {
		// ipv6 address example: [2606:4700:4700::1111]:443
		// strip '[' and ']'
		s = strings.ReplaceAll(s, "[", "")
		s = strings.ReplaceAll(s, "]", "")

		ix = strings.LastIndexAny(s, ":")
		if ix == -1 {
			return s
		}
	} else {
		// ipv4
		ix = strings.IndexRune(s, ':')
		if ix == -1 {
			return s
		}
	}
	return s[:ix]
}

func TLSConfigFromCA(ca *tls.Certificate) func(host string, ctx *Pcontext) (*tls.Config, error) {
	return func(host string, ctx *Pcontext) (*tls.Config, error) {
		var err error
		var cert *tls.Certificate

		hostname := stripPort(host)
		config := defaultTLSConfig.Clone()
		ctx.Log_P("signing for %s", stripPort(host))

		genCert := func() (*tls.Certificate, error) {
			return signer.SignHost(*ca, []string{hostname})
		}
		if ctx.certStore != nil {
			cert, err = ctx.certStore.Fetch(hostname, genCert)
		} else {
			cert, err = genCert()
		}

		if err != nil {
			ctx.WarnP("Cannot sign host certificate with provided CA: %s", err)
			return nil, err
		}

		config.Certificates = append(config.Certificates, *cert)
		return config, nil
	}
}

func (proxy *CoreHttpServer) dial(ctx *Pcontext, network, addr string) (c net.Conn, err error) {
	// 用户自定义二级代理，用于扩展规则代理
	if ctx.Dialer != nil {
		return ctx.Dialer(ctx.Req.Context(), network, addr)
	}
	// 作为最底层的 TCP 拨号，无需再重复打印复杂的路由逻辑
	// 避免与上层 Router 的日志混淆
	return net.Dial(network, addr)
}

func (proxy *CoreHttpServer) connectDial(ctx *Pcontext, network, addr string) (c net.Conn, err error) {
	if ctx.Dialer != nil {
		return ctx.Dialer(ctx.Req.Context(), network, addr)
	}

	if proxy.ConnectDial == nil && proxy.ConnectWithReqDial == nil {
		return proxy.dial(ctx, network, addr)
	}

	if !proxy.Config.GetConfig().RouteEnable {
		return proxy.dial(ctx, network, addr)
	}

	// 完成规则代理完成规则转发
	if proxy.ConnectWithReqDial != nil {
		return proxy.ConnectWithReqDial(ctx.Req, network, addr)
	}

	// 默认二级代理，通过https_proxy环境变量设置
	// 尽可能先通过ConnectWithReqDial规则代理完成规则转发
	proxy.Logger.Printf("WARN: [路由匹配] 回退默认二级代理 -> HTTPS_PROXY")
	return proxy.ConnectDial(network, addr)
}

func DialerFromEnv(proxy *CoreHttpServer) func(network, addr string) (net.Conn, error) {
	httpsProxy := os.Getenv("HTTPS_PROXY")
	if httpsProxy == "" {
		httpsProxy = os.Getenv("https_proxy")
	}
	if httpsProxy == "" {
		return nil
	}
	return proxy.NewConnectDialToProxy(httpsProxy)
}

func (proxy *CoreHttpServer) NewConnectDialToProxy(httpsProxy string) func(network, addr string) (net.Conn, error) {
	return proxy.NewConnectDialToProxyWithHandler(httpsProxy, nil)
}

func (proxy *CoreHttpServer) NewConnectDialToProxyWithHandler(
	httpsProxy string,
	connectReqHandler func(req *http.Request),
) func(network, addr string) (net.Conn, error) {
	u, err := url.Parse(httpsProxy)
	if err != nil {
		return nil
	}
	// 二级普通代理。我们暂时不考虑二级加密代理
	if u.Scheme == "" || u.Scheme == "http" {
		if !strings.ContainsRune(u.Host, ':') {
			u.Host += ":80"
		}
		return func(network, addr string) (net.Conn, error) {
			connectReq := &http.Request{
				Method: http.MethodConnect,
				URL:    &url.URL{Opaque: addr},
				Host:   addr,
				Header: make(http.Header),
			}
			if connectReqHandler != nil {
				// 可通过connectReqHandler注入自定义header等进行认证，也可修改二级代理地址
				// 二级代理基本不需要使用，因为通过proxy.dial和proxy.ConnectDialWithReq
				// 已经完全满足灵活的二级代理，这里只考虑需要header认证时使用
				connectReqHandler(connectReq)
			}
			// 建立tcp连接
			c, err := proxy.dial(&Pcontext{Req: &http.Request{}}, network, u.Host)
			if err != nil {
				return nil, err
			}
			// 发起connect请求
			_ = connectReq.Write(c)
			// Read response.
			// Okay to use and discard buffered reader here, because
			// TLS server will not speak until spoken to.
			br := bufio.NewReader(c)
			resp, err := http.ReadResponse(br, connectReq)
			if err != nil {
				_ = c.Close()
				return nil, err
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				resp, err := io.ReadAll(io.LimitReader(resp.Body, _errorRespMaxLength))
				if err != nil {
					return nil, err
				}
				_ = c.Close()
				return nil, errors.New("proxy refused connection" + string(resp))
			}
			// 普通隧道
			log.Println("二级隧道建立*******************************")
			return c, nil
		}
	}
	log.Println("Warn: 只支持二级普通代理，检查是否误用二级加密代理")
	return nil
}

func httpError(w io.WriteCloser, ctx *Pcontext, err error) {
	if ctx.core_proxy.ConnectionErrHandler != nil {
		ctx.core_proxy.ConnectionErrHandler(w, ctx, err)
	} else {
		errorMessage := err.Error()
		// 直接回写响应头
		errStr := fmt.Sprintf(
			"HTTP/1.1 502 Bad Gateway\r\nContent-Type: text/plain\r\nContent-Length: %d\r\n\r\n%s",
			len(errorMessage),
			errorMessage,
		)
		if _, err := io.WriteString(w, errStr); err != nil {
			ctx.WarnP("Error responding to client: %s", err)
		}
	}
	if err := w.Close(); err != nil {
		ctx.WarnP("Error closing client connection: %s", err)
	}
}

// httpErrorNoClose 写入错误响应但不关闭连接
// 用于 HTTP-MITM 模式，连接的关闭统一由外层 defer clientConn.Close() 负责
func httpErrorNoClose(w io.Writer, ctx *Pcontext, err error) {
	if ctx.core_proxy.ConnectionErrHandler != nil {
		ctx.core_proxy.ConnectionErrHandler(w, ctx, err)
	} else {
		errorMessage := err.Error()
		errStr := fmt.Sprintf(
			"HTTP/1.1 502 Bad Gateway\r\nContent-Type: text/plain\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
			len(errorMessage),
			errorMessage,
		)
		if _, writeErr := io.WriteString(w, errStr); writeErr != nil {
			ctx.WarnP("Error responding to client: %s", writeErr)
		}
	}
}

func copyAndClose(ctx *Pcontext, dst, src halfClosable, wg *sync.WaitGroup) {
	_, err := io.Copy(dst, src)
	if err != nil && !errors.Is(err, net.ErrClosed) {
		ctx.WarnP("Error copying to client: %s", err.Error())
	}

	_ = dst.CloseWrite()
	_ = src.CloseRead()
	wg.Done()
}

// 缺陷一：
// 是直接调用close会导致io.Copy报错连接异常关闭，比如client FIN->proxy，proxy会立刻close target
// 导致target->proxy端的管道突然关闭，io.Copy报错
// 缺陷二：
// 没有半关闭的情况下，FIN_wait2无法收到额外的结束消息。
func copyOrWarn(ctx *Pcontext, dst io.Writer, src io.Reader) error {
	_, err := io.Copy(dst, src)
	if err != nil && errors.Is(err, net.ErrClosed) {
		// Discard closed connection errors,在没有半关闭的情况下，并发时连接关闭是正常的，不可控的
		err = nil
	} else if err != nil {
		ctx.WarnP("Error copying to client: %s", err)
	}
	return err
}

func (proxy *CoreHttpServer) MyHttpsHandle(w http.ResponseWriter, r *http.Request) {
	// 统计connect的session号，是最基础的tcp连接，所有数据都通过该隧道
	topctx := &Pcontext{
		core_proxy:     proxy,
		Req:            r,
		TrafficCounter: &TrafficCounter{},
		Session:        atomic.AddInt64(&proxy.sess, 1),
	}

	// 创建hijack
	hijk, ok := w.(http.Hijacker)
	if !ok {
		panic("httpsSever not support hijack")
	}

	connFromClinet, _, err := hijk.Hijack()

	if err != nil {
		panic("hijack connection fail" + err.Error())
	}

	// 创建顶层隧道连接记录
	tunnelSession := topctx.Session
	proxy.Connections.Store(tunnelSession, &ConnectionInfo{
		Session:     tunnelSession,
		ParentSess:  0,
		Host:        r.URL.Host,
		Method:      "CONNECT",
		URL:         r.URL.Host,
		RemoteAddr:  r.RemoteAddr,
		Protocol:    "TUNNEL",
		StartTime:   time.Now(),
		Status:      "Active",
		UploadRef:   &topctx.TrafficCounter.req_sum,
		DownloadRef: &topctx.TrafficCounter.resp_sum,
		OnClose:     func() { connFromClinet.Close() },
	})

	topctx.Log_P("处理器数量Have %d CONNECT handlers", len(proxy.httpsHandlers))

	strategy, host := OkConnect, r.URL.Host

	// MITM 开关：根据端口选择默认策略
	if !proxy.Config.GetConfig().MitmEnabled {
		strategy = OkConnect
	} else {
		strategy = MitmConnect
	}

	// 切换处理状态（httpsHandlers 仍可覆盖上面的默认值）
	for i, h := range proxy.httpsHandlers {
		new_strategy, newhost := h.HandleConnect(host, topctx)
		// 和resphook一样返回nil说明没有匹配上条件，如果不等于nil则匹配上了就替换新的策略
		if new_strategy != nil {
			strategy, host = new_strategy, newhost
			topctx.Log_P("Excuted %dth handler: %v %s", i, strategy, host)
			break
		}
	}

	switch strategy.Action {
	case ConnectAccept:
		if !Port.MatchString(host) {
			host += ":80"
		}
		connRemoteSite, err := proxy.connectDial(topctx, "tcp", host)

		if err != nil {
			topctx.WarnP("拨号获取套接字错误Error dialing to %s: %s", host, err.Error())
			httpError(connFromClinet, topctx, err) // 如果出错手动关闭客户端连接
			proxy.MarkConnectionClosed(tunnelSession)
			return
		}
		topctx.Log_P("Accepting CONNECT to %s", host)

		_, err = connFromClinet.Write([]byte("HTTP/1.0 200 Connection established\r\n\r\n"))
		if err != nil {
			topctx.WarnP("200 Connection fail established")
			proxy.MarkConnectionClosed(tunnelSession)
			return
		}

		// 用client端统计上行流量和下行流量
		proxyClientTCP, clientOK := newTunnelTrafficClient(connFromClinet)
		proxyClientTCPNo := newtunnelTrafficClientNoClosable(connFromClinet)

		Counter_Ctxt := &Pcontext{
			core_proxy:                    proxy,
			Req:                           r,
			tunnelTrafficClient:           proxyClientTCP,
			tunnelTrafficClientNoClosable: proxyClientTCPNo,
			Session:                       topctx.Session,
		}

		url_, err := url.Parse("http:" + r.URL.String())
		// 注册连接（隧道模式作为整体长连接）
		proxy.Connections.Store(Counter_Ctxt.Session, &ConnectionInfo{
			Session:     topctx.Session,
			Host:        host,
			ParentSess:  tunnelSession,
			Method:      "TUNNEL",
			URL:         url_.String(),
			RemoteAddr:  r.RemoteAddr,
			Protocol:    "HTTPS-Tunnel",
			StartTime:   time.Now(),
			Status:      "Active",
			UploadRef:   &proxyClientTCP.nread,  // nread = 从客户端读 = Upload
			DownloadRef: &proxyClientTCP.nwrite, // nwrite = 写给客户端 = Download
			OnClose:     func() { connFromClinet.Close() },
		})
		// 注册回调完成流量统计，清理顶层隧道连接记录
		proxyClientTCP.onClose = func() {
			topctx.Log_P("[流量统计] 上行: %d | 下行: %d | 总计: %d ",
				proxyClientTCP.nread,
				proxyClientTCP.nwrite,
				proxyClientTCP.nread+proxyClientTCP.nwrite,
			)
			proxy.MarkConnectionClosed(topctx.Session)
		}
		proxyClientTCPNo.onClose = func() {
			topctx.Log_P("[流量统计] 上行: %d | 下行: %d | 总计: %d ",
				proxyClientTCPNo.nread,
				proxyClientTCPNo.nwrite,
				proxyClientTCPNo.nread+proxyClientTCPNo.nwrite,
			)
			proxy.MarkConnectionClosed(topctx.Session)
		}

		proxy.filterRequest(r, Counter_Ctxt)

		targetTCP, targetOK := connRemoteSite.(halfClosable)

		if targetOK && clientOK {
			go func() {
				var wg sync.WaitGroup
				wg.Add(2)
				// io.copy的退出取决于参数接口的行为，如果参数是req.Body / resp.Body就会在读到Content-Length / Chunk结束符退出。
				// 这里参数是net.Conn接口，net.Conn的底层行为是和tcp socket的生命周期一致，会一直保持到tcp断开
				go copyAndClose(topctx, targetTCP, proxyClientTCP, &wg)
				go copyAndClose(topctx, proxyClientTCP, targetTCP, &wg)
				wg.Wait()
				// 最后调用close保证了连接能够正常断开
				proxyClientTCP.Close()
				targetTCP.Close()
			}()
		} else {
			go func() {
				// 使用包装后的 reader/writer 进行流量统计
				err := copyOrWarn(topctx, targetTCP, proxyClientTCPNo)
				if err != nil && proxy.ConnectionErrHandler != nil {
					// 向客户端发送错误信息
					proxy.ConnectionErrHandler(connFromClinet, topctx, err)
				}
				_ = targetTCP.Close()
			}()

			go func() {
				_ = copyOrWarn(topctx, proxyClientTCPNo, targetTCP)
				_ = proxyClientTCP.Close()
			}()
		}
	// 调用用户自己的劫持逻辑，暂时没想到怎么使用
	// case ConnectHijack:
	// 	strategy.Hijack(r, connFromClinet, ctxt)
	// 统一 MITM 分支：首字节嗅探自动区分 HTTP/HTTPS
	case ConnectHTTPMitm, ConnectMitm:
		_, _ = connFromClinet.Write([]byte("HTTP/1.0 200 OK\r\n\r\n"))
		topctx.Log_P("MITM 模式启动, 协议自动嗅探")

		defer proxy.MarkConnectionClosed(tunnelSession)

		// --- 1. 首字节嗅探 ---
		readBuffer := bufio.NewReader(connFromClinet)
		peek, _ := readBuffer.Peek(1)
		isTLS := len(peek) > 0 && peek[0] == _tlsRecordTypeHandshake

		var mitmClientConn net.Conn = &readBufferedConn{Conn: connFromClinet, r: readBuffer}
		defer mitmClientConn.Close()

		scheme := "http"
		protocolLabel := "HTTP-MITM"

		// --- 2. TLS 握手（仅 TLS 流量） ---
		if isTLS {
			scheme = "https"
			protocolLabel = "HTTPS-MITM"

			tlsConfig := defaultTLSConfig
			if strategy.TLSConfig != nil {
				var err error
				tlsConfig, err = strategy.TLSConfig(host, topctx)
				if err != nil {
					httpError(mitmClientConn, topctx, err)
					return
				}
			}

			tlsConn := tls.Server(mitmClientConn, tlsConfig)
			if err := tlsConn.HandshakeContext(topctx.Req.Context()); err != nil {
				topctx.WarnP("TLS 握手失败 Cannot handshake client %v %v", r.Host, err)
				return
			}
			mitmClientConn = tlsConn
		}

		// --- 3. 请求循环（从客户端隧道读取 HTTP/1.1 Keep-Alive 请求流）---
		// 注：for 循环是读取客户端请求的标准模式
		// proxy→target 的连接复用由 Transport 连接池自动管理
		reqReader := http1parser.NewRequestReader(proxy.Config.GetConfig().PreventParseHeader, mitmClientConn)

		for !reqReader.IsEOF() {
			req, err := reqReader.ReadRequest()

			ctxt := &Pcontext{
				Req:            req,
				Session:        atomic.AddInt64(&proxy.sess, 1),
				core_proxy:     proxy,
				parCtx:         topctx,
				UserData:       topctx.UserData,     // 继承用户数据
				RoundTripper:   topctx.RoundTripper, // 继承自定义 RoundTripper
				TrafficCounter: &TrafficCounter{},
			}
			ctxt.StartCapture(tunnelSession)

			if err != nil && !errors.Is(err, io.EOF) {
				ctxt.WarnP("协议解析错误 Cannot read request from client %v %v", r.Host, err)
			}
			if err != nil {
				return
			}

			req.RemoteAddr = r.RemoteAddr
			ctxt.Log_P("client ip: %v, request Host %v", r.RemoteAddr, r.Host)

			if !strings.HasPrefix(req.URL.String(), scheme+"://") {
				req.URL, err = url.Parse(scheme + "://" + r.Host + req.URL.String())
			}

			requestOk := func(req *http.Request) bool {
				requestContext, finishRequest := context.WithCancel(req.Context())
				req = req.WithContext(requestContext)
				defer finishRequest()

				proxy.Connections.Store(ctxt.Session, &ConnectionInfo{
					Session:      ctxt.Session,
					ParentSess:   tunnelSession,
					Host:         r.Host,
					Method:       req.Method,
					URL:          req.URL.String(),
					RemoteAddr:   r.RemoteAddr,
					Protocol:     protocolLabel,
					StartTime:    time.Now(),
					Status:       "Active",
					PuploadRef:   &ctxt.parCtx.TrafficCounter.req_sum,
					PdownloadRef: &ctxt.parCtx.TrafficCounter.resp_sum,
					UploadRef:    &ctxt.TrafficCounter.req_sum,
					DownloadRef:  &ctxt.TrafficCounter.resp_sum,
					OnClose:      func() { finishRequest() },
				})
				defer proxy.MarkConnectionClosed(ctxt.Session)

				ctxt.Req = req
				req, resp := proxy.filterRequest(req, ctxt)
				ctxt.CaptureRequest(req)

				if resp == nil {
					if err != nil {
						ctxt.SetCaptureError(err)
						if req.URL != nil {
							ctxt.WarnP("Illegal URL %s", scheme+"://"+r.Host+req.URL.Path)
						} else {
							ctxt.WarnP("Illegal URL %s", scheme+"://"+r.Host)
						}
						return false
					}

					RemoveProxyHeaders(ctxt, req)

					// 100-continue 由 Transport 自动处理，无需特殊代码
					resp, err = func() (*http.Response, error) {
						defer req.Body.Close()
						return ctxt.RoundTrip(req)
					}()
					if err != nil {
						ctxt.SetCaptureError(err)
						ctxt.WarnP("Cannot read response from mitm'd server %v", err)
						return false
					}
					ctxt.Log_P("resp %v", resp.Status)
				}

				// bodyModified 检测
				origBody := resp.Body
				resp = proxy.filterResponse(resp, ctxt)
				bodyModified := resp.Body != origBody
				defer resp.Body.Close()

				// WebSocket 检测
				isWebsocket := isWebSocketHandshake(resp.Header)
				if isWebsocket {
					ctxt.SetCaptureSkip()
				}

				// [修复2] RFC7230 头部处理（用 req.Method 替代 resp.Request.Method，修复7）
				if isWebsocket || req.Method == http.MethodHead {
					// HEAD 请求和 WebSocket 不修改 Content-Length
				} else if (resp.StatusCode >= 100 && resp.StatusCode < 200) ||
					resp.StatusCode == http.StatusNoContent {
					resp.Header.Del("Content-Length")
				} else if bodyModified {
					resp.Header.Del("Content-Length")
					resp.Header.Set("Transfer-Encoding", "chunked")
				}

				if !isWebsocket && !proxy.Config.GetConfig().ConnectMaintain {
					resp.Header.Set("Connection", "close")
				}

				// 手动写回响应头
				text := resp.Status
				statusCode := strconv.Itoa(resp.StatusCode) + " "
				text = strings.TrimPrefix(text, statusCode)
				if _, err := io.WriteString(mitmClientConn, "HTTP/1.1"+" "+statusCode+text+"\r\n"); err != nil {
					ctxt.WarnP("Cannot write response HTTP status from mitm'd client: %v", err)
					return false
				}
				if err := resp.Header.Write(mitmClientConn); err != nil {
					ctxt.WarnP("Cannot write response header from mitm'd client: %v", err)
					return false
				}
				if _, err = io.WriteString(mitmClientConn, "\r\n"); err != nil {
					ctxt.WarnP("Cannot write response header end from mitm'd client: %v", err)
					return false
				}

				// WebSocket 处理：使用 origBody 获取底层 ReadWriter
				// （filterResponse 的 respBodyReader 包装器不实现 Write 方法）
				if isWebsocket {
					ctxt.Log_P("Response looks like websocket upgrade.")
					wsConn, ok := origBody.(io.ReadWriter)
					if !ok {
						ctxt.WarnP("Unable to use Websocket connection")
						return false
					}
					proxy.proxyWebsocket(ctxt, wsConn, mitmClientConn)
					proxy.MarkConnectionClosed(ctxt.Session)
					return false
				}

				// [修复2] RFC7230 Body 写回
				if req.Method == http.MethodHead ||
					(resp.StatusCode >= 100 && resp.StatusCode < 200) ||
					resp.StatusCode == http.StatusNoContent ||
					resp.StatusCode == http.StatusNotModified {
					// RFC7230: 这些情况不写 Body
				} else if bodyModified {
					// [修复1] filterResponse 修改了 Body，使用 chunked 编码
					chunked := newChunkedWriter(mitmClientConn)
					if _, err := io.Copy(chunked, resp.Body); err != nil {
						ctxt.WarnP("Cannot write response body: %v", err)
						return false
					}
					if err := chunked.Close(); err != nil {
						ctxt.WarnP("Cannot write chunked EOF: %v", err)
						return false
					}
					if _, err = io.WriteString(mitmClientConn, "\r\n"); err != nil {
						ctxt.WarnP("Cannot write chunked trailer: %v", err)
						return false
					}
				} else {
					if _, err := io.Copy(mitmClientConn, resp.Body); err != nil {
						ctxt.WarnP("Cannot write response body: %v", err)
						return false
					}
				}

				// [修复6] Connection: close 检查
				if resp.Close || strings.EqualFold(resp.Header.Get("Connection"), "close") {
					ctxt.WarnP("收到服务器close响应, client->proxy->target连接关闭")
					return false
				}

				return true
			}

			if !requestOk(req) {
				return
			}
		}
		topctx.Log_P("Connect Tunnel Normal Exiting on Client EOF")
	case ConnectReject:
		if topctx.Resp != nil {
			if err := topctx.Resp.Write(connFromClinet); err != nil {
				topctx.WarnP("Cannot write response that reject http CONNECT: %v", err)
			}
		}
		_ = connFromClinet.Close()
		proxy.MarkConnectionClosed(tunnelSession)
	}
}
