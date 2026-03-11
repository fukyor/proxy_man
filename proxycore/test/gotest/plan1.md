# proxy_man 压力基准测试实施计划

## 背景

为 proxy_man 代理服务器实施高负载基准测试，将基准测试作为"流量大炮"（Load Generator），配合 pprof 监控代理服务器在高并发场景下的性能表现。

## 目标

1. 产生持续、稳定的高并发流量
2. 不让客户端成为瓶颈（避免磁盘 I/O、客户端 CPU 限制）
3. 为 pprof 监控提供足够的压力场景
4. 覆盖 HTTP/HTTPS、上传/下载多种场景

## 关键文件

| 文件路径                        | 操作         | 说明                                    |
| ------------------------------- | ------------ | --------------------------------------- |
| `test/gotest/benchmark_test.go` | 修改         | 添加压力测试函数                        |
| `test/gotest/common.go`         | 新建         | 提取共享配置，避免与 fetch_test.go 重复 |
| `test/backend_server.go`        | **新增接口** | 添加 `/test/download/chunked` 接口      |
| `main.go`                       | 不变         | pprof 已配置在 `:6060`                  |

## 实施步骤

### 步骤 1：backend_server.go 新增 Chunked 下载接口

在 `test/backend_server.go` 中新增处理函数：

```go
// handleTestDownloadChunked 处理 Chunked 编码下载请求
// 关键：不设置 Content-Length，让 HTTP 自动使用 Transfer-Encoding: chunked
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

    // 关键：只设置 Content-Type，不设置 Content-Length
    // 这样 http.ResponseWriter 会自动使用 Transfer-Encoding: chunked
    w.Header().Set("Content-Type", "application/octet-stream")
    w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", filename))
    // 故意不设置 Content-Length，触发 Chunked 编码

    start := time.Now()
    written, err := io.Copy(w, file)
    duration := time.Since(start)

    if err != nil {
        log.Printf("[Chunked下载失败] 文件: %s | 已发送: %d | 错误: %v", filename, written, err)
    } else {
        log.Printf("[Chunked下载] 文件: %s | 大小: %d 字节 | 耗时: %v | 速度: %.2f MB/s",
            filename, written, duration, float64(written)/(1024*1024)/duration.Seconds())
    }
}
```

注册路由（在 `main()` 函数的 `mux` 中添加）：

```go
mux.HandleFunc("/test/download/chunked", handleTestDownloadChunked)
```

### 步骤 2：创建共享配置文件 `test/gotest/common.go`

```go
package main_test

const (
    // 代理服务器地址
    ProxyAddr = "127.0.0.1:8080"

    // 后端服务器地址
    HttpBackendBaseURL  = "http://127.0.0.1:9001"
    HttpsBackendBaseURL = "https://127.0.0.1:9002"

    // 测试数据目录
    TestDataDir = `E:\D\zuoyewenjian\MyProject\proxy_man\test\data`
)

var (
    // 全局测试数据，在 init() 中加载
    Payload100K []byte // 100KB
    Payload1M   []byte // 1MB
    Payload5M   []byte // 5MB
)
```

### 步骤 3：扩展 `test/gotest/benchmark_test.go`

添加八种压力测试函数，覆盖上传/下载、协议和传输模式的组合：

```go
// ========== 上行压力测试（Upload）==========

// 1. HTTP 上传 + 已知文件大小（Content-Length 明确）
func Benchmark_Stress_HTTP_Upload_KnownSize(b *testing.B)

// 2. HTTP 上传 + 未知文件大小（触发 Transfer-Encoding: chunked）
func Benchmark_Stress_HTTP_Upload_Chunked(b *testing.B)

// 3. HTTPS (MITM) 上传 + 已知文件大小
func Benchmark_Stress_HTTPS_Upload_KnownSize(b *testing.B)

// 4. HTTPS (MITM) 上传 + 未知文件大小（触发 chunked）
func Benchmark_Stress_HTTPS_Upload_Chunked(b *testing.B)

// ========== 下行压力测试（Download）==========

// 5. HTTP 下载（后端返回已知大小文件）
func Benchmark_Stress_HTTP_Download_KnownSize(b *testing.B)

// 6. HTTP 下载 Chunked（后端使用 Transfer-Encoding: chunked）
func Benchmark_Stress_HTTP_Download_Chunked(b *testing.B)

// 7. HTTPS (MITM) 下载（已知大小）
func Benchmark_Stress_HTTPS_Download_KnownSize(b *testing.B)

// 8. HTTPS (MITM) 下载 Chunked
func Benchmark_Stress_HTTPS_Download_Chunked(b *testing.B)
```

