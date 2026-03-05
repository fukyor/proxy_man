package mproxy

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
)

// extractHost 从请求中提取纯 host（不含端口），统一小写
func extractHost(req *http.Request) string {
	host := req.URL.Host
	if host == "" {
		host = req.Host
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.ToLower(host)
}

// ======================== OutboundDialer 接口 ========================

// OutboundDialer 出站拨号器接口，用于路由到不同的代理节点
type OutboundDialer interface {
	Dial(network, addr string) (net.Conn, error)
	Name() string
}

// DirectDialer 直连拨号器
type DirectDialer struct{}

func (d *DirectDialer) Dial(network, addr string) (net.Conn, error) {
	return net.Dial(network, addr)
}

func (d *DirectDialer) Name() string { return "Direct" }

// HttpProxyDialer HTTP 二级代理拨号器
type HttpProxyDialer struct {
	name     string
	proxyURL string
	dialer   func(network, addr string) (net.Conn, error)
}

// NewHttpProxyDialer 创建 HTTP 二级代理拨号器，复用 CoreHttpServer.NewConnectDialToProxy
func NewHttpProxyDialer(proxy *CoreHttpServer, name, proxyURL string) (*HttpProxyDialer, error) {
	dialer := proxy.NewConnectDialToProxy(proxyURL)
	if dialer == nil {
		return nil, fmt.Errorf("无效的代理 URL: %s (仅支持 HTTP scheme)", proxyURL)
	}
	return &HttpProxyDialer{name: name, proxyURL: proxyURL, dialer: dialer}, nil
}

func (d *HttpProxyDialer) Dial(network, addr string) (net.Conn, error) {
	return d.dialer(network, addr)
}

func (d *HttpProxyDialer) Name() string { return d.name }

// ======================== Router 路由引擎 ========================

// RoutingRule 路由规则，包含条件和目标拨号器名称
type RoutingRule struct {
	Condition ReqCondition
	Target    string
}

// Router 路由引擎，根据规则将请求分发到不同的出站拨号器
type Router struct {
	proxy   *CoreHttpServer
	mu      sync.RWMutex
	Dialers map[string]OutboundDialer
	Rules   []RoutingRule
	Default OutboundDialer
}

// NewRouter 创建路由引擎
func NewRouter(proxy *CoreHttpServer) *Router {
	return &Router{
		proxy:   proxy,
		Dialers: make(map[string]OutboundDialer),
		Default: &DirectDialer{},
	}
}

// AddDialer 注册出站拨号器
func (r *Router) AddDialer(name string, dialer OutboundDialer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Dialers[name] = dialer
}

// AddRule 添加路由规则（按添加顺序优先匹配）
func (r *Router) AddRule(condition ReqCondition, target string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Rules = append(r.Rules, RoutingRule{Condition: condition, Target: target})
}

// RouteDial 路由分发函数，签名兼容 ConnectWithReqDial
func (r *Router) RouteDial(req *http.Request, network, addr string) (net.Conn, error) {
	ctx := &Pcontext{Req: req, core_proxy: r.proxy}

	r.mu.RLock()
	rules := r.Rules
	dialers := r.Dialers
	defaultDialer := r.Default
	r.mu.RUnlock()

	for _, rule := range rules {
		if rule.Condition.HandleReq(req, ctx) {
			if dialer, ok := dialers[rule.Target]; ok {
				// 只有在建立tcp连接时才会打印一次，因为存在连接复用所以并不会每次请求都打印
				r.proxy.Logger.Printf("INFO: [路由匹配] %s -> %s", addr, rule.Target)
				return dialer.Dial(network, addr)
			}
			r.proxy.Logger.Printf("WARN: [路由匹配] 目标节点 '%s' 不存在，回退Direct", rule.Target)
			break
		}
	}
	r.proxy.Logger.Printf("WARN: [路由匹配] 未匹配到规则 -> Direct")
	return defaultDialer.Dial(network, addr)
}

// ======================== 规则构建函数 ========================

// DomainSuffixRule 域名后缀匹配规则（自动剥离端口）
func DomainSuffixRule(suffixes ...string) ReqConditionFunc {
	for i, s := range suffixes {
		suffixes[i] = strings.ToLower(s)
	}
	return func(req *http.Request, ctx *Pcontext) bool {
		host := extractHost(req)
		for _, suffix := range suffixes {
			if host == suffix || strings.HasSuffix(host, "."+suffix) {
				return true
			}
		}
		return false
	}
}

// DomainKeywordRule 域名关键词匹配规则（自动剥离端口）
func DomainKeywordRule(keywords ...string) ReqConditionFunc {
	for i, kw := range keywords {
		keywords[i] = strings.ToLower(kw)
	}
	return func(req *http.Request, ctx *Pcontext) bool {
		host := extractHost(req)
		for _, kw := range keywords {
			if strings.Contains(host, kw) {
				return true
			}
		}
		return false
	}
}

// IPRule IP 精确匹配规则
func IPRule(ipList ...string) ReqConditionFunc {
	ipSet := make(map[string]bool, len(ipList))
	for _, ip := range ipList {
		ipSet[ip] = true
	}
	return func(req *http.Request, ctx *Pcontext) bool {
		host := extractHost(req)
		if net.ParseIP(host) != nil {
			return ipSet[host]
		}
		return false
	}
}
