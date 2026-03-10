访问控制功能实施计划（完整版）
Context
本功能旨在为代理服务器添加访问控制能力，根据目标域名（后缀匹配、正则匹配）和目标 IP 拦截特定请求，同时记录拦截统计信息并通过 WebUI 展示拦截日志。

技术架构分析
现有推送机制（高性能设计）：

StartLogPusher(): 批量收集 + 定时推送（300ms 或 200 条），按 LogLevel 预序列化

StartConnectionPusher(): 每 500ms 全量快照（Active 优先，最多 350 条）

broadcastToTopic(): 预序列化一次，分发给所有订阅客户端

前端虚拟列表（已验证）：

HistoryConnections.vue: 使用 @tanstack/vue-virtual，45px 行高，overscan 20

Connections.vue: 动态行高 + measureElement，支持父子节点展开

配置热更新（已实现）：

/api/config 端点已支持 GET/POST

UpdateConfig() 后调用 Router.ReloadFromConfig() 热重载路由规则

审查结果
发现的问题
[CRITICAL] 计划中原定删除 router.go 中的规则函数需要重新组织

DomainSuffixRule, DomainKeywordRule, IPRule 被 ReloadFromConfig 直接调用

影响范围：10个执行流程，风险等级 CRITICAL

修正：将这些函数移动到 hooks.go，与 ReqCondition 接口和其他条件函数（如 UrlHook、UrlRegHook）放在一起，实现更好的代码组织

[MEDIUM] TextResponse 返回 202 而非 403

现有 TextResponse 函数使用 http.StatusAccepted (202) 而非 StatusForbidden (403)

修正：访问控制应使用 http.StatusForbidden，可在 response_auto.go 中新增专门的 ForbiddenResponse 函数

[LOW] 拦截统计的并发安全

计划中的 InterceptStats 需要使用读写锁保证并发安全

修正：在设计统计结构体时明确使用 sync.RWMutex

优化建议
简化统计记录：无需存储完整拦截记录切片，仅保留计数和最近 N 条记录即可

支持热更新：访问控制规则应支持配置热更新，类似路由功能的 ReloadFromConfig

修订后的实施计划
Phase 1: Configuration
文件：mproxy/config.go
新增 AccessRule 结构体（在 RouteRule 后添加）：

// AccessRule 访问控制规则配置
type AccessRule struct {
    Id      int    `json:"Id"`      // 前端生成的唯一 ID
    Type    string `json:"Type"`    // "DomainSuffix" | "DomainKeyword" | "IP"
    Value   string `json:"Value"`   // "twitter.com" 等值，逗号分隔支持多个
    Enable  bool   `json:"Enable"`  // 该条规则的独立开关
    Remarks string `json:"Remarks"` // 用户备注
}
修改 ServerConfig 结构体（在现有字段后添加）：

// 访问控制相关配置
AccessEnable bool         `json:"AccessEnable"` // 访问控制总开关
AccessRules  []AccessRule `json:"AccessRules"`  // 访问控制规则列表
修改 DefaultConfig 函数（在现有返回值中添加字段初始化）：

AccessEnable: false,
AccessRules:  []AccessRule{},
Phase 2: Stats & Records
文件：mproxy/core_proxy.go
新增 InterceptStats 结构体（在文件顶部 TrafficCounter 附近添加）：

// InterceptRecord 单条拦截记录
type InterceptRecord struct {
    ClientIP    string    `json:"client_ip"`    // 客户端 IP
    Target      string    `json:"target"`       // 被拦截的目标（域名/IP）
    RuleType    string    `json:"rule_type"`    // 触发的规则类型
    RuleValue   string    `json:"rule_value"`   // 触发的规则值
    Timestamp   time.Time `json:"timestamp"`    // 拦截时间
}
​
// InterceptStats 拦截统计（并发安全）
type InterceptStats struct {
    mu              sync.RWMutex
    TotalIntercepts int64             // 总拦截次数
    RecentRecords   []InterceptRecord // 最近 100 条记录
    MaxRecords      int               // 最大记录数
}
​
// AddRecord 添加拦截记录（线程安全）
func (s *InterceptStats) AddRecord(clientIP, target, ruleType, ruleValue string) {
    s.mu.Lock()
    defer s.mu.Unlock()
​
    s.TotalIntercepts++
    record := InterceptRecord{
        ClientIP:  clientIP,
        Target:    target,
        RuleType:  ruleType,
        RuleValue: ruleValue,
        Timestamp: time.Now(),
    }
​
    // 保持最多 MaxRecords 条记录
    if len(s.RecentRecords) >= s.MaxRecords {
        s.RecentRecords = s.RecentRecords[1:]
    }
    s.RecentRecords = append(s.RecentRecords, record)
}
​
// GetStats 获取统计快照（线程安全）
func (s *InterceptStats) GetStats() (int64, []InterceptRecord) {
    s.mu.RLock()
    defer s.mu.RUnlock()
​
    records := make([]InterceptRecord, len(s.RecentRecords))
    copy(records, s.RecentRecords)
    return s.TotalIntercepts, records
}
修改 CoreHttpServer 结构体（在现有字段后添加）：

