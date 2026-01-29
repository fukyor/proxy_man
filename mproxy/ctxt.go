package mproxy
import(
	"net/http"
	"crypto/tls"
)

// 上下文机制很重要，保留本次连接的转发fd和请求fd，保证通路。
// 就和多线程生产者消费者代理一样，每一个线程都有一个上下文，保证能够找到client<->proxy<->remote。
// 同时上下文机制可以完美替代全局转发表，无需查表，因为每个请求都有独立的上下文。
type Pcontext struct{
	core_proxy *CoreHttpServer
	Req *http.Request
	Resp *http.Response
	RoundTripper RoundTripper
	UserData any

	certStore CertStorage
	Error error
	Session int64

	parCtx *Pcontext // 顶层隧道流量统计，通过保存父上下文，确保父隧道能够正确统计请求头	
	TrafficCounter *TrafficCounter // http和mitm流量统计
	tunnelTrafficClient	 *tunnelTrafficClient //加密tunnel流量统计
	tunnelTrafficClientNoClosable *tunnelTrafficClientNoClosable // 加密tunnel流量统计
}

/*
自定义roundtrip接口，替代http.roundtrip，不但可以将roundtrip请求深度绑定Proxycontext，而且实现了热调用
*/
type RoundTripper interface{
	RoundTrip(req *http.Request, ctx *Pcontext) (*http.Response, error)
}


func (ctx *Pcontext) RoundTrip(req *http.Request) (*http.Response, error) {
	if ctx.RoundTripper != nil {
		// 热调用，RoundTripper如果被赋值，就可以完成父子调用。父类是接口，子类是结构图或适配器。
		return ctx.RoundTripper.RoundTrip(req, ctx)
	}
	// 如果没有自定义发送函数，则使用transport的
	return ctx.core_proxy.Transport.RoundTrip(req) 
}

type CertStorage interface {
	Fetch(hostname string, gen func() (*tls.Certificate, error)) (*tls.Certificate, error)
}


func (ctx *Pcontext) WarnP(msg string, argv ...any) {
	ctx.printf("WARN: "+msg, argv...)
}


/*日志打印做了两层抽象，一个私有一个公有*/
func (ctx *Pcontext) printf(msg string, argv ...any) {
	ctx.core_proxy.Logger.Printf("[%03d] "+msg+"\n", append([]any{ctx.Session & 0xFFFF}, argv...)...)
}

func (ctx *Pcontext) Log_P(msg string, argv ...any){
	if ctx.core_proxy.Verbose == true{
		ctx.printf("INFO: "+msg, argv...)
	}
}

