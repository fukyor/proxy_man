package mproxy

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

var (
	sessionRe = regexp.MustCompile(`\[(\d+)\]`)
	payloadRe = regexp.MustCompile(`^\s*(?:\[\d+\]\s*)?(?:(?:INFO|WARN|ERROR|DEBUG):\s*)?(.*)$`)
)

type Logger interface {
	Printf(format string, v ...any)
}

// 日志消息结构
type LogMessage struct {
	Level    string    `json:"level"`
	Session  int64     `json:"session"`
	Message  string    `json:"message"`
	Category string    `json:"category"`
	Time     time.Time `json:"time"`
}

// 全局日志 Channel
var LogChan = make(chan LogMessage, 1000)

// InterceptLogMessage 单条拦截日志消息（用于 WebSocket 实时推送）
type InterceptLogMessage struct {
	ClientIP  string    `json:"client_ip"`
	Target    string    `json:"target"`
	RuleType  string    `json:"rule_type"`
	RuleValue string    `json:"rule_value"`
	Time      time.Time `json:"time"`
}

// InterceptLogChan 拦截日志推送通道
var InterceptLogChan = make(chan InterceptLogMessage, 500)

// InterceptCount 记录当前后端进程内累计拦截次数
var InterceptCount atomic.Uint64

// GetInterceptCount 返回当前后端进程内累计拦截次数
func GetInterceptCount() uint64 {
	return InterceptCount.Load()
}

// 日志收集器，包装原有 Logger
type LogCollector struct {
	Underlying Logger
}

func NewLogCollector(underlying Logger) *LogCollector {
	return &LogCollector{Underlying: underlying}
}

func (l *LogCollector) Printf(format string, v ...any) {
	msg := fmt.Sprintf(format, v...)
	level, session, payload := ParseLogMessage(msg)
	category := ClassifyLogMessage(payload)

	// 非阻塞发送到 Channel
	select {
	case LogChan <- LogMessage{
		Level:    level,
		Session:  session,
		Message:  payload,
		Category: category,
		Time:     time.Now(),
	}:
	default:
		// Channel 满时丢弃，避免阻塞主流程
	}

	// 同时输出到原始 Logger
	l.Underlying.Printf(format, v...)
}

// 解析日志消息，提取级别、Session、内容
func ParseLogMessage(msg string) (level string, session int64, payload string) {
	level = "INFO"
	session = 0
	payload = strings.TrimSpace(msg)

	// 匹配 Session ID: [001] 格式
	if matches := sessionRe.FindStringSubmatch(msg); len(matches) > 1 {
		session, _ = strconv.ParseInt(matches[1], 10, 64)
	}

	// 判断日志级别
	lowerMsg := strings.ToLower(msg)
	if strings.Contains(msg, "ERROR:") || strings.Contains(lowerMsg, "error") {
		level = "ERROR"
	} else if strings.Contains(msg, "WARN:") || strings.Contains(lowerMsg, "warn") {
		level = "WARN"
	} else if strings.Contains(msg, "DEBUG:") || strings.Contains(lowerMsg, "debug") {
		level = "DEBUG"
	}

	// 提取 payload（移除前缀）
	if matches := payloadRe.FindStringSubmatch(strings.TrimSpace(msg)); len(matches) > 1 {
		payload = strings.TrimSpace(matches[1])
	}

	return
}

// ClassifyLogMessage 将日志正文映射到前端筛选使用的稳定分类
func ClassifyLogMessage(payload string) string {
	text := strings.TrimSpace(payload)
	lower := strings.ToLower(text)

	switch {
	case containsAny(text, "[访问控制]", "访问控制", "非法来源 IP", "来源 IP 拦截", "拦截规则值"):
		return "access"
	case containsAny(text, "[路由匹配]", "路由已热重载", "路由热重载", "目标节点", "规则目标", "未知规则类型"):
		return "route"
	case strings.Contains(text, "路由") && containsAny(text, "热重载", "规则", "节点"):
		return "route"
	case strings.Contains(text, "节点") && strings.Contains(text, "创建失败"):
		return "route"
	case containsAny(text, "[流量统计]", "头部大小解析"):
		return "traffic"
	case strings.Contains(text, "Sending request") || strings.HasPrefix(text, "req ") || strings.HasPrefix(text, "resp "):
		return "request"
	case containsAnyFold(lower, "websocket", "hijack", "upgrade"):
		return "websocket"
	case containsAnyFold(lower, "mitm", "tls", "handshake", "signing for") || strings.Contains(text, "证书"):
		return "mitm"
	case containsAnyFold(lower, "roundtrip", "error dialing", "dial", "url", "parser") ||
		containsAny(text, "拨号", "协议解析", "响应读写失败", "写回响应", "写入响应", "读取响应", "响应体失败", "响应头失败", "URL"):
		return "network"
	case containsAny(text, "配置已热重载", "处理器数量"):
		return "system"
	default:
		return "general"
	}
}

func containsAny(text string, values ...string) bool {
	for _, value := range values {
		if strings.Contains(text, value) {
			return true
		}
	}
	return false
}

func containsAnyFold(lowerText string, values ...string) bool {
	for _, value := range values {
		if strings.Contains(lowerText, value) {
			return true
		}
	}
	return false
}
