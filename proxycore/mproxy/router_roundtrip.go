package mproxy

import "net/http"

// RouterRoundTripper 使 MITM 流量也经过路由规则分发
type RouterRoundTripper struct {
	proxy  *CoreHttpServer
	router *Router
}

// NewRouterRoundTripper 创建路由感知的 RoundTripper
func NewRouterRoundTripper(proxy *CoreHttpServer, router *Router) *RouterRoundTripper {
	return &RouterRoundTripper{proxy: proxy, router: router}
}

// RoundTrip 实现 mproxy.RoundTripper 接口
// 直接使用对应节点的专属 Transport，天然隔离连接池，不受 Keep-Alive 复用影响
func (rt *RouterRoundTripper) RoundTrip(req *http.Request, ctx *Pcontext) (*http.Response, error) {
	targetName, dialer := rt.router.MatchRoute(req)
	rt.proxy.Logger.Printf("INFO: [路由匹配] %s %s -> %s", req.Method, req.URL.Host, targetName)
	// 直接使用对应节点的专属 Transport，天然隔离连接池
	return dialer.GetTransport().RoundTrip(req)
}
