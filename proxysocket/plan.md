# ConnectHTTPMitm Expect: 100-continue 死锁修复计划

## 问题总结

### 死锁场景

当客户端使用 `Expect: 100-continue` 头上传大文件时，`req.Write()` 同步阻塞导致无法及时读取和转发 Target 的 `100 Continue` 响应。

### 根本原因

`mproxy/https.go:422-431` 的同步执行模式：

```go
// 1. req.Write() 同步发送整个请求（包括大文件 Body）
if err := req.Write(connRemoteSite); err != nil { ... }

// 2. 只有发送完成后才能读取响应
resp, err = func() (*http.Response, error) {
    defer req.Body.Close()
    return http.ReadResponse(remote_res, req)
}()
```

## 关键发现：req.Body.Close() 的阻塞风险

### Body 包装层次

```
原始 Body（来自客户端连接）
    ↓
reqBodyReader（流量统计层）  ← mproxy/https_traffic.go:26-46
    ↓
bodyCaptReader（MinIO 捕获层） ← myminio/minioUpload.go:24-31
    ↓
req.Body
```

### bodyCaptReader.Close() 的阻塞行为

`myminio/minioUpload.go:172-186`:

```go
func (r *bodyCaptReader) Close() error {
    if r.pipeWriter != nil {
        r.pipeWriter.Close()  // 通知上传协程数据结束
    }
    if r.doneCh != nil {
        <-r.doneCh  // ⚠️ 阻塞等待 MinIO 上传完成（最长30分钟！）
    }
    return r.inner.Close()
}
```

### req.Body.Close() 应该在什么时候调用？

**结论**：必须在 `req.Write()` 完成后调用，且应该在 goroutine 中调用，避免阻塞主线程。

**原因分析**：

1. `req.Write()` 完成后，Body 已被完全读取（EOF）
2. 必须调用 `Close()` 来关闭 `pipeWriter`，否则 MinIO 上传协程会永远等待数据
3. `Close()` 会阻塞等待 MinIO 上传完成（可能很长时间）
4. 如果在主线程调用 `Close()`，会阻塞响应处理

## 修复方案

### 修改文件

- `mproxy/https.go`：`ConnectHTTPMitm` case（第 422-436 行）

### 修改内容

**替换第 422-436 行为**：

```go
// ============= 修复 Expect: 100-continue 死锁 =============
// 创建错误通道（缓冲区防止 goroutine 泄漏）
writeErrCh := make(chan error, 1)

// 启动 goroutine 异步发送请求
go func() {
    err := req.Write(connRemoteSite)
    writeErrCh <- err
    close(writeErrCh)

    // 在 goroutine 中关闭 Body
    // 这会阻塞等待 MinIO 上传完成，但不影响主线程
    req.Body.Close()
}()

// 主线程立即开始读取响应，处理 1xx 中间状态
resp, err = func() (*http.Response, error) {
    for {
        respTmp, readErr := http.ReadResponse(remote_res, req)

        // 读取失败时，检查是否由写入错误导致
        if readErr != nil {
            select {
            case writeErr := <-writeErrCh:
                if writeErr != nil {
                    return nil, fmt.Errorf("读取响应失败: %v (写入错误: %v)", readErr, writeErr)
                }
            default:
            }
            return nil, readErr
        }

        // 处理 1xx 中间状态响应（如 100 Continue）
        if respTmp.StatusCode >= 100 && respTmp.StatusCode < 200 {
            // 构造状态行
            statusCodeStr := strconv.Itoa(respTmp.StatusCode) + " "
            text := strings.TrimPrefix(respTmp.Status, statusCodeStr)
            statusLine := "HTTP/1.1 " + statusCodeStr + text + "\r\n"

            // 转发给客户端
            if _, err := io.WriteString(connFromClinet, statusLine); err != nil {
                return nil, err
            }
            if err := respTmp.Header.Write(connFromClinet); err != nil {
                return nil, err
            }
            if _, err := io.WriteString(connFromClinet, "\r\n"); err != nil {
                return nil, err
            }

            // 清理临时响应的 Body
            if respTmp.Body != nil {
                io.Copy(io.Discard, respTmp.Body)
                respTmp.Body.Close()
            }

            // 继续读取最终响应
            continue
        }

        // 返回最终响应（>= 200）
        return respTmp, nil
    }
}()
// ============= 修复结束 =============
```

### 关于 req.Body.Close() 位置的说明

**为什么在 goroutine 内部调用 Close()**：

1. `req.Write()` 完成后，Body 数据已全部读取并发送
2. 需要调用 `Close()` 来关闭 MinIO 的 pipeWriter，让上传协程知道数据已结束
3. `Close()` 会阻塞等待 MinIO 上传完成（可能30分钟）
4. 在 goroutine 中调用可以避免阻塞主线程的响应处理

**不能在主线程调用的原因**：

- 如果在主线程调用 `Close()`，需要等待 MinIO 上传完成
- 这会延迟响应处理，与修复死锁的目标矛盾

**不能不调用的原因**：

- 如果不调用 `Close()`，MinIO 上传协程会永远等待更多数据
- 会导致 goroutine 泄漏和资源无法释放

## 流量统计影响

**无影响**：

- `reqBodyReader.Read()` 在 `req.Write()` 内部被调用时仍会正常统计
- 使用 `atomic.Int64` 进行全局计数，线程安全
- 统计发生在读取时，与 Close() 时机无关

## 验证步骤

1. **编译测试**

   ```bash
   go build -o proxy_man main.go
   ```

2. **功能测试 - 100 Continue 场景**

   ```bash
   # 创建测试文件
   dd if=/dev/zero of=test_100mb.bin bs=1M count=100
   
   # 使用 Curl 上传（会自动发送 Expect: 100-continue）
   curl -v --proxy http://localhost:8080 \
     -X POST \
     -H "Content-Type: application/octet-stream" \
     --data-binary @test_100mb.bin \
     http://httpbin.org/post
   ```

3. **验证日志输出**

   - 检查 100 Continue 是否被正确转发
   - 检查流量统计是否正确
   - 检查请求是否成功完成

4. **边界测试**

   - 小文件上传
   - 客户端中途断连
   - Target 拒绝请求（返回 417 Expectation Failed）

## 风险评估

| 风险                             | 级别 | 缓解措施                                  |
| -------------------------------- | ---- | ----------------------------------------- |
| goroutine 泄漏（MinIO 不可用时） | 中   | MinIO 有 30 分钟超时                      |
| 连接复用问题                     | 低   | `connRemoteSite` 在循环中复用，修改不影响 |
| 错误处理不完整                   | 低   | 使用 select 非阻塞检查写入错误            |

## 总结

1. **核心修复**：将 `req.Write()` 移到 goroutine 中，主线程立即读取响应
2. **1xx 处理**：循环读取响应，检测并转发 1xx 中间状态
3. **Body 关闭**：在 goroutine 中 `req.Write()` 完成后调用 `Close()`，避免阻塞主线程