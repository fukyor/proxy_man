package myminio

import (
	"net/url"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

func TestDefaultMinioConfigUsesDockerEndpoint(t *testing.T) {
	cfg := DefaultMinioConfig()

	if cfg.Endpoint != "minio:9000" {
		t.Fatalf("默认内部 Endpoint 错误：期望 minio:9000，实际 %s", cfg.Endpoint)
	}
	if cfg.PublicEndpoint != "" {
		t.Fatalf("默认 PublicEndpoint 应为空字符串，实际 %s", cfg.PublicEndpoint)
	}
}

func TestPresignedURLUsesInternalEndpointWhenPublicEndpointEmpty(t *testing.T) {
	cfg := *DefaultMinioConfig()

	rawClient, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		Secure: cfg.UseSSL,
		Region: "us-east-1",
	})
	if err != nil {
		t.Fatalf("创建测试 MinIO 客户端失败：%v", err)
	}

	client := &Client{
		Client: rawClient,
		Config: cfg,
	}
	presignedURL, err := client.GetPresignedURL("mitm-data/test/1/req", time.Hour, "body.bin")
	if err != nil {
		t.Fatalf("生成预签名链接失败：%v", err)
	}

	parsedURL, err := url.Parse(presignedURL)
	if err != nil {
		t.Fatalf("解析预签名链接失败：%v", err)
	}
	if parsedURL.Host != "minio:9000" {
		t.Fatalf("预签名链接 Host 错误：期望 minio:9000，实际 %s", parsedURL.Host)
	}
}
