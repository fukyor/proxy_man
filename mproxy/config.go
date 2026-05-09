package mproxy

import (
	"encoding/json"
	"log"
	"os"
	"proxy_man/myminio"
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

// AccessRule 访问控制规则配置
type AccessRule struct {
	Id      int    `json:"Id"`      // 前端生成的唯一 ID
	Type    string `json:"Type"`    // "DomainSuffix" | "DomainKeyword" | "IP"
	Value   string `json:"Value"`   // "twitter.com" 等值，逗号分隔支持多个
	Enable  bool   `json:"Enable"`  // 该条规则的独立开关
	Remarks string `json:"Remarks"` // 用户备注
}

// UserBlockRule 来源 IP 拦截规则配置
type UserBlockRule struct {
	Id      int    `json:"Id"`      // 前端生成的唯一 ID
	Value   string `json:"Value"`   // 被拦截来源 IP，逗号分隔支持多个
	Enable  bool   `json:"Enable"`  // 该条规则的独立开关
	Remarks string `json:"Remarks"` // 用户备注
}

// ServerConfig 全局代理服务器配置接口定义
type ServerConfig struct {
	Port               int  `json:"Port"`      // HTTP 代理监听端口
	HTTPSPort          int  `json:"HTTPSPort"` // HTTPS 代理监听端口
	Verbose            bool `json:"Verbose"`
	KeepAcceptEncoding bool `json:"KeepAcceptEncoding"`
	PreventParseHeader bool `json:"PreventParseHeader"`
	KeepDestHeaders    bool `json:"KeepDestHeaders"`
	ConnectMaintain    bool `json:"ConnectMaintain"`
	MitmEnabled        bool `json:"MitmEnabled"`
	HttpMitmNoTunnel   bool `json:"HttpMitmNoTunnel"`

	// 新增字段：公网/外网 IP 列表（用于防止代理循环）
	PublicIPs []string `json:"PublicIPs"`

	// 路由相关配置
	RouteEnable bool        `json:"RouteEnable"`
	ProxyNodes  []ProxyNode `json:"ProxyNodes"` // 代理节点列表
	Routes      []RouteRule `json:"Routes"`

	// 访问控制相关配置
	AccessEnable   bool            `json:"AccessEnable"`   // 访问控制总开关
	AccessRules    []AccessRule    `json:"AccessRules"`    // 访问控制规则列表
	UserBlockRules []UserBlockRule `json:"UserBlockRules"` // 来源 IP 拦截规则列表

	// MinIO 对象存储配置
	MinioConfig myminio.Config `json:"MinioConfig"`
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
	cfg := cm.GetConfig()
	if len(cfg.PublicIPs) == 0 {
		log.Println("")
		log.Println("========================================================")
		log.Println("⚠️  警告：未配置 PublicIPs (公网/外网 IP)")
		log.Println("========================================================")
		log.Println("在云服务器上部署时，强烈建议配置公网 IP 以防止代理循环")
		log.Println("您可以通过 Web UI 控制面板进行配置")
		log.Println("========================================================")
		log.Println("")
	} else {
		log.Printf("✓ 已配置公网 IP 防环: %v", cfg.PublicIPs)
	}
	return cm
}

// DefaultConfig 提供一套开箱即用的默认配置
func DefaultConfig() *ServerConfig {
	return &ServerConfig{
		Port:               8080,
		HTTPSPort:          8443,
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
		AccessEnable:       false,
		AccessRules:        []AccessRule{},
		UserBlockRules:     []UserBlockRule{},
		MinioConfig:        *myminio.DefaultMinioConfig(),
	}
}

// Load 从本地磁盘读取 JSON 配置文件，反序列化合并到内存
func (cm *ConfigManager) Load() error {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	data, err := os.ReadFile(cm.FilePath)
	if err != nil {
		log.Printf("读取配置文件失败: %v", err)
		if os.IsNotExist(err) {
			// 文件不存在时，使用当前默认配置立即新建并写入一份
			return cm.saveLocked()
		}
		return err
	}

	// 将文件内容覆盖到当前配置
	if err := json.Unmarshal(data, cm.Current); err != nil {
		log.Printf("解析配置文件失败: %v", err)
		return err
	}

	// 强制写回磁盘一次：确保像 MinIO 这样新增的默认配置字段，
	// 能够回写到已有的 config.json 文件中，从而让用户可见
	cm.saveLocked()

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
