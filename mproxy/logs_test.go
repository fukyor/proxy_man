package mproxy

import "testing"

func TestParseLogMessage(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		msg         string
		wantLevel   string
		wantSession int64
		wantPayload string
	}{
		{
			name:        "带会话的 INFO 前缀",
			msg:         "[042] INFO: [路由匹配] example.com -> Direct\n",
			wantLevel:   "INFO",
			wantSession: 42,
			wantPayload: "[路由匹配] example.com -> Direct",
		},
		{
			name:        "无会话的 WARN 前缀",
			msg:         "WARN: 未知访问控制规则类型 UserAgent",
			wantLevel:   "WARN",
			wantSession: 0,
			wantPayload: "未知访问控制规则类型 UserAgent",
		},
		{
			name:        "无会话的 INFO 前缀",
			msg:         "INFO: [访问控制] 拦截请求: 客户端=127.0.0.1",
			wantLevel:   "INFO",
			wantSession: 0,
			wantPayload: "[访问控制] 拦截请求: 客户端=127.0.0.1",
		},
		{
			name:        "ERROR 优先于 WARN",
			msg:         "[007] WARN: ERROR: TLS 握手失败",
			wantLevel:   "ERROR",
			wantSession: 7,
			wantPayload: "ERROR: TLS 握手失败",
		},
		{
			name:        "保留普通正文",
			msg:         "Connect Tunnel Normal Exiting on Client EOF",
			wantLevel:   "INFO",
			wantSession: 0,
			wantPayload: "Connect Tunnel Normal Exiting on Client EOF",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			level, session, payload := ParseLogMessage(tc.msg)
			if level != tc.wantLevel {
				t.Fatalf("level = %q, want %q", level, tc.wantLevel)
			}
			if session != tc.wantSession {
				t.Fatalf("session = %d, want %d", session, tc.wantSession)
			}
			if payload != tc.wantPayload {
				t.Fatalf("payload = %q, want %q", payload, tc.wantPayload)
			}
		})
	}
}

func TestClassifyLogMessage(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		payload  string
		category string
	}{
		{name: "路由匹配", payload: "[路由匹配] GET example.com -> Direct", category: "route"},
		{name: "路由热重载", payload: "配置已热重载，路由已热重载，2 条规则，1 个节点", category: "route"},
		{name: "路由节点配置", payload: "节点 proxy1 创建失败: 无效的代理 URL", category: "route"},
		{name: "访问控制命中", payload: "[访问控制] 拦截请求: 客户端=127.0.0.1", category: "access"},
		{name: "访问控制 IP 配置", payload: "非法来源 IP 拦截规则值 \"bad-ip\"", category: "access"},
		{name: "流量统计", payload: "[流量统计] 本次连接上行: 10 | 下行: 20", category: "traffic"},
		{name: "头部大小解析", payload: "头部大小解析错误: EOF", category: "traffic"},
		{name: "请求发送", payload: "Sending request GET http://example.com", category: "request"},
		{name: "请求记录", payload: "req example.com", category: "request"},
		{name: "响应记录", payload: "resp 200 OK", category: "request"},
		{name: "WebSocket", payload: "Response looks like websocket upgrade.", category: "websocket"},
		{name: "MITM", payload: "MITM 模式启动, 协议自动嗅探", category: "mitm"},
		{name: "TLS", payload: "TLS 握手失败 Cannot handshake client example.com", category: "mitm"},
		{name: "网络", payload: "RoundTrip 失败: connection reset", category: "network"},
		{name: "协议解析", payload: "http协议解析错误1, 检查请求协议是否为http", category: "network"},
		{name: "系统", payload: "处理器数量Have 2 CONNECT handlers", category: "system"},
		{name: "通用", payload: "Connect Tunnel Normal Exiting on Client EOF", category: "general"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := ClassifyLogMessage(tc.payload); got != tc.category {
				t.Fatalf("ClassifyLogMessage(%q) = %q, want %q", tc.payload, got, tc.category)
			}
		})
	}
}
