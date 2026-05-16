package mproxy

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ruleMatcher 单条规则的运行时匹配器
type ruleMatcher struct {
	matchFunc func(host string) bool
	ruleType  string
	ruleValue string
}

// AccessController 访问控制器，支持热重载
type AccessController struct {
	proxy          *CoreHttpServer
	mu             sync.RWMutex
	matchers       []ruleMatcher
	blockedClients map[string]bool
}

// NewAccessController 创建访问控制器并绑定 Hook
func NewAccessController(proxy *CoreHttpServer) *AccessController {
	ac := &AccessController{proxy: proxy}

	// HTTP 请求拦截（绑定一次，执行时动态读取 matchers）
	proxy.HookOnReq().DoFunc(func(req *http.Request, ctx *Pcontext) (*http.Request, *http.Response) {
		if !ctx.core_proxy.Config.GetConfig().AccessEnable {
			return req, nil
		}
		host := req.URL.Host
		if host == "" {
			host = req.Host
		}
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		clientIP := ctx.ClientIP
		if clientIP == "" {
			clientIP = getClientIP(req)
		}

		ac.mu.RLock()
		matchers := ac.matchers
		blockedClients := ac.blockedClients
		ac.mu.RUnlock()

		if blockedClients[clientIP] {
			ctx.Log_P("[访问控制] 拦截请求: 客户端=%s, 目标=%s, 规则=%s:%s",
				clientIP, host, "UserIP", clientIP)
			pushInterceptLog(clientIP, host, "UserIP", clientIP)
			return req, ForbiddenResponse(req, "403 Access Denied")
		}

		for _, matcher := range matchers {
			if matcher.matchFunc(host) {
				ctx.Log_P("[访问控制] 拦截请求: 客户端=%s, 目标=%s, 规则=%s:%s",
					clientIP, host, matcher.ruleType, matcher.ruleValue)
				pushInterceptLog(clientIP, host, matcher.ruleType, matcher.ruleValue)
				return req, ForbiddenResponse(req, "403 Access Denied")
			}
		}
		return req, nil
	})

	// HTTPS/CONNECT 拦截
	proxy.HookOnReq().DoConnectFunc(func(host string, ctx *Pcontext) (*ConnectAction, string) {
		if !ctx.core_proxy.Config.GetConfig().AccessEnable {
			return nil, ""
		}
		hostOnly := stripPort(host)
		clientIP := ctx.ClientIP
		if clientIP == "" {
			clientIP = getClientIP(ctx.Req)
		}

		ac.mu.RLock()
		matchers := ac.matchers
		blockedClients := ac.blockedClients
		ac.mu.RUnlock()

		if blockedClients[clientIP] {
			ctx.Log_P("[访问控制] 拦截 CONNECT: 客户端=%s, 目标=%s, 规则=%s:%s",
				clientIP, host, "UserIP", clientIP)
			pushInterceptLog(clientIP, host, "UserIP", clientIP)
			ctx.Resp = ForbiddenResponse(ctx.Req, "403 Access Denied")
			return RejectConnect, host
		}

		for _, matcher := range matchers {
			if matcher.matchFunc(hostOnly) {
				ctx.Log_P("[访问控制] 拦截 CONNECT: 客户端=%s, 目标=%s, 规则=%s:%s",
					clientIP, host, matcher.ruleType, matcher.ruleValue)
				pushInterceptLog(clientIP, host, matcher.ruleType, matcher.ruleValue)
				ctx.Resp = ForbiddenResponse(ctx.Req, "403 Access Denied")
				return RejectConnect, host
			}
		}
		return nil, ""
	})

	// 初始加载
	ac.ReloadFromConfig()
	return ac
}

