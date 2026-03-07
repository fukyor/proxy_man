package mproxy

import (
	"net/http"
	"sync/atomic"
	"proxy_man/myminio"
	"time"
)

var exchangeIDCounter int64

// HttpExchange 实际发送给客户端的数据
type HttpExchange struct {
	ID        int64            `json:"id"`
	SessionID int64            `json:"sessionId"`
	ParentID  int64            `json:"parentId"`
	Time      int64            `json:"time"`
	Request   RequestSnapshot  `json:"request"`
	Response  ResponseSnapshot `json:"response"`
	Duration  int64            `json:"duration"`
	Error     string           `json:"error,omitempty"`
}

type RequestSnapshot struct {
	Method  string              `json:"method"`
	URL     string              `json:"url"`
	Host    string              `json:"host"`
	Header  map[string][]string `json:"header"`
	SumSize int64               `json:"sumSize"`
	// MinIO 存储信息
	BodyKey      string `json:"bodyKey,omitempty"`      // MinIO 对象 Key
	BodySize     int64  `json:"bodySize,omitempty"`     // Body 实际大小
	BodyUploaded bool   `json:"bodyUploaded,omitempty"` // 是否成功上传
	ContentType  string `json:"contentType,omitempty"`  // Content-Type
	BodyError    string `json:"bodyError,omitempty"`    // MinIO 上传错误
}

type ResponseSnapshot struct {
	StatusCode int                 `json:"statusCode"`
	Status     string              `json:"status"`
	Header     map[string][]string `json:"header"`
	SumSize    int64               `json:"sumSize"`
	// MinIO 存储信息
	BodyKey      string `json:"bodyKey,omitempty"`      // MinIO 对象 Key
	BodySize     int64  `json:"bodySize,omitempty"`     // Body 实际大小
	BodyUploaded bool   `json:"bodyUploaded,omitempty"` // 是否成功上传
	ContentType  string `json:"contentType,omitempty"`  // Content-Type
	BodyError    string `json:"bodyError,omitempty"`    // MinIO 上传错误
}

var GlobalExchangeChan = make(chan *HttpExchange, 5000) // 从 1000 扩容，避免高并发时静默丢弃

// ========== ExchangeCapture：封装捕获状态 ==========

// ExchangeCapture 封装单次请求的捕获状态，作为做一个中间层，统一上报给HttpExchange
type ExchangeCapture struct {
	startTime   time.Time
	reqSnap     RequestSnapshot
	parentID    int64
	skipSend    bool
	err         error
	sent        bool          // 防止重复发送
	reqBodyCapture  *myminio.BodyCapture  // minio上传状态捕获
	respBodyCapture *myminio.BodyCapture  // minio上传状态捕获
}

// StartCapture 初始化捕获，在 Pcontext 创建后调用
func (ctx *Pcontext) StartCapture(parentSession int64) {
	ctx.exchangeCapture = &ExchangeCapture{
		startTime: time.Now(),
		parentID:  parentSession,
	}
}

// CaptureRequest 捕获请求快照，在 filterRequest 之后调用
func (ctx *Pcontext) CaptureRequest(req *http.Request) {
	if ctx.exchangeCapture == nil {
		return
	}
	ctx.exchangeCapture.reqSnap = RequestSnapshot{
		Method: req.Method,
		URL:    req.URL.String(),
		Host:   req.Host,
		Header: cloneHeader(req.Header),
	}
}

// SkipCapture 标记跳过捕获（用于 WebSocket跳过minio捕获）
func (ctx *Pcontext) SetCaptureSkip() {
	if ctx.exchangeCapture != nil {
		ctx.exchangeCapture.skipSend = true
	}
}

// SetCaptureError 记录错误
func (ctx *Pcontext) SetCaptureError(err error) {
	if ctx.exchangeCapture != nil && err != nil {
		ctx.exchangeCapture.err = err
	}
}

// SendExchange 发送 Exchange 到全局通道，在响应完成后自动调用
// 这个方法会被 respBodyReader.onClose 触发
func (ctx *Pcontext) SendExchange() {
	if !ctx.core_proxy.Config.GetConfig().MitmEnabled {
		return // MitmEnabled 为总开关，关闭时不发送任何 Exchange
	}
	cap := ctx.exchangeCapture
	if cap == nil || cap.skipSend || cap.sent {
		return
	}
	cap.sent = true

	exchange := &HttpExchange{
		ID:        atomic.AddInt64(&exchangeIDCounter, 1),
		SessionID: ctx.Session,
		ParentID:  cap.parentID,
		Time:      cap.startTime.UnixMilli(),
		Request:   cap.reqSnap,
		Duration:  time.Since(cap.startTime).Milliseconds(),
	}

	// 从 TrafficCounter 读取请求总大小（头部+Body）
	if ctx.TrafficCounter != nil {
		exchange.Request.SumSize = ctx.TrafficCounter.req_sum
	}

	// 填充请求体 MinIO 信息
	if cap.reqBodyCapture != nil {
		exchange.Request.BodyKey = cap.reqBodyCapture.ObjectKey
		exchange.Request.BodySize = cap.reqBodyCapture.Size
		exchange.Request.BodyUploaded = cap.reqBodyCapture.Uploaded
		exchange.Request.ContentType = cap.reqBodyCapture.ContentType
		if cap.reqBodyCapture.Error != nil {
			exchange.Request.BodyError = cap.reqBodyCapture.Error.Error()
		}
	}

	// 从 ctx.Resp 读取响应信息
	if ctx.Resp != nil {
		exchange.Response = ResponseSnapshot{
			StatusCode: ctx.Resp.StatusCode,
			Status:     ctx.Resp.Status,
			Header:     cloneHeader(ctx.Resp.Header),
		}
		if ctx.TrafficCounter != nil {
			exchange.Response.SumSize = ctx.TrafficCounter.resp_sum
		}
	}

	// 填充响应体 MinIO 信息
	if cap.respBodyCapture != nil {
		exchange.Response.BodyKey = cap.respBodyCapture.ObjectKey
		exchange.Response.BodySize = cap.respBodyCapture.Size
		exchange.Response.BodyUploaded = cap.respBodyCapture.Uploaded
		exchange.Response.ContentType = cap.respBodyCapture.ContentType
		if cap.respBodyCapture.Error != nil {
			exchange.Response.BodyError = cap.respBodyCapture.Error.Error()
		}
	}

	if cap.err != nil {
		exchange.Error = cap.err.Error()
	}

	// 非阻塞发送
	select {
	case GlobalExchangeChan <- exchange:
	default:
	}
}

func cloneHeader(h http.Header) map[string][]string {
	if h == nil {
		return nil
	}
	result := make(map[string][]string, len(h))
	for k, v := range h {
		result[k] = append([]string{}, v...)
	}
	return result
}