InterceptStats *InterceptStats // 拦截统计
修改 NewCoreHttpSever 函数（在结构体初始化时添加）：

core_proxy.InterceptStats = &InterceptStats{
    MaxRecords: 100,
}
Phase 3: Response Helper
文件：mproxy/response_auto.go
新增专用拦截响应函数（在现有函数后添加）：

// ForbiddenResponse 返回 403 Forbidden 响应（用于访问控制拦截）
func ForbiddenResponse(r *http.Request, message string) *http.Response {
    return NewResponse(r, ContentTypeText, http.StatusForbidden, message)
}
Phase 4: Reorganize Rule Functions
文件：mproxy/router.go
[DELETE] 删除规则构建函数（删除第275-325行）：

删除 DomainSuffixRule 函数

删除 DomainKeywordRule 函数

删除 IPRule 函数

[MODIFY] 更新调用引用：

ReloadFromConfig 函数中的调用保持不变（Go 同一 package 内自动解析）

文件：mproxy/hooks.go
[NEW] 新增规则构建函数（在文件末尾添加，与 ContentTypeHook 等函数并列）：

// ======================== 规则构建函数 ========================
// 这些函数用于访问控制和路由功能，返回 ReqCondition 供 HookOnReq/DoFunc 使用
​
// DomainSuffixRule 域名后缀匹配规则（自动剥离端口，小写不区分）
func DomainSuffixRule(suffixes ...string) ReqConditionFunc {
    for i, s := range suffixes {
        suffixes[i] = strings.ToLower(s)
    }
    return func(req *http.Request, ctx *Pcontext) bool {
        host := extractHost(req)
        for _, suffix := range suffixes {
            if host == suffix || strings.HasSuffix(host, "."+suffix) {
                return true
            }
        }
        return false
    }
}
​
// DomainKeywordRule 域名正则匹配规则（自动剥离端口，忽略大小写）
func DomainKeywordRule(patterns ...string) ReqConditionFunc {
    regs := make([]*regexp.Regexp, 0, len(patterns))
    for _, p := range patterns {
        if r, err := regexp.Compile("(?i)" + p); err == nil {
            regs = append(regs, r)
        }
    }
    return func(req *http.Request, ctx *Pcontext) bool {
        host := extractHost(req)
        for _, r := range regs {
            if r.MatchString(host) {
                return true
            }
        }
        return false
    }
}
​
// IPRule IP 精确匹配规则（仅匹配纯 IP，不匹配域名）
func IPRule(ipList ...string) ReqConditionFunc {
    ipSet := make(map[string]bool, len(ipList))
    for _, ip := range ipList {
        ipSet[ip] = true
    }
    return func(req *http.Request, ctx *Pcontext) bool {
        host := extractHost(req)
        if net.ParseIP(host) != nil {
            return ipSet[host]
        }
        return false
    }
}
Phase 5: Access Control Implementation
文件：mproxy/actions.go
新增核心函数 AddAccessControl（在文件末尾添加）：

