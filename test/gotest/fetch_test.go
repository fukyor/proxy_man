package main_test

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ================= 环境配置 =================
const (
	// 代理服务器地址 (您手动启动的 proxy_man 监听地址)
	// 对应 main.go 中的 -addr 参数，默认为 :8080
	ProxyAddr = "127.0.0.1:8080"

	// 后端服务器地址 (您手动启动的 backend_server 地址)
	// 请根据实际情况修改端口，例如 8081, 8000 等
	HttpBackendBaseURL = "http://127.0.0.1:9001"
	HttpBackendAddr    = "127.0.0.1:9001"
	HttpsBackendBaseURL = "https://127.0.0.1:9002"

	// 测试数据目录
	TestDataDir = `E:\D\zuoyewenjian\MyProject\proxy_man\test\data`
)

// ===========================================

// ================= 压力测试配置 =================
var (
	Payload  []byte // 内存驻留数据（上传用）
	FileSize int64  // 文件实际大小

	stressClient      *http.Client // HTTP 压力测试客户端
	stressClientHTTPS *http.Client // HTTPS 压力测试客户端
)

func init() {
	var err error
	Payload, err = os.ReadFile(filepath.Join(TestDataDir, "large_2m.bin"))
	if err != nil {
		panic("加载测试数据失败: " + err.Error())
	}
	FileSize = int64(len(Payload))

	proxyURL, _ := url.Parse("http://" + ProxyAddr)

	stressClient = &http.Client{
		Transport: &http.Transport{
			Proxy:               http.ProxyURL(proxyURL),
			MaxIdleConns:        200,
			MaxIdleConnsPerHost: 200,
			MaxConnsPerHost:     0,
			IdleConnTimeout:     90 * time.Second,
			DisableCompression:  true,
		},
		Timeout: 60 * time.Second,
	}

	stressClientHTTPS = &http.Client{
		Transport: &http.Transport{
			Proxy:               http.ProxyURL(proxyURL),
			TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
			MaxIdleConns:        200,
			MaxIdleConnsPerHost: 200,
			MaxConnsPerHost:     0,
			IdleConnTimeout:     90 * time.Second,
			DisableCompression:  true,
		},
		Timeout: 60 * time.Second,
	}
}


// ========== 上行压力测试 ==========
// go test -bench=Benchmark_Stress_HTTP_Upload_KnownSize -benchtime=1s -run=^$ -v -cpu 2,6,12
// go tool pprof -http=:8081 http://localhost:6060/debug/pprof/heap
// go tool pprof -http=:8083 "http://localhost:6060/debug/pprof/profile?seconds=20"
func Benchmark_Stress_HTTP_Upload_KnownSize(b *testing.B) {
	targetURL := HttpBackendBaseURL + "/test/upload"
	b.SetBytes(FileSize)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			req, _ := http.NewRequest("POST", targetURL, bytes.NewReader(Payload))
			req.ContentLength = FileSize
			req.Header.Set("Content-Type", "application/octet-stream")
			resp, err := stressClient.Do(req)
			if err != nil {
				b.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	})
}


// go test -bench=Benchmark_Stress_HTTP_Upload_Chunked -benchtime=3s -run=^$ -v -cpu 2,6,12
// go tool pprof -http=:8081 http://localhost:6060/debug/pprof/heap
// go tool pprof -http=:8083 "http://localhost:6060/debug/pprof/profile?seconds=20"
func Benchmark_Stress_HTTP_Upload_Chunked(b *testing.B) {
	targetURL := HttpBackendBaseURL + "/test/upload"
	b.SetBytes(FileSize)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			req, _ := http.NewRequest("POST", targetURL, io.NopCloser(bytes.NewReader(Payload)))
			req.Header.Set("Content-Type", "application/octet-stream")
			resp, err := stressClient.Do(req)
			if err != nil {
				b.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	})
}

