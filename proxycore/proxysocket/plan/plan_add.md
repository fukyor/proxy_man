需要微调/补充的关键细节 (给计划加点盐)
虽然大方向极佳，但在具体“微观落地”时，我建议在代码实施环节注意以下几点：

A. 路由更新的热重载 (Hot Reload) 线程安全问题
在 

plan.md
 的步骤 1.3 configHandler 中，有一段注释：

go
// 重新加载路由（如果启用）
if updated.RouteEnable {
    // 重新构建路由规则
    // 这需要将 Router 和 ConfigManager 关联起来
}
建议： 在 

mproxy/router.go
 中，当前的 Rules []RoutingRule 只有 

AddRule
 方法。这意味着如果不特殊处理，每次点“保存配置”，规则只会越加越多。 我们需要在 

router.go
 给 

Router
 增加一个清理方法如 ClearRules()，然后再将新路由遍历 

AddRule
 进去。 并且这一切发生时，必须持有 

Router
 的 mu sync.RWMutex 写锁，防止瞬间进来的请求匹配到一张空壳路由表！

B. getDefaultProxyNode 的实现逻辑
计划中提到：

go
target = getDefaultProxyNode(cfg)
if target == "" {
    target = "Direct"
}
建议： 既然我们在 

config.go
 已经加了 ProxyNodes 列表，这非常好。但为了健壮性，您可以在 ProxyNode 结构体里加一个 IsDefault 布尔字段：

go
type ProxyNode struct {
    Name      string `json:"name"`
    URL       string `json:"url"`
    Enable    bool   `json:"enable"`
    IsDefault bool   `json:"isDefault"` // 新增
}
这样不仅能知道哪个是默认代理（因为可能启用了 3 个上游代理节点做选择，但遇到写着 "Action": "Proxy" 的规则时，指定去那个默认的），还能在 UI 上提供一个单选块让用户设置“默认节点”。

C. “拦截跳转与 HTTP 取配置”的时序问题（前端）
在您发我的差异对比中，您修改了 router.beforeEach：

javascript
const isLoginPage = to.path === '/'
  if (!isLoginPage && !wsStore.isConnected) {
    next({ path: '/' })
但计划 2.3 提到页面加载时调用 fetchConfig()。这暗示了数据加载通过 HTTP。 建议前端架构： 既然我们有了 HTTP GET /api/config，不需要必须等 WebSocket 连上才能渲染页面状态。 您可以让 Pinia 在应用启动 (如 App.vue 的 setup 或 login 成功后) 就异步执行 fetchConfig()，然后各页面直接拿 config.value 渲染。这也是我们之前设计“控制流分离”的初衷——HTTP 给得起更快的首屏内容加载。