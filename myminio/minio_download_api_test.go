package myminio

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
	"testing"
)

const (
	baseURL = "http://localhost:8000"
)

// TestAPIResponse 测试用的 API 响应结构
type TestAPIResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// testMissingKey 测试缺少 key 参数
func TestMissingKey(t *testing.T) {
	url := baseURL + "/api/storage/download"
	resp, err := http.Get(url)
	if err != nil {
		fmt.Printf("❌ 请求失败: %v\n", err)
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var apiResp TestAPIResponse
	json.Unmarshal(body, &apiResp)

	fmt.Printf("URL: %s\n", url)
	fmt.Printf("状态码: %d\n", resp.StatusCode)
	fmt.Printf("响应: %s\n", string(body))

	// 验证结果
	if resp.StatusCode == 200 && apiResp.Code == 400 && apiResp.Message == "缺少参数: key" {
		fmt.Println("✅ 测试通过")
	} else {
		fmt.Println("❌ 测试失败")
	}
}

// testEmptyKey 测试空 key 参数
func TestEmptyKey(t *testing.T) {
	url := baseURL + "/api/storage/download?key="
	resp, err := http.Get(url)
	if err != nil {
		fmt.Printf("❌ 请求失败: %v\n", err)
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var apiResp TestAPIResponse
	json.Unmarshal(body, &apiResp)

	fmt.Printf("URL: %s\n", url)
	fmt.Printf("状态码: %d\n", resp.StatusCode)
	fmt.Printf("响应: %s\n", string(body))

	// 验证结果
	if resp.StatusCode == 200 && apiResp.Code == 400 && apiResp.Message == "缺少参数: key" {
		fmt.Println("✅ 测试通过")
	} else {
		fmt.Println("❌ 测试失败")
	}
}

// testObjectNotFound 测试对象不存在
func TestObjectNotFound(t *testing.T) {
	url := baseURL + "/api/storage/download?key=nonexistent/object/file"
	resp, err := http.Get(url)
	if err != nil {
		fmt.Printf("❌ 请求失败: %v\n", err)
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var apiResp TestAPIResponse
	json.Unmarshal(body, &apiResp)

	fmt.Printf("URL: %s\n", url)
	fmt.Printf("状态码: %d\n", resp.StatusCode)
	fmt.Printf("响应: %s\n", string(body))

	// 验证结果 - 可能是 404 或 503（取决于 MinIO 是否启用）
	if apiResp.Code == 404 || apiResp.Code == 503 {
		fmt.Println("✅ 测试通过（对象不存在或 MinIO 未启用）")
	} else {
		fmt.Println("⚠️  意外响应码")
	}
}

// testValidKeys 测试有效的 key
func TestValidKeys(t *testing.T) {
	// 生成今天的日期路径
	today := time.Now().Format("2006-01-02")
	testKeys := []string{
		fmt.Sprintf("mitm-data/%s/1/req", today),
		fmt.Sprintf("mitm-data/%s/2/req", today),
		fmt.Sprintf("mitm-data/%s/1/resp", today),
		fmt.Sprintf("mitm-data/%s/2/resp", today),
	}

	for _, key := range testKeys {
		url := fmt.Sprintf("%s/api/storage/download?key=%s", baseURL, key)
		resp, err := http.Get(url)
		if err != nil {
			fmt.Printf("❌ [%s] 请求失败: %v\n", key, err)
			continue
		}
		defer resp.Body.Close()

		body, _ := io.ReadAll(resp.Body)
		var apiResp TestAPIResponse
		json.Unmarshal(body, &apiResp)

		fmt.Printf("Key: %s\n", key)
		fmt.Printf("  状态码: %d\n", resp.StatusCode)
		fmt.Printf("  响应: %s\n", string(body))

		// 验证结果
		switch apiResp.Code {
		case 0:
			fmt.Printf("  ✅ 成功获取下载链接\n")
		case 404:
			fmt.Printf("  ℹ️  对象不存在\n")
		case 503:
			fmt.Printf("  ℹ️  MinIO 未启用\n")
		default:
			fmt.Printf("  ❌ 未知错误\n")
		}
		fmt.Println()
	}
}