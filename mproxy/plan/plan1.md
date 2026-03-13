  # MITM 拦截泄漏与误捕获修订计划

  ## 摘要

  修复目标分成两件事：

  1. 被本地拦截的 MITM 请求必须释放请求体包装链，避免 bodyCaptReader 挂死、MinIO 临时文件残留和 ContentLength=17 with
     Body length 0 一类上传异常。
  2. 被过滤器直接拦截的 MITM 请求不进入 Exchange 展示，不污染前端 MITM 列表。

  现有计划保留“调整请求 hook 顺序”这一项，但应删除“新增 IsCaptureSkipped()/修改 actions.go”和“对所有请求统一顶层 defer
  req.Body.Close()”这两项。

  ## 关键改动

  - 在 main.go 调整注册顺序为 AddRouter -> AddAccessControl -> AddTrafficMonitor，确保访问控制先于请求体流量封装执行。
  - 在 http.go 的 myHttpHandleWithEngine.processRequest 中，filterRequest 之后立即处理本地拦截分支：
      - 若 resp != nil，先调用 ctxt.SetCaptureSkip()。
      - 若 req != nil && req.Body != nil，只在这个本地拦截分支关闭 req.Body。
      - 保留现有放行分支内部的 defer req.Body.Close()，不要改成全局统一关闭。
  - 在 https.go 的 requestOk 中做同样处理：
      - filterRequest 返回本地 resp 时，标记 SetCaptureSkip()。
      - 仅在该分支关闭 req.Body，避免 MinIO 管道和临时文件泄漏。
  - 不修改 actions.go 与 mitm_exchange.go 的公开接口：
  - 不把 ConnectReject 作为这次修复主路径：
      - 若后续仍观察到“CONNECT 被展示”，再单独追查 UI 数据来源或日志通道，而不是在本次计划里混入无关改动。

  ## 测试与验证

  - 先清理 proxycore/myminio/tmp 历史残留文件，再启动代理。
  - 回归 HTTP 复现链路：
      - go test -bench=Benchmark_Stress_HTTP_Upload_Chunked -benchtime=3s -run=^$ -v -cpu 2,6,12
  - 补充 HTTPS MITM 对称验证：
      - go test -bench=Benchmark_Stress_HTTPS_Upload_Chunked -benchtime=3s -run=^$ -v -cpu 2,6,12
  - 补充行为验证：
      - 命中 AccessRules 的请求应返回 403，但不应出现在 MITM Exchange 列表。
      - 运行期间和结束后，proxycore/myminio/tmp 不应持续堆积新的 .tmp 文件。
      - 日志中不再持续出现拦截请求对应的 ContentLength=17 with Body length 0 上传异常。
  - 若仓库已有 goroutine / MinIO 相关测试基线，可再补一条针对“本地拦截 + chunked body”的单测或基准，直接验证
    req.Body.Close() 在拦截路径被调用。

  ## 关键假设

  - 当前复现主链路是 config.json 中 HttpMitmNoTunnel=true 的 HTTP-MITM 引擎，而不是 CONNECT 拒绝路径。
  - 访问控制拦截发生在请求 hook 阶段，AddTrafficMonitor 调整到后面即可避免大多数不必要的 MinIO 包装。
  - 允许被拦截请求不进入 MITM Exchange 展示，但拦截日志通道 InterceptLogChan 继续保留，作为独立观测面。