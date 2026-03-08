# 代理循环问题解决计划（修订版）

## 上下文

### 问题描述

在云服务器（阿里云、腾讯云、AWS 等）上部署代理服务器时，公网 IP（如 `117.72.191.85`）通常绑定在 NAT 网关上，而非直接配置在本地网卡。本地网卡只有内网 IP（如 `172.16.0.8`）。

当客户端访问公网 IP 时，代理的 `isSelfLoop` 函数只能检测到本地网卡 IP，无法识别公网 IP 也是自环，导致：

1. 请求被转发到公网 IP
2. NAT 将请求回传到本机内网 IP
3. 代理再次接收并转发，形成**无限路由死循环**

### 修订说明

与原计划相比，本修订版进行了以下修正：

1. **移除 YAML 支持**：代码库仅使用 `config.json`，不存在 `config.yaml`
2. **修正字段标签**：移除不存在的 YAML tag，仅保留 JSON tag
3. **调整校验策略**：从"强制校验退出"改为"醒目警告但不退出"，允许首次启动时 `public_ips` 为空，用户可通过 Web UI 配置
4. **补充前端修改**：在 `AdvancedConfig.vue` 高级设置界面添加代理防环输入框，支持多 IP 配置，强制使用英文逗号分隔
5. **补充测试更新**：明确 `utils_loop_test.go` 的修改方式
6. **新增域名解析优化**：计划中的 DNS 解析逻辑需要增强以支持公网 IP 域名

---

## 实施计划

### 步骤 1：修改配置结构体

**文件**：`mproxy/config.go`

**位置**：第 28-43 行的 `ServerConfig` 结构体

**修改内容**：

```go
type ServerConfig struct {
    Port               int        `json:"Port"`
    Verbose            bool       `json:"Verbose"`
    KeepAcceptEncoding bool       `json:"KeepAcceptEncoding"`
    PreventParseHeader bool       `json:"PreventParseHeader"`
    KeepDestHeaders    bool       `json:"KeepDestHeaders"`
    ConnectMaintain    bool       `json:"ConnectMaintain"`
    MitmEnabled        bool       `json:"MitmEnabled"`
    HttpMitmNoTunnel   bool       `json:"HttpMitmNoTunnel"`
    // 新增字段：公网/外网 IP 列表（强制配置）
    PublicIPs          []string   `json:"public_ips"`

    // 路由相关配置
    RouteEnable bool        `json:"RouteEnable"`
    ProxyNodes  []ProxyNode `json:"ProxyNodes"`
    Routes      []RouteRule `json:"Routes"`
}
```

**注意**：仅保留 `json:"public_ips"` tag，与现有配置风格一致。

---

### 步骤 2：新增启动时醒目警告（不退出）

**文件**：`main.go`

**位置**：第 19 行 `cfg := cm.GetConfig()` 之后

**修改内容**：

```go
func main() {
    proxy := mproxy.NewCoreHttpSever()

    // 初始化配置管理器
    cm := mproxy.NewConfigManager("config.json")
    cfg := cm.GetConfig()

    // ===== 新增：public_ips 醒目警告 =====
    if len(cfg.PublicIPs) == 0 {
        log.Println("")
        log.Println("██████████████████████████████████████████████████████")
        log.Println("█⚠️  警告：未配置 public_ips (公网/外网 IP)                █")
        log.Println("██████████████████████████████████████████████████████")
        log.Println("█ 在云服务器上部署时，强烈建议配置公网 IP 以防止代理循环  █")
        log.Println("█ 您可以通过 Web UI 控制面板进行配置                     █")
        log.Println("█ 配置示例：\"public_ips\": [\"117.72.191.85\", \"gzyddyx.com\"] █")
        log.Println("██████████████████████████████████████████████████████")
        log.Println("")
    } else {
        log.Printf("✓ 已配置公网 IP 防护: %v", cfg.PublicIPs)
    }
    // ===== 新增结束 =====

    proxy.Config = cm
    // ... 后续代码
}
```

**设计说明**：

- 首次启动时 `public_ips` 为空数组 `[]`（Go 零值，无需显式初始化）
- 打印醒目的框线警告日志，但**不退出程序**
- 用户可通过 Web UI 控制面板后续配置 `public_ips`
- `DefaultConfig()` 中 `PublicIPs` 保持为零值空切片

---

### 步骤 3：修改 isSelfLoop 函数

**文件**：`mproxy/utils_loop.go`

**位置**：第 73-110 行

**修改内容**：

