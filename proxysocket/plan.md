# 优化计划：修复高并发下 WebSocket 推送瓶颈

## 上下文

在高并发基准测试（`Benchmark_Stress_HTTP_MITM_Upload_Chunked`）下，WebSocket 推送器存在严重的性能瓶颈，导致：

1. 前端 UI 卡死（尤其是连接列表页面）
2. 流量速率图表出现长时间断更
3. MITM 抓包数据丢失

当前性能：68.35 MB/s（cpu=2），需要进一步提升。

## 关键文件

| 文件                      | 作用                            |
| ------------------------- | ------------------------------- |
| `proxysocket/hub.go`      | WebSocket 推送核心（308 行）    |
| `mproxy/mitm_exchange.go` | MITM Exchange 通道（容量 1000） |

---

## 问题分析

### 问题 1：写入锁内执行 JSON 序列化（严重 🔴）

**位置**：`hub.go:36-51` `sendTo` 方法

**现状**：

```go
func (h *WebSocketHub) sendTo(conn, sub, msg any) error {
    sub.writeMu.Lock()
    defer sub.writeMu.Unlock()

    var buf bytes.Buffer
    encoder := json.NewEncoder(&buf)  // 🔴 在锁内序列化！
    encoder.SetEscapeHTML(false)
    encoder.Encode(msg)
    // ...
}
```

**影响**：JSON 序列化是 CPU 密集型操作，单次可能耗时 10-100ms。持锁期间阻塞所有其他推送，导致"饿死"现象。

---

### 问题 2：连接推送无上限（严重 🔴）

**位置**：`hub.go:134-163` `StartConnectionPusher`

**现状**：每 500ms 遍历**整个** `Connections` Map，可能包含上万条已关闭连接（2秒墓碑期）。

**影响**：

- 后端：序列化数万条数据榨干 CPU
- 前端：Vue DOM 渲染压力过大导致页面卡死

---

### 问题 3：Exchange 通道缓冲不足（中等 🟡）

**位置**：`mitm_exchange.go:51`

**现状**：`GlobalExchangeChan = make(chan *HttpExchange, 1000)`

**影响**：高并发时通道满载，`SendExchange` 的 `default` 分支静默丢弃数据。

---

### 问题 4：日志批量推送重复序列化（新增 🆕）

**位置**：`hub.go:231-264` `sendLogBatch`

**现状**：每个客户端都独立构建和序列化 `filteredBatch`：

```go
func (h *WebSocketHub) sendLogBatch(batch []*mproxy.LogMessage) {
    h.clients.Range(func(key, value any) bool {
        // 每个客户端都重新过滤和构建数据
        filteredBatch := make([]map[string]any, 0, len(batch))
        for _, msg := range batch {
            if shouldSendLog(msg.Level, sub.LogLevel) {
                filteredBatch = append(...)  // 🔴 重复构建
            }
        }
        data := map[string]any{"type": "log_batch", "data": filteredBatch}
        h.sendTo(conn, sub, data)  // 🔴 重复序列化
    })
}
```

**影响**：N 个客户端 = N 次数据构建 + N 次 JSON 序列化 + N 次锁竞争。

---

### 问题 5：冗余代码（轻微 🟢）

**位置**：`hub.go:205-228` `broadcastLog`

**现状**：该函数定义后从未被调用（代码库搜索验证）。

---

## 修订后的实施计划

### 优化 1：预序列化 + 缩短锁持有时间

**目标**：将 JSON 序列化移到锁外，锁内只执行网络写入。

**修改点**：`hub.go`

#### 步骤 1.1：新增 `sendToBytes` 方法

```go
// sendToBytes 直接发送已序列化的字节流（锁内仅网络写入）
func (h *WebSocketHub) sendToBytes(conn *websocket.Conn, sub *Subscription, msgBytes []byte) error {
    sub.writeMu.Lock()
    defer sub.writeMu.Unlock()

    conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
    err := conn.WriteMessage(websocket.TextMessage, msgBytes)
    conn.SetWriteDeadline(time.Time{})
    return err
}
```

#### 步骤 1.2：修改 `broadcastToTopic` 预序列化

```go
func (h *WebSocketHub) broadcastToTopic(topic string, msg any) {
    // 预序列化（锁外执行一次）
    msgBytes, err := json.Marshal(msg)
    if err != nil {
        return
    }

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
```

**影响**：`StartTrafficPusher`、`sendMitmBatch` 自动受益。

---

### 优化 2：连接推送限制 + 优先级排序

**目标**：单次最多推送 300 条高价值连接。

**修改点**：`hub.go:122-171` `StartConnectionPusher`

