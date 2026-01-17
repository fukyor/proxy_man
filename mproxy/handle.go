package mproxy

import "net/http"

/*****************************请求过滤器****************************/
type ReqHandler interface {
	Handle(req *http.Request, ctx *Pcontext) (*http.Request, *http.Response)
}

type FuncReqHandler func(req *http.Request, ctx *Pcontext) (*http.Request, *http.Response)

func (f FuncReqHandler) Handle(req *http.Request, ctx *Pcontext) (*http.Request, *http.Response) {
	return f(req, ctx)
}


/*******************响应过滤器******************************/
type RespHandler interface {
	Handle(resp *http.Response, ctx *Pcontext) *http.Response
}

type FuncRespHandler func(resp *http.Response, ctx *Pcontext) *http.Response


func (f FuncRespHandler) Handle(resp *http.Response, ctx *Pcontext) *http.Response {
	return f(resp, ctx)
}

/**************************连接选择器***********************************/
//HandleConnect 主要就是做一个简单的策略决策，返回预定义的连接动作即可，不需要像 Handle 那样处理复杂的请求/响应内容。
type HttpsHandler interface {
	HandleConnect(req string, ctx *Pcontext) (*ConnectAction, string)
}

type FuncHttpsHandler func(host string, ctx *Pcontext) (*ConnectAction, string)

func (f FuncHttpsHandler) HandleConnect(host string, ctx *Pcontext) (*ConnectAction, string) {
	return f(host, ctx)
}