// AddAccessControl 注入访问控制拦截逻辑
func AddAccessControl(proxy *CoreHttpServer) {
    if proxy.Config == nil || proxy.InterceptStats == nil {
        return
    }
​
    // 构建条件规则列表（从配置加载）
    var conditions []struct {
        cond      ReqCondition
        ruleType  string
        ruleValue string
    }
​
    cfg := proxy.Config.GetConfig()
    if !cfg.AccessEnable {
        return
    }
​
    for _, rule := range cfg.AccessRules {
        if !rule.Enable {
            continue
        }
​
        // 支持逗号分隔的多个值
        rawValues := strings.Split(rule.Value, ",")
        var values []string
        for _, v := range rawValues {
            v = strings.TrimSpace(v)
            if v != "" {
                values = append(values, v)
            }
        }
        if len(values) == 0 {
            continue
        }
​
        var cond ReqCondition
        switch rule.Type {
        case "DomainSuffix":
            cond = DomainSuffixRule(values...)
        case "DomainKeyword":
            cond = DomainKeywordRule(values...)
        case "IP":
            cond = IPRule(values...)
        default:
            proxy.Logger.Printf("WARN: 未知访问控制规则类型 %s", rule.Type)
            continue
        }
​
        conditions = append(conditions, struct {
            cond      ReqCondition
            ruleType  string
            ruleValue string
        }{cond, rule.Type, rule.Value})
    }
​
    if len(conditions) == 0 {
        return
    }
​
    // HTTP 请求拦截（拦截普通 HTTP 和 MITM 解密后的 HTTPS）
    proxy.HookOnReq(conditions...).DoFunc(func(req *http.Request, ctx *Pcontext) (*http.Request, *http.Response) {
        // 提取客户端 IP
        clientIP := getClientIP(req)
​
        // 提取目标
        target := req.URL.Host
        if target == "" {
            target = req.Host
        }
​
        // 匹配任意规则即拦截
        for _, rule := range conditions {
            if rule.cond.HandleReq(req, ctx) {
                ctx.Log_P("[访问控制] 拦截请求: 客户端=%s, 目标=%s, 规则=%s:%s",
                    clientIP, target, rule.ruleType, rule.ruleValue)
​
                // 记录拦截
                proxy.InterceptStats.AddRecord(clientIP, target, rule.ruleType, rule.ruleValue)
​
                // 返回 403 响应
                return nil, ForbiddenResponse(req, "403 Access Denied")
            }
        }
        return req, nil
    })
​
    // HTTPS/CONNECT 拦截
    proxy.HookOnReq(conditions...).DoConnectFunc(func(host string, ctx *Pcontext) (*ConnectAction, string) {
        clientIP := getClientIP(ctx.Req)
        target := host
​
        for _, rule := range conditions {
            if rule.cond.HandleReq(ctx.Req, ctx) {
                ctx.Log_P("[访问控制] 拦截 CONNECT: 客户端=%s, 目标=%s, 规则=%s:%s",
                    clientIP, target, rule.ruleType, rule.ruleValue)
​
                // 记录拦截
                proxy.InterceptStats.AddRecord(clientIP, target, rule.ruleType, rule.ruleValue)
​
                // 构造响应并赋值给 ctx.Resp
                ctx.Resp = ForbiddenResponse(ctx.Req, "403 Access Denied")
​
                // 返回 ConnectReject，https.go 会将响应写入连接并关闭
                return ConnectReject, host
            }
        }
        return OkConnect, host
    })
}
​
// getClientIP 从请求中提取客户端 IP
func getClientIP(req *http.Request) string {
    // 优先检查 X-Forwarded-For 和 X-Real-IP
    if xff := req.Header.Get("X-Forwarded-For"); xff != "" {
        // 取第一个 IP
        if idx := strings.Index(xff, ","); idx != -1 {
            return strings.TrimSpace(xff[:idx])
        }
        return xff
    }
    if xri := req.Header.Get("X-Real-IP"); xri != "" {
        return xri
    }
​
    // 从 RemoteAddr 提取
    if req.RemoteAddr != "" {
        if host, _, err := net.SplitHostPort(req.RemoteAddr); err == nil {
            return host
        }
        return req.RemoteAddr
    }
​
    return "unknown"
}
Phase 5: Integration
文件：main.go 或 proxysocket/proxy.go（根据项目入口）
在启动代理服务器时调用 AddAccessControl（在 AddRouter 之后添加）：

// 注入访问控制（需在 AddRouter 之后，因为要复用 DomainSuffixRule 等函数）
mproxy.AddAccessControl(proxy)
关键文件清单
文件	修改类型	说明
mproxy/config.go	[MODIFY]	新增 AccessRule 结构体和配置字段
mproxy/core_proxy.go	[MODIFY]	新增 InterceptStats 结构体和字段
mproxy/response_auto.go	[MODIFY]	新增 ForbiddenResponse 函数
mproxy/router.go	[DELETE]	删除 DomainSuffixRule/DomainKeywordRule/IPRule 函数
mproxy/hooks.go	[NEW]	新增 DomainSuffixRule/DomainKeywordRule/IPRule 函数
mproxy/actions.go	[NEW]	新增 AddAccessControl 函数
main.go 或 proxysocket/proxy.go	[MODIFY]	调用 AddAccessControl
验证方法
1. 配置文件测试
在 config.json 中添加访问控制配置：

{
  "AccessEnable": true,
  "AccessRules": [
    {
      "Id": 1,
      "Type": "DomainSuffix",
      "Value": "baidu.com",
      "Enable": true,
      "Remarks": "拦截百度"
    },
    {
      "Id": 2,
      "Type": "DomainKeyword",
      "Value": ".*google.*",
      "Enable": true,
      "Remarks": "拦截 Google 域名"
    },
    {
      "Id": 3,
      "Type": "IP",
      "Value": "1.1.1.1",
      "Enable": true,
      "Remarks": "拦截 Cloudflare DNS"
    }
  ]
}
2. HTTP 请求测试
# 测试 HTTP 请求拦截
curl -x http://127.0.0.1:8080 http://www.baidu.com
# 期望返回: 403 Access Denied
3. HTTPS CONNECT 测试
# 测试 HTTPS CONNECT 拦截
curl -x http://127.0.0.1:8080 --proxy-insecure https://1.1.1.1
# 期望: 快速断开或收到 403
4. 日志验证
检查控制台输出是否包含：

[访问控制] 拦截请求: 客户端=xxx, 目标=xxx, 规则=xxx

拦截计数增加

