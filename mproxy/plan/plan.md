# UI Freeze Fix Implementation Plan

## Goal Description

解决基准测试时前端 UI 卡死，以及测试结束后流量图表出现巨大畸形波峰并整体平移的问题。此外，顺带解决流量统计翻倍的问题。

## Root Cause Analysis (为何会发生这种现象？)

这是一个典型的前后端连锁堵塞问题，完整的时间线如下：

1. **后端产生极大数量级的并发（核心起因）**：在极短的压测时间（3-7秒）内，后端产生了数以万计的并发请求并全部完成。每一个完成的请求都会通过 [StartMitmDetailPusher](file:///e:/D/zuoyewenjian/MyProject/proxy_man/proxysocket/hub.go#198-211) 里的 `go h.broadcastToTopic("mitm_detail", msg)` 触发向前端独立发送一条包含完整信息的长 JSON 字符串。
2. **前端 JS 线程锁死**：每当有请求完成，后端通过 `mitm_detail` 主题推送详情给前端。前端 [websocket.js](file:///e:/D/zuoyewenjian/MyProject/proxy_man_ui/proxyui/src/stores/websocket.js) 中存在 `console.log(JSON.stringify(data, null, 2))`。在 7 秒的高频密集推送下，庞大 JSON 对象的序列化和控制台打印直接**榨干并锁死了浏览器的 JS 主线程**，导致图表绘制（Canvas 更新）暂停，产生了您看到的“卡死”现象。
3. **TCP 窗口填满反噬后端**：因为前端 JS 卡死，浏览器停止读取底层的 TCP WebSocket 数据包。这很快填满了操作系统的 TCP 接收缓冲区，进而填满了后端的 TCP 发送缓冲区。
4. **后端锁死与时钟跳跃**：当后端 TCP 缓冲区满后，[hub.go](file:///e:/D/zuoyewenjian/MyProject/proxy_man/proxysocket/hub.go) 中的 `conn.WriteMessage` 会陷入**同步阻塞**。这导致它一直霸占着该客户端的 `sub.writeMu` 互斥锁。此时，原本应该每 1 秒推送一次流量的 [StartTrafficPusher](file:///e:/D/zuoyewenjian/MyProject/proxy_man/proxysocket/hub.go#80-109) 因为等不到锁，也跟着被挡在了门外。且因为 Go 的 `time.Ticker` 在阻塞时会丢弃“滴答”，这段时间内的流量推送全部丢失。
5. **解除堵塞与波峰瞬移**：压测完成后，前端慢慢把积压的 `console.log` 跑完，主线程恢复正常，开始猛吸积压的 WebSocket 数据包。后端终于写完数据释放了锁。此时 [StartTrafficPusher](file:///e:/D/zuoyewenjian/MyProject/proxy_man/proxysocket/hub.go#80-109) 瞬间进场，计算出的 `deltaUp` 包含了**整个堵塞期间累积的总流量（几百 MB）**，并将其作为仅有的“一个数据点”推给了图表。这就形成了图表上那根细细长长、高达几十 MB/s 的畸形主峰。随着后续测试结束产生的一秒一个 `0 B/s` 的真实心跳，这根波峰自然就被慢慢往左推移，即您所说的“飘移到中间”。

## Proposed Changes

### 1. Frontend: 移除阻塞主线程的 Console 渲染

* 修改 [stores/websocket.js](file:///e:/D/zuoyewenjian/MyProject/proxy_man_ui/proxyui/src/stores/websocket.js)，完全移除或禁用针对 `mitm_exchange`、`traffic`、`connections` 的 `console.log(JSON.stringify(...))`，释放 UI 渲染能力。

### 2. Backend: 增加 WebSocket 写入超时（防御性编程）

* 修改 [proxysocket/hub.go](file:///e:/D/zuoyewenjian/MyProject/proxy_man/proxysocket/hub.go) 的 [sendTo](file:///e:/D/zuoyewenjian/MyProject/proxy_man/proxysocket/hub.go#34-55) 方法，在写入前增加 `conn.SetWriteDeadline(time.Now().Add(2 * time.Second))`。如果前端卡死，后端最多等待 2 秒就会丢弃该条消息丢出错误，从而保护后端其他 Pusher（如 TrafficPusher）不再被锁死，避免形成超级波峰。

## Verification Plan

1. **使用相同的压力测试命令重新运行**：

   ```bash
   go test -bench=Benchmark_Stress_HTTP_MITM_Upload_KnownSize -benchtime=3s -run=^$ -v -cpu 2
   ```

2. **监测前端表现**：观察压测进行时前端是否还会卡顿，图表是否能实时、平滑地绘制每一秒的波浪，而不是卡死后只出一个异常高的尖峰。