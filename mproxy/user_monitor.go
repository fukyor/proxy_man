package mproxy

import (
	"net"
	"sync"
	"sync/atomic"
)

// UserHostStats 单个 IP+Host 组合的流量统计（原子操作，热路径无锁）
type UserHostStats struct {
	Up   atomic.Int64
	Down atomic.Int64
}

// UserStats 单个 IP 的全部 Host 流量
type UserStats struct {
	mu    sync.RWMutex
	Hosts map[string]*UserHostStats
}

// GlobalUserMonitor 全局用户流量管理器
type GlobalUserMonitor struct {
	mu    sync.RWMutex
	Users map[string]*UserStats
}

// 全局单例
var GlobalUserTraffic = &GlobalUserMonitor{
	Users: make(map[string]*UserStats),
}

// GetOrCreateStats 获取指定 IP+Host 的统计指针（双重检查锁定，仅在握手期调用一次）
func (m *GlobalUserMonitor) GetOrCreateStats(ip, host string) *UserHostStats {
	// 第一级：IP 查找
	m.mu.RLock()
	us, ok := m.Users[ip]
	m.mu.RUnlock()
	if !ok {
		m.mu.Lock()
		us, ok = m.Users[ip]
		if !ok {
			us = &UserStats{Hosts: make(map[string]*UserHostStats)}
			m.Users[ip] = us
		}
		m.mu.Unlock()
	}
	// 第二级：Host 查找
	us.mu.RLock()
	stats, ok := us.Hosts[host]
	us.mu.RUnlock()
	if !ok {
		us.mu.Lock()
		stats, ok = us.Hosts[host]
		if !ok {
			stats = &UserHostStats{}
			us.Hosts[host] = stats
		}
		us.mu.Unlock()
	}
	return stats
}

// Snapshot 生成全局快照（前端推送用，RLock 遍历）
func (m *GlobalUserMonitor) Snapshot(activeIPs map[string]bool) []UserSnapshotItem {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]UserSnapshotItem, 0, len(m.Users))
	for ip, us := range m.Users {
		us.mu.RLock()
		var totalUp, totalDown int64
		hosts := make([]HostTraffic, 0, len(us.Hosts))
		for host, stats := range us.Hosts {
			up := stats.Up.Load()
			down := stats.Down.Load()
			totalUp += up
			totalDown += down
			hosts = append(hosts, HostTraffic{Host: host, Up: up, Down: down})
		}
		us.mu.RUnlock()
		result = append(result, UserSnapshotItem{
			IP: ip, Online: activeIPs[ip], TotalUp: totalUp, TotalDown: totalDown, Hosts: hosts,
		})
	}
	return result
}

// CleanOfflineUsers 清理不受保护的离线 IP 流量记录，返回删除数量
func (m *GlobalUserMonitor) CleanOfflineUsers(protectedIPs map[string]bool) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	deleted := 0
	for ip := range m.Users {
		if !protectedIPs[ip] {
			delete(m.Users, ip)
			deleted++
		}
	}
	return deleted
}

// 快照序列化辅助结构
type UserSnapshotItem struct {
	IP        string        `json:"ip"`
	Online    bool          `json:"online"`
	TotalUp   int64         `json:"totalUp"`
	TotalDown int64         `json:"totalDown"`
	Hosts     []HostTraffic `json:"hosts"`
}

type HostTraffic struct {
	Host string `json:"host"`
	Up   int64  `json:"up"`
	Down int64  `json:"down"`
}

// ExtractIP 从 "ip:port" 格式的地址中提取纯 IP（断连场景用）
func ExtractIP(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}

// ExtractHost 从请求中提取纯 Host（去端口，用于统计 key）
func ExtractHost(reqHost string) string {
	if h, _, err := net.SplitHostPort(reqHost); err == nil {
		return h
	}
	return reqHost
}