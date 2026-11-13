package onboard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 2026-11-12 实测断链:.env 只有 WECOM_BOT_ID/SECRET,而 .env.example 注释声称
// HUB_EVENT_URL「默认 http://127.0.0.1:9780/events」——代码里却是
// `process.env.HUB_EVENT_URL || ''` + `if (!HUB_EVENT_URL) return;`,
// 未配置时六类消息帧被静默丢弃,chats 表的企微会话停在 2026-11-02。
// 以下用例固化「事件桥三处配置必须与实现一致」,防止再次悄悄断链。

// TestWecomEventBridgeEnvWhitelist 断言子进程能拿到全部事件上报变量。
// HUB_EVENT_RETRY_MAX 原本既不在 env 白名单、也不匹配 env_prefixes,
// 适配器永远读到默认 5 —— 配了也白配。
func TestWecomEventBridgeEnvWhitelist(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(moduleRoot(), "config.json"))
	if err != nil {
		t.Fatalf("读 config.json 失败: %v", err)
	}
	var cfg struct {
		Servers []struct {
			Name string   `json:"name"`
			Env  []string `json:"env"`
		} `json:"servers"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("解析 config.json 失败: %v", err)
	}
	var wecom []string
	for _, s := range cfg.Servers {
		if s.Name == "wecom" {
			wecom = s.Env
		}
	}
	if wecom == nil {
		t.Fatal("config.json 中找不到 wecom server 配置")
	}
	for _, want := range []string{"WECOM_BOT_ID", "WECOM_BOT_SECRET", "HUB_EVENT_URL", "HUB_EVENT_SECRET", "HUB_EVENT_RETRY_MAX"} {
		found := false
		for _, k := range wecom {
			if k == want {
				found = true
			}
		}
		if !found {
			t.Errorf("wecom.env 白名单缺少 %q(适配器子进程拿不到该变量): %v", want, wecom)
		}
	}
}

// TestWecomAdapterFallsBackToDefaultEventURL 断言「注释里的默认地址」在代码中真实存在,
// 且用 ?? 保留「显式设空可关闭上报」的能力(onboard 真连校验依赖显式失效地址)。
func TestWecomAdapterFallsBackToDefaultEventURL(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(moduleRoot(), "adapters", "wecom-aibot-mcp", "server.js"))
	if err != nil {
		t.Fatalf("读企微适配器失败: %v", err)
	}
	src := string(raw)
	if !strings.Contains(src, "'http://127.0.0.1:9780/events'") {
		t.Fatal("适配器未内置默认事件端点地址,.env.example 的注释与实现不一致")
	}
	if !strings.Contains(src, "process.env.HUB_EVENT_URL ??") {
		t.Fatal("HUB_EVENT_URL 必须用 ?? 回落默认值,不能用 || 吃掉显式空串")
	}
	if !strings.Contains(src, "if (!HUB_EVENT_URL) return;") {
		t.Fatal("显式关闭上报的守卫丢失,真连校验可能真的投递事件")
	}
}

// TestDotEnvExampleDoesNotDisableEventBridge 防止 cp .env.example .env 时,
// 把 HUB_EVENT_URL= 的空值写进去,反而把上行链路关掉。
func TestDotEnvExampleDoesNotDisableEventBridge(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(moduleRoot(), ".env.example"))
	if err != nil {
		t.Fatalf("读 .env.example 失败: %v", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "HUB_EVENT_URL=") {
			t.Fatalf(".env.example 不应提供未注释的 HUB_EVENT_URL= 空值(会关闭事件上报): %q", trimmed)
		}
	}
}
