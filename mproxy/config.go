package mproxy

import (
	"encoding/json"
	"os"
	"sync"
)

// ProxyNode 代理节点配置（配置了即启用）
type ProxyNode struct {
	Name string `json:"Name"` // 节点名称，如 "clash"
	URL  string `json:"URL"`  // 代理地址，如 "http://127.0.0.1:7892"
}

// RouteRule 路由规则接口定义
type RouteRule struct {
	Id      int    `json:"Id"`      // 前端生成的唯一 ID
	Type    string `json:"Type"`    // "DomainSuffix" | "DomainKeyword" | "IP"
	Value   string `json:"Value"`   // "twitter.com" 等值
	Action  string `json:"Action"`  // 直接填写拨号器名称，如 "clash" 或 "Direct"
	Enable  bool   `json:"Enable"`  // 该条规则的独立开关
	Remarks string `json:"Remarks"` // 用户备注
}

// ServerConfig 全局代理服务器配置接口定义
type ServerConfig struct {
	Port               int  `json:"Port"` // 代理监听端口
	Verbose            bool `json:"Verbose"`
	KeepAcceptEncoding bool `json:"KeepAcceptEncoding"`
	PreventParseHeader bool `json:"PreventParseHeader"`
	KeepDestHeaders    bool `json:"KeepDestHeaders"`
	ConnectMaintain    bool `json:"ConnectMaintain"`
	MitmEnabled        bool `json:"MitmEnabled"`
	HttpMitmNoTunnel   bool `json:"HttpMitmNoTunnel"`

	// 路由相关配置
	RouteEnable bool        `json:"RouteEnable"`
	ProxyNodes  []ProxyNode `json:"ProxyNodes"` // 代理节点列表
	Routes      []RouteRule `json:"Routes"`
}

// ConfigManager 负责配置的线程安全读写及文件持久化
type ConfigManager struct {
	FilePath string
	Current  *ServerConfig
	mu       sync.RWMutex
}

// NewConfigManager 初始化配置管理器。如果配置文件不存在，则创建默认配置并写入
func NewConfigManager(filePath string) *ConfigManager {
	cm := &ConfigManager{
		FilePath: filePath,
		Current:  DefaultConfig(),
	}
	cm.Load() // 尝试从磁盘加载
	return cm
}

// DefaultConfig 提供一套开箱即用的默认配置
func DefaultConfig() *ServerConfig {
	return &ServerConfig{
		Port:               8080,
		Verbose:            true,
		KeepAcceptEncoding: false,
		PreventParseHeader: false,
		KeepDestHeaders:    true,
		ConnectMaintain:    false,
		MitmEnabled:        false,
		HttpMitmNoTunnel:   false,
		RouteEnable:        false,
		ProxyNodes:         []ProxyNode{},
		Routes:             []RouteRule{},
	}
}

// Load 从本地磁盘读取 JSON 配置文件，反序列化合并到内存
func (cm *ConfigManager) Load() error {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	data, err := os.ReadFile(cm.FilePath)
	if err != nil {
		if os.IsNotExist(err) {
			// 文件不存在时，使用当前默认配置立即新建并写入一份
			return cm.saveLocked()
		}
		return err
	}

	// 将文件内容覆盖到当前配置
	if err := json.Unmarshal(data, cm.Current); err != nil {
		return err
	}
	return nil
}

// Save 将当前内存配置持久化写入磁盘。写入时使用临时文件原子重命名以防损坏。
func (cm *ConfigManager) Save() error {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.saveLocked()
}

// saveLocked 是内部写磁盘的实际逻辑，调用方需确保已持有锁(至少RLock，因为是读内存写磁盘)
func (cm *ConfigManager) saveLocked() error {
	data, err := json.MarshalIndent(cm.Current, "", "  ")
	if err != nil {
		return err
	}

	tmpFile := cm.FilePath + ".tmp"
	if err := os.WriteFile(tmpFile, data, 0644); err != nil {
		return err
	}

	// 原子性重命名
	return os.Rename(tmpFile, cm.FilePath)
}

// UpdateConfig 用结构体覆盖当前配置并持久化
func (cm *ConfigManager) UpdateConfig(cfg *ServerConfig) error {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.Current = cfg
	return cm.saveLocked()
}

// GetConfig 线程安全获取配置副本
func (cm *ConfigManager) GetConfig() ServerConfig {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return *cm.Current
}