// ReloadFromConfig 从配置热重载规则匹配器（锁外构建，锁内原子替换）
func (ac *AccessController) ReloadFromConfig() {
	cfg := ac.proxy.Config.GetConfig()

	// 锁外构建（正则编译等耗时操作）
	var newMatchers []ruleMatcher
	for _, rule := range cfg.AccessRules {
		if !rule.Enable {
			continue
		}
		values := splitCSVValues(rule.Value)
		if len(values) == 0 {
			continue
		}

		var matchFunc func(host string) bool
		switch rule.Type {
		case "DomainSuffix":
			suffixes := make([]string, len(values))
			for i, v := range values {
				suffixes[i] = strings.ToLower(v)
			}
			matchFunc = func(host string) bool {
				host = strings.ToLower(host)
				for _, suffix := range suffixes {
					if host == suffix || strings.HasSuffix(host, "."+suffix) {
						return true
					}
				}
				return false
			}
		case "DomainKeyword":
			regs := make([]*regexp.Regexp, 0, len(values))
			for _, p := range values {
				if r, err := regexp.Compile("(?i)" + p); err == nil {
					regs = append(regs, r)
				}
			}
			matchFunc = func(host string) bool {
				for _, r := range regs {
					if r.MatchString(host) {
						return true
					}
				}
				return false
			}
		case "IP":
			ipSet := make(map[string]bool, len(values))
			for _, ip := range values {
				ipSet[ip] = true
			}
			matchFunc = func(host string) bool {
				if net.ParseIP(host) != nil {
					return ipSet[host]
				}
				return false
			}
		default:
			ac.proxy.Logger.Printf("WARN: 未知访问控制规则类型 %s", rule.Type)
			continue
		}

		newMatchers = append(newMatchers, ruleMatcher{
			matchFunc: matchFunc,
			ruleType:  rule.Type,
			ruleValue: rule.Value,
		})
	}

	newBlockedClients := make(map[string]bool)
	for _, rule := range cfg.UserBlockRules {
		if !rule.Enable {
			continue
		}
		for _, rawIP := range splitCSVValues(rule.Value) {
			normalized := NormalizeIP(rawIP)
			if normalized == "" {
				ac.proxy.Logger.Printf("WARN: 非法来源 IP 拦截规则值 %q", rawIP)
				continue
			}
			newBlockedClients[normalized] = true
		}
	}

	// 锁内原子替换
	ac.mu.Lock()
	ac.matchers = newMatchers
	ac.blockedClients = newBlockedClients
	ac.mu.Unlock()
}

// CloseBlockedConnections 关闭当前命中来源 IP 黑名单的活跃连接
func (ac *AccessController) CloseBlockedConnections() {
	if !ac.proxy.Config.GetConfig().AccessEnable {
		return
	}
	ac.proxy.Connections.Range(func(key, value any) bool {
		info := value.(*ConnectionInfo)
		if info.Status != "Active" {
			return true
		}
		if !ac.isBlockedClient(ResolveConnectionClientIP(info)) {
			return true
		}
		ac.proxy.CloseAndRemoveConnection(key.(int64))
		return true
	})
}

func (ac *AccessController) isBlockedClient(clientIP string) bool {
	clientIP = NormalizeIP(clientIP)
	if clientIP == "" {
		return false
	}
	ac.mu.RLock()
	blocked := ac.blockedClients[clientIP]
	ac.mu.RUnlock()
	return blocked
}

func splitCSVValues(raw string) []string {
	rawValues := strings.Split(raw, ",")
	values := make([]string, 0, len(rawValues))
	for _, v := range rawValues {
		v = strings.TrimSpace(v)
		if v != "" {
			values = append(values, v)
		}
	}
	return values
}

func pushInterceptLog(clientIP, target, ruleType, ruleValue string) {
	InterceptCount.Add(1)
	select {
	case InterceptLogChan <- InterceptLogMessage{
		ClientIP:  clientIP,
		Target:    target,
		RuleType:  ruleType,
		RuleValue: ruleValue,
		Time:      time.Now(),
	}:
	default:
	}
}