5. 统计验证
通过 API 或日志检查 InterceptStats 的统计数据是否正确记录。

设计说明
为什么将规则函数移动到 hooks.go？
代码组织一致性：ReqCondition 接口在 hooks.go 中定义，规则函数是实现该接口的条件函数，应与 UrlHook、UrlRegHook、ContentTypeHook 等函数放在一起

使用模式统一：proxy.HookOnReq(DomainSuffixRule("...")) 符合现有的 hooks 调用模式

职责清晰：router.go 专注于路由逻辑（Router 结构体、拨号器管理），hooks.go 专注于条件判断和钩子机制

同 package 调用：所有文件都在 package mproxy 中，移动后 router.go 的 ReloadFromConfig 无需修改即可调用

为什么使用 403 而非 202？
http.StatusAccepted (202) 表示请求已接受，不符合拦截场景

http.StatusForbidden (403) 是访问控制的标准响应码

为什么限制记录数量？
避免内存无限增长

保留最近 100 条记录足以满足排查需求

完整日志可通过 Logger 输出到文件

后续扩展（可选）
API 接口：添加 REST API 查询拦截统计和规则

白名单模式：支持白名单（仅允许特定域名/IP）

时间段控制：支持在特定时间段内启用/禁用规则

客户端 IP 过滤：支持按客户端 IP 进行访问控制

WebUI 实施方案
后端扩展：拦截日志推送器
文件：mproxy/logs.go
新增拦截日志通道（在现有通道后添加）：

// InterceptLogChan 拦截日志通道（全局）
var InterceptLogChan = make(chan *InterceptLogMessage, 500)
​
// InterceptLogMessage 拦截日志消息
type InterceptLogMessage struct {
    Level    string    `json:"level"`              // "INFO" | "WARN"
    Session  int64     `json:"session"`            // 会话 ID（可为 0）
    Message  string    `json:"message"`            // 日志消息
    Time     time.Time `json:"time"`               // 时间戳
    ClientIP string    `json:"client_ip"`          // 客户端 IP
    Target   string    `json:"target"`             // 被拦截的目标
    RuleType string    `json:"rule_type"`          // 规则类型
    RuleValue string   `json:"rule_value"`         // 规则值
}
文件：mproxy/actions.go
修改 AddAccessControl 函数，在拦截时发送日志到通道：

// 在拦截后添加日志发送
proxy.InterceptStats.AddRecord(clientIP, target, rule.ruleType, rule.ruleValue)
​
// 发送拦截日志到 WebSocket 推送通道
mproxy.InterceptLogChan <- &mproxy.InterceptLogMessage{
    Level:    "INFO",
    Session:  0, // 拦截时无 Session
    Message:  fmt.Sprintf("拦截访问: 客户端=%s, 目标=%s, 规则=%s:%s", clientIP, target, rule.ruleType, rule.ruleValue),
    Time:     time.Now(),
    ClientIP: clientIP,
    Target:   target,
    RuleType: rule.ruleType,
    RuleValue: rule.ruleValue,
}
文件：proxysocket/hub.go
1. 修改 Subscription 结构体（添加拦截日志订阅）：

type Subscription struct {
    Traffic     bool
    Connections bool
    Logs        bool
    LogLevel    string
    MitmDetail  bool
    InterceptLogs bool // 新增：拦截日志订阅
    writeMu     sync.Mutex
}
2. 修改 updateSubscription 函数（支持拦截日志订阅）：

func (h *WebSocketHub) updateSubscription(sub *Subscription, msg map[string]any) {
    if topics, ok := msg["topics"].([]any); ok {
        sub.Traffic = contains(topics, "traffic")
        sub.Connections = contains(topics, "connections")
        sub.Logs = contains(topics, "logs")
        sub.MitmDetail = contains(topics, "mitm_detail")
        sub.InterceptLogs = contains(topics, "intercept_logs") // 新增
    }
    if logLevel, ok := msg["logLevel"].(string); ok {
        sub.LogLevel = logLevel
    }
}
3. 修改 broadcastToTopic 函数（支持拦截日志主题）：

func (h *WebSocketHub) broadcastToTopic(topic string, msg any) {
    // ... 现有预序列化代码 ...
​
    h.clients.Range(func(key, value any) bool {
        conn := key.(*websocket.Conn)
        sub := value.(*Subscription)
​
        var shouldSend bool
        switch topic {
        case "traffic":
            shouldSend = sub.Traffic
        case "connections":
            shouldSend = sub.Connections
        case "mitm_detail":
            shouldSend = sub.MitmDetail
        case "intercept_logs": // 新增
            shouldSend = sub.InterceptLogs
        }
​
        // ... 现有发送代码 ...
        return true
    })
}
4. 新增拦截日志推送器（在文件末尾添加，参考 StartLogPusher）：

