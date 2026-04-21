package mproxy

import (
	"net"
	"strconv"
	"sync"
	"time"
)

var (
	localIPs           []string
	localIPsMutex      sync.RWMutex
	localIPsLastUpdate time.Time
)

// refreshLocalIPs 刷新本机所有网络接口的 IP 地址
func refreshLocalIPs() {
	localIPsMutex.Lock()
	defer localIPsMutex.Unlock()

	// 如果距离上次更新不到 5 分钟，跳过刷新
	if time.Since(localIPsLastUpdate) < 5*time.Minute && len(localIPs) > 0 {
		return
	}

	var ips []string
	interfaces, err := net.Interfaces()
	if err != nil {
		// 降级：至少包含回环地址
		ips = []string{"127.0.0.1", "::1", "localhost"}
	} else {
		for _, iface := range interfaces {
			addrs, err := iface.Addrs()
			if err != nil {
				continue
			}
			for _, addr := range addrs {
				var ip net.IP
				switch v := addr.(type) {
				case *net.IPNet:
					ip = v.IP
				case *net.IPAddr:
					ip = v.IP
				}
				if ip != nil {
					ips = append(ips, ip.String())
				}
			}
		}
		// 始终包含回环地址
		ips = append(ips, "127.0.0.1", "::1", "localhost")
	}

	localIPs = ips
	localIPsLastUpdate = time.Now()
}

// getLocalIPs 获取本机所有网络接口的 IP 地址（带缓存和自动刷新）
func getLocalIPs() []string {
	localIPsMutex.RLock()
	needRefresh := time.Since(localIPsLastUpdate) >= 5*time.Minute || len(localIPs) == 0
	localIPsMutex.RUnlock()

	if needRefresh {
		refreshLocalIPs()
	}

	localIPsMutex.RLock()
	defer localIPsMutex.RUnlock()
	return localIPs
}

// proxyListenPorts 从配置中提取当前代理监听端口集合。
func proxyListenPorts(cfg ServerConfig) []int {
	ports := make([]int, 0, 2)
	if cfg.Port > 0 {
		ports = append(ports, cfg.Port)
	}
	if cfg.HTTPSPort > 0 && cfg.HTTPSPort != cfg.Port {
		ports = append(ports, cfg.HTTPSPort)
	}
	return ports
}

// isSelfLoop 检查目标地址是否指向代理服务器自身
// addr 格式：host:port，例如 "117.72.191.85:8080"
// proxyPorts: 代理服务器监听端口集合
// publicIPs: 配置的公网/外网 IP 列表
func isSelfLoop(addr string, proxyPorts []int, publicIPs []string) bool {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		// 无法解析地址，不是自环
		return false
	}

	// 检查端口是否命中任一监听端口
	portMatched := false
	for _, proxyPort := range proxyPorts {
		if portStr == strconv.Itoa(proxyPort) {
			portMatched = true
			break
		}
	}
	if !portMatched {
		// 端口不匹配，不是自环
		return false
	}

	// ===== 新增：检查公网 IP 列表 =====
	for _, publicIP := range publicIPs {
		if host == publicIP {
			return true
		}
	}
	// ===== 新增结束 =====

	// 检查 IP 是否是本机 IP
	localIPList := getLocalIPs()
	for _, localIP := range localIPList {
		if host == localIP {
			return true
		}
	}

	// 检查是否是本机的域名（通过 DNS 解析）
	ips, err := net.LookupIP(host)
	if err == nil {
		for _, ip := range ips {
			// ===== 新增：检查解析后的 IP 是否在公网 IP 列表中 =====
			for _, publicIP := range publicIPs {
				if ip.String() == publicIP {
					return true
				}
			}
			// ===== 新增结束 =====
			for _, localIP := range localIPList {
				if ip.String() == localIP {
					return true
				}
			}
		}
	}

	return false
}
