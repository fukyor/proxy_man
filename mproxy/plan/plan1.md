# MinIO 配置合并至 config.json 实施方案

## Context

当前 MinIO 配置通过独立的 `minio.json` 文件管理（路径写死为 `filepath.Join(GetExecutableDir(), "myminio", "minio.json")`）。编译后部署到云服务器时，可执行文件同级目录下不存在 `myminio/minio.json`，导致 MinIO 永远无法正确配置。

本方案将 MinIO 配置合并到主配置 `config.json` 中（由 `ConfigManager` 统一管理），并在前端高级设置页面新增 MinIO 配置 UI，实现通过 Web UI 热重载 MinIO 客户端。

---

## 数据结构设计

### ServerConfig 新增字段

在 `proxycore/mproxy/config.go` 的 `ServerConfig` 结构体末尾新增：

```go
MinioConfig myminio.Config `json:"MinioConfig"`
```

直接复用现有 `myminio.Config` 结构体（7 个字段：endpoint, publicEndpoint, accessKeyId, secretAccessKey, useSSL, bucket, enabled），无需新增类型。

### config.json 最终结构

```json
{
  "Port": 8080,
  "Verbose": true,
  "...": "...(现有字段不变)...",
  "MinioConfig": {
    "endpoint": "127.0.0.1:9000",
    "publicEndpoint": "",
    "accessKeyId": "root",
    "secretAccessKey": "12345678",
    "useSSL": false,
    "bucket": "bodydata",
    "enabled": true
  }
}
```

### CoreHttpServer 结构体变更

删除 `MinioConfig *myminio.MinioConfigManager` 字段（第 38 行），保留 `MinioClient *myminio.Client`。MinIO 配置统一从 `proxy.Config.GetConfig().MinioConfig` 读取。

### NewClient 签名变更

```go
// 改前
func NewClient(cm *MinioConfigManager) (*Client, error)
// 改后
func NewClient(cfg Config) (*Client, error)
```

直接接收 `Config` 值类型，不再依赖 `MinioConfigManager`。

---

## 实施步骤

### 步骤 1：改造 myminio/config.go

**文件**：`proxycore/myminio/config.go`

- **保留**：`Config` 结构体、`Client` 结构体、`DefaultMinioConfig()` 函数
- **删除**：`MinioConfigManager` 结构体及其所有方法（`NewMinioConfigManager`, `Load`, `Save`, `saveLocked`, `GetConfig`）— 约第 36-114 行
- **改造 `NewClient`**：签名从 `NewClient(cm *MinioConfigManager)` 改为 `NewClient(cfg Config)`
  - 内部直接使用 `cfg` 参数，移除 `cm.GetConfig()` 调用
  - 移除强制开启逻辑（第 119-126 行的 `if !cfg.Enabled` 块），改为：`Enabled == false` 时返回 `nil, nil`（不创建客户端，由调用方处理）
  - 将日志 "请在 minio.json 中补充填写此项" 改为 "请在 Web UI 高级设置中配置 MinIO PublicEndpoint"
- **删除多余 import**：移除 `os`, `path/filepath`, `sync`（仅 `MinioConfigManager` 使用）

### 步骤 2：改造 mproxy/config.go

**文件**：`proxycore/mproxy/config.go`

- `ServerConfig` 结构体末尾新增 `MinioConfig myminio.Config` 字段
- `DefaultConfig()` 中补充默认值：`MinioConfig: *myminio.DefaultMinioConfig()`
- 确保文件头 import 包含 `"proxy_man/myminio"`

### 步骤 3：改造 mproxy/core_proxy.go

**文件**：`proxycore/mproxy/core_proxy.go`

- **CoreHttpServer 结构体**：删除 `MinioConfig *myminio.MinioConfigManager` 字段（第 38 行）
- **改造 `InitMinio` 函数**（第 91-106 行）：
  - 从 `proxy.Config.GetConfig().MinioConfig` 读取配置
  - 调用 `myminio.NewClient(minioCfg)` 创建客户端
  - `Enabled == false` 时：记录日志，`proxy.MinioClient` 保持 `nil`，**不 Fatal**
  - 连接失败时：`log.Printf` 替代 `log.Fatalf`，`proxy.MinioClient` 保持 `nil`（下游代码已有 `!= nil` 检查）
- **新增 `ReloadMinioClient` 方法**（挂在 `*CoreHttpServer` 上）：
  - 读取 `proxy.Config.GetConfig().MinioConfig`
  - 调用 `myminio.NewClient(cfg)` 创建新客户端
  - 成功：替换 `proxy.MinioClient`（指针赋值，正在使用旧客户端的 goroutine 不受影响）
  - 失败：记录错误日志，保持旧客户端不变，返回 `error`
- 删除不再需要的 `"path/filepath"` import（如果文件中只有 `InitMinio` 使用了它，需确认 `GetExecutableDir` 是否还需要 — 经确认 `GetExecutableDir` 定义在 `config.go` 中，`core_proxy.go` 中不再需要 `filepath`）