#### 测试矩阵

| 场景 | 方向 | 协议  | Content-Length | Transfer-Encoding | 后端接口                 |
| ---- | ---- | ----- | -------------- | ----------------- | ------------------------ |
| 1    | 上传 | HTTP  | ✓ 明确         | -                 | `/test/upload`           |
| 2    | 上传 | HTTP  | -              | ✓ chunked         | `/test/upload`           |
| 3    | 上传 | HTTPS | ✓ 明确         | -                 | `/test/upload`           |
| 4    | 上传 | HTTPS | -              | ✓ chunked         | `/test/upload`           |
| 5    | 下载 | HTTP  | ✓ 明确         | -                 | `/test/download`         |
| 6    | 下载 | HTTP  | -              | ✓ chunked         | `/test/download/chunked` |
| 7    | 下载 | HTTPS | ✓ 明确         | -                 | `/test/download`         |
| 8    | 下载 | HTTPS | -              | ✓ chunked         | `/test/download/chunked` |

### 步骤 4：压力测试核心设计

#### 上行测试（Upload）

**已知文件大小（Content-Length）**

```go
func Benchmark_Stress_HTTP_Upload_KnownSize(b *testing.B) {
    url := HttpBackendBaseURL + "/test/upload"
    b.SetBytes(int64(len(Payload1M)))  // 记录吞吐量
    b.RunParallel(func(pb *testing.PB) {
        for pb.Next() {
            reader := bytes.NewReader(Payload1M)
            req, _ := http.NewRequest("POST", url, reader)
            req.ContentLength = int64(len(Payload1M))  // ← 关键：明确设置
            req.Header.Set("Content-Type", "application/octet-stream")
            resp, _ := stressClient.Do(req)
            if resp != nil {
                io.Copy(io.Discard, resp.Body)
                resp.Body.Close()
            }
        }
    })
}
```

**未知文件大小（触发 chunked）**

```go
func Benchmark_Stress_HTTP_Upload_Chunked(b *testing.B) {
    url := HttpBackendBaseURL + "/test/upload"
    b.SetBytes(int64(len(Payload1M)))
    b.RunParallel(func(pb *testing.PB) {
        for pb.Next() {
            // 使用 io.NopCloser 包装，Go 自动使用 chunked
            reader := io.NopCloser(bytes.NewReader(Payload1M))
            req, _ := http.NewRequest("POST", url, reader)
            // 故意不设置 ContentLength，触发 Transfer-Encoding: chunked
            req.Header.Set("Content-Type", "application/octet-stream")
            resp, _ := stressClient.Do(req)
            if resp != nil {
                io.Copy(io.Discard, resp.Body)
                resp.Body.Close()
            }
        }
    })
}
```

#### 下行测试（Download）

**HTTP 下载（已知大小）**

```go
func Benchmark_Stress_HTTP_Download_KnownSize(b *testing.B) {
    url := HttpBackendBaseURL + "/test/download?file=large_1m.bin"
    b.SetBytes(int64(len(Payload1M)))
    b.RunParallel(func(pb *testing.PB) {
        for pb.Next() {
            resp, _ := stressClient.Get(url)
            if resp != nil {
                io.Copy(io.Discard, resp.Body)
                resp.Body.Close()
            }
        }
    })
}
```

**HTTP 下载 Chunked**

