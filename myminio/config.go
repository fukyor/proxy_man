package myminio

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
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

// MinioConfigManager 负责 MinIO 配置的线程安全读写及文件持久化
type MinioConfigManager struct {
	FilePath string
	Current  *Config
	mu       sync.RWMutex
}

// DefaultMinioConfig 提供一套开箱即用的默认 MinIO 配置
func DefaultMinioConfig() *Config {
	return &Config{
		Endpoint:        "127.0.0.1:9000",
		PublicEndpoint:  "",
		AccessKeyID:     "root",
		SecretAccessKey: "12345678",
		UseSSL:          false,
		Bucket:          "bodydata",
		Enabled:         true,
	}
}

// NewMinioConfigManager 接受绝对路径，由调用方负责拼接
func NewMinioConfigManager(filePath string) *MinioConfigManager {
	cm := &MinioConfigManager{
		FilePath: filePath,
		Current:  DefaultMinioConfig(),
	}
	cm.Load()
	return cm
}

// Load 从本地磁盘读取 JSON 配置文件
func (cm *MinioConfigManager) Load() error {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	data, err := os.ReadFile(cm.FilePath)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("MinIO 配置文件不存在，将自动创建默认配置: %s", cm.FilePath)
			return cm.saveLocked()
		}
		log.Printf("读取 MinIO 配置文件失败: %v", err)
		return err
	}
	if err := json.Unmarshal(data, cm.Current); err != nil {
		log.Printf("解析 MinIO 配置文件失败: %v", err)
		return err
	}
	return nil
}

// Save 将当前内存配置持久化写入磁盘
func (cm *MinioConfigManager) Save() error {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.saveLocked()
}

func (cm *MinioConfigManager) saveLocked() error {
	dir := filepath.Dir(cm.FilePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("创建目录失败: %w", err)
	}
	data, err := json.MarshalIndent(cm.Current, "", "  ")
	if err != nil {
		return err
	}
	tmpFile := cm.FilePath + ".tmp"
	if err := os.WriteFile(tmpFile, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmpFile, cm.FilePath)
}

// GetConfig 线程安全获取配置副本
func (cm *MinioConfigManager) GetConfig() Config {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return *cm.Current
}

// NewClient 创建新的 MinIO 客户端
func NewClient(cm *MinioConfigManager) (*Client, error) {
	cfg := cm.GetConfig()
	if !cfg.Enabled {
		return nil, fmt.Errorf("致命错误: MinIO 存储未开启 (Enabled: false)。系统强制要求必须开启 MinIO！")
	}

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
		log.Println("警告：如果未配置该项，您将无法生成并在外网完成远程直链下载！")
		log.Println("请在 minio.json 中补充填写此项，以便代理正常下发直链。")
		log.Println("========================================================")
		log.Println("")
	}

	return c, nil
}
