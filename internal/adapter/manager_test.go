package adapter

import (
	"strings"
	"testing"

	"github.com/example/mcp-hub/internal/config"
)

// P1-3:路由表与真实工具一致性校验。
func TestCheckRoutes(t *testing.T) {
	mounted := map[string][]string{
		"feishu": {"im_v1_message_create"},
		"wecom":  {}, // 已挂载但无工具
		// dingtalk 未挂载
	}
	routes := map[string]map[string]string{
		"im.send_text": {
			"feishu":   "im_v1_message_create",      // 正常
			"wecom":    "wecom_send_text",           // 工具不存在
			"dingtalk": "sendMessageToGroupByRobot", // 平台未挂载
		},
	}
	issues := CheckRoutes(mounted, routes)
	if len(issues) != 2 {
		t.Fatalf("应检出 2 个问题, 实际 %d: %+v", len(issues), issues)
	}
	reasons := map[string]string{}
	for _, i := range issues {
		reasons[i.Platform] = i.Reason
	}
	if reasons["wecom"] != "tool_not_found" || reasons["dingtalk"] != "platform_not_mounted" {
		t.Fatalf("问题分类错误: %+v", issues)
	}
	if _, ok := reasons["feishu"]; ok {
		t.Fatal("正常路由不应被检出")
	}
}

// P2-4:env 前缀透传 + 白名单并存。
func TestBuildEnvPrefixPassthrough(t *testing.T) {
	t.Setenv("DINGTALK_Client_ID", "id-1")
	t.Setenv("DINGTALK_NEW_FLAG", "x")
	t.Setenv("OTHER_VAR", "nope")
	sc := config.ServerConfig{
		Name:        "dingtalk",
		Env:         []string{"DINGTALK_Client_ID"},
		EnvPrefixes: []string{"DINGTALK"},
	}
	got := map[string]bool{}
	for _, kv := range buildEnv(sc) {
		got[kv] = true
	}
	if !got["DINGTALK_Client_ID=id-1"] {
		t.Error("白名单变量缺失")
	}
	if !got["DINGTALK_NEW_FLAG=x"] {
		t.Error("前缀透传变量缺失")
	}
	if got["OTHER_VAR=nope"] {
		t.Error("未匹配前缀的变量不应透传")
	}
}

func TestWeComMultiBotSendRequiresExplicitName(t *testing.T) {
	cfg := &config.Config{WeCom: &config.WeComConfig{Bots: []config.WeComBotConfig{
		{Name: "测试", BotID: "b1", Secret: "s1"},
		{Name: "mcp-hub", BotID: "b2", Secret: "s2"},
	}}}
	m := &Manager{bots: map[string][]config.NamedBot{"wecom": cfg.WeComBots()}}
	_, err := m.CallTool(t.Context(), "wecom", "wecom_send_text", map[string]any{"chatid": "chat1", "content": "hi", "bot_name": ""})
	if err == nil || !strings.Contains(err.Error(), "need_bot_selection") ||
		!strings.Contains(err.Error(), "测试") || !strings.Contains(err.Error(), "mcp-hub") {
		t.Fatalf("多机器人缺少选择时必须拒绝并返回候选: %v", err)
	}
	_, err = m.CallTool(t.Context(), "wecom", "wecom_send_text", map[string]any{
		"chatid": "chat1", "content": "hi", "bot_name": "不存在",
	})
	if err == nil || !strings.Contains(err.Error(), "unknown_bot") {
		t.Fatalf("未知机器人必须在连接下游前拒绝: %v", err)
	}
}

// P2-5:限流错误特征识别。
func TestLooksLikeLimitError(t *testing.T) {
	for _, s := range []string{"HTTP 429 too many requests", "rate limit exceeded", "超过调用频率限制", "quota exhausted"} {
		if !looksLikeLimitError(s) {
			t.Errorf("应识别为限流: %s", s)
		}
	}
	if looksLikeLimitError("record not found") {
		t.Error("普通错误不应误判为限流")
	}
}