// go test -bench=Benchmark_Stress_HTTPS_Upload_KnownSize -benchtime=3s -run=^$ -v -cpu 2,6,12
// go tool pprof -http=:8081 http://localhost:6060/debug/pprof/heap
// go tool pprof -http=:8083 "http://localhost:6060/debug/pprof/profile?seconds=20"
func Benchmark_Stress_HTTPS_Upload_KnownSize(b *testing.B) {
	targetURL := HttpsBackendBaseURL + "/test/upload"
	b.SetBytes(FileSize)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			req, _ := http.NewRequest("POST", targetURL, bytes.NewReader(Payload))
			req.ContentLength = FileSize
			req.Header.Set("Content-Type", "application/octet-stream")
			resp, err := stressClientHTTPS.Do(req)
			if err != nil {
				b.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	})
}

// go test -bench=Benchmark_Stress_HTTPS_Upload_Chunked -benchtime=3s -run=^$ -v -cpu 2,6,12
// go tool pprof -http=:8081 http://localhost:6060/debug/pprof/heap
// go tool pprof -http=:8083 "http://localhost:6060/debug/pprof/profile?seconds=20"
func Benchmark_Stress_HTTPS_Upload_Chunked(b *testing.B) {
	targetURL := HttpsBackendBaseURL + "/test/upload"
	b.SetBytes(FileSize)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			req, _ := http.NewRequest("POST", targetURL, io.NopCloser(bytes.NewReader(Payload)))
			req.Header.Set("Content-Type", "application/octet-stream")
			resp, err := stressClientHTTPS.Do(req)
			if err != nil {
				b.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	})
}

// ========== 下行压力测试 ==========
// go test -bench=Benchmark_Stress_HTTP_Download_KnownSize -benchtime=3s -run=^$ -v -cpu 2,6,12
// go tool pprof -http=:8081 http://localhost:6060/debug/pprof/heap
// go tool pprof -http=:8083 "http://localhost:6060/debug/pprof/profile?seconds=20"
func Benchmark_Stress_HTTP_Download_KnownSize(b *testing.B) {
	targetURL := HttpBackendBaseURL + "/test/download?file=large_2m.bin"
	b.SetBytes(FileSize)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			resp, err := stressClient.Get(targetURL)
			if err != nil {
				b.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	})
}

// go test -bench=Benchmark_Stress_HTTP_Download_Chunked -benchtime=3s -run=^$ -v -cpu 2,6,12
// go tool pprof -http=:8081 http://localhost:6060/debug/pprof/heap
// go tool pprof -http=:8083 "http://localhost:6060/debug/pprof/profile?seconds=20"
func Benchmark_Stress_HTTP_Download_Chunked(b *testing.B) {
	targetURL := HttpBackendBaseURL + "/test/download/chunked?file=large_2m.bin"
	b.SetBytes(FileSize)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			resp, err := stressClient.Get(targetURL)
			if err != nil {
				b.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	})
}

// go test -bench=Benchmark_Stress_HTTPS_Download_KnownSize -benchtime=3s -run=^$ -v -cpu 2,6,12
// go tool pprof -http=:8081 http://localhost:6060/debug/pprof/heap
// go tool pprof -http=:8083 "http://localhost:6060/debug/pprof/profile?seconds=20"
func Benchmark_Stress_HTTPS_Download_KnownSize(b *testing.B) {
	targetURL := HttpsBackendBaseURL + "/test/download?file=large_2m.bin"
	b.SetBytes(FileSize)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			resp, err := stressClientHTTPS.Get(targetURL)
			if err != nil {
				b.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	})
}

// go test -bench=Benchmark_Stress_HTTPS_Download_Chunked -benchtime=8s -run=^$ -v -cpu 2,6,12
// go tool pprof -http=:8081 http://localhost:6060/debug/pprof/heap
// go tool pprof -http=:8083 "http://localhost:6060/debug/pprof/profile?seconds=20"
func Benchmark_Stress_HTTPS_Download_Chunked(b *testing.B) {
	targetURL := HttpsBackendBaseURL + "/test/download/chunked?file=large_2m.bin"
	b.SetBytes(FileSize)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			resp, err := stressClientHTTPS.Get(targetURL)
			if err != nil {
				b.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	})
}

// ========== HTTP-MITM 辅助函数 ==========

