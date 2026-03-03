package proxysocket

import (
	"bytes"
	"encoding/json"
	"sort"
	"time"
	//"log"
	"github.com/gorilla/websocket"
	"proxy_man/mproxy"
)

// 更新订阅
func (h *WebSocketHub) updateSubscription(sub *Subscription, msg map[string]any) {
	if topics, ok := msg["topics"].([]any); ok {
		sub.Traffic = contains(topics, "traffic")
		sub.Connections = contains(topics, "connections")
		sub.Logs = contains(topics, "logs")
		sub.MitmDetail = contains(topics, "mitm_detail")
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

// sendToBytes 向指定客户端发送已序列化的消息（线程安全）
// 锁内仅执行网络写入，JSON 序列化已在外部完成
func (h *WebSocketHub) sendToBytes(conn *websocket.Conn, sub *Subscription, msgBytes []byte) error {
	sub.writeMu.Lock()
	defer sub.writeMu.Unlock()
	conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	err := conn.WriteMessage(websocket.TextMessage, msgBytes)
	conn.SetWriteDeadline(time.Time{})
	return err
}

// broadcastToTopic 广播到订阅指定主题的客户端，预序列化一次后分发给所有客户端
func (h *WebSocketHub) broadcastToTopic(topic string, msg any) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(msg); err != nil {
		return
	}
	msgBytes := buf.Bytes()

	h.clients.Range(func(key, value any) bool {
		conn := key.(*websocket.Conn)
		sub := value.(*Subscription)

		var shouldSend bool
		switch topic {
		case "traffic":
			shouldSend = sub.Traffic
		case "connections":
			shouldSend = sub.Connections
		case "mitm_detail":
			shouldSend = sub.MitmDetail
		}

		if shouldSend {
			if err := h.sendToBytes(conn, sub, msgBytes); err != nil {
				h.clients.Delete(conn)
				conn.Close()
			}
		}
		return true
	})
}

// 全局流量推送器（每秒推送一次，速率归一化为 bytes/s）
func (h *WebSocketHub) StartTrafficPusher() {
	var lastUp, lastDown int64
	var lastTime time.Time

	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()

		lastTime = time.Now()
		lastUp = mproxy.GlobalTrafficUp.Load()
		lastDown = mproxy.GlobalTrafficDown.Load()

		for range ticker.C {
			now := time.Now()
			currentUp := mproxy.GlobalTrafficUp.Load()
			currentDown := mproxy.GlobalTrafficDown.Load()

			elapsed := now.Sub(lastTime).Seconds()
			if elapsed <= 0 {
				elapsed = 1
			}

			// 归一化为每秒速率，避免阻塞恢复后产生畸形峰值
			deltaUp := int64(float64(currentUp-lastUp) / elapsed)
			deltaDown := int64(float64(currentDown-lastDown) / elapsed)

			lastUp, lastDown = currentUp, currentDown
			lastTime = now

			h.broadcastToTopic("traffic", map[string]any{
				"type": "traffic",
				"data": map[string]int64{
					"up":        deltaUp,
					"down":      deltaDown,
					"totalUp":   currentUp,
					"totalDown": currentDown,
				},
			})
		}
	}()
}

// 连接推送器（每 500ms 推送一次，Active 优先、Session 倒序，最多 300 条）
func (h *WebSocketHub) StartConnectionPusher() {
	const tombstoneRetention = 2500 * time.Millisecond
	const maxConnections = 350

	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()

		for range ticker.C {
			now := time.Now()
			allConns := make([]*mproxy.ConnectionInfo, 0, 512)

			h.proxy.Connections.Range(func(key, value any) bool {
				session := key.(int64)
				info := value.(*mproxy.ConnectionInfo)
				// 垃圾回收：已关闭且超过保留时间，物理删除
				if info.Status == "Closed" && now.Sub(info.EndTime) > tombstoneRetention {
					h.proxy.Connections.Delete(session)
					return true
				}
				allConns = append(allConns, info)
				return true
			})

			// Active 优先，同状态按 Session 倒序（最新优先）
			sort.Slice(allConns, func(i, j int) bool {
				if allConns[i].Status != allConns[j].Status {
					return allConns[i].Status == "Active"
				}
				return allConns[i].Session > allConns[j].Session
			})

			limit := len(allConns)
			if limit > maxConnections {
				limit = maxConnections
			}

			connections := make([]map[string]any, 0, limit)
			for _, info := range allConns[:limit] {
				connData := map[string]any{
					"id":        info.Session,
					"parentId":  info.ParentSess,
					"host":      info.Host,
					"method":    info.Method,
					"url":       info.URL,
					"remote":    info.RemoteAddr,
					"protocol":  info.Protocol,
					"startTime": info.StartTime,
					"status":    info.Status,
				}
				if info.UploadRef != nil {
					connData["up"] = *info.UploadRef
				}
				if info.DownloadRef != nil {
					connData["down"] = *info.DownloadRef
				}
				connections = append(connections, connData)
			}

			h.broadcastToTopic("connections", map[string]any{
				"type":  "connections",
				"data":  connections,
				"total": len(allConns),
			})
		}
	}()
}

