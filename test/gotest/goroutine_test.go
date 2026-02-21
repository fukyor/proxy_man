package main_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"proxy_man/myminio"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/stretchr/testify/assert"
	"go.uber.org/goleak"
)

// 专门测试 BuildBodyReader 是否正确释放了协程
func TestBodyReader_Leak(t *testing.T) {
	// 初始化 MinIO
	endpoint := "127.0.0.1:9000"
	accessKeyID := "root"
	secretAccessKey := "12345678"
	useSSL := false
    bucketName := "bodydata"
	tr := &http.Transport{}

	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKeyID, secretAccessKey, ""),
		Secure: useSSL,
		Transport: tr, // <--- 关键点：在这里注入
	})
	// 检查 Bucket 是否存在（可选，确保服务是通的）
    exists, err := client.BucketExists(context.Background(), bucketName)
    if err != nil || !exists {
        // 如果连不上 MinIO，测试也没法跑，可以选择跳过或报错
        t.Logf("警告: 无法连接 MinIO (%v)，测试可能不准确", err)
    }
	myminio.GlobalClient = &myminio.Client{
		Client: client,
		Config: myminio.Config{
            Enabled: true, 
            Bucket:  "bodydata",
        },
	}

	defer goleak.VerifyNone(t)
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
    defer cancel()
	defer tr.CloseIdleConnections() // <--- 测试结束强制关闭空闲连接
	// 2. 构造数据
	
	fakeBody := io.NopCloser(strings.NewReader("test data"))
	
	// 3. 调用被测函数（这会启动一个 goroutine）
	reader := myminio.BuildBodyReader(fakeBody, 123, "req", "text/plain", 9)

	rn, err := io.Copy(io.Discard, reader)
	if err != nil {
		t.Fatal("❌ iocopy读取失败")
	}

	// 5. 关键：调用 Close。如果 Close 实现有问题，uploadToMinIO 协程就会挂起
	done := make(chan struct{})
	go func() {
        // 这里的 Close 如果卡死，只会卡死这个协程，不会卡死主测试线程
        reader.Close()
        close(done)
    }()

    select {
    case <-done:
        // A面：Close 在 1 分钟内成功返回
        // 在这里进行正常的 Assert 验证，例如验证 Size 或 Error
        assert.Equal(t, rn, reader.Capture.Size)
        
    case <-ctx.Done():
        // B面：超时时间到了，Close 还没返回
        // 直接判定测试失败，打印堆栈信息，强制结束当前测试用例
        t.Fatal("❌ 测试超时：reader.Close() 发生死锁或耗时过长，已强制终止")
    }
    
    // 函数返回时，defer goleak.VerifyNone 会自动执行检查
}

// setupRealMinioClient 初始化真实 MinIO 客户端，返回 Transport 供测试结束时清理连接池
func setupRealMinioClient(t *testing.T) *http.Transport {
	endpoint := "127.0.0.1:9000"
	accessKeyID := "root"
	secretAccessKey := "12345678"
	bucketName := "bodydata"

	tr := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 100,
		IdleConnTimeout:     90 * time.Second,
	}

	client, err := minio.New(endpoint, &minio.Options{
		Creds:     credentials.NewStaticV4(accessKeyID, secretAccessKey, ""),
		Secure:    false,
		Transport: tr,
	})
	if err != nil {
		t.Fatalf("MinIO 客户端初始化失败: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := client.BucketExists(ctx, bucketName); err != nil {
		t.Skipf("MinIO 不可用，跳过真实网络测试: %v", err)
	}

	myminio.GlobalClient = &myminio.Client{
		Client: client,
		Config: myminio.Config{
			Bucket:  bucketName,
			Enabled: true,
		},
	}

	return tr
}