// 拦截日志推送器（批量收集 + 定时推送，类比 StartLogPusher）
func (h *WebSocketHub) StartInterceptLogPusher() {
    go func() {
        batch := make([]*mproxy.InterceptLogMessage, 0, 100)
        ticker := time.NewTicker(500 * time.Millisecond)
        defer ticker.Stop()
​
        for {
            select {
            case log, ok := <-mproxy.InterceptLogChan:
                if !ok {
                    if len(batch) > 0 {
                        h.sendInterceptLogBatch(batch)
                    }
                    return
                }
                batch = append(batch, log)
                if len(batch) >= 100 {
                    h.sendInterceptLogBatch(batch)
                    batch = batch[:0]
                }
            case <-ticker.C:
                if len(batch) > 0 {
                    h.sendInterceptLogBatch(batch)
                    batch = batch[:0]
                }
            }
        }
    }()
}
​
// sendInterceptLogBatch 发送拦截日志批次（预序列化一次，分发给所有客户端）
func (h *WebSocketHub) sendInterceptLogBatch(batch []*mproxy.InterceptLogMessage) {
    // 构建 map 形式
    allItems := make([]map[string]any, len(batch))
    for i, log := range batch {
        allItems[i] = map[string]any{
            "level":     log.Level,
            "session":   log.Session,
            "message":   log.Message,
            "time":      log.Time,
            "client_ip": log.ClientIP,
            "target":    log.Target,
            "rule_type": log.RuleType,
            "rule_value": log.RuleValue,
        }
    }
​
    // 预序列化一次
    var buf bytes.Buffer
    encoder := json.NewEncoder(&buf)
    encoder.SetEscapeHTML(false)
    encoder.Encode(map[string]any{"type": "intercept_log_batch", "data": allItems})
    msgBytes := buf.Bytes()
​
    // 分发给所有订阅拦截日志的客户端
    h.clients.Range(func(key, value any) bool {
        conn := key.(*websocket.Conn)
        sub := value.(*Subscription)
        if !sub.InterceptLogs {
            return true
        }
        if err := h.sendToBytes(conn, sub, msgBytes); err != nil {
            h.clients.Delete(conn)
            conn.Close()
        }
        return true
    })
}
5. 修改 StartControlServer（启动拦截日志推送器）：

// 启动推送服务
hub.StartTrafficPusher()
hub.StartConnectionPusher()
hub.StartLogPusher()
hub.StartMitmDetailPusher()
hub.StartInterceptLogPusher() // 新增
前端实现
文件：proxyui/src/stores/websocket.js
1. 修改订阅配置（添加拦截日志订阅）：

const subscriptions = ref({
  traffic: true,
  connections: true,
  logs: true,
  logLevel: 'INFO',
  mitm: true,
  interceptLogs: true // 新增
})
2. 修改 subscribe 函数（发送拦截日志订阅）：

function subscribe() {
  if (!socket.value || socket.value.readyState !== WebSocket.OPEN) return
​
  socket.value.send(JSON.stringify({
    action: 'subscribe',
    topics: [
      subscriptions.value.traffic && 'traffic',
      subscriptions.value.connections && 'connections',
      subscriptions.value.logs && 'logs',
      subscriptions.value.mitm && 'mitm_detail',
      subscriptions.value.interceptLogs && 'intercept_logs' // 新增
    ].filter(Boolean),
    logLevel: subscriptions.value.logLevel
  }))
}
3. 添加拦截日志状态和订阅者：

// 数据
const interceptLogs = ref([]) // 拦截日志列表
const MAX_INTERCEPT_LOGS = 500
​
// 订阅者回调
const interceptLogsSubscribers = ref(new Set())
​
// 订阅拦截日志
function subscribeInterceptLogs(callback) {
  interceptLogsSubscribers.value.add(callback)
  return () => interceptLogsSubscribers.value.delete(callback)
}
4. 修改 handleMessage 函数（处理拦截日志消息）：

function handleMessage(msg) {
  switch (msg.type) {
    // ... 现有 case ...
    case 'intercept_log_batch':
      // 追加新日志，保持最大数量限制
      msg.data.forEach(log => {
        interceptLogs.value.push(log)
      })
      if (interceptLogs.value.length > MAX_INTERCEPT_LOGS) {
        interceptLogs.value = interceptLogs.value.slice(-MAX_INTERCEPT_LOGS)
      }
      // 通知订阅者
      interceptLogsSubscribers.value.forEach(callback => callback(interceptLogs.value))
      break
  }
}
5. 导出拦截日志相关功能：

return {
  // ... 现有导出 ...
  interceptLogs,
  subscribeInterceptLogs,
  clearInterceptLogs: () => { interceptLogs.value = [] }
}
文件：proxyui/src/views/SecurityPolicy.vue（新建）
完整实现（复用 HistoryConnections.vue 的虚拟列表模式）：

<template>
  <div class="security-policy">
    <h1>安全策略</h1>
