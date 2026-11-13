// Package adapter 管理下游 MCP Server 连接:挂载、工具聚合(命名空间隔离)、调用转发。
// 单平台连接失败只降级不致命,保证凭据未齐时骨架仍可启动。
package adapter

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/example/mcp-hub/internal/config"
	"github.com/example/mcp-hub/internal/health"
)

// Tool 是一个带平台命名空间的下游工具引用。
type Tool struct {
	Platform    string
	Name        string // 原始工具名(调用下游时使用)
	ExposedName string // 对客户端暴露名:{platform}.{name}
	Description string
	InputSchema any // 下游工具的 JSON Schema,便于调用方构造 payload
}

// Manager 持有所有下游连接。
type Manager struct {
	mu      sync.RWMutex
	clients map[string]*client.Client
	tools   []Tool
	health  *health.Registry
	bots    map[string][]config.NamedBot
}

// NewManager 按配置逐个连接下游 MCP Server;失败的平台记录日志后跳过。
func NewManager(ctx context.Context, servers []config.ServerConfig, h *health.Registry) *Manager {
	return NewManagerWithConfig(ctx, &config.Config{Servers: servers}, h)
}

// NewManagerWithConfig 保留原 NewManager 兼容入口,并支持顶层企微多机器人配置。
func NewManagerWithConfig(ctx context.Context, cfg *config.Config, h *health.Registry) *Manager {
	m := &Manager{clients: make(map[string]*client.Client), health: h,
		bots: map[string][]config.NamedBot{"wecom": cfg.WeComBots()}}
	for _, sc := range cfg.Servers {
		c, err := connectWithWeCom(ctx, sc, cfg)
		if err != nil {
			slog.Error("下游 MCP 挂载失败(降级跳过)", "platform", sc.Name, "err", err)
			h.SetMounted(sc.Name, false)
			h.SetError(sc.Name, "挂载失败: "+err.Error())
			continue
		}
		tools, err := c.ListTools(ctx, mcp.ListToolsRequest{})
		if err != nil {
			slog.Error("tools/list 失败(降级跳过)", "platform", sc.Name, "err", err)
			h.SetMounted(sc.Name, false)
			h.SetError(sc.Name, "tools/list 失败: "+err.Error())
			continue
		}
		m.clients[sc.Name] = c
		h.SetMounted(sc.Name, true)
		for _, t := range tools.Tools {
			m.tools = append(m.tools, Tool{
				Platform:    sc.Name,
				Name:        t.Name,
				ExposedName: sc.Name + "." + t.Name,
				Description: t.Description,
				InputSchema: t.InputSchema,
			})
		}
		slog.Info("下游 MCP 挂载成功", "platform", sc.Name, "tools", len(tools.Tools))
	}
	return m
}

// RouteIssue 描述一条路由与真实工具不一致的问题(P1-3)。
type RouteIssue struct {
	Capability string `json:"capability"`
	Platform   string `json:"platform"`
	Tool       string `json:"tool"`
	Reason     string `json:"reason"` // platform_not_mounted / tool_not_found
}

// ValidateRoutes 将路由表与已挂载真实工具逐一比对,返回问题清单并标注 degraded。
func (m *Manager) ValidateRoutes(routes map[string]map[string]string, h *health.Registry) []RouteIssue {
	m.mu.RLock()
	mountedTools := make(map[string][]string)
	for name := range m.clients {
		mountedTools[name] = nil
	}
	for _, t := range m.tools {
		mountedTools[t.Platform] = append(mountedTools[t.Platform], t.Name)
	}
	m.mu.RUnlock()
	issues := CheckRoutes(mountedTools, routes)
	byPlatform := make(map[string][]string)
	for _, i := range issues {
		switch i.Reason {
		case "platform_not_mounted":
			byPlatform[i.Platform] = append(byPlatform[i.Platform], "路由 "+i.Capability+" 指向未挂载平台")
		case "tool_not_found":
			byPlatform[i.Platform] = append(byPlatform[i.Platform], "路由 "+i.Capability+" 指向不存在的工具 "+i.Tool)
		}
	}
	for p, reasons := range byPlatform {
		h.SetDegraded(p, reasons)
		for _, r := range reasons {
			slog.Error("路由自检不通过", "platform", p, "reason", r)
		}
	}
	if len(issues) == 0 {
		slog.Info("路由自检通过", "routes", len(routes))
	}
	return issues
}