```go
func Benchmark_Stress_HTTP_Download_Chunked(b *testing.B) {
    url := HttpBackendBaseURL + "/test/download/chunked?file=large_1m.bin"
    b.SetBytes(int64(len(Payload1M)))
    b.RunParallel(func(pb *testing.PB) {
        for pb.Next() {
            resp, _ := stressClient.Get(url)
            if resp != nil {
                io.Copy(io.Discard, resp.Body)
                resp.Body.Close()
            }
        }
    })
}
```

**HTTPS 下载（已知大小）**

```go
func Benchmark_Stress_HTTPS_Download_KnownSize(b *testing.B) {
    url := HttpsBackendBaseURL + "/test/download?file=large_1m.bin"
    b.SetBytes(int64(len(Payload1M)))
    b.RunParallel(func(pb *testing.PB) {
        for pb.Next() {
            resp, _ := stressClientHTTPS.Get(url)
            if resp != nil {
                io.Copy(io.Discard, resp.Body)
                resp.Body.Close()
            }
        }
    })
}
```

**HTTPS 下载 Chunked**

```go
func Benchmark_Stress_HTTPS_Download_Chunked(b *testing.B) {
    url := HttpsBackendBaseURL + "/test/download/chunked?file=large_1m.bin"
    b.SetBytes(int64(len(Payload1M)))
    b.RunParallel(func(pb *testing.PB) {
        for pb.Next() {
            resp, _ := stressClientHTTPS.Get(url)
            if resp != nil {
                io.Copy(io.Discard, resp.Body)
                resp.Body.Close()
            }
        }
    })
}
```

#### HTTP Client 配置

```go
var stressClient = &http.Client{
    Transport: &http.Transport{
        Proxy: http.ProxyURL(proxyUrl),
        MaxIdleConns:        100,
        MaxIdleConnsPerHost: 100,
        MaxConnsPerHost:     100,
        IdleConnTimeout:     90 * time.Second,
        DisableCompression:  true,
    },
    Timeout: 30 * time.Second,
}

// HTTPS 客户端（跳过证书验证，仅用于测试）
var stressClientHTTPS = &http.Client{
    Transport: &http.Transport{
        Proxy: http.ProxyURL(proxyUrl),
        TLSClientConfig: &tls.Config{InsecureSkipVerify: true},  // MITM 证书自签名
        MaxIdleConns:        100,
        MaxIdleConnsPerHost: 100,
        MaxConnsPerHost:     100,
        DisableCompression:  true,
    },
    Timeout: 30 * time.Second,
}
```

#### 内存驻留数据

```go
func init() {
    var err error
    Payload1M, err = os.ReadFile(filepath.Join(TestDataDir, "large_1m.bin"))
    if err != nil {
        panic(err)
    }
    // 类似加载其他文件...
}
```

### 步骤 4：执行方案

#### 启动基础设施

```bash
# 终端 1：启动后端服务器
cd test
go run backend_server.go

# 终端 2：启动 proxy_man（pprof 在 :6060）
./proxy_man -v
```

#### 运行压力测试

```bash
cd test/gotest

# 八种场景分别测试（持续 60 秒，8 并发）

# ========== 上行测试 ==========
# 1. HTTP 上传 + 已知大小
go test -bench=Benchmark_Stress_HTTP_Upload_KnownSize -benchtime=60s -cpu=8 -v

# 2. HTTP 上传 + Chunked
go test -bench=Benchmark_Stress_HTTP_Upload_Chunked -benchtime=60s -cpu=8 -v

# 3. HTTPS 上传 + 已知大小
go test -bench=Benchmark_Stress_HTTPS_Upload_KnownSize -benchtime=60s -cpu=8 -v

# 4. HTTPS 上传 + Chunked
go test -bench=Benchmark_Stress_HTTPS_Upload_Chunked -benchtime=60s -cpu=8 -v

# ========== 下行测试 ==========
# 5. HTTP 下载 + 已知大小
go test -bench=Benchmark_Stress_HTTP_Download_KnownSize -benchtime=60s -cpu=8 -v

# 6. HTTP 下载 Chunked
go test -bench=Benchmark_Stress_HTTP_Download_Chunked -benchtime=60s -cpu=8 -v

# 7. HTTPS 下载 + 已知大小
go test -bench=Benchmark_Stress_HTTPS_Download_KnownSize -benchtime=60s -cpu=8 -v

# 8. HTTPS 下载 Chunked
go test -bench=Benchmark_Stress_HTTPS_Download_Chunked -benchtime=60s -cpu=8 -v

# 或一次性运行所有压力测试
go test -bench=Benchmark_Stress -benchtime=60s -cpu=8 -v
```

