package myminio

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// APIResponse 通用 API 响应格式
type APIResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// DownloadData 下载信息
type DownloadData struct {
	DownloadURL string `json:"downloadUrl"` // 浏览器可访问的下载链接
	ExpiresAt   string `json:"expiresAt"`   // 链接过期时间
	Filename    string `json:"filename"`    // 建议文件名
	Size        int64  `json:"size"`        // 文件大小（字节）
}

// writeJSON 写入 JSON 响应（禁用 HTML 转义）
func writeJSON(w http.ResponseWriter, v any) error {
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(v)
}

// HandleDownload 处理下载请求（*Client 方法）
// GET /api/storage/download?key=mitm-data/2026-02-04/10086/req
func (c *Client) HandleDownload(w http.ResponseWriter, r *http.Request) {
	// 获取 ObjectKey 参数
	objectKey := r.URL.Query().Get("key")
	if objectKey == "" {
		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, APIResponse{
			Code:    400,
			Message: "缺少参数: key",
		})
		return
	}

	filename, err := ExtractFilename(objectKey)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, APIResponse{
			Code:    504,
			Message: err.Error(),
		})
		return
	}

	if r.URL.Query().Get("proxy") == "1" {
		c.handleProxyDownload(w, r, objectKey, filename)
		return
	}

	w.Header().Set("Content-Type", "application/json")

	// 检查对象是否存在
	info, err := c.StatObject(objectKey)
	if err != nil {
		writeJSON(w, APIResponse{
			Code:    404,
			Message: "对象不存在",
		})
		return
	}

	// 生成浏览器可访问的下载 URL：有公网 Endpoint 时用直链，否则回退到后端转发
	expiry := 1 * time.Hour
	downloadURL, err := c.buildDownloadURL(r, objectKey, expiry, filename)
	if err != nil {
		writeJSON(w, APIResponse{
			Code:    500,
			Message: "生成下载链接失败",
		})
		return
	}

	// 返回成功响应
	writeJSON(w, APIResponse{
		Code:    0,
		Message: "success",
		Data: DownloadData{
			DownloadURL: downloadURL,
			ExpiresAt:   time.Now().Add(expiry).Format(time.RFC3339),
			Filename:    filename,
			Size:        info.Size,
		},
	})
}

func (c *Client) buildDownloadURL(r *http.Request, objectKey string, expiry time.Duration, filename string) (string, error) {
	if c.PublicClient != nil {
		return c.GetPresignedURL(objectKey, expiry, filename)
	}
	return buildProxyDownloadURL(r, objectKey), nil
}

func buildProxyDownloadURL(r *http.Request, objectKey string) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if forwardedProto := firstForwardedValue(r.Header.Get("X-Forwarded-Proto")); forwardedProto != "" {
		scheme = forwardedProto
	}

	host := r.Host
	if forwardedHost := firstForwardedValue(r.Header.Get("X-Forwarded-Host")); forwardedHost != "" {
		host = forwardedHost
	}

	query := url.Values{}
	query.Set("key", objectKey)
	query.Set("proxy", "1")

	downloadURL := url.URL{
		Scheme:   scheme,
		Host:     host,
		Path:     r.URL.Path,
		RawQuery: query.Encode(),
	}
	return downloadURL.String()
}

func firstForwardedValue(value string) string {
	if value == "" {
		return ""
	}
	parts := strings.Split(value, ",")
	return strings.TrimSpace(parts[0])
}

func (c *Client) handleProxyDownload(w http.ResponseWriter, r *http.Request, objectKey string, filename string) {
	info, err := c.StatObject(objectKey)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		writeJSON(w, APIResponse{
			Code:    404,
			Message: "对象不存在",
		})
		return
	}

	object, err := c.GetObject(r.Context(), objectKey)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, APIResponse{
			Code:    500,
			Message: "读取对象失败",
		})
		return
	}
	defer object.Close()

	contentType := info.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", "attachment; filename=\""+filename+"\"")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
	w.Header().Set("Cache-Control", "no-store")

	if _, err := io.Copy(w, object); err != nil {
		log.Printf("转发 MinIO 下载失败: key=%s, err=%v", objectKey, err)
	}
}

// ExtractFilename 从 ObjectKey 提取文件名
// 格式: mitm-data/2026-02-04/10086/req -> 10086_req.bin
func ExtractFilename(key string) (string, error) {
	parts := strings.Split(key, "/")
	if len(parts) >= 2 {
		// 倒数第二个是 SessionID，倒数第一个是 bodyType
		sessionID := parts[len(parts)-2]
		bodyType := parts[len(parts)-1]
		return sessionID + "_" + bodyType + ".bin", nil
	}
	return "", fmt.Errorf("字符串解析错误")
}
