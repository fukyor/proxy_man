package proxysocket

import (
	"encoding/json"
	"time"

	"github.com/gorilla/websocket"
	"proxy_man/mproxy"
)

// 更新订阅
func (h *WebSocketHub) updateSubscription(sub *Subscription, msg map[string]any) {
	if topics, ok := msg["topics"].([]any); ok {
		sub.Traffic = contains(topics, "traffic")
		sub.Connections = contains(topics, "connections")
		sub.Logs = contains(topics, "logs")
	}
	if logLevel, ok := msg["logLevel"].(string); ok {
		sub.LogLevel = logLevel
	}
}

func contains(slice []any, item string) bool {
	for _, s := range slice {
		if str, ok := s.(string); ok && str == item {
			return true
		}
	}
	return false
}

// 向指定客户端发送消息（线程安全）
// 同一个订阅者的流量推送器，连接推送器会共享websocket所以要注意线程安全
func (h *WebSocketHub) sendTo(conn *websocket.Conn, sub *Subscription, msg any) {
	sub.writeMu.Lock()
	defer sub.writeMu.Unlock()
	data, _ := json.Marshal(msg)
	conn.WriteMessage(websocket.TextMessage, data)
}

// 广播到订阅指定主题的客户端
// 这里完美解决了每个客户端的个性化订阅
func (h *WebSocketHub) broadcastToTopic(topic string, msg any) {
	h.clients.Range(func(key, value any) bool {
		conn := key.(*websocket.Conn)
		sub := value.(*Subscription)

		var shouldSend bool
		switch topic {
		case "traffic":
			shouldSend = sub.Traffic
		case "connections":
			shouldSend = sub.Connections
		}

		if shouldSend {
			h.sendTo(conn, sub, msg)
		}
		return true
	})
}

// 全局流量推送器（每秒推送一次）
func (h *WebSocketHub) StartTrafficPusher() {
	var lastUp, lastDown int64

	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()

		for range ticker.C {
			currentUp := mproxy.GlobalTrafficUp.Load()
			currentDown := mproxy.GlobalTrafficDown.Load()

			deltaUp := currentUp - lastUp
			deltaDown := currentDown - lastDown

			lastUp, lastDown = currentUp, currentDown

			h.broadcastToTopic("traffic", map[string]any{
				"type": "traffic",
				"data": map[string]int64{"up": deltaUp, "down": deltaDown},
			})
		}
	}()
}

// 连接推送器（每 2 秒推送一次）
func (h *WebSocketHub) StartConnectionPusher() {
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()

		for range ticker.C {
			connections := make([]map[string]any, 0)

			h.proxy.Connections.Range(func(key, value any) bool {
				info := value.(*mproxy.ConnectionInfo)
				connData := map[string]any{
					"id":       info.Session,
					"host":     info.Host,
					"method":   info.Method,
					"url":      info.URL,
					"remote":   info.RemoteAddr,
					"protocol": info.Protocol,
				}

				// 读取实时流量（如果有引用）
				if info.UploadRef != nil {
					connData["up"] = *info.UploadRef
				}
				if info.DownloadRef != nil {
					connData["down"] = *info.DownloadRef
				}

				connections = append(connections, connData)
				return true
			})

			h.broadcastToTopic("connections", map[string]any{
				"type": "connections",
				"data": connections,
			})
		}
	}()
}

// 日志推送器（实时推送）
func (h *WebSocketHub) StartLogPusher() {
	go func() {
		for msg := range mproxy.LogChan {
			h.broadcastLog(msg)
		}
	}()
}

func (h *WebSocketHub) broadcastLog(msg mproxy.LogMessage) {
	data := map[string]any{
		"type": "log",
		"data": map[string]any{
			"level":   msg.Level,
			"session": msg.Session,
			"message": msg.Message,
			"time":    msg.Time,
		},
	}

	h.clients.Range(func(key, value any) bool {
		conn := key.(*websocket.Conn)
		sub := value.(*Subscription)

		if sub.Logs && shouldSendLog(msg.Level, sub.LogLevel) {
			h.sendTo(conn, sub, data)
		}
		return true
	})
}

var logLevels = map[string]int{"DEBUG": 0, "INFO": 1, "WARN": 2, "ERROR": 3}

func shouldSendLog(msgLevel, clientLevel string) bool {
	return logLevels[msgLevel] >= logLevels[clientLevel]
}
