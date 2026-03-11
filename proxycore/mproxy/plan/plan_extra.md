2. 顶层隧道 (tunnelSession) 的关闭清理安全 在第三阶段合并后的代码中：

go
case ConnectHTTPMitm, ConnectMitm:
    // ...
    defer proxy.MarkConnectionClosed(tunnelSession)
    go func() {
        defer connFromClinet.Close()
defer proxy.MarkConnectionClosed(tunnelSession) 是放在外层函数的，这意味着 ConnectHTTPMitm 这个外层 switch 主线程执行完（启动了协程后），这句 defer 立刻就会被调用，导致在外壳看起来隧道是关闭的，但其实内部协程还在处理成百上千个复用的 HTTP 请求。

修正写法（在实施代码时）： 确保 defer proxy.MarkConnectionClosed(tunnelSession) 放在它所属的协程内部紧接着 connFromClinet.Close() 之后，才能保证该隧道生命周期和实际 TCP 的打通时间等长：

go
case ConnectHTTPMitm, ConnectMitm:
    _, _ = connFromClinet.Write([]byte("HTTP/1.0 200 OK\r\n\r\n"))
    
    go func() {
        defer connFromClinet.Close()
        defer proxy.MarkConnectionClosed(tunnelSession) // 移动到这里！保证长连接结束才清理
        // ... (读缓冲、抓握手、大循环)
    }()