```go
// isSelfLoop 检查目标地址是否指向代理服务器自身
// addr 格式：host:port，例如 "117.72.191.85:8080"
// proxyPort: 代理服务器监听端口
// publicIPs: 配置的公网/外网 IP 列表
func isSelfLoop(addr string, proxyPort int, publicIPs []string) bool {
    host, portStr, err := net.SplitHostPort(addr)
    if err != nil {
        // 无法解析地址，不是自环
        return false
    }

    // 检查端口是否匹配
    if portStr != strconv.Itoa(proxyPort) {
        // 端口不匹配，不是自环
        return false
    }

    // ===== 新增：检查公网 IP 列表 =====
    for _, publicIP := range publicIPs {
        if host == publicIP {
            return true
        }
    }
    // ===== 新增结束 =====

    // 检查 IP 是否是本机 IP
    localIPList := getLocalIPs()
    for _, localIP := range localIPList {
        if host == localIP {
            return true
        }
    }

    // 检查是否是本机的域名（通过 DNS 解析）
    ips, err := net.LookupIP(host)
    if err == nil {
        for _, ip := range ips {
            // ===== 新增：检查解析后的 IP 是否在公网 IP 列表中 =====
            for _, publicIP := range publicIPs {
                if ip.String() == publicIP {
                    return true
                }
            }
            // ===== 新增结束 =====
            for _, localIP := range localIPList {
                if ip.String() == localIP {
                    return true
                }
            }
        }
    }

    return false
}
```

---

### 步骤 4：更新函数调用点

#### 4.1 修改 `mproxy/https.go`

**位置**：第 136 行

**原代码**：

```go
if isSelfLoop(addr, cfg.Port) {
```

**修改为**：

```go
if isSelfLoop(addr, cfg.Port, cfg.PublicIPs) {
```

#### 4.2 修改 `mproxy/router.go`（3 处）

**位置**：第 60、79、124 行

**原代码**：

```go
if isSelfLoop(addr, cfg.Port) {
```

**修改为**：

```go
if isSelfLoop(addr, cfg.Port, cfg.PublicIPs) {
```

---

### 步骤 5：更新测试文件

**文件**：`mproxy/utils_loop_test.go`

**位置**：第 25 行

**原代码**：

```go
result := isSelfLoop(tt.addr, proxyPort)
```

**修改为**：

```go
result := isSelfLoop(tt.addr, proxyPort, tt.publicIPs)
```

**同时需要**：在测试用例结构体中添加 `publicIPs []string` 字段，并更新所有测试用例。

---

### 步骤 6：更新配置文件示例

**文件**：`config.json`

**添加示例**：

```json
{
  "Port": 8080,
  "public_ips": ["117.72.191.85", "gzyddyx.com"],
  "Verbose": true,
  "KeepDestHeaders": true,
  "RouteEnable": false,
  "ProxyNodes": [],
  "Routes": []
}
```

---

### 步骤 7：前端高级设置界面添加代理防环输入框

**文件**：`E:\D\zuoyewenjian\MyProject\proxyui\src\views\AdvancedConfig.vue`

**位置**：在第 116 行 `</div>`（代理端口 section 结束）之后添加新的 section

**修改内容**：

```vue
<!-- ===== 新增：代理防环配置 ===== -->
<div class="section">
  <h3>代理防环</h3>
  <div class="public-ips-row">
    <span>公网 IP 列表</span>
    <input
      type="text"
      v-model="publicIPsText"
      class="input input-ips"
      placeholder="117.72.191.85, gzyddyx.com"
      @blur="validateAndParseIPs"
    />
    <span class="hint">多个 IP/域名用英文逗号分隔，云服务器部署时必须配置</span>
  </div>
  <div v-if="ipValidationError" class="alert alert-error" style="margin-top: 10px;">
    {{ ipValidationError }}
  </div>
  <div v-if="localConfig.PublicIPs && localConfig.PublicIPs.length > 0" class="ips-display">
    <span class="ips-label">已配置：</span>
    <span v-for="(ip, idx) in localConfig.PublicIPs" :key="idx" class="ip-tag">{{ ip }}</span>
  </div>
</div>
<!-- ===== 新增结束 ===== -->
```

**在 `<script setup>` 部分添加**：

```javascript
// 公网 IP 输入（逗号分隔的字符串）
const publicIPsText = ref('')
const ipValidationError = ref('')

// 将数组转换为逗号分隔字符串
const arrayToCommaString = (arr) => {
  if (!arr || !Array.isArray(arr) || arr.length === 0) return ''
  return arr.join(', ')
}

// 验证并解析 IP 输入
function validateAndParseIPs() {
  const text = publicIPsText.value.trim()
  ipValidationError.value = ''

  if (!text) {
    localConfig.value.PublicIPs = []
    return
  }

  // 检测分隔符：必须使用英文逗号
  if (text.includes('，') || text.includes('、')) {
    ipValidationError.value = '错误：必须使用英文逗号 (,) 分隔 IP 地址'
    // 恢复原有值
    publicIPsText.value = arrayToCommaString(localConfig.value.PublicIPs)
    return
  }

  // 分割并清理
  const parts = text.split(',').map(s => s.trim()).filter(s => s)

  if (parts.length === 0) {
    localConfig.value.PublicIPs = []
    return
  }

  // 基本验证：每个部分不能为空
  for (const part of parts) {
    if (!part) {
      ipValidationError.value = '错误：IP 地址不能为空'
      publicIPsText.value = arrayToCommaString(localConfig.value.PublicIPs)
      return
    }
  }

  // 保存解析后的 IP 列表
  localConfig.value.PublicIPs = parts
  publicIPsText.value = arrayToCommaString(parts)
}
```