// CheckRoutes 纯函数:路由表 vs 已挂载工具清单(可单测)。
// mountedTools: platform -> 工具名列表;key 存在但列表为空表示已挂载但无工具。
func CheckRoutes(mountedTools map[string][]string, routes map[string]map[string]string) []RouteIssue {
	sets := make(map[string]map[string]bool, len(mountedTools))
	for p, tools := range mountedTools {
		sets[p] = make(map[string]bool, len(tools))
		for _, t := range tools {
			sets[p][t] = true
		}
	}
	var issues []RouteIssue
	for cap, perPlatform := range routes {
		for platform, tool := range perPlatform {
			toolSet, ok := sets[platform]
			if !ok {
				issues = append(issues, RouteIssue{cap, platform, tool, "platform_not_mounted"})
				continue
			}
			if !toolSet[tool] {
				issues = append(issues, RouteIssue{cap, platform, tool, "tool_not_found"})
			}
		}
	}
	return issues
}

func connect(ctx context.Context, sc config.ServerConfig) (*client.Client, error) {
	return connectWithWeCom(ctx, sc, nil)
}

// connectWithWeCom 将多机器人凭据仅注入企微子进程;其他平台看不到这些值。
func connectWithWeCom(ctx context.Context, sc config.ServerConfig, cfg *config.Config) (*client.Client, error) {
	switch strings.ToLower(sc.Transport) {
	case "stdio":
		env := buildEnvWithWeCom(sc, cfg)
		c, err := client.NewStdioMCPClient(sc.Command, env, sc.Args...)
		if err != nil {
			return nil, err
		}
		if err := c.Start(ctx); err != nil {
			return nil, err
		}
		if err := initialize(ctx, c, sc.Name); err != nil {
			return nil, err
		}
		return c, nil
	case "http":
		var opts []transport.StreamableHTTPCOption
		if len(sc.Headers) > 0 {
			opts = append(opts, transport.WithHTTPHeaders(sc.Headers))
		}
		c, err := client.NewStreamableHttpClient(sc.URL, opts...)
		if err != nil {
			return nil, err
		}
		if err := c.Start(ctx); err != nil {
			return nil, err
		}
		if err := initialize(ctx, c, sc.Name); err != nil {
			return nil, err
		}
		return c, nil
	default:
		return nil, fmt.Errorf("unknown transport %q", sc.Transport)
	}
}

func buildEnv(sc config.ServerConfig) []string {
	return buildEnvWithWeCom(sc, nil)
}

func buildEnvWithWeCom(sc config.ServerConfig, cfg *config.Config) []string {
	extra := ""
	if sc.Name == "wecom" && cfg != nil {
		if encoded, ok, err := cfg.WeComCredentials(); err == nil && ok {
			extra = encoded
		}
		// Config.Load 已校验命名凭据;这里保持旧 buildEnv 的非错误签名。
		// 若绕过 Load 直接构造非法配置,不展开半成品凭据,适配器会拒绝缺省值。
	}
	seen := make(map[string]bool)
	var env []string
	for _, k := range sc.Env {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
			seen[k] = true
		}
	}
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if seen[name] {
			continue
		}
		for _, p := range sc.EnvPrefixes {
			if strings.HasPrefix(strings.ToUpper(name), strings.ToUpper(p)) {
				env = append(env, kv)
				seen[name] = true
				break
			}
		}
	}
	if extra != "" {
		env = append(env, "WECOM_BOTS="+extra)
	}
	return env
}

func initialize(ctx context.Context, c *client.Client, platform string) error {
	req := mcp.InitializeRequest{}
	req.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	req.Params.ClientInfo = mcp.Implementation{Name: "mcp-hub", Version: "0.1.0"}
	if _, err := c.Initialize(ctx, req); err != nil {
		return fmt.Errorf("initialize %s: %w", platform, err)
	}
	return nil
}

// Tools 返回全部已挂载工具(带命名空间)。
func (m *Manager) Tools() []Tool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]Tool(nil), m.tools...)
}

// Platforms 返回成功挂载的平台列表。
func (m *Manager) Platforms() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.clients))
	for p := range m.clients {
		out = append(out, p)
	}
	return out
}