// 流量计数器
func AddTrafficMonitor(proxy *CoreHttpServer) {
	// 请求阶段
	proxy.HookOnReq().DoFunc(func(req *http.Request, ctx *Pcontext) (*http.Request, *http.Response) {
		if ctx.TrafficCounter == nil {
			return req, nil
		}
		// 记录请求头大小
		ctx.TrafficCounter.req_header = GetHeaderSize(req, ctx)
		ctx.TrafficCounter.req_sum = ctx.TrafficCounter.req_header

		var parentCounter *TrafficCounter
		if ctx.parCtx != nil {
			ctx.parCtx.TrafficCounter.req_sum += ctx.TrafficCounter.req_header
			parentCounter = ctx.parCtx.TrafficCounter
		}

		GlobalTrafficUp.Add(ctx.TrafficCounter.req_header)

		// 获取用户流量统计指针（仅在握手期获取一次）
		clientIP := ctx.ClientIP
		if clientIP == "" {
			clientIP = getClientIP(req)
		}
		host := ExtractHost(req.Host)
		if host == "" {
			host = ExtractHost(req.URL.Host)
		}
		var userStats *UserHostStats
		if clientIP != "" && clientIP != "unknown" && host != "" {
			userStats = GlobalUserTraffic.GetOrCreateStats(clientIP, host)
			userStats.Up.Add(ctx.TrafficCounter.req_header)
		}

		// 如果有请求体，包装它
		if req.Body != nil {
			// roundripe自动调用req.Body.read读取body
			// roundripe从req的map中读取header

			// 第一层：流量统计
			trafficReader := &reqBodyReader{
				ReadCloser: req.Body,
				counter:    ctx.TrafficCounter,
				Pcounter:   parentCounter,
				onClose:    nil,
				userStats:  userStats,
			}

			// 第二层：MinIO 捕获（仅 MITM 开启且 MinIO 客户端可用时执行）
			if ctx.exchangeCapture != nil && ctx.core_proxy.Config.GetConfig().MitmEnabled && ctx.core_proxy.MinioClient != nil {
				contentType := req.Header.Get("Content-Type")
				captReader := ctx.core_proxy.MinioClient.BuildBodyReader(trafficReader, ctx.Session, "req", contentType, req.ContentLength)
				ctx.exchangeCapture.reqBodyCapture = captReader.Capture
				req.Body = captReader
			} else {
				req.Body = trafficReader
			}
		}
		return req, nil
	})

	// 响应阶段
	proxy.HookOnResp().DoFunc(func(resp *http.Response, ctx *Pcontext) *http.Response {
		if resp == nil || ctx.TrafficCounter == nil {
			return resp
		}

		// 记录响应头大小
		ctx.TrafficCounter.resp_header = GetHeaderSize(resp, ctx)
		ctx.TrafficCounter.resp_sum = ctx.TrafficCounter.resp_header // 子连接统计请求头大小

		var parentCounter *TrafficCounter
		if ctx.parCtx != nil {
			ctx.parCtx.TrafficCounter.resp_sum += ctx.TrafficCounter.resp_header // 父隧道统计请求头大小
			parentCounter = ctx.parCtx.TrafficCounter
		}

		GlobalTrafficDown.Add(ctx.TrafficCounter.resp_header)

		// 获取用户流量统计指针
		var userStats *UserHostStats
		if ctx.Req != nil {
			clientIP := ctx.ClientIP
			if clientIP == "" {
				clientIP = getClientIP(ctx.Req)
			}
			host := ExtractHost(ctx.Req.Host)
			if host == "" && ctx.Req.URL != nil {
				host = ExtractHost(ctx.Req.URL.Host)
			}
			if clientIP != "" && clientIP != "unknown" && host != "" {
				userStats = GlobalUserTraffic.GetOrCreateStats(clientIP, host)
				userStats.Down.Add(ctx.TrafficCounter.resp_header)
			}
		}

		if resp.Body == nil {
			ctx.TrafficCounter.UpdateTotal()
			var pReqSum, pRespSum, pTotal int64
			if ctx.parCtx != nil {
				ctx.parCtx.TrafficCounter.UpdateTotal()
				pReqSum = ctx.parCtx.TrafficCounter.req_sum
				pRespSum = ctx.parCtx.TrafficCounter.resp_sum
				pTotal = ctx.parCtx.TrafficCounter.total
			}

			ctx.Log_P("[流量统计] 本次连接上行: %d (header:%d body:%d) | 本次连接下行: %d (header:%d body:0) | 本次连接总计: %d | 隧道总上行: %d | 隧道总下行: %d | 隧道流量总计: %d |  %s | %s ",
				ctx.TrafficCounter.req_sum, ctx.TrafficCounter.req_header, ctx.TrafficCounter.req_body,
				ctx.TrafficCounter.resp_header, ctx.TrafficCounter.resp_header, ctx.TrafficCounter.total,
				pReqSum, pRespSum, pTotal,
				ctx.Req.Method, ctx.Req.URL.String())
			return resp
		}

		// 包装响应体
		// 第一层：流量统计
		trafficReader := &respBodyReader{
			ReadCloser: resp.Body,
			counter:    ctx.TrafficCounter,
			Pcounter:   parentCounter,
			userStats:  userStats,
			onClose: func() {
				ctx.TrafficCounter.UpdateTotal()
				var pReqSum, pRespSum, pTotal int64
				if ctx.parCtx != nil {
					ctx.parCtx.TrafficCounter.UpdateTotal()
					pReqSum = ctx.parCtx.TrafficCounter.req_sum
					pRespSum = ctx.parCtx.TrafficCounter.resp_sum
					pTotal = ctx.parCtx.TrafficCounter.total
				}

				ctx.Log_P("[流量统计] 本次连接上行: %d (header:%d body:%d) | 本次连接下行: %d (header:%d body:%d) | 本次连接总计: %d | 隧道总上行: %d | 隧道总下行: %d | 隧道流量总计: %d | %s | %s | %s",
					ctx.TrafficCounter.req_sum, ctx.TrafficCounter.req_header, ctx.TrafficCounter.req_body,
					ctx.TrafficCounter.resp_sum, ctx.TrafficCounter.resp_header, ctx.TrafficCounter.resp_body,
					ctx.TrafficCounter.total, pReqSum, pRespSum,
					pTotal, ctx.Req.Method, ctx.Req.URL.String(), resp.Status)

				ctx.SendExchange() // 触发 MITM Exchange 发送
			},
		}

		// 第二层：MinIO 捕获（仅 MITM 开启且 MinIO 客户端可用且未被拦截时执行）
		if ctx.exchangeCapture != nil && !ctx.exchangeCapture.skipSend && ctx.core_proxy.Config.GetConfig().MitmEnabled && ctx.core_proxy.MinioClient != nil {
			contentType := resp.Header.Get("Content-Type")
			captReader := ctx.core_proxy.MinioClient.BuildBodyReader(trafficReader, ctx.Session, "resp", contentType, resp.ContentLength)
			ctx.exchangeCapture.respBodyCapture = captReader.Capture
			resp.Body = captReader
		} else {
			resp.Body = trafficReader
		}
		return resp
	})
}