### 步骤 4：改造 proxysocket/proxy.go

**文件**：`proxycore/proxysocket/proxy.go`

- `handleConfig()` 的 POST 分支（第 233 行之后），在访问控制热重载之后追加：

  ```
  MinIO 热重载调用 → ws.Proxy.ReloadMinioClient()
  ```

- 第 83-84 行的 `MinioClient != nil` 检查和 `HandleDownload` 调用保持不变

### 步骤 5：改造前端 AdvancedConfig.vue

**文件**：`proxyui/src/views/AdvancedConfig.vue`

- **localConfig 扩展**：新增 `MinioConfig` 嵌套对象（7 个字段，含默认值）
- **loadConfig() 初始化**：用 `?.` 可选链 + `??` 空值合并处理旧 config.json 没有 MinioConfig 字段的情况
- **模板新增 "MinIO 对象存储" section**：插入在"代理防环"和"代理行为"之间
  - Enabled 开关 + 红色警告（"禁用存储将导致 Body 无法持久化"）
  - Endpoint 文本输入
  - PublicEndpoint 文本输入
  - AccessKey 文本输入
  - SecretKey 密码输入（`type="password"`）
  - Bucket 文本输入
  - UseSSL 开关 + 红色警告（"如无特殊需求请勿开启"）
- **saveConfig() 无需改动**：现有的 `{ ...wsStore.config, ...localConfig }` 合并逻辑会自然携带 MinioConfig

### 步骤 6：修复测试文件

**文件**：`proxycore/myminio/mino_upload_test.go`

- 第 18 行 `cm := NewMinioConfigManager("minio_test.json")` 改为直接构造 `Config{}` 值并调用 `NewClient(cfg)`

其他测试文件（`minioStore_test.go`, `goroutine_test.go`）直接构造 `myminio.Client{}` 字面量，不依赖 `MinioConfigManager`，无需修改。

---

## 热重载调用链路

```
用户点击"保存配置" (前端 AdvancedConfig.vue)
  → POST /api/config (携带完整 ServerConfig，含 MinioConfig)
  → proxysocket/proxy.go handleConfig()
    → ConfigManager.UpdateConfig() — 写入内存 + 持久化到 config.json
    → Router.ReloadFromConfig() — 路由热重载（已有）
    → AccessControl.ReloadFromConfig() — 访问控制热重载（已有）
    → proxy.ReloadMinioClient() — 【新增】MinIO 客户端热重载
      → proxy.Config.GetConfig().MinioConfig — 读最新配置
      → myminio.NewClient(cfg) — 创建新客户端
      → 成功: proxy.MinioClient = newClient（指针替换）
      → 失败: log.Printf, 保持旧客户端
```

---

## 兼容性处理

| 场景                               | 处理方式                                                     |
| ---------------------------------- | ------------------------------------------------------------ |
| 旧 config.json 无 MinioConfig 字段 | `ConfigManager.Load()` 先调 `DefaultConfig()` 初始化再 `json.Unmarshal` 覆盖，缺失字段保留默认值 |
| MinIO 连接失败                     | `log.Printf` 替代 `log.Fatalf`，`MinioClient` 为 nil，下游已有 nil 检查 |
| 热重载期间并发请求                 | 旧 goroutine 持有旧 Client 引用不受影响，GC 自动回收         |
| minio.json 文件                    | 不再被代码引用，保留在原位不影响运行，无需迁移               |

---

## 关键文件清单

| 文件                                    | 操作                                                         |
| --------------------------------------- | ------------------------------------------------------------ |
| `proxycore/myminio/config.go`           | 删除 MinioConfigManager，改造 NewClient 签名                 |
| `proxycore/mproxy/config.go`            | ServerConfig 新增 MinioConfig 字段，DefaultConfig 补默认值   |
| `proxycore/mproxy/core_proxy.go`        | 删除 MinioConfig 字段，改造 InitMinio，新增 ReloadMinioClient |
| `proxycore/proxysocket/proxy.go`        | handleConfig POST 追加 MinIO 热重载                          |
| `proxyui/src/views/AdvancedConfig.vue`  | 新增 MinIO 配置 UI 区块                                      |
| `proxycore/myminio/mino_upload_test.go` | 修复测试编译错误                                             |

---

## 验证方法

1. **编译验证**：`go build` 通过，无编译错误
2. **启动验证**：
   - 首次启动（无旧 config.json）：自动生成包含 MinioConfig 的 config.json
   - 旧 config.json 启动：自动填充 MinioConfig 默认值，MinIO 正常初始化
3. **前端验证**：打开"高级设置"页面，确认 MinIO 存储设置区块出现，字段正确绑定
4. **热重载验证**：修改 Endpoint 为错误地址后保存，观察日志输出连接失败；改回正确地址保存，确认恢复
5. **云部署验证**：编译后仅部署可执行文件，启动后通过 Web UI 配置 MinIO，确认功能正常