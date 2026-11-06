// Package config 负责加载 Hub 配置:MCP 服务器清单 + 统一语义路由表。
// 配置中所有字符串支持 $ENV 环境变量展开,凭据只放 .env,不进仓库。
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// ServerConfig 描述一个下游 MCP Server 的接入方式。
type ServerConfig struct {
	Name        string            `json:"name"`         // 平台命名空间,如 feishu / dingtalk / tencent-docs / wecom
	Transport   string            `json:"transport"`    // stdio | http
	Command     string            `json:"command"`      // stdio: 可执行命令
	Args        []string          `json:"args"`         // stdio: 参数
	Env         []string          `json:"env"`          // stdio: 追加注入子进程的环境变量名(值取自宿主机)
	EnvPrefixes []string          `json:"env_prefixes"` // stdio: 按前缀透传宿主机环境变量(如 ["DINGTALK"]),与 env 白名单并存(P2-4)
	URL         string            `json:"url"`          // http: 远程 MCP 端点
	Headers     map[string]string `json:"headers"`      // http: 请求头
}

// Config 是 Hub 顶层配置。
type Config struct {
	Servers []ServerConfig `json:"servers"`
	// Routes: capability -> platform -> 下游真实工具名
	Routes map[string]map[string]string `json:"routes"`
	// DBPath 持久化 DB 路径(默认 hub.db,支持 $ENV 展开)
	DBPath string `json:"db_path"`
	// Workers 事件消费并发数(默认 4)
	Workers int `json:"workers"`
	// PushTargets M3 推送目标群:platform -> 会话 ID(P0-3,配置驱动,不再人工 @ 抓 ID)
	PushTargets map[string]string `json:"push_targets"`
	// DefaultPlatformOrder 语义调用未指定 platform 时的平台优先级(如 ["feishu","wecom","dingtalk"]);
	// 为空则按能力支持平台的字典序选第一个可用者
	DefaultPlatformOrder []string `json:"default_platform_order"`
	// Report M3 场景 B:每日 Bitable 增量 → 腾讯文档日报 → 推 IM
	Report ReportConfig `json:"report"`
}

// ReportConfig 日报联动配置(全部可选;enabled=false 时只保留手动触发)。
type ReportConfig struct {
	Enabled       bool     `json:"enabled"`        // 是否启用内置 cron 调度
	Cron          string   `json:"cron"`           // 标准 5 段 cron 表达式,如 "0 18 * * *"
	Timezone      string   `json:"timezone"`       // IANA 时区,默认 Asia/Shanghai
	BackfillHours int      `json:"backfill_hours"` // 首次运行回填窗口,默认 24h
	CreatedField  string   `json:"created_field"`  // Bitable 创建时间字段名,默认 创建时间
	PushPlatforms []string `json:"push_platforms"` // 推送平台开关,默认 ["wecom"];可选 feishu/dingtalk
}

// Load 读取配置文件并做环境变量展开。
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	expanded := os.ExpandEnv(string(raw))
	var cfg Config
	if err := json.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return &cfg, nil
}

// LoadDotEnv 解析 .env(KEY=VALUE 逐行)写入进程环境变量,已存在的变量不覆盖。
// .env 缺失不再静默:返回 ErrDotEnvMissing,由调用方升级 ERROR 并计入体检表(P1-5)。
var ErrDotEnvMissing = errors.New(".env 不存在")

func LoadDotEnv(path string) error {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ErrDotEnvMissing
	}
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		if _, exists := os.LookupEnv(k); !exists {
			os.Setenv(k, strings.Trim(strings.TrimSpace(v), `"'`))
		}
	}
	return nil
}