​
    <!-- 上半部分：规则配置表单 -->
    <div class="rules-section">
      <div class="section-header">
        <h2>访问控制规则</h2>
        <button @click="handleAddRule" class="btn-add">+ 新增规则</button>
      </div>
​
      <!-- 规则列表 -->
      <div class="rules-list">
        <div v-for="rule in accessRules" :key="rule.Id" class="rule-item" :class="{ disabled: !rule.Enable }">
          <div class="rule-header">
            <input type="checkbox" v-model="rule.Enable" @change="handleRuleChange" class="rule-enable" />
            <span class="rule-id">#{{ rule.Id }}</span>
            <span class="rule-remarks">{{ rule.Remarks || '未命名规则' }}</span>
            <button @click="handleDeleteRule(rule.Id)" class="btn-delete" title="删除规则">✕</button>
          </div>
          <div class="rule-body">
            <div class="rule-field">
              <label>规则类型:</label>
              <select v-model="rule.Type" @change="handleRuleChange" :disabled="!rule.Enable">
                <option value="DomainSuffix">域名后缀</option>
                <option value="DomainKeyword">域名正则</option>
                <option value="IP">IP 地址</option>
              </select>
            </div>
            <div class="rule-field">
              <label>匹配值:</label>
              <input v-model="rule.Value" @input="handleRuleChange" :disabled="!rule.Enable" placeholder="逗号分隔多个值" />
            </div>
          </div>
        </div>
        <div v-if="accessRules.length === 0" class="no-rules">暂无规则</div>
      </div>
​
      <!-- 总开关 -->
      <div class="global-toggle">
        <label class="toggle-switch">
          <input type="checkbox" v-model="accessEnable" @change="handleConfigSave" />
          <span class="slider"></span>
        </label>
        <span>启用访问控制</span>
      </div>
    </div>
​
    <!-- 下半部分：拦截日志（虚拟列表） -->
    <div class="logs-section">
      <div class="section-header">
        <h2>拦截日志</h2>
        <button @click="handleClearLogs" class="btn-clear">清空日志</button>
      </div>
​
      <div class="logs-scroller" ref="logsScrollerRef">
        <!-- sticky 表头 -->
        <div class="logs-grid-row thead-row">
          <div class="th">时间</div>
          <div class="th">规则类型</div>
          <div class="th">规则值</div>
          <div class="th">客户端 IP</div>
          <div class="th">目标</div>
          <div class="th">消息</div>
        </div>
​
        <!-- 空数据提示 -->
        <div v-if="filteredInterceptLogs.length === 0" class="no-data">
          {{ searchQuery ? '未找到匹配的拦截日志' : '暂无拦截记录' }}
        </div>
​
        <!-- 虚拟滚动容器 -->
        <div :style="{ position: 'relative', height: logsTotalSize + 'px' }">
          <div
            v-for="virtualRow in logsVirtualRows"
            :key="virtualRow.key"
            :style="{
              position: 'absolute',
              top: 0,
              left: 0,
              width: '100%',
              transform: `translateY(${virtualRow.start}px)`
            }"
          >
            <div class="logs-grid-row data-row">
              <div class="td time-cell">{{ formatTime(filteredInterceptLogs[virtualRow.index].time) }}</div>
              <div class="td">
                <span class="badge rule-type-badge" :class="getRuleTypeClass(filteredInterceptLogs[virtualRow.index].rule_type)">
                  {{ filteredInterceptLogs[virtualRow.index].rule_type }}
                </span>
              </div>
              <div class="td">{{ filteredInterceptLogs[virtualRow.index].rule_value }}</div>
              <div class="td">{{ filteredInterceptLogs[virtualRow.index].client_ip }}</div>
              <div class="td">{{ filteredInterceptLogs[virtualRow.index].target }}</div>
              <div class="td message-cell">{{ filteredInterceptLogs[virtualRow.index].message }}</div>
            </div>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>

