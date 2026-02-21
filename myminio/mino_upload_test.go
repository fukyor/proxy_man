package myminio

import (
	"crypto/rand"
    "io"
    "testing"
    "github.com/stretchr/testify/assert"
	"bytes"
    "log"
)

func setUp() {
    // 初始化 MinIO
	minioConfig := Config{
		Endpoint:        "127.0.0.1:9000",
		AccessKeyID:     "root",
		SecretAccessKey: "12345678",
		UseSSL:          false,
		Bucket:          "bodydata",
		Enabled:         true,
	}
	client, err := NewClient(minioConfig)
	if err != nil {
		log.Printf("警告: MinIO 初始化失败: %v，Body 捕获功能将被禁用", err)
		return
	} else {
		GlobalClient = client
		log.Printf("MinIO 存储已启用: %s/%s", minioConfig.Endpoint, minioConfig.Bucket)
	}
}

func TestBodyCapture_Streaming(t *testing.T) {
    setUp()
    // 模拟请求体
    body := io.NopCloser(bytes.NewReader([]byte("test body data")))

    // 创建流式上传 Reader
    reader := BuildBodyReader(body, 1, "req", "text/plain", -1)

    // 读取数据（会同时触发上传）
    data := make([]byte, 100)
    n, _ := reader.Read(data)

    assert.Equal(t, 14, n)  // "test body data" 长度

    // 关闭 Reader（等待上传完成）
    reader.Close()

    // 验证上传结果
    size := reader.Capture.Size
    assert.NoError(t, reader.Capture.Error)
    assert.Equal(t, int64(14), size)
}

func TestBodyCapture_EmptyBody(t *testing.T) {
    setUp()
    // 测试空 Body
    body := io.NopCloser(bytes.NewReader([]byte{}))
       reader := BuildBodyReader(body, 2, "req", "text/plain", -1)

    reader.Close()

    size := reader.Capture.Size
    assert.NoError(t, reader.Capture.Error)
    assert.Equal(t, int64(0), size)
}

func TestBodyCapture_LargeFile(t *testing.T) {
    setUp()
    // 测试大文件流式上传（验证内存占用稳定）
    const streamSize = 100 * 1024 * 1024 // 100MB
    body := io.NopCloser(io.LimitReader(rand.Reader, streamSize))  
    reader := BuildBodyReader(body, 1, "resp", "application/octet-stream", streamSize)

    _, err := io.Copy(io.Discard, reader)
    assert.NoError(t, err, "Copy should complete without error")
    assert.NoError(t, reader.Capture.Error, "上传错误")
    reader.Close()
    size := reader.Capture.Size

    assert.Equal(t, int64(100*1024*1024), size)
}

