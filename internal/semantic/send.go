package semantic

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/example/mcp-hub/internal/adapter"
)

// SendText 按平台构造 im.send_text 的下游 payload 并发送,屏蔽三家入参结构差异。
func SendText(ctx context.Context, reg *Registry, mgr *adapter.Manager, platform, chatID, text string) (string, error) {
	tool, err := reg.Resolve("im.send_text", platform)
	if err != nil {
		return "", err
	}
	var args map[string]any
	switch platform {
	case "feishu":
		content, _ := json.Marshal(map[string]string{"text": text})
		args = map[string]any{
			"params": map[string]any{"receive_id_type": "chat_id"},
			"data":   map[string]any{"receive_id": chatID, "msg_type": "text", "content": string(content)},
		}
	case "dingtalk":
		args = map[string]any{
			"openConversationId": chatID,
			"msgParam":           map[string]any{"title": "MCP Hub", "text": text},
		}
	case "wecom":
		args = map[string]any{"chatid": chatID, "content": text}
	default:
		return "", fmt.Errorf("SendText 未支持平台 %q", platform)
	}
	return mgr.CallTool(ctx, platform, tool, args)
}