// CallTool 调用指定平台的下游工具,返回文本结果。
// 附带调用计数与限流分类告警(P2-5):429/配额类错误单独计数并 ERROR 级告警。
func (m *Manager) CallTool(ctx context.Context, platform, tool string, args map[string]any) (string, error) {
	m.mu.RLock()
	bots := append([]config.NamedBot(nil), m.bots[platform]...)
	m.mu.RUnlock()
	if platform == "wecom" && tool == "wecom_send_text" && len(bots) > 1 {
		name, _ := args["bot_name"].(string)
		if strings.TrimSpace(name) == "" {
			return "", fmt.Errorf("need_bot_selection: 企业微信配置了多个机器人,候选 %s;必须询问用户并显式传 bot_name;本次未发送", weComBotNames(bots))
		}
		known := false
		for _, bot := range bots {
			if bot.Name == strings.TrimSpace(name) {
				known = true
				break
			}
		}
		if !known {
			return "", fmt.Errorf("unknown_bot: 企业微信机器人 %q 不在配置中;本次未发送", name)
		}
	}
	m.mu.RLock()
	c, ok := m.clients[platform]
	m.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("platform %q 未挂载(凭据缺失或连接失败)", platform)
	}
	m.health.AddCall(platform)
	req := mcp.CallToolRequest{}
	req.Params.Name = tool
	req.Params.Arguments = args
	res, err := c.CallTool(ctx, req)
	if err != nil {
		m.classifyLimit(platform, err)
		return "", fmt.Errorf("%s.%s 调用失败: %w", platform, tool, err)
	}
	var sb strings.Builder
	for _, content := range res.Content {
		if tc, ok := content.(mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	// MCP 业务错误必须沿调用链返回;多机器人消歧等错误不能伪装成发送成功。
	text := sb.String()
	if res.IsError {
		return text, fmt.Errorf("%s.%s: %s", platform, tool, text)
	}
	// 下游把业务错误包在 200 响应里的情况(腾讯文档限流即如此):扫描文本特征
	if looksLikeLimitError(text) {
		m.classifyLimit(platform, fmt.Errorf("%s", truncate(text, 200)))
	}
	return text, nil
}

func weComBotNames(bots []config.NamedBot) string {
	names := make([]string, 0, len(bots))
	for _, bot := range bots {
		names = append(names, bot.Name)
	}
	return strings.Join(names, "、")
}

func (m *Manager) classifyLimit(platform string, err error) {
	if looksLikeLimitError(err.Error()) {
		m.health.AddLimitError(platform, err)
		slog.Error("⚠️ 平台限流/配额告警", "platform", platform, "err", err)
	}
}

// looksLikeLimitError 识别限流/配额类错误特征(429 / rate limit / quota / 频率)。
func looksLikeLimitError(s string) bool {
	s = strings.ToLower(s)
	for _, kw := range []string{"429", "rate limit", "ratelimit", "quota", "too many requests", "频率", "超限"} {
		if strings.Contains(s, kw) {
			return true
		}
	}
	return false
}

// IsLimitError 判定错误文本是否为限流/配额类(供报表等链路做错误分类,不盲目重试)。
func IsLimitError(s string) bool { return looksLikeLimitError(s) }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// Close 关闭所有下游连接与子进程。
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, c := range m.clients {
		if err := c.Close(); err != nil {
			slog.Warn("关闭下游连接失败", "platform", name, "err", err)
		}
	}
}

// Reload 热重载单个平台:关闭旧连接,用新配置(新凭据)重新挂载。
// 供 onboard 收尾链使用——写完 .env 后无需重启进程。
func (m *Manager) Reload(ctx context.Context, sc config.ServerConfig) error {
	return m.ReloadWithConfig(ctx, sc, nil)
}

func (m *Manager) ReloadWithConfig(ctx context.Context, sc config.ServerConfig, cfg *config.Config) error {
	// 1. 摘除旧连接与该平台全部工具
	m.mu.Lock()
	if cfg != nil && sc.Name == "wecom" {
		m.bots[sc.Name] = cfg.WeComBots()
	}
	if old, ok := m.clients[sc.Name]; ok {
		if err := old.Close(); err != nil {
			slog.Warn("热重载:关闭旧连接失败", "platform", sc.Name, "err", err)
		}
		delete(m.clients, sc.Name)
	}
	kept := m.tools[:0]
	for _, t := range m.tools {
		if t.Platform != sc.Name {
			kept = append(kept, t)
		}
	}
	m.tools = kept
	m.mu.Unlock()

	// 2. 用新环境重新连接(stdio 的 buildEnv 在 connect 时读取宿主机环境,
	//    因此调用方需先 Setenv;http 的 headers 展开在 config.Load,调用方需重载配置传入)
	c, err := connectWithWeCom(ctx, sc, cfg)
	if err != nil {
		h := m.health
		h.SetMounted(sc.Name, false)
		h.SetError(sc.Name, "热重载挂载失败: "+err.Error())
		return err
	}
	tools, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		c.Close()
		m.health.SetMounted(sc.Name, false)
		m.health.SetError(sc.Name, "热重载 tools/list 失败: "+err.Error())
		return err
	}

	// 3. 装配新连接与工具
	m.mu.Lock()
	m.clients[sc.Name] = c
	for _, t := range tools.Tools {
		m.tools = append(m.tools, Tool{
			Platform:    sc.Name,
			Name:        t.Name,
			ExposedName: sc.Name + "." + t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		})
	}
	m.mu.Unlock()
	m.health.SetMounted(sc.Name, true)
	m.health.SetError(sc.Name, "")
	slog.Info("平台热重载成功", "platform", sc.Name, "tools", len(tools.Tools))
	return nil
}
