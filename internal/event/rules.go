package event

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/example/mcp-hub/internal/adapter"
	"github.com/example/mcp-hub/internal/semantic"
)

// BitableTarget 描述「#记录」指令写入的飞书多维表格位置。
type BitableTarget struct {
	AppToken string // env: BITABLE_APP_TOKEN
	TableID  string // env: BITABLE_TABLE_ID
	Field    string // env: BITABLE_CONTENT_FIELD(默认 内容)
}

// RecordRule 场景 A 规则:IM 中出现「#记录 <内容>」→ 写入 Bitable → 回执来源群。
type RecordRule struct {
	Keyword string // 默认 #记录
	Target  BitableTarget
	Reg     *semantic.Registry
	Mgr     *adapter.Manager
}

func (r *RecordRule) keyword() string {
	if r.Keyword == "" {
		return "#记录"
	}
	return r.Keyword
}

// Handle 订阅 Bus 的处理函数。
func (r *RecordRule) Handle(ctx context.Context, evt Event) error {
	if evt.Type != "im.message" {
		return nil
	}
	text, _ := evt.Payload["text"].(string)
	slog.Info("规则评估", "platform", evt.Platform, "text", text)
	idx := strings.Index(text, r.keyword())
	if idx < 0 {
		return nil
	}
	content := strings.TrimSpace(text[idx+len(r.keyword()):])
	if content == "" {
		return nil
	}
	chatID, _ := evt.Payload["chat_id"].(string)
	sender, _ := evt.Payload["sender"].(string)
	if chatID == "" {
		slog.Warn("事件缺少 chat_id,跳过", "platform", evt.Platform)
		return nil
	}

	fields := map[string]any{
		r.Target.Field: content,
		"来源平台":         evt.Platform,
		"发送者":          sender,
	}
	resp, err := r.Mgr.CallTool(ctx, "feishu", "bitable_v1_appTableRecord_create", map[string]any{
		"path": map[string]any{
			"app_token": r.Target.AppToken,
			"table_id":  r.Target.TableID,
		},
		"data": map[string]any{"fields": fields},
	})
	if err != nil {
		slog.Error("写 Bitable 失败", "err", err, "platform", evt.Platform)
		r.ack(ctx, evt.Platform, botName(evt), chatID, fmt.Sprintf("❌ 记录失败:%v", err))
		return nil // 已回执,错误不外抛阻断总线
	}
	slog.Info("已写入 Bitable", "platform", evt.Platform, "sender", sender, "resp", resp)
	r.ack(ctx, evt.Platform, botName(evt), chatID, fmt.Sprintf("✅ 已记录到多维表格(来源:%s)", evt.Platform))
	return nil
}

func (r *RecordRule) ack(ctx context.Context, platform, botName, chatID, text string) {
	if _, err := semantic.SendTextWithBot(ctx, r.Reg, r.Mgr, platform, botName, chatID, text); err != nil {
		slog.Error("回执发送失败", "platform", platform, "err", err)
	}
}

// botName 保留来源机器人身份,防止多机器人共用 chatid 时回执串号。
func botName(evt Event) string {
	name, _ := evt.Payload["bot_name"].(string)
	return name
}
