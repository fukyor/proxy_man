两个需求新的需求（层级显示连接、概览仅显示活跃连接）
### 核心设计思路

目前的后端数据是“扁平化”的数组，通过 `parentId` 关联。为了实现 UI 上的父子折叠效果，前端需要在接收数据后进行**结构化转换**（Flat-to-Tree），而不应直接修改后端存储结构。

#### 1. 数据结构设计 (ViewModel Layer)

我们需要在前端将接收到的扁平 `connections` 数组转换为以 `id` 为索引的树形结构或映射结构。

**原始数据 (Flat):**

```json
[
  { "id": 1, "parentId": 0, ... },
  { "id": 2, "parentId": 1, ... }
]

```

**目标数据结构 (Tree/Grouped):**
建议为每个父节点对象扩展两个属性：

1. `children`: 数组，存放所有 `parentId` 等于该节点 `id` 的子连接。
2. `_expanded`: 布尔值，用于控制 UI 上的折叠/展开状态（UI 状态字段）。

```javascript
// 转换后的父节点对象示例
{
  "id": 1,
  "parentId": 0,
  "host": "baidu.com:443",
  "children": [
    {
      "id": 2,
      "parentId": 1,
      "host": "baidu.com:443",
      "protocol": "HTTPS-MITM",
      // ... 子节点数据
    }
  ],
  "_expanded": false, // UI控制字段
  // ... 其他原始字段
}

```

---

### 方案一：详细连接界面 (Connections.vue)

当前 `Connections.vue` 中的 `updateConnections` 函数有一行代码 `data.filter(conn => conn.parentId === 0)`，这直接丢弃了所有子连接。你需要修改这里的逻辑。

#### 1. 数据处理逻辑 (Process Logic)

不要在接收数据时直接过滤。建议在 `computed` 属性中执行“扁平转树形”的算法：

1. **建立索引**：创建一个 Map 或 Object，以 `id` 为键，存储所有连接对象的引用。
2. **构建树**：遍历原始数组。
* 如果 `parentId === 0`，将其视为**根节点**放入结果数组。
* 如果 `parentId !== 0`，在 Map 中找到对应的父节点，将当前节点 push 到父节点的 `children` 数组中。


3. **统计聚合**（可选）：父节点的 `up`/`down` 流量通常需要包含子节点的流量，或者仅显示父节点自身的隧道流量。根据业务逻辑，你可能需要累加子节点的流量到父节点用于排序。

#### 2. UI 渲染逻辑 (Render Strategy)

在 `<table class="connections-table">` 中，不能简单地使用一个 `v-for`。建议采用 **Fragment** 或 **Flattened View** 的方式渲染：

* **行结构设计**：
* **父行 (Parent Row)**：显示 `parentId: 0` 的连接。第一列添加“展开/收起”图标（由 `_expanded` 状态控制）。
* **子行 (Child Row)**：紧跟在父行之后。使用 `v-if="parent._expanded"` 控制渲染。
* **样式区分**：子行需要添加缩进（Indent）或不同的背景色，以体现层级关系。


* **排序处理**：
* 排序功能（`handleSort`）应当仅作用于**父节点列表**。
* 子节点在父节点内部通常按 `id` 或 `startTime` 排序即可，不应受全局排序影响打乱层级。



#### 3. 搜索处理

* 如果搜索匹配到了一个**子节点**，逻辑上应当自动显示其**父节点**，并自动将父节点设为 `expanded = true`，否则用户无法看到匹配结果。

---

### 方案二：概览界面 (Overview.vue)

当前 `Overview.vue` 直接接收全量数据。需求是仅显示 `status` 为 Active 的连接。

#### 1. 数据过滤逻辑 (Filter Logic)

在 `Overview.vue` 中，利用 Vue 的 `computed` 属性对 `wsStore.connections` 进行过滤。

* **过滤条件**：
根据你提供的 JSON，已关闭的连接状态为 `"status": "Closed"`。
过滤逻辑应为：`status !== 'Closed'` (或者根据你的定义，等于 `"Active"`, `"Open"`, `"Established"` 等明确的活跃状态)。

#### 2. 状态更新机制

* 由于 WebSocket 推送的是全量或增量更新，`wsStore` 中的数据可能是混合了历史记录（如果后端没清理）。
* **确保**：`Overview.vue` 的 `computed` 属性必须依赖 `wsStore.connections`。当 WebSocket 更新 Store 时，Overview 的列表会自动刷新，剔除变为 `Closed` 的连接。

---

### 总结：实施步骤规划

1. **修改 Store (`websocket.js`) 或 组件逻辑**：
* 保留原始的扁平化数据在 Store 中（保证数据源的单一事实来源）。
* 不要在 Store 层面做破坏性的过滤（除非数据量巨大需要性能优化）。


2. **重构 `Connections.vue**`：
* **废弃**：删除 `connections.value = data.filter(conn => conn.parentId === 0)` 这种直接丢弃数据的做法。
* **新增**：引入 `buildConnectionTree(flatData)` 函数。
* **状态**：引入一个 `expandedRowIds` 的 `Set` 结构来管理展开的行 ID（比修改数据对象更纯粹，避免响应式污染）。
* **模板**：修改 `<table>` 结构，支持通过 `expandedRowIds.has(conn.id)` 来显示/隐藏子行。


3. **重构 `Overview.vue**`：
* **新增**：创建一个 `activeConnections` 的计算属性：
`return connections.value.filter(c => c.status === 'Active')`（具体状态字符串需与后端对其）。
* **替换**：模板中的 `v-for` 遍历源改为这个新的计算属性。



通过这种设计，`Connections` 视图负责展示完整的调用链关系（HTTP over Tunnel），而 `Overview` 视图负责展示当前的实时负载情况，两者职责分离，数据源统一。