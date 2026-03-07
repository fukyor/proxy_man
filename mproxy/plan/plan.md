# 添加详细连接页面单条连接关闭功能 - 修订计划

## 上下文

用户需求：在前端"详细连接"页面每个连接条目末尾添加"小红叉"按钮，点击后关闭指定连接。

**关键要求**：

1. 关闭父隧道时，同时关闭其所有子连接
2. 关闭子连接时，不影响父隧道
3. 关闭包括：断开底层连接（`OnClose()`）和清除连接记录

4. **墓碑机制保留**：系统正常关闭仍使用 2.5 秒墓碑期，但手动关闭时需立竿见影

**设计决策**：采用"即时墓碑"方案——在删除前先更新 `Status = "Closed"` 和 `EndTime`，然后立即 `Delete()`。这样既满足用户期望（连接立即消失），又保留了完整的状态标记用于审计和调试。

---

## 后端修改 (Go)

### 文件：`proxysocket/proxy.go`

#### 修改位置：`handleWebSocket` 函数的 switch 语句（约第 128-139 行）

**新增 `case "closeConnection"` 分支**：

```go
case "closeConnection":
    // 解析连接 ID
    idFloat, ok := msg["id"].(float64)
    if !ok {
        break
    }
    id := int64(idFloat)

    // 查找目标连接
    value, ok := hub.proxy.Connections.Load(id)
    if !ok {
        break
    }
    targetInfo := value.(*mproxy.ConnectionInfo)

    // 判断是否为父隧道（ParentSess == 0 表示父隧道）
    isParent := targetInfo.ParentSess == 0

    // 收集需要关闭的 ID 列表
    toClose := []int64{id}

    if isParent {
        // 父隧道：找出所有子连接
        hub.proxy.Connections.Range(func(key, value any) bool {
            info := value.(*mproxy.ConnectionInfo)
            if info.ParentSess == id && info.Session != id {
                toClose = append(toClose, info.Session)
            }
            return true
        })
    }

    // 关闭所有收集的连接
    for _, closeId := range toClose {
        if val, ok := hub.proxy.Connections.Load(closeId); ok {
            info := val.(*mproxy.ConnectionInfo)
            // 1. 调用 OnClose() 断开底层连接
            if info.OnClose != nil {
                info.OnClose()
            }
            // 2. 即时墓碑：更新状态和时间（用于日志/审计）
            info.Status = "Closed"
            info.EndTime = time.Now()
            // 3. 立即物理删除（用户主动关闭，无需等待墓碑期）
            hub.proxy.Connections.Delete(closeId)
        }
    }
```

**注意事项**：

- 需要在文件开头导入 `"time"` 包（如果没有）
- 父隧道判断：`ParentSess == 0`（而非计划中的 `ParentSess == id`）
- 子连接查找条件：`info.ParentSess == id && info.Session != id`

---

## 前端修改 (Vue)

### 文件：`src/stores/websocket.js`

#### 修改位置：return 语句（约第 316-335 行）

**1. 新增 `closeConnection` 方法**（在 `closeAllConnections` 函数后添加）：

```javascript
/**
 * 关闭指定连接
 * @param {number} id - 连接 ID
 */
function closeConnection(id) {
  if (socket.value?.readyState === WebSocket.OPEN) {
    socket.value.send(JSON.stringify({ action: 'closeConnection', id }))
  }
}
```

**2. 在 return 语句中导出**：

```javascript
return {
  socket,
  isConnected,
  subscriptions,
  trafficHistory,
  connections,
  logs,
  mitmExchanges,
  apiUrl,
  connect,
  disconnect,
  updateSubscriptions,
  closeAllConnections,
  closeConnection,        // 新增
  subscribeTraffic,
  subscribeConnections,
  subscribeLogs,
  clearLogs,
  subscribeMITM,
  clearMitmExchanges
}
```

---

### 文件：`src/views/Connections.vue`

#### 修改位置 1：CSS Grid 布局（约第 463 行）

**修改 `.conn-grid-row` 的 `grid-template-columns`**：

```css
.conn-grid-row {
  display: grid;
  /* 末尾新增 50px 操作列 */
  grid-template-columns: 100px 80px 200px 1fr 120px 80px 80px 50px;
  align-items: center;
  color: #cba376;
}
```

#### 修改位置 2：表头（约第 40-70 行）

**在 `.thead-row` 末尾新增表头格子**：

```html
<div class="conn-grid-row thead-row">
  <div @click="handleSort('id')" class="th sortable">
    <span class="expand-header-placeholder"></span>
    ID
    <span class="sort-icon" v-if="sortBy === 'id'">{{ sortOrder === 'asc' ? '▲' : '▼' }}</span>
  </div>
  <!-- 其他表头... -->
  <div class="th">操作</div>  <!-- 新增 -->
</div>
```

