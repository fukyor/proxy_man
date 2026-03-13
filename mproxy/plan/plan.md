  # MITM 拦截泄漏与误捕获修订计划（最终建议）

  ## 摘要

  保留当前三项主改动方向：

  1. 调整 hook 注册顺序，让访问控制先于请求体监控。
  2. 在 HTTP-MITM 本地拦截分支补 SetCaptureSkip() 和 req.Body.Close()。
  3. 在 HTTPS-MITM 本地拦截分支做同样补偿。

  但要再补一项响应侧约束：已标记 skip 的本地拦截响应，不再进入 MinIO 响应体捕获。

  ## 修订后的实施计划

  - 修改 main.go：
      - 调整为 AddRouter -> AddAccessControl -> AddTrafficMonitor。
      - 目标仅限“请求阶段拦截不再先包裹 req.Body”。
  - 修改 http.go 的 myHttpHandleWithEngine.processRequest：
      - filterRequest 后若 resp != nil，立即 ctxt.SetCaptureSkip()。
      - 仅在该分支里关闭 req.Body。
      - 放行分支保留现有 RoundTrip 内部 defer req.Body.Close()。
  - 修改 https.go 的 requestOk：
      - filterRequest 返回本地响应时，同样执行 SetCaptureSkip()。
      - 仅在该分支关闭 req.Body。
      - 不改动放行分支的关闭语义。
  - 补充修改 actions.go 的响应监控逻辑：
      - 对已标记 skip 的上下文，保留必要的基础流量统计即可，但跳过 MinIO BuildBodyReader 和 Exchange 相关响应捕获。
      - 目的不是新增公开接口，而是确保“被拦截请求不进入 MITM 捕获链”落实到请求侧和响应侧，而不只是“不展示到前端列表”。

  - main.go
  - https.go
  - actions.go

  ## 验证方法

  - 运行：
      - go test -bench=Benchmark_Stress_HTTP_Upload_Chunked -benchtime=3s -run=^$ -v -cpu 2,6,12
      - go test -bench=Benchmark_Stress_HTTPS_Upload_Chunked -benchtime=3s -run=^$ -v -cpu 2,6,12
  - 验证命中 AccessRules 的请求：
      - 返回 403。
      - 不出现在 MITM Exchange 列表。
      - 不触发 MinIO 拦截响应体保存。
      - proxycore/myminio/tmp 不持续新增残留文件。
  - 增补一个自动化回归场景：
      - 构造命中访问控制的 chunked 上传请求。
      - 断言请求结束后无残留临时文件，且无持续上传异常日志。

  ## 假设

  - 本次修复聚焦你当前配置下的 MITM 引擎路径：MitmEnabled=true 且 HttpMitmNoTunnel=true。
  - CONNECT 阶段的 ConnectReject 不是这次压测复现主因，不纳入本轮核心修复。