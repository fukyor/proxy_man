package mproxy

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"proxy_man/myminio"
	"strings"
)

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
			}

			// 第二层：MinIO 捕获（仅 MITM 开启且当前请求有 exchangeCapture 时执行）
			if ctx.exchangeCapture != nil && ctx.core_proxy.MitmEnabled {
				contentType := req.Header.Get("Content-Type")
				captReader := myminio.BuildBodyReader(trafficReader, ctx.Session, "req", contentType, req.ContentLength)
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
		if ctx.TrafficCounter == nil {
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

		// 第二层：MinIO 捕获（仅 MITM 开启且当前请求有 exchangeCapture 时执行）
		if ctx.exchangeCapture != nil && ctx.core_proxy.MitmEnabled {
			contentType := resp.Header.Get("Content-Type")
			captReader := myminio.BuildBodyReader(trafficReader, ctx.Session, "resp", contentType, resp.ContentLength)
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

func AddRouter(proxy *CoreHttpServer) {
	if proxy.RouteEnable {
		// 设置默认隧道透传的二级代理, 因为全局代理优先级高所以无法被触发，我们注释掉这个功能
		//proxy.ConnectDial = DialerFromEnv(proxy)

		// 创建路由引擎
		router := NewRouter(proxy)

		// 注册二级代理节点（示例，按实际环境修改）
		proxy1, err := NewHttpProxyDialer(proxy, "clash", "http://127.0.0.1:7892")
		if err != nil {
			proxy.Logger.Printf("Warn:创建 Proxy1 失败: %v", err)
		} else {
			router.AddDialer("clash", proxy1)
		}

		// 配置路由规则（按优先级从高到低）
		router.AddRule(DomainKeywordRule("youtube", "google"), "clash")
		router.AddRule(DomainSuffixRule("twitter.com", "x.com"), "clash")
		router.AddRule(IPRule("127.0.0.1"), "clash")

		// 规则代理（透明隧道模式使用）
		proxy.ConnectWithReqDial = router.RouteDial

		// 规则代理 (http，http/https MITM使用) 通过自定义 RoundTrip
		routerRT := NewRouterRoundTripper(proxy, router)
		proxy.HookOnReq().DoFunc(func(req *http.Request, ctx *Pcontext) (*http.Request, *http.Response) {
			if ctx.RoundTripper == nil {
				ctx.RoundTripper = routerRT
			}
			return req, nil
		})
	}
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
