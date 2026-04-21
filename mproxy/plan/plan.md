# 用户拦截功能实现方案

提供一个新的用户级别的拦截机制，允许代理管理员通过精确匹配用户的源 IP 地址，阻断这些特定用户的访问请求。

## Proposed Changes

### 后端核心 (Backend Proxy Core)

#### [MODIFY] config.go (e:/D/zuoyewenjian/MyProject/proxy_man/proxycore/mproxy/config.go)
- 新增 `UserBlockRule` 结构体：
```go
type UserBlockRule struct {
	Id      int    `json:"Id"`      // 前端生成的唯一 ID
	Value   string `json:"Value"`   // 被拦截用户的 IP（支持逗号分隔多个 IP）
	Enable  bool   `json:"Enable"`  // 该条规则的独立开关
	Remarks string `json:"Remarks"` // 备注
}
```
- 在 `ServerConfig` 中增加 `UserBlockRules []UserBlockRule` 字段。
- 在 `DefaultConfig()` 初始化时附带空的 `[]UserBlockRule{}` 以保证向后兼容。

#### [MODIFY] actions.go (e:/D/zuoyewenjian/MyProject/proxy_man/proxycore/mproxy/actions.go)
- 在 `AccessController` 结构体中增加成员 `blockedClients map[string]bool`。
- 在 `ReloadFromConfig()` 方法中，遍历 `cfg.UserBlockRules` 并解析其中合法的 IP 地址（通过 `,` 分割处理），更新至 `newBlockedClients`，在 `ac.mu.Lock()` 区域完成映射的原子替换。
- 修改 HTTP `HookOnReq` 与 HTTPS CONNECT `DoConnectFunc` 拦截逻辑：
  1. 通过 `getClientIP` 获取来访者的真实 IP。
  2. 在 `RLock()` 区域以 O(1) 性能检查 `ac.blockedClients[clientIP]` 是否存在。
  3. 如果用户在拦截黑名单内，则直接抛弃（拦截）此请求并记录拦截日志，不再进行后续域名规则的匹配判断。日志规则类型可设定为 `UserIP`。

---

### 前端 UI 控制台 (Frontend Proxy UI)

#### [MODIFY] AccessControl.vue (e:/D/zuoyewenjian/MyProject/proxy_man/proxyui/src/views/AccessControl.vue)
-   **脚本区域重构**：
    1.  `localConfig.value` 和 `saveConfig()` 中的合并保存逻辑，加入 `UserBlockRules: []`。
    2.  增加关联的响应式变量 `newUserBlockRule = ref({ Value: '', Enable: true, Remarks: '' })` 以及计算属性 `nextUserBlockId`。
    3.  提供 `addUserBlock()`, `removeUserBlock(index)` 方法管理用户拦截配置项。
-   **模板区域增加全新 Div**：
    在原本的 “拦截规则 (AccessRules)” `<div class="section">` 下面，增加一个专门用于 “用户拦截” 的 `<div class="section">`，其中包含：
    1.  表格结构，展示已添加的用户 IP、独立启用开关与备注。
    2.  添加行，提供简单的 IP （值）与请求备注的输入框。

## Open Questions

无。

## Verification Plan

### Automated Tests
无。

### Manual Verification
1.  启动代理并编译前端环境。
2.  进入 “访问控制” 页面，应该可以看到在原有 “拦截规则” 下新增的 “用户拦截” 独立区块。
3.  添加本机的回环 IP (127.0.0.1) 到用户拦截列表中并启用配置。
4.  保存配置，通过代理发起 HTTP/HTTPS 请求。
5.  在界面和控制台中检查该请求是否被以 HTTP 状态 403 成功拦截。
6.  将被拦截日志输出，检查日志显示的规则触发是否正确，目标识别是否为 `UserIP`。
