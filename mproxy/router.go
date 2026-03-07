package mproxy

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
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

// createBaseTransport 创建基础 Transport，每个出站节点持有独立实例，实现连接池隔离
func createBaseTransport() *http.Transport {
	return &http.Transport{
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: true},
		MaxIdleConns:          300,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 2 * time.Second,
	}
}

// ======================== OutboundDialer 接口 ========================

// OutboundDialer 出站拨号器接口，用于路由到不同的代理节点
type OutboundDialer interface {
	Dial(network, addr string) (net.Conn, error)
	Name() string
	GetTransport() *http.Transport
}

// DirectDialer 直连拨号器
type DirectDialer struct {
	transport *http.Transport
}

func NewDirectDialer() *DirectDialer {
	return &DirectDialer{
		transport: createBaseTransport(),
	}
}

func (d *DirectDialer) Dial(network, addr string) (net.Conn, error) {
	return net.Dial(network, addr)
}

func (d *DirectDialer) Name() string { return "Direct" }

func (d *DirectDialer) GetTransport() *http.Transport {
	return d.transport
}

// HttpProxyDialer HTTP 二级代理拨号器
type HttpProxyDialer struct {
	name      string
	proxyURL  string
	dialer    func(network, addr string) (net.Conn, error)
	transport *http.Transport
}

// NewHttpProxyDialer 创建 HTTP 二级代理拨号器，复用 CoreHttpServer.NewConnectDialToProxy
func NewHttpProxyDialer(proxy *CoreHttpServer, name, proxyURL string) (*HttpProxyDialer, error) {
	dialer := proxy.NewConnectDialToProxy(proxyURL)
	if dialer == nil {
		return nil, fmt.Errorf("无效的代理 URL: %s (仅支持 HTTP scheme)", proxyURL)
	}
	tr := createBaseTransport()
	tr.DialContext = func(c context.Context, network, addr string) (net.Conn, error) {
		return dialer(network, addr)
	}
	return &HttpProxyDialer{name: name, proxyURL: proxyURL, dialer: dialer, transport: tr}, nil
}

func (d *HttpProxyDialer) Dial(network, addr string) (net.Conn, error) {
	return d.dialer(network, addr)
}

func (d *HttpProxyDialer) Name() string { return d.name }

func (d *HttpProxyDialer) GetTransport() *http.Transport {
	return d.transport
}

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
		Default: NewDirectDialer(),
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

// MatchRoute 路由匹配纯计算函数，不执行拨号操作
// 返回：目标名称、对应的拨号器
func (r *Router) MatchRoute(req *http.Request) (string, OutboundDialer) {
	ctx := &Pcontext{Req: req, core_proxy: r.proxy}

	r.mu.RLock()
	rules := r.Rules
	dialers := r.Dialers
	defaultDialer := r.Default
	r.mu.RUnlock()

	for _, rule := range rules {
		if rule.Condition.HandleReq(req, ctx) {
			if dialer, ok := dialers[rule.Target]; ok {
				return rule.Target, dialer
			}
			r.proxy.Logger.Printf("WARN: [路由匹配] 目标节点 '%s' 不存在 -> Direct", rule.Target)
			break
		}
	}
	return "Direct", defaultDialer
}

// RouteDial 路由分发函数，签名兼容 ConnectWithReqDial
// 隧道透传模式专用入口（不经过 RoundTrip，必须在此打印日志）
func (r *Router) RouteDial(req *http.Request, network, addr string) (net.Conn, error) {
	target, dialer := r.MatchRoute(req)
	r.proxy.Logger.Printf("INFO: [路由匹配] %s -> %s", addr, target)
	return dialer.Dial(network, addr)
}

// ReloadFromConfig 从配置热重载路由规则（线程安全）
// 锁外构建耗时操作，锁内原子替换，极短临界区无死锁
func (r *Router) ReloadFromConfig(cfg *ServerConfig) error {
	// === 锁外构建（耗时操作不持锁）===

	// 1. 构建拨号器
	directDialer := NewDirectDialer()
	newDialers := map[string]OutboundDialer{"Direct": directDialer}
	for _, node := range cfg.ProxyNodes {
		dialer, err := NewHttpProxyDialer(r.proxy, node.Name, node.URL)
		if err != nil {
			r.proxy.Logger.Printf("WARN: 节点 %s 创建失败: %v", node.Name, err)
			continue
		}
		newDialers[node.Name] = dialer
	}

	// 2. 构建规则（Action 直接是拨号器名称）
	newRules := make([]RoutingRule, 0, len(cfg.Routes))
	for _, route := range cfg.Routes {
		if !route.Enable {
			continue
		}

		rawValues := strings.Split(route.Value, ",")
		var values []string
		for _, v := range rawValues {
			v = strings.TrimSpace(v)
			if v != "" {
				values = append(values, v)
			}
		}
		if len(values) == 0 {
			continue
		}

		var condition ReqCondition
		switch route.Type {
		case "DomainSuffix":
			condition = DomainSuffixRule(values...)
		case "DomainKeyword":
			condition = DomainKeywordRule(values...)
		case "IP":
			condition = IPRule(values...)
		default:
			r.proxy.Logger.Printf("WARN: 未知规则类型 %s", route.Type)
			continue
		}
		// 验证目标拨号器存在
		if _, ok := newDialers[route.Action]; !ok {
			r.proxy.Logger.Printf("WARN: 规则目标 '%s' 对应的节点不存在，跳过", route.Action)
			continue
		}
		newRules = append(newRules, RoutingRule{Condition: condition, Target: route.Action})
	}

	// === 锁内原子替换（默认行为始终直连）===
	r.mu.Lock()
	r.Dialers = newDialers
	r.Rules = newRules
	r.Default = directDialer
	r.mu.Unlock()

	r.proxy.Logger.Printf("INFO: 配置已热重载，路由已热重载，%d 条规则，%d 个节点", len(newRules), len(newDialers)-1)
	return nil
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

// DomainKeywordRule 域名正则匹配规则（自动剥离端口，忽略大小写）
func DomainKeywordRule(patterns ...string) ReqConditionFunc {
	regs := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		if r, err := regexp.Compile("(?i)" + p); err == nil {
			regs = append(regs, r)
		}
	}
	return func(req *http.Request, ctx *Pcontext) bool {
		host := extractHost(req)
		for _, r := range regs {
			if r.MatchString(host) {
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