// httpMitmConnect 通过代理建立 HTTP-MITM 隧道
// 要求代理已配置 HttpMitmMode 或对目标端口启用了 HTTPMitmConnect
func httpMitmConnect(backend string) (net.Conn, *bufio.Reader, error) {
	conn, err := net.DialTimeout("tcp", ProxyAddr, 10*time.Second)
	if err != nil {
		return nil, nil, err
	}
	connectReq := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", backend, backend)
	if _, err := conn.Write([]byte(connectReq)); err != nil {
		conn.Close()
		return nil, nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: "CONNECT"})
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	if resp.StatusCode != 200 {
		conn.Close()
		return nil, nil, fmt.Errorf("CONNECT failed: %s", resp.Status)
	}
	return conn, br, nil
}

// ========== HTTP-MITM 上行压力测试 ==========

// go test -bench=Benchmark_Stress_HTTP_MITM_Upload_KnownSize -benchtime=8s -run=^$ -v -cpu 2,6,12
// go tool pprof -http=:8081 http://localhost:6060/debug/pprof/heap
// go tool pprof -http=:8083 "http://localhost:6060/debug/pprof/profile?seconds=20"
func Benchmark_Stress_HTTP_MITM_Upload_KnownSize(b *testing.B) {
	b.SetBytes(FileSize)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			conn, br, err := httpMitmConnect(HttpBackendAddr)
			if err != nil {
				b.Fatal(err)
			}
			req, _ := http.NewRequest("POST", "http://"+HttpBackendAddr+"/test/upload", bytes.NewReader(Payload))
			req.ContentLength = FileSize
			req.Header.Set("Content-Type", "application/octet-stream")
			if err := req.Write(conn); err != nil {
				conn.Close()
				b.Fatal(err)
			}
			resp, err := http.ReadResponse(br, req)
			if err != nil {
				conn.Close()
				b.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			conn.Close()
		}
	})
}

// go test -bench=Benchmark_Stress_HTTP_MITM_Upload_Chunked -benchtime=8s -run=^$ -v -cpu 2,6,12
func Benchmark_Stress_HTTP_MITM_Upload_Chunked(b *testing.B) {
	b.SetBytes(FileSize)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			conn, br, err := httpMitmConnect(HttpBackendAddr)
			if err != nil {
				b.Fatal(err)
			}
			req, _ := http.NewRequest("POST", "http://"+HttpBackendAddr+"/test/upload", io.NopCloser(bytes.NewReader(Payload)))
			req.Header.Set("Content-Type", "application/octet-stream")
			if err := req.Write(conn); err != nil {
				conn.Close()
				b.Fatal(err)
			}
			resp, err := http.ReadResponse(br, req)
			if err != nil {
				conn.Close()
				b.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			conn.Close()
		}
	})
}

// ========== HTTP-MITM 下行压力测试 ==========

// go test -bench=Benchmark_Stress_HTTP_MITM_Download_KnownSize -benchtime=8s -run=^$ -v -cpu 2,6,12
func Benchmark_Stress_HTTP_MITM_Download_KnownSize(b *testing.B) {
	b.SetBytes(FileSize)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			conn, br, err := httpMitmConnect(HttpBackendAddr)
			if err != nil {
				b.Fatal(err)
			}
			req, _ := http.NewRequest("GET", "http://"+HttpBackendAddr+"/test/download?file=large_2m.bin", nil)
			if err := req.Write(conn); err != nil {
				conn.Close()
				b.Fatal(err)
			}
			resp, err := http.ReadResponse(br, req)
			if err != nil {
				conn.Close()
				b.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			conn.Close()
		}
	})
}

// go test -bench=Benchmark_Stress_HTTP_MITM_Download_Chunked -benchtime=8s -run=^$ -v -cpu 2,6,12
func Benchmark_Stress_HTTP_MITM_Download_Chunked(b *testing.B) {
	b.SetBytes(FileSize)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			conn, br, err := httpMitmConnect(HttpBackendAddr)
			if err != nil {
				b.Fatal(err)
			}
			req, _ := http.NewRequest("GET", "http://"+HttpBackendAddr+"/test/download/chunked?file=large_2m.bin", nil)
			if err := req.Write(conn); err != nil {
				conn.Close()
				b.Fatal(err)
			}
			resp, err := http.ReadResponse(br, req)
			if err != nil {
				conn.Close()
				b.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			conn.Close()
		}
	})
}
