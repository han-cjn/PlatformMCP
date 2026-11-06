package semantic

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
)

// Caller 抽象下游调用,便于单测注入假实现(不依赖真实 MCP 连接)。
type Caller interface {
	CallTool(ctx context.Context, platform, tool string, args map[string]any) (string, error)
}

// InvokeResult 一次语义调用的结果与元信息(供审计记录)。
type InvokeResult struct {
	Text       string // 下游返回文本
	Platform   string // 实际命中平台
	Capability string // 能力名
	Tool       string // 下游工具名
}

// Invoke 统一入口:校验领域入参 → 映射为该平台参数 → 调用下游。
// 参数:
//   - cat      能力目录
//   - caller   下游调用器(生产传 *adapter.Manager,测试传假实现)
//   - capName  能力名,如 im.send_text
//   - platform 目标平台;为空时按默认顺序自动选择第一个可用平台
//   - domain   领域入参(平台无关)
//   - preferred 默认平台顺序(为空则按能力支持的平台名字典序)
func Invoke(ctx context.Context, cat *Catalog, caller Caller, capName, platform string,
	domain map[string]any, preferred []string) (*InvokeResult, error) {

	cap, ok := cat.Get(capName)
	if !ok {
		return nil, fmt.Errorf("未知能力 %q(已注册: %s)", capName, strings.Join(cat.Names(), ", "))
	}

	// 平台缺省:按指定顺序 / 字典序选第一个声明可用的平台
	if platform == "" {
		platform = pickPlatform(cap, preferred)
		if platform == "" {
			return nil, fmt.Errorf("能力 %q 暂无可用平台(已声明支持: %s)",
				capName, strings.Join(cap.SupportedPlatforms(), ", "))
		}
	}

	m, ok := cap.Platforms[platform]
	if !ok {
		return nil, fmt.Errorf("能力 %q 未声明平台 %q(支持: %s)",
			capName, platform, strings.Join(cap.SupportedPlatforms(), ", "))
	}
	if m.Unsupported != "" {
		return nil, fmt.Errorf("能力 %q 在 %s 上不可用: %s", capName, platform, m.Unsupported)
	}

	// 1. 领域入参归一化 + 校验
	filled, err := cap.Validate(domain)
	if err != nil {
		return nil, fmt.Errorf("%s 入参非法: %w", capName, err)
	}

	// 2. 复杂场景走具名适配器
	if m.Adapter != "" {
		fn, ok := adapters[m.Adapter]
		if !ok {
			return nil, errAdapterMissing(m.Adapter)
		}
		text, err := fn(ctx, caller, platform, m.Tool, filled)
		if err != nil {
			return nil, err
		}
		return &InvokeResult{Text: text, Platform: platform, Capability: capName, Tool: m.Tool}, nil
	}

	// 3. 声明式映射 → 调用
	args, err := m.Build(filled)
	if err != nil {
		return nil, fmt.Errorf("%s 参数映射失败: %w", capName, err)
	}
	slog.Debug("语义调用", "capability", capName, "platform", platform, "tool", m.Tool)
	text, err := caller.CallTool(ctx, platform, m.Tool, args)
	if err != nil {
		return nil, err
	}
	return &InvokeResult{Text: text, Platform: platform, Capability: capName, Tool: m.Tool}, nil
}

// pickPlatform 选默认平台:优先按 preferred 顺序,其次字典序;只考虑声明可用的。
func pickPlatform(cap *Capability, preferred []string) string {
	supported := cap.SupportedPlatforms()
	if len(supported) == 0 {
		return ""
	}
	set := make(map[string]bool, len(supported))
	for _, p := range supported {
		set[p] = true
	}
	for _, p := range preferred {
		if set[p] {
			return p
		}
	}
	return supported[0] // SupportedPlatforms 已排序
}

// MatrixReport 生成能力矩阵的文本视图(启动自检用)。
// mounted 为实际已挂载平台集合,用于区分"声明支持"与"当前可用"。
func MatrixReport(cat *Catalog, platforms []string, mounted []string) string {
	sorted := append([]string(nil), platforms...)
	sort.Strings(sorted)
	mountedSet := make(map[string]bool, len(mounted))
	for _, p := range mounted {
		mountedSet[p] = true
	}

	width := 14
	for _, c := range cat.All() {
		if len(c.Name) > width {
			width = len(c.Name)
		}
	}
	var sb strings.Builder
	sb.WriteString(padRight("capability", width))
	for _, p := range sorted {
		sb.WriteString(padRight(shortPlatform(p), 14))
	}
	sb.WriteString("\n")
	sb.WriteString(strings.Repeat("-", width+14*len(sorted)))
	sb.WriteString("\n")

	for _, c := range cat.All() {
		sb.WriteString(padRight(c.Name, width))
		mx := c.Matrix(sorted)
		for _, p := range sorted {
			sb.WriteString(padRight(cellMark(mx[p], mountedSet[p]), 14))
		}
		sb.WriteString("\n")
	}
	sb.WriteString("\n图例:✓ 支持且已挂载   ○ 支持但未挂载   - 未声明   ✗ 明确不支持")
	return sb.String()
}

// cellMark 根据声明状态与挂载状态给出矩阵符号。
func cellMark(state string, mounted bool) string {
	switch state {
	case "supported":
		if mounted {
			return "✓"
		}
		return "○"
	case "unsupported":
		return "✗"
	default:
		return "-"
	}
}

func shortPlatform(p string) string {
	if len(p) > 13 {
		return p[:13]
	}
	return p
}

// padRight 按显示宽度右填充(ASCII 场景足够;中文能力名已避免出现在列头)。
func padRight(s string, w int) string {
	if len(s) >= w {
		return s + " "
	}
	return s + strings.Repeat(" ", w-len(s))
}
