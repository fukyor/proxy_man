package myminio

import (
	"context"
	"io"
	"net/url"
	"time"
	"fmt"
	"github.com/minio/minio-go/v7"
)

// PutObject 上传对象到 MinIO
func (c *Client) PutObject(ctx context.Context, key string, reader io.Reader, contentType string) (minio.UploadInfo, error) {
	opts := minio.PutObjectOptions{
		ContentType: contentType,
	}
	return c.Client.PutObject(ctx, c.Config.Bucket, key, reader, -1, opts)
}

// PutObjectWithSize 上传对象到 MinIO（指定大小）
// size >= 0 时直接流式上传，size < 0 时 SDK 会缓冲到内存
func (c *Client) PutObjectWithSize(ctx context.Context, key string, reader io.Reader, size int64, contentType string) (minio.UploadInfo, error) {
	opts := minio.PutObjectOptions{
		ContentType: contentType,
	}
	return c.Client.PutObject(ctx, c.Config.Bucket, key, reader, size, opts)
}

// StatObject 获取对象信息
func (c *Client) StatObject(key string) (minio.ObjectInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return c.Client.StatObject(ctx, c.Config.Bucket, key, minio.StatObjectOptions{})
}

// GetPresignedURL 生成预签名下载 URL
// filename: 下载时显示的文件名
func (c *Client) GetPresignedURL(key string, expiry time.Duration, filename string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 设置响应头覆盖参数，强制浏览器下载（参考官方文档）
	reqParams := make(url.Values)
	reqParams.Set("response-content-disposition", "attachment; filename=\""+filename+"\"")

	presignedURL, err := c.Client.PresignedGetObject(ctx, c.Config.Bucket, key, expiry, reqParams)
	if err != nil {
		return "", err
	}
	return presignedURL.String(), nil
}

// IsEnabled 检查 MinIO 是否已启用
func IsEnabled() bool {
	return GlobalClient != nil && GlobalClient.Config.Enabled
}

// GetObjectKey 生成对象存储的 Key
// 格式: mitm-data/YYYY-MM-DD/SessionID/bodyType
// 示例: mitm-data/2026-02-04/10086/req
func GetObjectKey(sessionID int64, bodyType string) string {
	date := time.Now().Format("2006-01-02")
	return fmt.Sprintf("mitm-data/%s/%d/%s", date, sessionID, bodyType)
}