func PrintReqHeader(proxy *CoreHttpServer) {
	proxy.HookOnReq().DoFunc(func(req *http.Request, ctx *Pcontext) (*http.Request, *http.Response) {
		dumpBytes, err := httputil.DumpRequest(req, false)
		if err != nil {
			fmt.Println("DumpRequest error:", err)
		} else {
			// 打印出来的就是标准的 HTTP 协议文本
			fmt.Printf("\n=== [DEBUG] Request Dump ===\n%s\n============================\n", dumpBytes)
		}
		return req, nil
	})
}

func PrintRespHeader(proxy *CoreHttpServer) {
	proxy.HookOnResp().OnRespByReq().DoFunc(func(resp *http.Response, ctx *Pcontext) *http.Response {
		dumpBytes, err := httputil.DumpResponse(resp, false)
		if err != nil {
			fmt.Println("DumpResponse error:", err)
		} else {
			// 打印出来的就是标准的 HTTP 协议文本
			fmt.Printf("\n=== [DEBUG] Response Dump ===\n%s\n============================\n", dumpBytes)
		}
		return resp
	})
}

var httpDomains = map[string]bool{
	"example.com": true,
}

func StatusChange(proxy *CoreHttpServer) {
	proxy.HookOnReq().DoConnectFunc(func(host string, ctx *Pcontext) (*ConnectAction, string) {
		hostname := host
		if colonIdx := strings.LastIndex(host, ":"); colonIdx != -1 {
			hostname = host[:colonIdx]
		}

		// 1. 域名白名单判断
		if httpDomains[hostname] {
			return HTTPMitmConnect, host
		}

		// 2. 端口判断
		if strings.HasSuffix(host, ":80") {
			return HTTPMitmConnect, host
		}

		// 3. 默认情况
		return OkConnect, host
	})
}

