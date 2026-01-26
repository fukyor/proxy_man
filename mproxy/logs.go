package mproxy

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Logger interface {
	Printf(format string, v ...any)
}

// 日志消息结构
type LogMessage struct {
	Level   string    `json:"level"`
	Session int64     `json:"session"`
	Message string    `json:"message"`
	Time    time.Time `json:"time"`
}

// 全局日志 Channel
var LogChan = make(chan LogMessage, 1000)

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

	// 非阻塞发送到 Channel
	select {
	case LogChan <- LogMessage{
		Level:   level,
		Session: session,
		Message: payload,
		Time:    time.Now(),
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
	payload = msg

	// 匹配 Session ID: [001] 格式
	sessionRe := regexp.MustCompile(`\[(\d+)\]`)
	if matches := sessionRe.FindStringSubmatch(msg); len(matches) > 1 {
		session, _ = strconv.ParseInt(matches[1], 10, 64)
	}

	// 判断日志级别
	if strings.Contains(msg, "WARN:") || strings.Contains(msg, "warn") {
		level = "WARN"
	} else if strings.Contains(msg, "ERROR:") || strings.Contains(msg, "error") {
		level = "ERROR"
	} else if strings.Contains(msg, "DEBUG:") {
		level = "DEBUG"
	}

	// 提取 payload（移除前缀）
	payloadRe := regexp.MustCompile(`\[\d+\]\s*(?:INFO|WARN|ERROR|DEBUG)?:?\s*(.*)`)
	if matches := payloadRe.FindStringSubmatch(msg); len(matches) > 1 {
		payload = strings.TrimSpace(matches[1])
	}

	return
}