<script setup>
import { ref, computed, onMounted, onUnmounted } from 'vue'
import { useVirtualizer } from '@tanstack/vue-virtual'
import { useWebSocketStore } from '@/stores/websocket'
​
const wsStore = useWebSocketStore()
​
// 响应式数据
const accessRules = ref([])
const accessEnable = ref(false)
const interceptLogs = ref([])
const searchQuery = ref('')
const logsScrollerRef = ref(null)
​
let unsubscribeInterceptLogs = null
let nextRuleId = 1
​
// 计算属性：过滤后的拦截日志
const filteredInterceptLogs = computed(() => {
  let result = interceptLogs.value
  if (searchQuery.value) {
    const query = searchQuery.value.toLowerCase()
    result = result.filter(log =>
      log.client_ip?.toLowerCase().includes(query) ||
      log.target?.toLowerCase().includes(query) ||
      log.rule_value?.toLowerCase().includes(query) ||
      log.message?.toLowerCase().includes(query)
    )
  }
  return result
})
​
// 虚拟滚动器
const logsVirtualizer = useVirtualizer(
  computed(() => ({
    count: filteredInterceptLogs.value.length,
    getScrollElement: () => logsScrollerRef.value,
    estimateSize: () => 50,
    overscan: 20,
    getItemKey: (index) => 'il-' + filteredInterceptLogs.value[index].time + '-' + index,
  }))
)
​
const logsVirtualRows = computed(() => logsVirtualizer.value.getVirtualItems())
const logsTotalSize = computed(() => logsVirtualizer.value.getTotalSize())
​
// 新增规则
function handleAddRule() {
  accessRules.value.push({
    Id: nextRuleId++,
    Type: 'DomainSuffix',
    Value: '',
    Enable: true,
    Remarks: ''
  })
  handleConfigSave()
}
​
// 删除规则
function handleDeleteRule(id) {
  accessRules.value = accessRules.value.filter(r => r.Id !== id)
  handleConfigSave()
}
​
// 规则变更
function handleRuleChange() {
  handleConfigSave()
}
​
// 保存配置
async function handleConfigSave() {
  const config = wsStore.config
  if (!config) return
​
  config.AccessRules = accessRules.value
  config.AccessEnable = accessEnable.value
​
  try {
    await wsStore.updateConfig(config)
    console.log('[SecurityPolicy] 配置已保存')
  } catch (err) {
    console.error('[SecurityPolicy] 保存配置失败:', err)
  }
}
​
// 清空日志
function handleClearLogs() {
  wsStore.clearInterceptLogs()
}
​
// 格式化时间
function formatTime(timeStr) {
  if (!timeStr) return ''
  const date = new Date(timeStr)
  return date.toLocaleTimeString('zh-CN', { hour12: false })
}
​
// 规则类型样式
function getRuleTypeClass(type) {
  const map = {
    'DomainSuffix': 'rule-suffix',
    'DomainKeyword': 'rule-keyword',
    'IP': 'rule-ip'
  }
  return map[type] || 'rule-default'
}
​
// 生命周期
onMounted(async () => {
  // 加载配置
  const config = wsStore.config
  if (config) {
    accessRules.value = config.AccessRules || []
    accessEnable.value = config.AccessEnable || false
    if (accessRules.value.length > 0) {
      nextRuleId = Math.max(...accessRules.value.map(r => r.Id)) + 1
    }
  }
​
  // 订阅拦截日志
  unsubscribeInterceptLogs = wsStore.subscribeInterceptLogs((data) => {
    interceptLogs.value = data
  })
​
  // 初始化数据
  interceptLogs.value = wsStore.interceptLogs || []
})
​
onUnmounted(() => {
  if (unsubscribeInterceptLogs) unsubscribeInterceptLogs()
})
</script>

