package mproxy

import (
	"crypto/tls"
	"os"
	"proxy_man/signer"
	"sort"
	"strings"
)

// BuildProxyListenerTLSConfig 为 HTTPS 代理监听口生成单张多 SAN 证书。
func BuildProxyListenerTLSConfig(cfg ServerConfig) (*tls.Config, error) {
	hostSet := map[string]struct{}{
		"127.0.0.1": {},
		"localhost": {},
		"::1":       {},
	}

	if hostname, err := os.Hostname(); err == nil {
		hostname = strings.TrimSpace(hostname)
		if hostname != "" {
			hostSet[hostname] = struct{}{}
		}
	}

	for _, host := range cfg.PublicIPs {
		host = strings.TrimSpace(host)
		if host == "" {
			continue
		}
		hostSet[host] = struct{}{}
	}

	hosts := make([]string, 0, len(hostSet))
	for host := range hostSet {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)

	cert, err := signer.SignHost(Proxy_ManCa, hosts)
	if err != nil {
		return nil, err
	}

	return &tls.Config{
		Certificates: []tls.Certificate{*cert},
		NextProtos:   []string{"http/1.1"},
	}, nil
}
