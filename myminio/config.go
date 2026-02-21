package myminio

import (
	"context"
	"fmt"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Config MinIO 配置结构
type Config struct {
	Endpoint        string
	AccessKeyID     string
	SecretAccessKey string
	UseSSL          bool
	Bucket          string
	Enabled         bool
}

// Client MinIO 客户端封装
type Client struct {
	Client *minio.Client
	Config Config
}

// GlobalClient 全局 MinIO 客户端实例
var GlobalClient *Client

// NewClient 创建新的 MinIO 客户端
func NewClient(cfg Config) (*Client, error) {
	if !cfg.Enabled {
		return nil, nil
	}

	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		Secure: cfg.UseSSL,
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

	return &Client{Client: client, Config: cfg}, nil
}
