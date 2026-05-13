package mproxy

import "testing"

func TestNormalizeDockerConfigMigratesLocalhostMinioEndpoint(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MinioConfig.Endpoint = "127.0.0.1:9000"

	cm := &ConfigManager{
		FilePath: "/data/proxy_config/config.json",
		Current:  cfg,
	}

	cm.normalizeDockerConfig()

	if cfg.MinioConfig.Endpoint != "minio:9000" {
		t.Fatalf("Docker 配置路径下应迁移为 minio:9000，实际 %s", cfg.MinioConfig.Endpoint)
	}
}

func TestNormalizeDockerConfigKeepsLocalhostOutsideDockerPath(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MinioConfig.Endpoint = "127.0.0.1:9000"

	cm := &ConfigManager{
		FilePath: "config.json",
		Current:  cfg,
	}

	cm.normalizeDockerConfig()

	if cfg.MinioConfig.Endpoint != "127.0.0.1:9000" {
		t.Fatalf("非 Docker 配置路径不应迁移 Endpoint，实际 %s", cfg.MinioConfig.Endpoint)
	}
}