<style scoped>
.security-policy {
  padding: 20px;
  height: calc(100vh - 40px);
  display: flex;
  flex-direction: column;
  gap: 20px;
}
​
h1 {
  color: #cba376;
  margin: 0;
  flex-shrink: 0;
}
​
/* 规则配置区 */
.rules-section {
  flex: 0 0 auto;
  background: #2a2a2a;
  border-radius: 8px;
  padding: 20px;
  max-height: 40vh;
  overflow: hidden;
  display: flex;
  flex-direction: column;
}
​
.section-header {
  display: flex;
  justify-content: space-between;
  align-items: center;
  margin-bottom: 15px;
}
​
.section-header h2 {
  color: #cba376;
  margin: 0;
  font-size: 1.1em;
}
​
.rules-list {
  flex: 1;
  overflow-y: auto;
  padding-right: 10px;
}
​
.rule-item {
  background: #333;
  border-radius: 6px;
  padding: 12px;
  margin-bottom: 10px;
  border-left: 3px solid #cba376;
}
​
.rule-item.disabled {
  opacity: 0.5;
  border-left-color: #666;
}
​
.rule-header {
  display: flex;
  align-items: center;
  gap: 10px;
  margin-bottom: 8px;
}
​
.rule-enable {
  width: 16px;
  height: 16px;
  cursor: pointer;
}
​
.rule-id {
  color: #999;
  font-size: 0.85em;
}
​
.rule-remarks {
  flex: 1;
  color: #cba376;
  font-weight: 500;
}
​
.rule-body {
  display: flex;
  gap: 15px;
  flex-wrap: wrap;
}
​
.rule-field {
  display: flex;
  align-items: center;
  gap: 8px;
}
​
.rule-field label {
  color: #999;
  font-size: 0.9em;
}
​
.rule-field select,
.rule-field input {
  background: #222;
  border: 1px solid #444;
  border-radius: 4px;
  padding: 6px 10px;
  color: #cba376;
  font-size: 0.9em;
}
​
.rule-field input {
  width: 300px;
}
​
.btn-add, .btn-clear {
  background: #28a745;
  color: white;
  border: none;
  padding: 8px 16px;
  border-radius: 4px;
  cursor: pointer;
  font-size: 0.9em;
}
​
.btn-delete {
  background: transparent;
  color: #dc3545;
  border: none;
  cursor: pointer;
  font-size: 16px;
  padding: 4px 8px;
  border-radius: 4px;
}
​
.btn-delete:hover {
  background: rgba(220, 53, 69, 0.2);
}
​
.no-rules {
  text-align: center;
  color: #999;
  padding: 40px;
}
​
.global-toggle {
  display: flex;
  align-items: center;
  gap: 10px;
  margin-top: 15px;
  padding-top: 15px;
  border-top: 1px solid #444;
}
​
.toggle-switch {
  position: relative;
  width: 50px;
  height: 24px;
}
​
.toggle-switch input {
  opacity: 0;
  width: 0;
  height: 0;
}
​
.slider {
  position: absolute;
  cursor: pointer;
  top: 0;
  left: 0;
  right: 0;
  bottom: 0;
  background-color: #666;
  transition: 0.3s;
  border-radius: 24px;
}
​
.slider:before {
  position: absolute;
  content: "";
  height: 18px;
  width: 18px;
  left: 3px;
  bottom: 3px;
  background-color: white;
  transition: 0.3s;
  border-radius: 50%;
}
​
input:checked + .slider {
  background-color: #cba376;
}
​
input:checked + .slider:before {
  transform: translateX(26px);
}
​
/* 拦截日志区 */
.logs-section {
  flex: 1;
  background: #2a2a2a;
  border-radius: 8px;
  padding: 20px;
  display: flex;
  flex-direction: column;
  min-height: 0;
}
​
.logs-scroller {
  flex: 1;
  overflow: auto;
  overscroll-behavior: contain;
}
​
.logs-grid-row {
  display: grid;
  grid-template-columns: minmax(80px, 0.8fr) minmax(100px, 1fr) minmax(150px, 2fr) minmax(120px, 1.5fr) minmax(150px, 2fr) 1fr;
  color: #cba376;
  min-width: 800px;
}
​
.thead-row {
  background: #1a1a1a;
  border-bottom: 2px solid #cba376;
  position: sticky;
  top: 0;
  z-index: 10;
}
​
.th {
  padding: 10px;
  font-weight: 600;
  white-space: nowrap;
}
​
.td {
  padding: 8px 10px;
  border-bottom: 1px solid #3a3a3a;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
​
.message-cell {
  white-space: normal;
  word-break: break-all;
}
​
.data-row:hover .td {
  background: #333;
}
​
.no-data {
  text-align: center;
  color: #999;
  padding: 40px;
}
​
.badge {
  display: inline-block;
  padding: 3px 8px;
  border-radius: 4px;
  font-size: 0.8em;
  font-weight: 600;
}
​
.rule-type-badge {
  min-width: 80px;
  text-align: center;
}
​
.rule-suffix { background: rgba(40, 167, 69, 0.2); color: #28a745; }
.rule-keyword { background: rgba(255, 193, 7, 0.2); color: #ffc107; }
.rule-ip { background: rgba(0, 123, 255, 0.2); color: #007bff; }
​
.logs-scroller::-webkit-scrollbar {
  width: 8px;
}
​
.logs-scroller::-webkit-scrollbar-track {
  background: #1a1a1a;
}
​
.logs-scroller::-webkit-scrollbar-thumb {
  background: #444;
  border-radius: 4px;
}
</style>
文件：proxyui/src/router/index.js
新增路由（在 children 数组中添加）：

{
  path: 'security-policy',
  name: 'security-policy',
  component: () => import('../views/SecurityPolicy.vue'),
},
文件：proxyui/src/views/dashboard.vue
在导航菜单中添加安全策略菜单项（在路由配置菜单项之前添加）：

<RouterLink to="/dashboard/security-policy" class="nav-item" active-class="active">
  <!-- Icon for Security Policy -->
  <svg
    width="18"
    height="18"
    viewBox="0 0 24 24"
    fill="none"
    stroke="currentColor"
    stroke-width="2"
    stroke-linecap="round"
    stroke-linejoin="round"
  >
    <path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z"></path>
    <path d="M9 12l2 2 4-4"></path>
  </svg>
  <span>安全策略</span>
</RouterLink>
WebUI 实施文件清单
文件	修改类型	说明
mproxy/logs.go	[MODIFY]	新增 InterceptLogChan 和 InterceptLogMessage
mproxy/actions.go	[MODIFY]	拦截时发送日志到通道
proxysocket/hub.go	[MODIFY]	新增拦截日志推送器和订阅支持
proxyui/src/stores/websocket.js	[MODIFY]	添加拦截日志状态和订阅
proxyui/src/views/SecurityPolicy.vue	[NEW]	安全策略页面（规则配置 + 拦截日志）
proxyui/src/router/index.js	[MODIFY]	注册 security-policy 路由
proxyui/src/views/dashboard.vue	[MODIFY]	添加安全策略菜单项