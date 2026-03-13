  # 一键清理离线用户方案修订版

  ## 摘要

  - 保持当前前端“在线状态”的展示逻辑不变：同一 IP 只要仍存在连接记录，包括墓碑期 Closed 连接，也继续显示为在线，以兼容
    短连接场景。
  - 新增一条独立的“可清理离线”判定口径，仅供后端执行清理时使用；该口径要求 IP 不存在任何连接记录，或至少不存在仍处于展
    示保活窗口内的连接记录。
  - 前端只增加顶部“一键清理离线”按钮，无二次确认；点击后仅做简单按钮态变化，不增加复杂提示。

  ## 审查结果

  ### 发现的问题

  - 高：原计划把“前端在线展示口径”和“后端安全清理口径”混为一谈。按你补充的要求，二者必须拆开。
  - 高：原计划建议只看 Active 判定在线，这会破坏当前为短连接设计的前端体验，与你的现有逻辑冲突。
  - 高：原计划仍把 WebSocket action 处理位置写偏了，实际应在 proxy.go 里处理，而不是只改 hub.go。
  - 中：当前 StartUserTrafficPusher() 在快照为空时不广播，这会导致清理完成后前端可能残留旧列表，必须修正。
  - 中：仓库中不存在“管理员用户”概念；本次功能应继续按现有 IP 维度处理，不引入权限系统。

  ### 优化建议

  - 明确区分两个集合：
      - displayOnlineIPs：给前端 online 字段使用，保持与当前逻辑一致，包含墓碑期连接。
      - cleanupProtectedIPs：给清理动作使用，用于保护“仍应视为在线/近期活跃”的 IP，不允许被删除。
  - 复用同一套连接扫描逻辑，但分别产出不同集合，避免后续维护时再出现语义混淆。
  - 清理后立即推送一次最新 user_traffic 快照，空数组也必须广播。

  ## 关键接口与行为变更

  - 新增 WebSocket action：
      - { "action": "cleanOfflineUsers" }
  - 后端新增清理方法：
      - func (m *GlobalUserMonitor) CleanOfflineUsers(protectedIPs map[string]bool) int
  - 不新增前端消息类型，仍复用：
      - { "type": "user_traffic", "data": [...] }

  ## 修订后的实施计划

  ### 1. 保留现有“在线展示”口径

  - 在 hub.go 保持当前用户监控页面的 online 语义与现状一致：
      - 只要某个 IP 在 Connections 中仍有记录，包括墓碑期 Closed 连接，就继续记入 displayOnlineIPs。
  - StartUserTrafficPusher() 继续把 displayOnlineIPs 传给 Snapshot()，保证短连接用户不会频繁闪成离线。

  ### 2. 新增“清理保护”口径

  - 在 proxysocket 层新增一个专用于清理的 IP 集合构建逻辑，命名上明确区分于展示口径，例如 buildCleanupProtectedIPs()。
  - 该集合的规则与“当前检查用户在线的逻辑保持一致”：
      - 只要该 IP 当前仍在 Connections 中出现，无论 Active 还是墓碑期 Closed，都视为受保护，不允许被清理。
  - 这样清理语义变为：
      - 仅清理那些已经完全不在当前连接表中的 IP 的流量统计记录。

  ### 3. 后端清理实现

  - 在 user_monitor.go 新增：
      - func (m *GlobalUserMonitor) CleanOfflineUsers(protectedIPs map[string]bool) int
  - 实现要求：
      - 持有 m.mu.Lock() 遍历 m.Users。
      - 对每个 IP，仅当 protectedIPs[ip] == false 时删除。
      - 返回删除数量，便于日志和调试。
  - 不增加管理员字段，不增加单 IP 清理，不修改 UserSnapshotItem 结构。

  ### 4. WebSocket 动作处理

  - 在 proxy.go 的 switch action 中新增 cleanOfflineUsers 分支。
  - 固定处理流程：
      1. 基于当前连接表构建 cleanupProtectedIPs。
      2. 调用 mproxy.GlobalUserTraffic.CleanOfflineUsers(cleanupProtectedIPs)。
      3. 再基于当前连接表构建 displayOnlineIPs。
      4. 调用 Snapshot(displayOnlineIPs)。
      5. 立即广播 user_traffic，即使结果为空也发送。
  - 不接收前端传入的 IP 列表，不做二次确认，不新增权限判断。

  ### 5. 前端按钮与交互

  - 在 UserMonitoring.vue 的 actions-bar 中，在搜索框右侧新增顶部按钮 一键清理离线。
  - 新增局部状态：
      - isCleaning
      - pendingCleanupRequest
  - 按钮行为：
      - 点击后直接调用 wsStore.cleanOfflineUsers()。
      - 点击瞬间切换按钮为 清理中...，同时禁用按钮，并应用简单的按压/变暗样式。
      - 当收到下一次 user_traffic 更新且 pendingCleanupRequest 为真时，恢复按钮状态。
  - 禁用条件：
      - WebSocket 未连接时禁用。
      - 当前 userTrafficList 为空时禁用。
  - 不弹 confirm，不做 toast，不显示额外结果栏。

  ### 6. Store 扩展

  - 在 websocket.js 新增：
      - cleanOfflineUsers()，发送 { action: 'cleanOfflineUsers' }
  - handleMessage('user_traffic') 保持全量替换 userTrafficList，供页面在收到刷新后结束按钮 loading。
  ## 测试与验收
  - 展示逻辑：
      - 短连接刚断开进入墓碑期时，该 IP 在用户监控页仍显示在线，不卡顿、不闪烁。
      - 墓碑期结束并且连接表中已无该 IP 后，后续快照中该 IP 才显示为离线或被清理掉。
  - 清理逻辑：
      - 点击“一键清理离线”后，只删除 GlobalUserTraffic 中那些已经不在当前连接表里的 IP。
      - 当前仍有 Active 或墓碑期 Closed 连接的 IP，绝不能被清理。
      - 清理后若列表为空，前端必须收到空数组并清空页面。
  - 前端交互：
      - 无确认框。
      - 点击后按钮进入 清理中...，下一次 user_traffic 刷新后恢复。
      - 快速连续点击时按钮禁用，避免重复发送。
  - 回归验证：
      - 原有 closeUserConnections(ip) 功能不受影响。
      - 用户监控的搜索、展开/收起、流量排序与统计栏逻辑不受影响。

  ## 假设与默认值

  - “检查是否离线”在本功能中按“当前连接表是否还存在该 IP”定义，而不是按严格 Active 连接定义。
  - “连接记录”仍指 GlobalUserTraffic.Users 中的 IP 流量统计记录，不涉及历史连接页面数据。
  - 不引入管理员、角色、权限或配置化白名单能力。
  - 按钮交互体验默认采用最小实现：loading 文案、禁用态、简单样式变化。
