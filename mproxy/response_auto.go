package mproxy

import (
	"bytes"
	"io"
	"net/http"
)

func NewResponse(r *http.Request, contentType string, status int, body string) *http.Response {
	resp := &http.Response{}
	resp.Request = r
	resp.TransferEncoding = r.TransferEncoding
	resp.Header = make(http.Header)
	resp.Header.Add("Content-Type", contentType)
	resp.StatusCode = status
	resp.Status = http.StatusText(status)
	buf := bytes.NewBufferString(body)
	resp.ContentLength = int64(buf.Len())
	resp.Body = io.NopCloser(buf)
	return resp
}

const (
	ContentTypeText = "text/plain"
	ContentTypeHtml = "text/html"
)

// Alias for NewResponse(r,ContentTypeText,http.StatusAccepted,text).
func TextResponse(r *http.Request, text string) *http.Response {
	return NewResponse(r, ContentTypeText, http.StatusAccepted, text)
}

// ForbiddenResponse 返回 403 Forbidden 响应（用于访问控制拦截）
// 注意：访问控制应使用 403 而非 202（StatusAccepted）
func ForbiddenResponse(r *http.Request, message string) *http.Response {
	return NewResponse(r, ContentTypeText, http.StatusForbidden, message)
}
