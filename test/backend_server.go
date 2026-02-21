package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"time"
	"os"
	"strconv"
)

// TestData 测试数据结构
type TestData struct {
	Name   string
	Size   int64
	Data   []byte
}


// generateBytes 生成指定长度的测试字节流
func generateBytes(size int64) []byte {
	data := make([]byte, size)
	for i := int64(0); i < size; i++ {
		data[i] = byte(i % 256)
	}
	return data
}

// handleTestDownload 处理测试下载请求
func handleTestDownload(w http.ResponseWriter, r *http.Request) {
	// 获取文件名参数
	filename := r.URL.Query().Get("file")
	if filename == "" {
		http.Error(w, "缺少 file 参数", http.StatusBadRequest)
		return
	}
    // 从磁盘读取文件
    filePath := filepath.Join(`E:\D\zuoyewenjian\MyProject\proxy_man\test\data`, filename)

    file, err := os.Open(filePath)
    if err != nil {
        http.Error(w, "文件不存在", http.StatusNotFound)
        return
    }
	defer file.Close()

	fileInfo, err := file.Stat()
	if err != nil {
		http.Error(w, "无法获取文件信息", http.StatusInternalServerError)
		return
	}

    // 保留原有的自定义逻辑
    w.Header().Set("Content-Type", "application/octet-stream")
    w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", filename))
    w.Header().Set("Content-Length", strconv.FormatInt(fileInfo.Size(), 10))

    start := time.Now()
    written, err := io.Copy(w, file)
    duration := time.Since(start)

    if err != nil {
        log.Printf("[下载失败] 文件: %s | 已发送: %d | 错误: %v", filename, written, err)
    } else {
        log.Printf("[下载] 文件: %s | 大小: %d 字节 | 耗时: %v | 速度: %.2f MB/s",
            filename, written, duration, float64(written)/(1024*1024)/duration.Seconds())
    }
}

// handleTestUpload 处理测试上传请求
func handleTestUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "只支持 POST 方法", http.StatusMethodNotAllowed)
		return
	}

	start := time.Now()
	size, err := io.Copy(io.Discard, r.Body)
	r.Body.Close()
	duration := time.Since(start)

	if err != nil {
		log.Printf("[上传] 错误: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	speed := float64(size) / (1024 * 1024) / duration.Seconds()
	log.Printf("[上传] 接收大小: %d 字节 | 耗时: %v | 速度: %.2f MB/s",
		size, duration, speed)

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"status":"success","size":%d,"duration_ms":%d,"speed_mb_s":%.2f}`,
		size, duration.Milliseconds(), speed)
}

// handleTestDownloadChunked 处理 Chunked 编码下载请求
func handleTestDownloadChunked(w http.ResponseWriter, r *http.Request) {
	filename := r.URL.Query().Get("file")
	if filename == "" {
		http.Error(w, "缺少 file 参数", http.StatusBadRequest)
		return
	}

	filePath := filepath.Join(`E:\D\zuoyewenjian\MyProject\proxy_man\test\data`, filename)
	file, err := os.Open(filePath)
	if err != nil {
		http.Error(w, "文件不存在", http.StatusNotFound)
		return
	}
	defer file.Close()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	// 不设置 Content-Length → 自动 Transfer-Encoding: chunked

	flusher, canFlush := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	var written int64
	start := time.Now()

	for {
		n, err := file.Read(buf)
		if n > 0 {
			w.Write(buf[:n])
			written += int64(n)
			if canFlush {
				flusher.Flush()
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Printf("[Chunked下载失败] %s | 已发送: %d | %v", filename, written, err)
			return
		}
	}

	duration := time.Since(start)
	log.Printf("[Chunked下载] %s | %d字节 | %v | %.2fMB/s",
		filename, written, duration, float64(written)/(1024*1024)/duration.Seconds())
}

// handleRoot 根路径，显示可用接口
func handleRoot(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	html := `
<!DOCTYPE html>
<html>
<head>
    <title>测试后端服务器</title>
    <meta charset="utf-8">
    <style>
        body { font-family: monospace; margin: 40px; background: #1e1e1e; color: #d4d4d4; }
        h1 { color: #4ec9b0; }
        .endpoint { background: #252526; padding: 15px; margin: 10px 0; border-left: 3px solid #4ec9b0; }
        .method { color: #dcdcaa; font-weight: bold; }
        .path { color: #9cdcfe; }
        .desc { color: #6a9955; margin-top: 5px; }
    </style>
</head>
<body>
    <h1>🧪 测试后端服务器 (端口 9001)</h1>
    <div class="endpoint">
        <div><span class="method">GET</span> <span class="path">http://localhost:9001/test/download?file=small_1k.bin</span></div>
        <div class="desc">返回 1KB 测试数据</div>
    </div>
    <div class="endpoint">
        <div><span class="method">GET</span> <span class="path">http://localhost:9001/test/download?file=medium_100k.bin</span></div>
        <div class="desc">返回 100KB 测试数据</div>
    </div>
    <div class="endpoint">
        <div><span class="method">GET</span> <span class="path">http://localhost:9001/test/download?file=large_1m.bin</span></div>
        <div class="desc">返回 1MB 测试数据</div>
    </div>
    <div class="endpoint">
        <div><span class="method">POST</span> <span class="path">http://localhost:9001/test/upload</span></div>
        <div class="desc">接收上传数据并返回统计信息</div>
    </div>
    <div class="endpoint">
        <div><span class="method">GET</span> <span class="path">/health</span></div>
        <div class="desc">健康检查</div>
    </div>
</body>
</html>
`
	w.Write([]byte(html))
}

// handleHealth 健康检查
func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"status":"healthy","time":"%s"}`, time.Now().Format(time.RFC3339))
}

func main() {
	// 1. 创建共享的路由多路复用器 (Mux)
	// 这样 HTTP 和 HTTPS 会使用完全相同的处理逻辑
	mux := http.NewServeMux()
	mux.HandleFunc("/", handleRoot)
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/test/download", handleTestDownload)
	mux.HandleFunc("/test/download/chunked", handleTestDownloadChunked)
	mux.HandleFunc("/test/upload", handleTestUpload)

	// 2. 配置 HTTP 服务器 (端口 9001)
	httpServer := &http.Server{
		Addr:         ":9001",
		Handler:      mux, // 使用共享的 mux
		ReadTimeout:  5 * time.Minute,
		WriteTimeout: 5 * time.Minute,
	}

	// 3. 配置 HTTPS 服务器 (端口 9002)
	httpsServer := &http.Server{
		Addr:         ":9002",
		Handler:      mux, // 使用共享的 mux
		ReadTimeout:  5 * time.Minute,
		WriteTimeout: 5 * time.Minute,
	}

	// 4. 在 Goroutine 中启动 HTTP 服务器
	// 使用 go 关键字使其在后台运行，不会阻塞后续代码
	go func() {
		log.Println("🚀 HTTP  服务器启动在 :9001 (无加密)")
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP 服务器启动失败: %v", err)
		}
	}()

	// 5. 在主线程启动 HTTPS 服务器
	// 注意：这里需要传入刚才生成的证书路径
	log.Println("🔒 HTTPS 服务器启动在 :9002 (TLS加密)")
	log.Println("📄 访问 http://localhost:9001 或 https://localhost:9002")
	
	// ListenAndServeTLS 会阻塞主线程，保持程序运行
	if err := httpsServer.ListenAndServeTLS("server.crt", "server.key"); err != nil && err != http.ErrServerClosed {
		log.Fatal("HTTPS 服务器启动失败:", err)
	}
}