// TestConcurrent_RealNetwork_KnownLength 已知长度路径的高并发协程泄露测试
// 50 个并发 goroutine，使用 100MB 真实测试文件，走流式直传路径
// go test -v -run=TestConcurrent_RealNetwork_KnownLength -timeout 10m
func TestConcurrent_RealNetwork_KnownLength(t *testing.T) {
	tr := setupRealMinioClient(t)
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	// 读取 100MB 真实测试文件（一次性加载，所有 goroutine 共享同一块内存）
	testData, err := os.ReadFile(`E:\D\zuoyewenjian\MyProject\proxy_man\test\data\huge_100m.bin`)
	if err != nil {
		t.Fatalf("读取测试数据文件失败: %v", err)
	}
	dataSize := int64(len(testData))
	t.Logf("测试数据大小: %d bytes (%.1f MB)", dataSize, float64(dataSize)/1024/1024)

	concurrency := 50
	var wg sync.WaitGroup
	var errCount atomic.Int64
	wg.Add(concurrency)

	start := time.Now()

	for i := 0; i < concurrency; i++ {
		go func(id int) {
			defer wg.Done()

			// bytes.NewReader 不复制数据，仅引用同一块 testData
			fakeBody := io.NopCloser(bytes.NewReader(testData))
			reqID := int64(30000 + id)
			reader := myminio.BuildBodyReader(fakeBody, reqID, "req", "application/octet-stream", dataSize)

			rn, err := io.Copy(io.Discard, reader)
			if err != nil {
				t.Errorf("goroutine %d 读取错误: %v", id, err)
				errCount.Add(1)
				return
			}

			// 死锁检测：Close 超过 2 分钟则视为死锁
			closeDone := make(chan struct{})
			go func() {
				reader.Close()
				close(closeDone)
			}()

			select {
			case <-closeDone:
				assert.Equal(t, rn, reader.Capture.Size)
				// 正常关闭
			case <-time.After(2 * time.Minute):
				t.Errorf("goroutine %d: Close() 超时，疑似死锁", id)
				errCount.Add(1)
			}
		}(i)
	}

	wg.Wait()
	t.Logf("所有并发请求完成，耗时: %v", time.Since(start))

	if errCount.Load() > 0 {
		t.Errorf("共有 %d 个 goroutine 出错", errCount.Load())
	}

	tr.CloseIdleConnections()
	// 很关键，等待一段时间让GC回收goroutine
	time.Sleep(500 * time.Millisecond)
}

// TestConcurrent_RealNetwork_Chunked 未知长度路径（chunked）的高并发协程泄露测试
// 30 个并发 goroutine，contentLength=-1 走临时文件路径
// go test -v -run=TestConcurrent_RealNetwork_Chunked -timeout 10m
func TestConcurrent_RealNetwork_Chunked(t *testing.T) {
	tr := setupRealMinioClient(t)
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	concurrency := 30
	// 读取 200MB 真实测试文件（一次性加载，所有 goroutine 共享同一块内存）
	testData, err := os.ReadFile(`E:\D\zuoyewenjian\MyProject\proxy_man\test\data\huge_200m.bin`)
	if err != nil {
		t.Fatalf("读取测试数据文件失败: %v", err)
	}
	dataSize := int64(len(testData))
	t.Logf("测试数据大小: %d bytes (%.1f MB)", dataSize, float64(dataSize)/1024/1024)

	var wg sync.WaitGroup
	var errCount atomic.Int64
	wg.Add(concurrency)

	for i := 0; i < concurrency; i++ {
		go func(id int) {
			defer wg.Done()

			fakeBody := io.NopCloser(bytes.NewReader(testData))
			reqID := int64(40000 + id)
			reader := myminio.BuildBodyReader(fakeBody, reqID, "resp", "application/json", -1)

			rn, err := io.Copy(io.Discard, reader)
			if err != nil {
				t.Errorf("goroutine %d 读取错误: %v", id, err)
				errCount.Add(1)
				return
			}

			closeDone := make(chan struct{})
			go func() {
				reader.Close()
				close(closeDone)
			}()

			select {
			case <-closeDone:
				assert.Equal(t, rn, reader.Capture.Size)
			case <-time.After(10 * time.Minute):
				t.Errorf("goroutine %d: Close() 超时，疑似死锁", id)
				errCount.Add(1)
			}
		}(i)
	}

	wg.Wait()

	if errCount.Load() > 0 {
		t.Errorf("共有 %d 个 goroutine 出错", errCount.Load())
	}

	tr.CloseIdleConnections()
	// 很关键，等待一段时间让GC回收goroutine
	time.Sleep(500 * time.Millisecond)
}
