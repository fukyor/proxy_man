package main

import (
	"crypto/rand"
	"fmt"
	"log"
	"os"
	"path/filepath"
)

// TestDataConfig 测试数据配置
type TestDataConfig struct {
	Name string
	Size int64
	Desc string
}

// 测试数据配置列表
var testDataConfigs = []TestDataConfig{
	{"small_1k.bin", 1024, "1KB 小文件"},
	{"medium_100k.bin", 100 * 1024, "100KB 中等文件"},
	{"large_1m.bin", 1024 * 1024, "1MB 大文件"},
	{"large_2m.bin", 2 * 1024 * 1024, "2MB 大文件"},
	{"huge_5m.bin", 5 * 1024 * 1024, "5MB 超大文件"},
	{"huge_100m.bin", 100 * 1024 * 1024, "100MB 超大文件"},
	{"huge_200m.bin", 200 * 1024 * 1024, "200MB 超大文件"},
}

// generateRandomBytes 生成随机字节流（更真实的测试数据）
func generateRandomBytes(size int64) ([]byte, error) {
	data := make([]byte, size)
	_, err := rand.Read(data)
	if err != nil {
		return nil, fmt.Errorf("生成随机数据失败: %w", err)
	}
	return data, nil
}

// ensureDir 确保目录存在
func ensureDir(dir string) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("创建目录失败: %w", err)
	}
	return nil
}

// writeTestFile 写入测试文件
func writeTestFile(dir string, config TestDataConfig) error {
	filePath := filepath.Join(dir, config.Name)

	// 检查文件是否已存在
	if info, err := os.Stat(filePath); err == nil {
		if info.Size() == config.Size {
			log.Printf("✓ %s 已存在，大小正确 (%d KB)", config.Name, config.Size/1024)
			return nil
		}
		log.Printf("⚠ %s 已存在但大小不匹配，重新生成", config.Name)
	}

	log.Printf("生成 %s (%d bytes)...", config.Desc, config.Size)

	data, err := generateRandomBytes(config.Size)
	if err != nil {
		return err
	}

	if err := os.WriteFile(filePath, data, 0644); err != nil {
		return fmt.Errorf("写入文件失败: %w", err)
	}

	log.Printf("✓ 已生成: %s (%d bytes)", config.Name, config.Size)
	return nil
}

func main() {
	execDir := `E:\D\zuoyewenjian\MyProject\proxy_man\`
	outputDir := filepath.Join(execDir, "test/data")
	fmt.Println(outputDir)

	// 确保输出目录存在
	if err := ensureDir(outputDir); err != nil {
		log.Fatal("❌ 错误:", err)
	}

	// 生成所有测试文件
	for _, config := range testDataConfigs {
		if err := writeTestFile(outputDir, config); err != nil {
			log.Fatal("❌ 生成文件失败:", err)
		}
	}
}