**修改 `loadConfig` 函数**（第 51-60 行），添加：

```javascript
localConfig.value = {
  Port: cfg.Port ?? 8080,
  Verbose: cfg.Verbose ?? true,
  KeepAcceptEncoding: cfg.KeepAcceptEncoding ?? false,
  PreventParseHeader: cfg.PreventParseHeader ?? false,
  KeepDestHeaders: cfg.KeepDestHeaders ?? true,
  ConnectMaintain: cfg.ConnectMaintain ?? false,
  MitmEnabled: cfg.MitmEnabled ?? false,
  HttpMitmNoTunnel: cfg.HttpMitmNoTunnel ?? false,
  PublicIPs: cfg.PublicIPs ?? []  // 新增
}
// 新增：同步到文本框
publicIPsText.value = arrayToCommaString(localConfig.value.PublicIPs)
originalConfigStr.value = JSON.stringify(localConfig.value)
```

**在 `<style scoped>` 部分添加**：

```css
.public-ips-row {
  display: flex;
  align-items: center;
  gap: 12px;
  flex-wrap: wrap;
}

.input-ips {
  flex: 1;
  min-width: 300px;
}

.ips-display {
  margin-top: 12px;
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}

.ips-label {
  font-size: 0.85rem;
  color: #888;
}

.ip-tag {
  background: #0d4a65;
  color: #cba376;
  padding: 4px 10px;
  border-radius: 4px;
  font-size: 0.8rem;
  font-family: monospace;
}
```

---

## 关键文件清单

| 文件                                   | 修改类型             | 行号        |
| -------------------------------------- | -------------------- | ----------- |
| **后端文件**                           |                      |             |
| `mproxy/config.go`                     | 新增字段             | 28-43       |
| `main.go`                              | 新增警告逻辑         | 19 后       |
| `mproxy/utils_loop.go`                 | 修改函数签名和实现   | 73-110      |
| `mproxy/https.go`                      | 更新函数调用         | 136         |
| `mproxy/router.go`                     | 更新函数调用（3 处） | 60, 79, 124 |
| `mproxy/utils_loop_test.go`            | 更新测试用例         | 25          |
| `config.json`                          | 添加配置示例         | -           |
| **前端文件**                           |                      |             |
| `proxyui/src/views/AdvancedConfig.vue` | 新增代理防环输入框   | 116 后      |

---

## 验证方法

### 1. 编译验证

```bash
go build -o proxy_man.exe
```

### 2. 启动验证

**测试场景 A**：未配置 `public_ips`（首次启动）

- 预期结果：程序正常启动，输出醒目的框线警告日志

**测试场景 B**：已配置 `public_ips`

- 预期结果：程序正常启动，显示"✓ 已配置公网 IP 防护"

### 3. 前端验证

**测试场景 A**：输入有效 IP（英文逗号分隔）

- 输入：`117.72.191.85, gzyddyx.com`
- 预期结果：显示已配置的 IP 标签，保存成功

**测试场景 B**：输入中文逗号

- 输入：`117.72.191.85，gzyddyx.com`
- 预期结果：显示错误"必须使用英文逗号 (,) 分隔 IP 地址"

**测试场景 C**：输入空值

- 输入：清空输入框
- 预期结果：`PublicIPs` 为空数组，保存成功

### 4. 功能验证

**测试场景 A**：访问配置的公网 IP

```bash
curl http://117.72.191.85:8080/test
```

- 预期结果：返回 502 错误，日志显示 "proxy self-loop detected"

**测试场景 B**：访问公网 IP 的域名

```bash
curl http://gzyddyx.com:8080/test
```

- 预期结果：返回 502 错误（域名解析后匹配公网 IP）

**测试场景 C**：访问其他地址

```bash
curl http://example.com:8080/test
```

- 预期结果：正常转发

---

## 风险评估

| 风险项       | 风险等级 | 缓解措施                       |
| ------------ | -------- | ------------------------------ |
| 破坏现有功能 | 低       | 仅为参数扩展，不改变核心逻辑   |
| 用户升级影响 | 中       | 强制配置可能导致旧用户升级失败 |
| 性能影响     | 低       | 增加一次数组遍历，影响可忽略   |