#### 采集 pprof 数据（在压测期间）

```bash
# CPU Profile（采集 30 秒）
go tool pprof -http=:8081 http://127.0.0.1:6060/debug/pprof/profile?seconds=30

# Heap Profile
go tool pprof -http=:8082 http://127.0.0.1:6060/debug/pprof/heap

# Goroutine Profile
go tool pprof -http=:8083 http://127.0.0.1:6060/debug/pprof/goroutine

# 查看 5 秒内的 CPU 样本
go tool pprof -http=:8084 http://127.0.0.1:6060/debug/pprof/profile?seconds=5
```

## 与 plan1.md 的主要差异

| 方面         | plan1.md                        | 本计划                                 |
| ------------ | ------------------------------- | -------------------------------------- |
| 文件组织     | 新建 `benchmark_stress_test.go` | 扩展现有 `benchmark_test.go`           |
| 测试场景     | 下载/上传通用场景               | **8种精确场景**（上传4种 + 下载4种）   |
| Chunked 测试 | 未提及                          | **重点测试**（上传/下载都覆盖）        |
| 后端接口     | 使用现有接口                    | **新增 `/test/download/chunked`** 接口 |
| HTTPS 场景   | 未包含                          | 包含 MITM 场景                         |
| MaxIdleConns | 1000                            | 100（更合理）                          |
| 错误处理     | 未提及                          | 容错机制                               |

## 测试场景说明

### Chunked 编码触发方式

```go
// 已知大小：设置 ContentLength
req.ContentLength = int64(len(data))  // HTTP 头：Content-Length: 1048576

// 未知大小：不设置 ContentLength，Go 自动使用 chunked
// HTTP 头：Transfer-Encoding: chunked
```

## 验证步骤

1. **后端服务器验证**

   ```bash
   cd test
   go run backend_server.go
   
   # 在另一个终端验证新接口
   curl "http://localhost:9001/test/download/chunked?file=large_1m.bin" -o test.bin
   # 检查响应头是否包含 Transfer-Encoding: chunked
   curl -I "http://localhost:9001/test/download/chunked?file=large_1m.bin"
   ```

2. **编译验证**

   ```bash
   cd test/gotest
   go test -c
   ```

3. **功能验证（短时间测试）**

   ```bash
   # 快速验证八种场景都能正常运行
   go test -bench=Benchmark_Stress -benchtime=3s -v
   ```

4. **压力验证（长时间测试）**

   ```bash
   # 上行测试
   go test -bench=Benchmark_Stress_HTTP_Upload -benchtime=60s -cpu=8 -v
   go test -bench=Benchmark_Stress_HTTPS_Upload -benchtime=60s -cpu=8 -v
   
   # 下行测试
   go test -bench=Benchmark_Stress_HTTP_Download -benchtime=60s -cpu=8 -v
   go test -bench=Benchmark_Stress_HTTPS_Download -benchtime=60s -cpu=8 -v
   ```

5. **pprof 验证**

   - 访问 `http://localhost:6060/debug/pprof/` 确认数据可采集
   - 使用 `go tool pprof` 生成火焰图

6. **预期差异观察**

   - **Chunked vs KnownSize**: Chunked 场景下代理服务器 CPU 开销可能略高（需要处理分块编码）
   - **HTTP vs HTTPS**: HTTPS 场景 CPU 明显更高（TLS 加解密开销）
   - **上传 vs 下载**: 两者流量方向不同，但代理服务器都需要处理双向转发
   - **内存占用**: HTTPS MITM 场景内存占用可能更高
   - **Chunked 下载**: 测试代理服务器对 chunked 响应的正确解析和转发