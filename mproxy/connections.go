package mproxy

import "time"

type ConnectionInfo struct {
	Session     int64     `json:"id"`
	Host        string    `json:"host"`
	Method      string    `json:"method"`
	URL         string    `json:"url"`
	RemoteAddr  string    `json:"remote"`
	Protocol    string    `json:"protocol"`  // HTTP / HTTPS-Tunnel / HTTPS-MITM
	StartTime   time.Time `json:"startTime"`
	UploadRef   *int64    `json:"-"`         // 引用流量计数器（用于读取实时值）
	DownloadRef *int64    `json:"-"`
	ParentID    *int64    `json:"parentId"` // 父连接ID
	OnClose     func()    `json:"-"`
}