## 函数设计规范

### CoreHttpServer 作为核心参数
`CoreHttpServer` 是本项目代理功能的核心结构体，承载了代理服务器的所有关键状态和配置。为了保持代码的一致性和可维护性，在设计函数时应遵循以下规范：

- **优先传递完整结构体**：当函数需要访问代理服务器的任何成员（配置、状态、连接池等）时，应直接传入 `proxy *CoreHttpServer` 作为参数，而不是传递零散的字段
- **统一参数风格**：保持函数签名的一致性，避免部分函数传结构体、部分函数传单个字段的混乱情况
- **便于扩展**：当 `CoreHttpServer` 新增字段时，已有函数无需修改签名即可访问新成员

**推荐写法**：
```go
func handleRequest(proxy *CoreHttpServer, req *http.Request) error {
    // 可以直接访问 proxy.Config, proxy.Router 等所有成员
}
```

**避免写法**：
```go
func handleRequest(config *Config, router *Router, req *http.Request) error {
    // 参数过多，且与 CoreHttpServer 耦合
}
```


