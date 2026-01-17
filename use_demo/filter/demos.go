package main

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"proxy_man/mproxy"
)

/*******************过滤器hooks****************
// url简单匹配："baidu.com\","\src"
func UrlIs(urls ...string) ReqConditionFunc {
	urlSet := make(map[string]bool)
	for _, u := range urls {
		urlSet[u] = true
	}
	return func(req *http.Request, ctx *) bool {
		_, pathOk := urlSet[req.URL.Path]
		_, hostAndOk := urlSet[req.URL.Host+req.URL.Path]
		return pathOk || hostAndOk
	}
}

// url正则匹配
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


// 响应Content-Type匹配
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
***********************************************/

func demo() {
	proxy := mproxy.NewCoreHttpSever()

	/****************************打印请求头*************************************/
	proxy.HookOnReq().DoFunc(func(req *http.Request, ctx *mproxy.Pcontext) (*http.Request, *http.Response) {
		dumpBytes, err := httputil.DumpRequest(req, true)
		if err != nil {
			fmt.Println("DumpRequest error:", err)
		} else {
			// 打印出来的就是标准的 HTTP 协议文本
			fmt.Printf("\n=== [DEBUG] Request Dump ===\n%s\n============================\n", dumpBytes)
		}
		return req, nil
	})

	/*************************************打印响应头************************************/
	// 同时用请求条件和响应条件（链式调用）
	proxy.HookOnResp(mproxy.ContentTypeHook("text/html")).OnRespByReq(mproxy.UrlHook("/api")).DoFunc(func(resp *http.Response, ctx *mproxy.Pcontext) *http.Response {
		dumpBytes, err := httputil.DumpResponse(resp, true)
		if err != nil {
			fmt.Println("DumpResponse error:", err)
		} else {
			// 打印出来的就是标准的 HTTP 协议文本
			fmt.Printf("\n=== [DEBUG] Response Dump ===\n%s\n============================\n", dumpBytes)
		}
		return resp
	})

	proxy.HookOnResp().OnRespByReq().DoFunc(func(resp *http.Response, ctx *mproxy.Pcontext) *http.Response {
		resp.Header.Del("Content-Type")
		return resp
	})
}
