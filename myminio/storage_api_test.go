package myminio

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestBuildDownloadURLFallsBackToProxyWhenPublicEndpointEmpty(t *testing.T) {
	cfg := *DefaultMinioConfig()
	cfg.PublicEndpoint = ""
	client := &Client{Config: cfg}
	req := httptest.NewRequest(http.MethodGet, "http://localhost:8000/api/storage/download", nil)

	downloadURL, err := client.buildDownloadURL(req, "mitm-data/2026-05-11/4/resp", time.Hour, "4_resp.bin")
	if err != nil {
		t.Fatalf("构造下载链接失败：%v", err)
	}

	parsedURL, err := url.Parse(downloadURL)
	if err != nil {
		t.Fatalf("解析下载链接失败：%v", err)
	}
	if parsedURL.Scheme != "http" {
		t.Fatalf("下载链接协议错误：期望 http，实际 %s", parsedURL.Scheme)
	}
	if parsedURL.Host != "localhost:8000" {
		t.Fatalf("下载链接 Host 错误：期望 localhost:8000，实际 %s", parsedURL.Host)
	}
	if parsedURL.Path != "/api/storage/download" {
		t.Fatalf("下载链接路径错误：期望 /api/storage/download，实际 %s", parsedURL.Path)
	}
	if parsedURL.Query().Get("proxy") != "1" {
		t.Fatalf("下载链接缺少 proxy=1 参数：%s", downloadURL)
	}
	if parsedURL.Query().Get("key") != "mitm-data/2026-05-11/4/resp" {
		t.Fatalf("下载链接 key 参数错误：%s", downloadURL)
	}
}

func TestBuildProxyDownloadURLUsesForwardedHeaders(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8000/api/storage/download", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "proxy.example.com")

	downloadURL := buildProxyDownloadURL(req, "mitm-data/2026-05-11/4/resp")
	parsedURL, err := url.Parse(downloadURL)
	if err != nil {
		t.Fatalf("解析下载链接失败：%v", err)
	}
	if parsedURL.Scheme != "https" {
		t.Fatalf("转发协议错误：期望 https，实际 %s", parsedURL.Scheme)
	}
	if parsedURL.Host != "proxy.example.com" {
		t.Fatalf("转发 Host 错误：期望 proxy.example.com，实际 %s", parsedURL.Host)
	}
}
