package mproxy

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"time"
)

type contextKey string

const routingReqKey contextKey = "routing_req"

// RouterRoundTripper 使 MITM 流量也经过路由规则分发
type RouterRoundTripper struct {
	proxy     *CoreHttpServer
	router    *Router
	transport *http.Transport // 全局唯一，连接池生效
}

// NewRouterRoundTripper 创建路由感知的 RoundTripper
func NewRouterRoundTripper(proxy *CoreHttpServer, router *Router) *RouterRoundTripper {
	rt := &RouterRoundTripper{proxy: proxy, router: router}
	rt.transport = &http.Transport{
		DialContext: func(c context.Context, network, addr string) (net.Conn, error) {
			req, ok := c.Value(routingReqKey).(*http.Request)
			if !ok {
				return net.Dial(network, addr) // 兜底直连
			}
			return rt.router.RouteDial(req, network, addr)
		},
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: true},
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return rt
}

// RoundTrip 实现 mproxy.RoundTripper 接口（ctxt.go:36-38）
func (rt *RouterRoundTripper) RoundTrip(req *http.Request, ctx *Pcontext) (*http.Response, error) {
	reqWithCtx := req.WithContext(context.WithValue(req.Context(), routingReqKey, req))
	return rt.transport.RoundTrip(reqWithCtx)
}
