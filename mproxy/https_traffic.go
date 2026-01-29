package mproxy

import (
	"io"
	"net/http"
	"net/http/httputil"
	"sync/atomic"
)

var (
	GlobalTrafficUp   atomic.Int64 // 累计上行流量
	GlobalTrafficDown atomic.Int64 // 累计下行流量
)

type TrafficCounter struct {
	resp_header int64	// remote->client的头部总大小
	resp_body int64		// remote->client的body总大小
	req_header int64	// client->proxy头部总大小
	req_body int64		// client->proxy body总大小
	req_sum int64		// 等于 req_header+req_body
	resp_sum int64		// 等于 resp_header+resp_body
	total int64        // 等于req_sum + resp_sum
}


type reqBodyReader struct {
	io.ReadCloser       // 嵌入原始 req.Body(包含字段有header,body,socket)
	counter *TrafficCounter // 指向全局计数器
	Pcounter *TrafficCounter
	onClose func()  // 关闭时的回调函数，用于打印/记录日志
}

func (r *reqBodyReader) Read(p []byte) (n int, err error) {
	n, err = r.ReadCloser.Read(p)
	r.counter.req_sum += int64(n)
	r.counter.req_body += int64(n)
	GlobalTrafficUp.Add(int64(n)) // 实时累加全局上行
	if r.Pcounter != nil {
		r.Pcounter.req_sum += int64(n)
	}
	return n, err
}

func (r *reqBodyReader) Close() error {
	return r.ReadCloser.Close()
}



type respBodyReader struct {
	io.ReadCloser       // 嵌入原始 resp.Body(包含字段有header,body,socket)
	counter *TrafficCounter // 指向全局计数器
	Pcounter *TrafficCounter
	onClose func()  // 关闭时的回调函数，用于打印/记录日志
}

func (r *respBodyReader) Read(p []byte) (n int, err error) {
	n, err = r.ReadCloser.Read(p)
	r.counter.resp_sum += int64(n)
	r.counter.resp_body += int64(n) // 记录响应体大小
	GlobalTrafficDown.Add(int64(n)) // 实时累加全局下行
	if r.Pcounter != nil {
		r.Pcounter.resp_sum += int64(n)
	}
	return n, err
}

func (r *respBodyReader) Close() error {
	if r.onClose != nil {
		r.onClose()
		r.onClose = nil
	}
	return r.ReadCloser.Close()
}


func (c *TrafficCounter) UpdateTotal(){
	c.total = c.req_sum + c.resp_sum
}

func GetHeaderSize(r any, ctx *Pcontext) int64{
	var tmp []byte
	var err error
	switch v := r.(type) {
	case *http.Request:
		tmp, err = httputil.DumpRequest(v, false) 
	case *http.Response:
		tmp, err = httputil.DumpResponse(v, false)
	}
	if err != nil{
		ctx.Log_P("头部大小解析错误", err)
		return 0
	}
	return int64(len(tmp))
}