#### 修改位置 3：数据行（约第 92-126 行）

**在 `.data-row` 末尾新增关闭按钮**：

```html
<div
  class="conn-grid-row data-row"
  :class="{
    'parent-row': sortedConnections[virtualRow.index].isParent,
    'child-row': !sortedConnections[virtualRow.index].isParent
  }"
>
  <div class="td">
    <span
      v-if="sortedConnections[virtualRow.index].isParent && hasChildren(sortedConnections[virtualRow.index].id)"
      @click.stop="toggleExpand(sortedConnections[virtualRow.index].id)"
      class="expand-icon"
    >
      {{ expandedIds.has(sortedConnections[virtualRow.index].id) ? '▼' : '▶' }}
    </span>
    <span v-else class="expand-placeholder"></span>
    {{ sortedConnections[virtualRow.index].id }}
  </div>
  <!-- 其他单元格... -->
  <div class="td">  <!-- 新增操作列 -->
    <button
      @click.stop="handleCloseConnection(sortedConnections[virtualRow.index])"
      class="btn-close-single"
      title="关闭连接"
    >×</button>
  </div>
</div>
```

#### 修改位置 4：脚本逻辑（约第 297 行后）

**新增 `handleCloseConnection` 方法**：

```javascript
// 关闭单个连接
function handleCloseConnection(conn) {
  wsStore.closeConnection(conn.id)
}
```

#### 修改位置 5：样式（约第 630 行前）

**新增关闭按钮样式**：

```css
/* 单条关闭按钮 */
.btn-close-single {
  width: 28px;
  height: 28px;
  padding: 0;
  background: transparent;
  border: 1px solid #d9534f;
  color: #d9534f;
  border-radius: 4px;
  cursor: pointer;
  font-size: 18px;
  line-height: 1;
  transition: all 0.2s;
  display: inline-flex;
  align-items: center;
  justify-content: center;
}

.btn-close-single:hover {
  background: #d9534f;
  color: white;
}

.btn-close-single:active {
  transform: scale(0.95);
}
```

---

## 关键文件

| 文件                        | 修改类型                 |
| --------------------------- | ------------------------ |
| `proxysocket/proxy.go`      | 新增 case 分支           |
| `src/stores/websocket.js`   | 新增方法并导出           |
| `src/views/Connections.vue` | 新增列、按钮、方法和样式 |

---

## 验证方法

### 测试场景

1. **子连接关闭**：
   - 产生 HTTPS MITM 连接（`curl -x 127.0.0.1:8080 -k https://baidu.com`）
   - 展开父隧道，显示子连接
   - 点击子连接的红叉
   - **预期**：该子连接消失，父隧道保持活跃

2. **父隧道关闭**：
   - 产生 HTTPS MITM 连接
   - 点击父隧道的红叉
   - **预期**：父隧道及所有子连接立即消失

3. **底层断开验证**：
   - 关闭一个正在传输的连接（如大文件下载）
   - **预期**：终端显示连接被终止

4. **UI 即时性验证**：
   - 点击红叉后
   - **预期**：条目立即消失，无需等待 2.5 秒

### 后端日志检查

```bash
# 观察关闭操作是否正确执行
# 连接应立即从 hub.proxy.Connections 中移除
```

---

## 与原计划的主要变更

| 方面       | 原计划                                  | 修订计划                                                     |
| ---------- | --------------------------------------- | ------------------------------------------------------------ |
| 墓碑处理   | 直接 Delete，不更新状态                 | **即时墓碑**：先更新 `Status` 和 `EndTime`，再 Delete        |
| 为什么修改 | 原方案简单直接                          | 保留状态标记用于审计，代码逻辑更清晰，且对用户无影响（微秒级差异） |
| 父隧道判断 | `ParentSess == 0 \|\| ParentSess == id` | `ParentSess == 0`（简化，父隧道的 ParentSess 永远是 0）      |
| 子连接条件 | `ParentSess == id && Session != id`     | 保持不变                                                     |
| 前端 CSS   | 不完整                                  | 完整（表头+按钮+样式）                                       |
| 方法导出   | 缺失                                    | 已补充                                                       |

### 即时墓碑设计说明

```
用户点击红叉
    ↓
后端接收 closeConnection 消息
    ↓
调用 OnClose() 断开底层 TCP 连接
    ↓
更新 Status = "Closed"      ← 标记状态
更新 EndTime = time.Now()   ← 记录关闭时间（用于审计）
    ↓
立即 Delete() 从 map 删除   ← 连接立即从列表消失
```

**与系统正常关闭的区别**：

- **系统正常关闭**：标记 Closed → 等待 2.5 秒 → 自动清理
- **手动关闭**：标记 Closed → 立即清理（不等待）