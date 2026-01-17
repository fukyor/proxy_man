package mproxy

import (
	"net/http"
	"regexp"
	"strings"
)

/************最底层的条件转换接口，和接口函数************/
// 条件判断函数适配给接口后，最终的执行函数就是HandleReq,HandleResp。我们前面都是在对接口做适配。
// ReqCondition实现RespCondition在响应过滤器中使用
type ReqCondition interface {
	RespCondition
	HandleReq(req *http.Request, ctx *Pcontext) bool
}

type RespCondition interface {
	HandleResp(resp *http.Response, ctx *Pcontext) bool
}

/**************************条件适配器(函数适配器，把任意函数通过函数适配器准确适配接口)*********************************/
type ReqConditionFunc func(req *http.Request, ctx *Pcontext) bool

type RespConditionFunc func(resp *http.Response, ctx *Pcontext) bool


// ReqCondition作为RespCondition的子类实现了父类接口。目的是在HookOnResp传参时可以把ReqConditionFunc直接传入，这样就能用Req的
// 条件在filterResp时也能生效。达到双层条件判断。
func (c ReqConditionFunc) HandleReq(req *http.Request, ctx *Pcontext) bool {
	return c(req, ctx)
}

func (c ReqConditionFunc) HandleResp(resp *http.Response, ctx *Pcontext) bool {
	return c(ctx.Req, ctx)
}

func (c RespConditionFunc) HandleResp(resp *http.Response, ctx *Pcontext) bool {
	return c(resp, ctx)
}


/**************************条件过滤器判断函数********************************/
func (proxy *CoreHttpServer) HookOnReq(conds ...ReqCondition) *ReqProxyConds {
	return &ReqProxyConds{proxy, conds}
}

func (proxy *CoreHttpServer) HookOnResp(respConds ...RespCondition) *RespProxyConds {
	return &RespProxyConds{proxy, nil, respConds}
}

// OnRespByReq 链式添加请求条件，用于在响应过滤时同时检查请求条件
func (pcond *RespProxyConds) OnRespByReq(conds ...ReqCondition) *RespProxyConds {
	pcond.reqConds = append(pcond.reqConds, conds...)
	return pcond
}


/*****************************条件过滤器执行函数*************************************/
// DoFunc是对Do的再封装，我们可以直接向DoFunc传函数而不需要先强转为FuncReqHandler适配器
func (pcond *ReqProxyConds) DoFunc(f func(req *http.Request, ctx *Pcontext) (*http.Request, *http.Response)) {
	pcond.Do(FuncReqHandler(f))
}

func (pcond *ReqProxyConds) Do(h ReqHandler) {
	// 核心逻辑：向 proxy 的 reqHandlers 列表中追加一个新的函数
	pcond.proxy.reqHandlers = append(pcond.proxy.reqHandlers,
		FuncReqHandler(func(r *http.Request, ctx *Pcontext) (*http.Request, *http.Response) {
			for _, cond := range pcond.reqConds {
				if !cond.HandleReq(r, ctx) { // 如果不满足过滤条件则nil继续转发，且不执行用户函数
					return r, nil
				}
			}
			return h.Handle(r, ctx)
		}))
}

func (pcond *RespProxyConds) DoFunc(f func(resp *http.Response, ctx *Pcontext) *http.Response) {
	pcond.Do(FuncRespHandler(f))
}

func (pcond *RespProxyConds) Do(h RespHandler) {
	pcond.proxy.respHandlers = append(pcond.proxy.respHandlers,
		FuncRespHandler(func(resp *http.Response, ctx *Pcontext) *http.Response {
			// 先检查请求条件（用 HandleReq）
			for _, cond := range pcond.reqConds {
				if !cond.HandleReq(ctx.Req, ctx) {
					return resp
				}
			}
			// 再检查响应条件（用 HandleResp）
			for _, cond := range pcond.respConds {
				if !cond.HandleResp(resp, ctx) {
					return resp
				}
			}
			return h.Handle(resp, ctx)
		}))
}

func (pcond *ReqProxyConds) DoConnectFunc(f func(host string, ctx *Pcontext) (*ConnectAction, string)) {
	pcond.HandleConnect(FuncHttpsHandler(f))
}

//HandleConnect 主要就是做一个简单的策略决策，返回预定义的连接动作即可，不需要像 Handle 那样处理复杂的请求/响应内容。
func (pcond *ReqProxyConds) HandleConnect(h HttpsHandler) {
	pcond.proxy.httpsHandlers = append(pcond.proxy.httpsHandlers,
		FuncHttpsHandler(func(host string, ctx *Pcontext) (*ConnectAction, string) {
			for _, cond := range pcond.reqConds {
				if !cond.HandleReq(ctx.Req, ctx) {
					return nil, "" // 返回值的用法
				}
			}
			return h.HandleConnect(host, ctx)
		}))
}


/***********************条件结构体*****************************/
type ReqProxyConds struct {
	proxy    *CoreHttpServer
	reqConds []ReqCondition
}

type RespProxyConds struct {
	proxy     *CoreHttpServer
	reqConds  []ReqCondition  // 请求条件（用 ReqCondition的HandleReq 检查）
	respConds []RespCondition // 响应条件（用 HandleResp 检查）
}



/********************条件过滤判断闭包函数***********************/
func UrlHook(urls ...string) ReqConditionFunc {
	urlSet := make(map[string]bool)
	for _, u := range urls {
		urlSet[u] = true
	}
	return func(req *http.Request, ctx *Pcontext) bool {
		_, pathOk := urlSet[req.URL.Path]
		_, hostAndOk := urlSet[req.URL.Host+req.URL.Path]
		return pathOk || hostAndOk
	}
}

func UrlRegHook(patterns ...string) ReqConditionFunc {
	// 1. 【预编译】正则表达式
	var rules []*regexp.Regexp
	for _, p := range patterns {
		rules = append(rules, regexp.MustCompile(p))
	}
	return func(req *http.Request, ctx *Pcontext) bool {
		// 2. 获取要匹配的目标字符串
		// 对于代理服务器，req.URL.String() 通常包含完整的 URL (如 http://baidu.com/abc)
		// 如果是 CONNECT 请求(HTTPS)，它可能是 host:443
		target := req.URL.String()
		// 兜底：有些情况下 URL String 可能不完整，手动拼接一下更稳健
		if target == "" {
			target = req.Host + req.URL.Path
		}
		for _, r := range rules {
			if r.MatchString(target) {
				return true
			}
		}
		return false
	}
}


func ContentTypeHook(typ string, types ...string) RespCondition {
	types = append(types, typ)
	return RespConditionFunc(func(resp *http.Response, ctx *Pcontext) bool {
		if resp == nil {
			return false
		}
		contentType := resp.Header.Get("Content-Type")
		for _, typ := range types {
			if contentType == typ || strings.HasPrefix(contentType, typ+";") {
				return true
			}
		}
		return false
	})
}
