package mproxy

import (
	"testing"
)

func TestIsSelfLoop(t *testing.T) {
	proxyPort := 8080

	tests := []struct {
		name      string
		addr      string
		publicIPs []string
		expected  bool
	}{
		{"本地回环", "127.0.0.1:8080", []string{}, true},
		{"localhost", "localhost:8080", []string{}, true},
		{"IPv6回环", "[::1]:8080", []string{}, true},
		{"不同端口", "127.0.0.1:9090", []string{}, false},
		{"外部IP", "8.8.8.8:8080", []string{}, false},
		{"无效地址", "invalid", []string{}, false},
		// 新增：公网 IP 测试用例
		{"公网IP匹配", "117.72.191.85:8080", []string{"117.72.191.85"}, true},
		{"公网IP不匹配端口", "117.72.191.85:9090", []string{"117.72.191.85"}, false},
		{"公网域名匹配", "gzyddyx.com:8080", []string{"gzyddyx.com"}, true},
		{"多个公网IP", "117.72.191.85:8080", []string{"1.2.3.4", "117.72.191.85"}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isSelfLoop(tt.addr, proxyPort, tt.publicIPs)
			if result != tt.expected {
				t.Errorf("isSelfLoop(%q, %d, %v) = %v, want %v",
					tt.addr, proxyPort, tt.publicIPs, result, tt.expected)
			}
		})
	}
}
