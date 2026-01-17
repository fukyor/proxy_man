package mproxy

import (
	"io"
	"net/http"
	"net/http/httputil"
)

type TrafficCounter struct {
	io.ReadCloser        // 嵌入原始 Body
	resp_header int64	// remote->proxy的头部总大小
	resp_body int64		
	req_header int64	// client->proxy头部总大小
	req_body int64		// client->proxy body总大小
	req_sum int64		// 等于 req_header+req_body
	resp_sum int64		// remote->client的body总大小
	total int64        // 等于req_sum + resp_sum
	onClose func(int64)  // 关闭时的回调函数，用于打印/记录日志
}

type reqBodyReader struct {
	io.ReadCloser       // 嵌入原始 req.Body(有header,body,socket)
	counter *TrafficCounter // 指向全局计数器
}

func (r *reqBodyReader) Read(p []byte) (n int, err error) {
	n, err = r.ReadCloser.Read(p)
	r.counter.req_body += int64(n)
	return n, err
}

func (r *reqBodyReader) Close() error {
	return r.ReadCloser.Close()
}

func (c *TrafficCounter) UpdateReqSum() {
	c.req_sum = c.req_header + c.req_body
}

func (c *TrafficCounter) UpdateRespSum() {
	c.resp_sum = c.resp_header + c.resp_body
}

func (c *TrafficCounter) UpdateTotal(){
	c.total = c.req_sum + c.resp_sum
}

func (c *TrafficCounter) Read(p []byte) (n int, err error){
	n, err = c.ReadCloser.Read(p)
	c.resp_sum += int64(n)
	return n, err
}

func (c *TrafficCounter) Close() error{
	// 在关闭流时触发回调，上报总流量
	if c.onClose != nil {
		c.onClose(c.resp_sum)
		c.onClose = nil

	}
	return c.ReadCloser.Close()
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