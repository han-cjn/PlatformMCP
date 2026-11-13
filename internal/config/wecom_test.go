package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWeComBotsValidationAndNames(t *testing.T) {
	cfg := &Config{WeCom: &WeComConfig{Bots: []WeComBotConfig{
		{Name: "mcp-hub", BotID: "b1", Secret: "s1"},
		{Name: "测试", BotID: "b2", Secret: "s2"},
	}}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	bots := cfg.WeComBots()
	if len(bots) != 2 || bots[0].Name != "mcp-hub" || bots[1].Name != "测试" {
		t.Fatalf("机器人名列表不符: %+v", bots)
	}
	if _, ok, err := cfg.WeComCredentials(); err != nil || !ok {
		t.Fatalf("应能展开凭据: %v/%v", ok, err)
	}

	bad := &Config{WeCom: &WeComConfig{Bots: []WeComBotConfig{
		{Name: "mcp-hub", BotID: "b1", Secret: "s1"},
		{Name: "mcp-hub", BotID: "b2", Secret: "s2"},
	}}}
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "重复机器人名") {
		t.Fatalf("重复名称必须拒绝: %v", err)
	}
}

func TestUpsertWeComBotPreservesOtherEntriesAndFormatting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	original := `{
  "db_path": "hub.db",
  "servers": [{"name": "wecom"}],
  "wecom": {
    "bots": [
      {"name": "测试", "bot_id": "old-id", "secret": "old-secret"},
      {"name": "mcp-hub", "bot_id": "$WECOM_BOT_ID", "secret": "$WECOM_BOT_SECRET"}
    ]
  }
}
`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := UpsertWeComBot(path, "mcp-hub", "$WECOM_BOT_ID", "$WECOM_BOT_SECRET"); err != nil {
		t.Fatal(err)
	}
	// 真实环境在 Load 前由 LoadDotEnv 注入;测试只设置合成值,验证引用展开。
	t.Setenv("WECOM_BOT_ID", "id2")
	t.Setenv("WECOM_BOT_SECRET", "secret2")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.WeCom.Bots) != 2 || cfg.WeCom.Bots[0].BotID != "old-id" || cfg.WeCom.Bots[1].BotID != "id2" || cfg.WeCom.Bots[1].Secret != "secret2" {
		t.Fatalf("其他机器人被覆盖或目标未更新: %+v", cfg.WeCom.Bots)
	}
	if cfg.Servers == nil || len(cfg.Servers) != 1 {
		t.Fatal("顶层配置应保留")
	}
	idKey, secretKey := WeComBotEnvKeys("运营机器人")
	if idKey == secretKey || !strings.HasPrefix(idKey, "WECOM_BOT_") {
		t.Fatalf("命名机器人应生成合法且不同的环境变量键: %q/%q", idKey, secretKey)
	}
	if err := UpsertWeComBot(path, "运营机器人", "$"+idKey, "$"+secretKey); err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	raw, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(raw, &root) != nil {
		t.Fatalf("配置无法解析: %v", err)
	}
	bots := root["wecom"].(map[string]any)["bots"].([]any)
	last := bots[len(bots)-1].(map[string]any)
	if last["bot_id"] != "$"+idKey || last["secret"] != "$"+secretKey {
		t.Fatalf("命名机器人应只保存环境变量引用: %+v", last)
	}
	if err := UpsertWeComBot(path, "运营机器人", "id3", "secret3"); err == nil {
		t.Fatal("原始凭据不得写进 config.json")
	}
	if idAgain, secretAgain := WeComBotEnvKeys("运营机器人"); idAgain != idKey || secretAgain != secretKey {
		t.Fatal("同一机器人重试接入时环境变量键必须稳定")
	}
}
