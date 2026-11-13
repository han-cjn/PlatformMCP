package semantic

import (
	"context"

	"github.com/example/mcp-hub/internal/adapter"
)

// SendText 发送纯文本消息(im.send_text 的便捷封装)。
//
// 保留此函数是为了不打断既有调用点(event 规则回执、日报推送),内部已改为
// 走统一的声明式引擎:平台参数差异由 ImSendText 的 PlatformMapping 表达,
// 不再有 switch platform 硬编码分支。
//
// reg 参数保留但不再用于解析下游工具名(路由已由能力定义内置);保留是为了
// 不修改调用方签名。新代码请直接用 Invoke。
func SendText(ctx context.Context, reg *Registry, mgr *adapter.Manager, platform, chatID, text string) (string, error) {
	text2, err := sendTextVia(ctx, mgr, platform, chatID, text)
	return text2, err
}

// SendTextWithBot 回复事件时沿用来源机器人;botName 为空时交给下游做消歧。
func SendTextWithBot(ctx context.Context, reg *Registry, mgr *adapter.Manager, platform, botName, chatID, text string) (string, error) {
	res, err := Invoke(ctx, BuiltinCatalog(), mgr, ImSendText.Name, platform,
		map[string]any{"target": chatID, "text": text, "bot_name": botName}, nil)
	if err != nil {
		return "", err
	}
	return res.Text, nil
}

// sendTextVia SendText 的实际实现,参数用 Caller 接口以便单测注入假下游。
func sendTextVia(ctx context.Context, caller Caller, platform, chatID, text string) (string, error) {
	res, err := Invoke(ctx, BuiltinCatalog(), caller, ImSendText.Name, platform,
		map[string]any{"target": chatID, "text": text, "bot_name": ""}, nil)
	if err != nil {
		return "", err
	}
	return res.Text, nil
}
