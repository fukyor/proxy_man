package myminio

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Config MinIO 配置结构
type Config struct {
	Endpoint        string `json:"endpoint"`
	PublicEndpoint  string `json:"publicEndpoint"` // 公网 Endpoint，用于生成预签名 URL
	AccessKeyID     string `json:"accessKeyId"`
	SecretAccessKey string `json:"secretAccessKey"`
	UseSSL          bool   `json:"useSSL"`
	Bucket          string `json:"bucket"`
	Enabled         bool   `json:"enabled"`
}

// Client MinIO 客户端封装
type Client struct {
	Client       *minio.Client
	PublicClient *minio.Client // 公网客户端（仅用于预签名）
	Config       Config
}

// DefaultMinioConfig 提供一套开箱即用的默认 MinIO 配置
func DefaultMinioConfig() *Config {
	return &Config{
		Endpoint:        "minio:9000",
		PublicEndpoint:  "",
		AccessKeyID:     "root",
		SecretAccessKey: "12345678",
		UseSSL:          false,
		Bucket:          "bodydata",
		Enabled:         true,
	}
}

// NewClient 创建新的 MinIO 客户端，调用方需在外层判断 cfg.Enabled
func NewClient(cfg Config) (*Client, error) {
	// 创建禁用代理的 Transport，防止 MinIO 内部请求受系统 HTTP_PROXY 影响
	var customTransport *http.Transport
	customTransport, err := minio.DefaultTransport(cfg.UseSSL)
	if err != nil {
		return nil, fmt.Errorf("创建 MinIO Transport 失败: %w", err)
	}
	customTransport.Proxy = nil // 显式禁用代理

	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:     credentials.NewStaticV4(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		Secure:    cfg.UseSSL,
		Transport: customTransport,
	})
	if err != nil {
		return nil, fmt.Errorf("创建 MinIO 客户端失败: %w", err)
	}

	// 检查 Bucket 是否存在
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	exists, err := client.BucketExists(ctx, cfg.Bucket)
	if err != nil {
		return nil, fmt.Errorf("检查 Bucket 失败: %w", err)
	}
	if !exists {
		return nil, fmt.Errorf("Bucket '%s' 不存在", cfg.Bucket)
	}

	c := &Client{Client: client, Config: cfg}

	// 如果配置了公网 Endpoint，创建公网客户端（用于预签名）
	if cfg.PublicEndpoint != "" {
		// 公网客户端同样禁用代理
		var pubTransport *http.Transport
		pubTransport, err := minio.DefaultTransport(cfg.UseSSL)
		if err != nil {
			return nil, fmt.Errorf("创建 MinIO 公网 Transport 失败: %w", err)
		}
		pubTransport.Proxy = nil

		pubClient, err := minio.New(cfg.PublicEndpoint, &minio.Options{
			Creds:     credentials.NewStaticV4(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
			Secure:    cfg.UseSSL,
			Transport: pubTransport,
		})
		if err != nil {
			return nil, fmt.Errorf("创建 MinIO 公网客户端失败 (PublicEndpoint=%s): %w", cfg.PublicEndpoint, err)
		}

		c.PublicClient = pubClient
		log.Printf("✓ MinIO 公网客户端已启用: %s", cfg.PublicEndpoint)
	} else {
		log.Println("")
		log.Println("========================================================")
		log.Println("⚠️  警告：未配置 MinIO PublicEndpoint (公网/外网 Endpoint)")
		log.Println("========================================================")
		log.Printf("内部 Endpoint 仅用于代理服务访问 MinIO: %s", cfg.Endpoint)
		log.Println("浏览器下载将通过控制服务转发，避免向 Windows 下发 minio:9000 这类容器内地址。")
		log.Println("如需绕过控制服务直连 MinIO，可在 Web UI 配置 PublicEndpoint。")
		log.Println("========================================================")
		log.Println("")
	}

	return c, nil
}
