package mproxy

import "time"

type ConnectionInfo struct {
	Session     int64     `json:"id"`
	ParentSess  int64     `json:"parentId"` // 父连接Session
	Host        string    `json:"host"`
	Method      string    `json:"method"`
	URL         string    `json:"url"`
	RemoteAddr  string    `json:"remote"`
	Protocol    string    `json:"protocol"`  // HTTP / HTTPS-Tunnel / HTTPS-MITM
	StartTime   time.Time `json:"startTime"`
	Status      string    `json:"status"`    // "Active" 或 "Closed"
	EndTime     time.Time `json:"endTime"`   // 连接关闭时间
	PuploadRef  *int64	  `json:"-"` 
	PdownloadRef *int64	  `json:"-"` 		// 用于读取父隧道实时值
	UploadRef   *int64    `json:"-"`         // 用于读取子连接实时值，这是活动连接流量实时更新的关键
	DownloadRef *int64    `json:"-"`
	OnClose     func()    `json:"-"`
}

// MarkClosed 标记连接为已关闭状态
func (c *ConnectionInfo) MarkClosed() {
	c.Status = "Closed"
	c.EndTime = time.Now()
}

// MarkConnectionClosed 标记指定会话的连接为已关闭状态（延迟删除）
func (proxy *CoreHttpServer) MarkConnectionClosed(session int64) {
	if value, ok := proxy.Connections.Load(session); ok {
		info := value.(*ConnectionInfo)
		info.MarkClosed()
	}
}