// AddRouter 配置路由引擎（配置驱动），返回 Router 实例供 API 热更新
func AddRouter(proxy *CoreHttpServer) *Router {
	router := NewRouter(proxy)

	// 从配置加载初始路由
	if proxy.Config != nil {
		cfg := proxy.Config.GetConfig()
		if cfg.RouteEnable {
			router.ReloadFromConfig(&cfg)
		}
	}

	// 隧道透传模式路由
	// 我们必须在这里绑定好动态路由器，在connectDial中决定是否使用
	proxy.ConnectWithReqDial = router.RouteDial

	// MITM 模式路由（通过自定义 RoundTripper）
	routerRT := NewRouterRoundTripper(proxy, router)
	proxy.HookOnReq().DoFunc(func(req *http.Request, ctx *Pcontext) (*http.Request, *http.Response) {
		if !ctx.core_proxy.Config.GetConfig().RouteEnable {
			return req, nil
		}
		if ctx.RoundTripper == nil {
			ctx.RoundTripper = routerRT
		}
		return req, nil
	})

	proxy.Router = router // 绑定到 proxy
	return router
}

func HttpsMitmMode(proxy *CoreHttpServer) {
	proxy.HookOnReq().DoConnectFunc(func(host string, ctx *Pcontext) (*ConnectAction, string) {
		return MitmConnect, host
	})
}

func HttpMitmMode(proxy *CoreHttpServer) {
	proxy.HookOnReq().DoConnectFunc(func(host string, ctx *Pcontext) (*ConnectAction, string) {
		return HTTPMitmConnect, host
	})
}

func TunnelMode(proxy *CoreHttpServer) {
	proxy.HookOnReq().DoConnectFunc(func(host string, ctx *Pcontext) (*ConnectAction, string) {
		return OkConnect, host
	})
}

// AddAccessControl 注入访问控制拦截逻辑，返回 AccessController 供热重载使用
func AddAccessControl(proxy *CoreHttpServer) *AccessController {
	if proxy.Config == nil {
		return nil
	}
	ac := NewAccessController(proxy)
	proxy.AccessControl = ac
	return ac
}

// getClientIP 从请求中提取客户端 IP
func getClientIP(req *http.Request) string {
	if req == nil {
		return "unknown"
	}

	// 优先检查 X-Forwarded-For 和 X-Real-IP
	if xff := req.Header.Get("X-Forwarded-For"); xff != "" {
		// 取第一个 IP
		if idx := strings.Index(xff, ","); idx != -1 {
			if ip := NormalizeIP(xff[:idx]); ip != "" {
				return ip
			}
		} else if ip := NormalizeIP(xff); ip != "" {
			return ip
		}
	}
	if ip := NormalizeIP(req.Header.Get("X-Real-IP")); ip != "" {
		return ip
	}

	// 从 RemoteAddr 提取
	if req.RemoteAddr != "" {
		host := req.RemoteAddr
		if parsedHost, _, err := net.SplitHostPort(req.RemoteAddr); err == nil {
			host = parsedHost
		}
		if ip := NormalizeIP(host); ip != "" {
			return ip
		}
	}

	return "unknown"
}
