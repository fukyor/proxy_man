package myminio

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
)

// BodyCapture Body上传状态
type BodyCapture struct {
	ObjectKey   string // MinIO 对象 Key
	Size        int64  // 上传后的实际大小
	Uploaded    bool   // 是否成功上传
	ContentType string // 内容类型
	Error       error  // 上传过程中的错误
}

// bodyCaptReader 流式上传包装器
type bodyCaptReader struct {
	inner         io.ReadCloser  // 内层 Reader（通常是流量统计层）
	pipeWriter    *io.PipeWriter // 用于向上传协程传输数据
	Capture       *BodyCapture   // 捕获状态
	doneCh        chan struct{}  // 上传完成信号
	skipUpload    bool           // 是否跳过捕获
	contentLength int64          // HTTP Content-Length（-1 表示未知）
}

// shouldSkipCapture 判断是否应该跳过捕获
func shouldSkipCapture(contentType string) bool {
	skipTypes := []string{
		"text/event-stream",         // SSE
		"websocket",                 // WebSocket
		"multipart/x-mixed-replace", // 流式响应
	}
	lowerType := strings.ToLower(contentType)
	for _, t := range skipTypes {
		if strings.Contains(lowerType, t) {
			return true
		}
	}
	return false
}

// BuildBodyReader 包装 Body 进行 MinIO 捕获
// 参数:
//   - inner: 内层 io.ReadCloser（通常是流量统计层）
//   - sessionID: 会话 ID
//   - bodyType: "req" 或 "resp"
//   - contentType: Content-Type 头
//   - contentLength: HTTP Content-Length（-1 表示未知）
// 返回:
//   - *bodyCaptReader: 包装后的 Reader
func BuildBodyReader(inner io.ReadCloser, sessionID int64, bodyType, contentType string, contentLength int64) *bodyCaptReader {
	// 如果 MinIO 未启用或内容类型需要跳过，直接返回透传 Reader
	if !IsEnabled() || shouldSkipCapture(contentType) {
		return &bodyCaptReader{inner: inner, skipUpload: true}
	}

	// 创建 Pipe 用于数据流转
	pr, pw := io.Pipe()
	reader := &bodyCaptReader{
		inner:         inner,
		pipeWriter:    pw,
		Capture: &BodyCapture{
			ObjectKey:   GetObjectKey(sessionID, bodyType),
			ContentType: contentType,
		},
		doneCh:        make(chan struct{}),
		contentLength: contentLength,
	}

	// 启动上传协程
	go reader.uploadToMinIO(pr)

	return reader
}

// uploadToMinIO 上传到 MinIO（在独立协程中运行）
func (r *bodyCaptReader) uploadToMinIO(pr *io.PipeReader) {
	defer close(r.doneCh)
	defer pr.Close()

	// 设置上传超时（30分钟）
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	var info minio.UploadInfo
	var err error

	if r.contentLength >= 0 {
		// 策略 A：已知长度（包括 0），直接流式上传
		info, err = GlobalClient.PutObjectWithSize(ctx, r.Capture.ObjectKey, pr, r.contentLength, r.Capture.ContentType)
	} else {
		// 策略 B：未知长度（chunked），使用临时文件
		info, err = r.uploadViaTempFile(ctx, pr)
	}

	if err != nil {
		r.Capture.Error = err
		return
	}

	// 记录上传成功信息
	r.Capture.Size = info.Size
	r.Capture.Uploaded = true
}

// uploadViaTempFile 通过临时文件上传（用于未知大小的情况）
func (r *bodyCaptReader) uploadViaTempFile(ctx context.Context, pr *io.PipeReader) (minio.UploadInfo, error) {
	// 使用相对路径便于调试观察
	if err := os.MkdirAll("myminio/tmp", 0755); err != nil {
		return minio.UploadInfo{}, fmt.Errorf("创建临时目录失败: %w", err)
	}
	tempFile, err := os.CreateTemp("myminio/tmp", "upload-*.tmp")
	if err != nil {
		return minio.UploadInfo{}, fmt.Errorf("创建临时文件失败: %w", err)
	}
	tempPath := tempFile.Name()

	// 确保清理（延迟到函数末尾）
	defer os.Remove(tempPath)

	// 1. 写入临时文件
	size, err := io.Copy(tempFile, pr)
	if err != nil {
		tempFile.Close()
		return minio.UploadInfo{}, fmt.Errorf("写入临时文件失败: %w", err)
	}

	// 2. 重置文件指针
	if _, err := tempFile.Seek(0, 0); err != nil {
		tempFile.Close()
		return minio.UploadInfo{}, fmt.Errorf("重置文件指针失败: %w", err)
	}

	// 3. 上传（使用已知 size）
	contentType := r.Capture.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	// 1. 不需要担心minio上传大文件导致OOM，因为size较大时minio会使用分片上传，大概也只会消耗几十MB的内存作为minio客户端和socket之间的缓冲
	// 2. 那为什么大文件上传不直接设置为-1，使用自动分片？因为S3 标准最多允许 10,000 个分片。10000个分片是存储桶中存储对象的分块数量，所以
	// 导致了SDK最多可以分10000块上传，我们传入文件大小辅助minio确定分片最小大小为size/10000。有助于更小的内存占用。
	info, err := GlobalClient.PutObjectWithSize(ctx, r.Capture.ObjectKey, tempFile, size, contentType)

	// 4. 上传完成后关闭文件（在 Remove 之前）
	tempFile.Close()

	return info, err
}

// Read 实现 io.Reader 接口
func (r *bodyCaptReader) Read(p []byte) (n int, err error) {
	// 从内层 Reader 读取数据
	n, err = r.inner.Read(p)

	// 如果没有跳过捕获且读取到数据，写入 Pipe
	if !r.skipUpload && n > 0 && r.pipeWriter != nil {
		if _, writeErr := r.pipeWriter.Write(p[:n]); writeErr != nil {
			// 记录写入错误但不影响主流程
			r.Capture.Error = writeErr
		}
	}

	return n, err
}

// Close 实现 io.Closer 接口
func (r *bodyCaptReader) Close() error {
	// 关闭 PipeWriter，通知上传协程数据已结束
	if r.pipeWriter != nil {
		r.pipeWriter.Close()
	}

	// 等待上传完成
	if r.doneCh != nil {
		<-r.doneCh
	}

	// 关闭内层 Reader
	return r.inner.Close()
}
