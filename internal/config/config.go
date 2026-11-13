// Package config 负责加载 Hub 配置:MCP 服务器清单 + 统一语义路由表。
// 配置中所有字符串支持 $ENV 环境变量展开,凭据只放 .env,不进仓库。
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
type WeComBotConfig struct {
	Name   string `json:"name"`
	BotID  string `json:"bot_id"`
	Secret string `json:"secret"`
}

type WeComConfig struct {
	Bots []WeComBotConfig `json:"bots"`
}

// NamedBot 是平台无关的命名账号视图;目前企微用于发送前消歧。
type NamedBot struct {
	Name string `json:"name"`
}

type Config struct {
	Servers []ServerConfig `json:"servers"`
	WeCom   *WeComConfig   `json:"wecom,omitempty"`
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
	// BotName 多机器人企微的定时推送身份;缺省时 Manager 拒绝发送。
	BotName string `json:"bot_name,omitempty"`
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
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return &cfg, nil
}

func (c *Config) Validate() error {
	if c == nil || c.WeCom == nil {
		return nil
	}
	seen := map[string]bool{}
	for i, bot := range c.WeCom.Bots {
		name := strings.TrimSpace(bot.Name)
		if name == "" || strings.TrimSpace(bot.BotID) == "" || strings.TrimSpace(bot.Secret) == "" {
			return fmt.Errorf("wecom.bots[%d] 的 name / bot_id / secret 均不能为空", i)
		}
		if seen[name] {
			return fmt.Errorf("wecom.bots 存在重复机器人名 %q", name)
		}
		seen[name] = true
	}
	return nil
}

// WeComBots 返回配置中的机器人名。旧版 servers + WECOM_BOT_ID/SECRET
// 没有名称元数据,按单个机器人兼容,不需要发送前选择。
func (c *Config) WeComBots() []NamedBot {
	if c == nil || c.WeCom == nil {
		return nil
	}
	out := make([]NamedBot, 0, len(c.WeCom.Bots))
	for _, bot := range c.WeCom.Bots {
		out = append(out, NamedBot{Name: strings.TrimSpace(bot.Name)})
	}
	return out
}

// WeComCredentials 把多机器人配置展开成适配器的 WECOM_BOTS JSON。
// 只传名称、Bot ID、Secret;调用方通过 env 白名单注入,日志不得记录其内容。
func (c *Config) WeComCredentials() (string, bool, error) {
	if err := c.Validate(); err != nil {
		return "", false, err
	}
	if c == nil || c.WeCom == nil || len(c.WeCom.Bots) == 0 {
		return "", false, nil
	}
	raw, err := json.Marshal(c.WeCom.Bots)
	if err != nil {
		return "", false, fmt.Errorf("encode wecom bots: %w", err)
	}
	return string(raw), true, nil
}

// WeComBotEnvKeys 为每个命名机器人生成稳定的环境变量名。
// 使用名称哈希而非中文/空格直拼,保证 .env 键合法且同机器人重试幂等;
// 旧版 WECOM_BOT_ID / WECOM_BOT_SECRET 与 mcp-hub 的现有引用保持不变。
func WeComBotEnvKeys(name string) (string, string) {
	if strings.TrimSpace(name) == "mcp-hub" {
		return "WECOM_BOT_ID", "WECOM_BOT_SECRET"
	}
	digest := sha256.Sum256([]byte(strings.TrimSpace(name)))
	suffix := strings.ToUpper(hex.EncodeToString(digest[:8]))
	return "WECOM_BOT_" + suffix + "_ID", "WECOM_BOT_" + suffix + "_SECRET"
}

// UpsertWeComBot 只更新指定名称的机器人引用;保留其他机器人和全部顶层配置。
// 不接受凭据值,避免在 config.json 固化 Secret;原子替换,不写日志或返回凭据。
func UpsertWeComBot(path, name, botIDRef, secretRef string) error {
	if strings.TrimSpace(name) == "" || !strings.HasPrefix(botIDRef, "$") || !strings.HasPrefix(secretRef, "$") {
		return fmt.Errorf("wecom bot name 非空,且 bot_id / secret 必须是 $ENV 引用")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	wecom, _ := root["wecom"].(map[string]any)
	if wecom == nil {
		wecom = map[string]any{}
	}
	bots, _ := wecom["bots"].([]any)
	entry := map[string]any{"name": name, "bot_id": botIDRef, "secret": secretRef}
	replaced := false
	for i, rawBot := range bots {
		bot, ok := rawBot.(map[string]any)
		if !ok || strings.TrimSpace(fmt.Sprint(bot["name"])) == "" ||
			strings.TrimSpace(fmt.Sprint(bot["bot_id"])) == "" || strings.TrimSpace(fmt.Sprint(bot["secret"])) == "" {
			return fmt.Errorf("wecom.bots[%d] 原有 name / bot_id / secret 无效,拒绝改写整个列表", i)
		}
		if bot["name"] == name {
			bots[i] = entry
			replaced = true
			break
		}
	}
	if !replaced {
		bots = append(bots, entry)
	}
	wecom["bots"] = bots
	root["wecom"] = wecom
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-wecom-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(append(out, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// 临时文件必须收紧权限,再原子替换;配置文件本身只保存引用,不含 Secret。
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
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