// 日志推送器（批量收集 + 定时推送，避免高负载时 UI 卡死）
func (h *WebSocketHub) StartLogPusher() {
	go func() {
		batch := make([]*mproxy.LogMessage, 0, 200)
		ticker := time.NewTicker(300 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case msg, ok := <-mproxy.LogChan:
				if !ok {
					if len(batch) > 0 {
						h.sendLogBatch(batch)
					}
					return
				}
				copied := msg // 值拷贝，避免引用被覆盖
				batch = append(batch, &copied)
				if len(batch) >= 200 {
					h.sendLogBatch(batch)
					batch = batch[:0]
				}
			case <-ticker.C:
				if len(batch) > 0 {
					h.sendLogBatch(batch)
					batch = batch[:0]
				}
			}
		}
	}()
}

// sendLogBatch 批量发送日志，按阈值级别预序列化（每级别序列化一次），避免 N 客户端重复序列化
// serialized["WARN"] 包含 WARN+ERROR，serialized["INFO"] 包含 INFO+WARN+ERROR，依此类推
func (h *WebSocketHub) sendLogBatch(batch []*mproxy.LogMessage) {
	// 阶段1：构建通用 map 形式（仅一次）
	allItems := make([]map[string]any, len(batch))
	for i, msg := range batch {
		allItems[i] = map[string]any{
			"level": msg.Level, "session": msg.Session,
			"message": msg.Message, "time": msg.Time,
		}
	}

	// 阶段2：按阈值级别累积过滤并预序列化（最多4次序列化）
	// serialized = {
	//	 "ERROR" → JSON([ERROR级别的日志])
	//	 "WARN"  → JSON([WARN + ERROR 级别的日志])
	//	 "INFO"  → JSON([INFO + WARN + ERROR 级别的日志])
	//	 "DEBUG" → JSON([所有日志])
	//	}
	serialized := make(map[string][]byte, 4)
	for _, threshold := range []string{"ERROR", "WARN", "INFO", "DEBUG"} {
		thresholdVal := logLevels[threshold]
		filtered := make([]map[string]any, 0, len(allItems))
		for i, item := range allItems {
			if logLevels[batch[i].Level] >= thresholdVal {
				filtered = append(filtered, item)
			}
		}
		if len(filtered) == 0 {
			continue
		}
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		enc.Encode(map[string]any{"type": "log_batch", "data": filtered})
		serialized[threshold] = buf.Bytes()
	}

	// 阶段3：按客户端 LogLevel 分发已序列化的消息
	h.clients.Range(func(key, value any) bool {
		conn := key.(*websocket.Conn)
		sub := value.(*Subscription)
		if !sub.Logs {
			return true
		}
		level := sub.LogLevel
		if level == "" {
			level = "INFO"
		}
		if msgBytes, ok := serialized[level]; ok {
			if err := h.sendToBytes(conn, sub, msgBytes); err != nil {
				h.clients.Delete(conn)
				conn.Close()
			}
		}
		return true
	})
}

var logLevels = map[string]int{"DEBUG": 0, "INFO": 1, "WARN": 2, "ERROR": 3}

// MITM Exchange 详细信息推送器（批量收集 + 同步广播，消除 goroutine 风暴）
func (h *WebSocketHub) StartMitmDetailPusher() {
	go func() {
		batch := make([]*mproxy.HttpExchange, 0, 100)
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case exchange, ok := <-mproxy.GlobalExchangeChan:
				if !ok {
					return
				}
				batch = append(batch, exchange)
				if len(batch) >= 100 {
					h.sendMitmBatch(batch)
					batch = batch[:0]
				}
			case <-ticker.C:
				if len(batch) > 0 {
					h.sendMitmBatch(batch)
					batch = batch[:0]
				}
			}
		}
	}()
}

// sendMitmBatch 同步广播一批 exchange，无 goroutine 避免数据竞争
func (h *WebSocketHub) sendMitmBatch(batch []*mproxy.HttpExchange) {
	msg := map[string]any{
		"type": "mitm_exchange_batch",
		"data": batch,
	}
	h.broadcastToTopic("mitm_detail", msg)
}