```go
func (h *WebSocketHub) StartConnectionPusher() {
    const tombstoneRetention = 2000 * time.Millisecond
    const maxConnections = 300  // 新增上限

    go func() {
        ticker := time.NewTicker(500 * time.Millisecond)
        defer ticker.Stop()

        for range ticker.C {
            connections := make([]map[string]any, 0, maxConnections)
            now := time.Now()

            // 收集所有连接
            allConns := make([]*mproxy.ConnectionInfo, 0)
            h.proxy.Connections.Range(func(key, value any) bool {
                session := key.(int64)
                info := value.(*mproxy.ConnectionInfo)

                // 垃圾回收
                if info.Status == "Closed" && now.Sub(info.EndTime) > tombstoneRetention {
                    h.proxy.Connections.Delete(session)
                    return true
                }
                allConns = append(allConns, info)
                return true
            })

            // 排序：活跃连接优先，然后按 Session 倒序（最新的在前）
            sort.Slice(allConns, func(i, j int) bool {
                if allConns[i].Status != allConns[j].Status {
                    return allConns[i].Status == "Active"  // Active 优先
                }
                return allConns[i].Session > allConns[j].Session  // 新的优先
            })

            // 限制数量
            limit := min(len(allConns), maxConnections)
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
                "type": "connections",
                "data": connections,
            })
        }
    }()
}
```

**新增 import**：`sort`

---

### 优化 3：Exchange 通道扩容

**修改点**：`mitm_exchange.go:51`

```go
var GlobalExchangeChan = make(chan *HttpExchange, 5000)  // 从 1000 提升到 5000
```

---

### 优化 4：日志推送分组预序列化（新增 🆕）

**目标**：避免每个客户端重复序列化相同数据。

**修改点**：`hub.go:231-264` `sendLogBatch`

```go
// sendLogBatch 批量发送日志，按日志级别分组预序列化
func (h *WebSocketHub) sendLogBatch(batch []*mproxy.LogMessage) {
    // 按日志级别分组
    groups := map[string][]map[string]any{
        "DEBUG": {},
        "INFO":  {},
        "WARN":  {},
        "ERROR": {},
    }

    for _, msg := range batch {
        data := map[string]any{
            "level":   msg.Level,
            "session": msg.Session,
            "message": msg.Message,
            "time":    msg.Time,
        }
        groups[msg.Level] = append(groups[msg.Level], data)
    }

    // 为每个级别预序列化
    serialized := map[string][]byte{}
    for level, data := range groups {
        if len(data) == 0 {
            continue
        }
        msg := map[string]any{"type": "log_batch", "data": data}
        bytes, err := json.Marshal(msg)
        if err != nil {
            continue
        }
        serialized[level] = bytes
    }

    // 按客户端级别发送
    h.clients.Range(func(key, value any) bool {
        conn := key.(*websocket.Conn)
        sub := value.(*Subscription)

        if !sub.Logs {
            return true
        }

        // 找到该客户端应该接收的最低级别
        var targetLevel string
        for _, level := range []string{"DEBUG", "INFO", "WARN", "ERROR"} {
            if logLevels[level] >= logLevels[sub.LogLevel] {
                targetLevel = level
                break
            }
        }

        if msgBytes, ok := serialized[targetLevel]; ok {
            if err := h.sendToBytes(conn, sub, msgBytes); err != nil {
                h.clients.Delete(conn)
                conn.Close()
            }
        }
        return true
    })
}
```

---

### 优化 5：清理冗余代码

**修改点**：删除 `hub.go:205-228` `broadcastLog` 函数（未使用）。

---

### 优化 6：SetWriteDeadline 优化（可选）

**位置**：`sendToBytes` 方法

**当前**：每次发送都设置和清除 deadline（2 次系统调用）

**优化**：考虑在连接建立时设置持久写入超时，或在发送错误时直接关闭连接。

**权衡**：需要评估 WebSocket 连接管理策略，暂不列入核心优化。

---

## 验证方法

### 1. 自动化基准测试

```bash
# 执行压测
go test -bench=Benchmark_Stress_HTTP_MITM_Upload_Chunked -benchtime=3s -run=^$ -v -cpu 2

# 期待指标
# - 吞吐量提升（68 MB/s → 目标 80+ MB/s）
# - 无 goroutine 泄漏
# - 无 panic
```

### 2. 人工验证

| 验证项    | 方法               | 期待结果                 |
| --------- | ------------------ | ------------------------ |
| 流量图表  | 压测期间观察前端   | 每秒稳定更新，无卡死     |
| 连接列表  | 观察详细连接页面   | 数量不超过 300，滚动流畅 |
| MITM 数据 | 压测后查看抓包列表 | 数据完整，无大面积丢失   |

### 3. pprof 分析

```bash
# 在压测期间采集 CPU profile
curl http://localhost:6060/debug/pprof/profile?seconds=10 > cpu.prof

# 分析
go tool pprof -http=:8080 cpu.prof

# 期待
# - json encoding 时间占比显著下降
# - sync.Mutex 等待时间减少
```

---

## 风险评估

| 风险                 | 缓解措施                                           |
| -------------------- | -------------------------------------------------- |
| 预序列化内存占用增加 | 批量数据量已有限制（日志 200、MITM 100、连接 300） |
| 连接排序增加 CPU     | 排序数量已限制（最多 300）                         |
| 日志分组逻辑错误     | 保留原 `shouldSendLog` 逻辑验证                    |

---

## 实施顺序

1. **优先级高**：优化 1（预序列化） + 优化 3（通道扩容）- 低风险高收益
2. **优先级高**：优化 2（连接限制） - 直接解决前端卡死
3. **优先级中**：优化 4（日志分组） - 进一步减少 CPU 占用
4. **优先级低**：优化 5（清理代码）- 代码整洁性