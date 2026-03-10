WebUI & API (proxyui & proxysocket)
[MODIFY] proxysocket/proxy.go & proxysocket/hub.go
在 WebSocket hub 中增加 interceptLogs 频道或在每次拦截时广播。
在保存配置的 /api/config 接口中，支持 AccessRules 字段的读取和写入，保存后触发核心代码重载拦截规则。
[NEW] proxyui/src/views/SecurityPolicy.vue
创建 SecurityPolicy 界面。
上半部分：设计表单用于展示、增加、删除通过目标域名/IP进行拦截的规则，与 /api/config 进行双向绑定。
下半部分：使用 @tanstack/vue-virtual 像 
HistoryConnections.vue
 一样构建高性能虚拟列表（Virtual Scrolling List），监听 WebSocket 或调用接口动态展示拦截日志数据（拦截规则类型、源IP、目标、拦截时间等）。
[MODIFY] proxyui/src/router/index.js
在 /dashboard/security-policy 注册这一新页面。
[MODIFY] proxyui/src/views/dashboard.vue
在侧边导航栏 <nav class="nav-menu"> 中补充“安全策略”菜单项及其对应 SVG 图标并配置跳转向 /dashboard/security-policy。