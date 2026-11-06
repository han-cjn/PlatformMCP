package event

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"github.com/example/mcp-hub/internal/health"
)

// stderrLarkLogger 适配飞书 SDK 日志接口:Warn/Error 走 stderr,Debug/Info 丢弃。
// 注意:WithLogger 传入自定义 logger 后 SDK 不再做级别过滤(级别只内置于默认 logger),
// 不过滤的话 WS 的 ping/pong(Debug 级)会持续刷 stderr。
type stderrLarkLogger struct{}

func (stderrLarkLogger) Debug(_ context.Context, _ ...interface{}) {}
func (stderrLarkLogger) Info(_ context.Context, _ ...interface{})  {}
func (stderrLarkLogger) Warn(_ context.Context, args ...interface{}) {
	fmt.Fprintln(os.Stderr, args...)
}
func (stderrLarkLogger) Error(_ context.Context, args ...interface{}) {
	fmt.Fprintln(os.Stderr, args...)
}

// StartFeishuWS 飞书事件订阅(WebSocket 长连接,免公网回调)。
// 需在开发者后台开启事件订阅长连接并订阅 im.message.receive_v1。
// P2-2:按 msg_type 分支;暂不支持的类型产生 im.message.unsupported 事件,由订阅方回执。
func StartFeishuWS(ctx context.Context, appID, appSecret string, ig *Ingress, h *health.Registry) error {
	// NewEventDispatcher 构造函数会向 os.Stdout 打一行 "[Info] event-dispatch is ready"
	// (SDK 内置 logger 在构造时绑定 os.Stdout),stdio 模式下该行会被宿主当作协议帧解析。
	// 构造期间临时把 os.Stdout 指向 stderr,构造完成立即恢复。
	oldStdout := os.Stdout
	os.Stdout = os.Stderr
	eventDispatcher := dispatcher.NewEventDispatcher("", "")
	os.Stdout = oldStdout

	handler := eventDispatcher.OnP2MessageReceiveV1(
		func(_ context.Context, e *larkim.P2MessageReceiveV1) error {
			msg := e.Event.Message
			if msg == nil || msg.ChatId == nil {
				return nil
			}
			h.TouchEvent("feishu")
			msgType := deref(msg.MessageType)
			payload := map[string]any{
				"chat_id":  *msg.ChatId,
				"msg_id":   deref(msg.MessageId),
				"msg_type": msgType,
			}
			if e.Event.Sender != nil && e.Event.Sender.SenderId != nil {
				payload["sender"] = deref(e.Event.Sender.SenderId.OpenId)
			}
			evtType := "im.message"
			switch msgType {
			case "text":
				if msg.Content != nil {
					var c struct {
						Text string `json:"text"`
					}
					if json.Unmarshal([]byte(*msg.Content), &c) == nil {
						payload["text"] = c.Text
					}
				}
			default:
				// 图片/富文本/合并转发等:产生不支持事件,保证有明确回执而非静默(P2-2)
				evtType = "im.message.unsupported"
			}
			ig.Ingest(Event{
				Platform: "feishu", Type: evtType,
				SourceID: e.EventV2Base.Header.EventID, Payload: payload,
			})
			return nil
		})
	cli := larkws.NewClient(appID, appSecret,
		larkws.WithEventHandler(handler), larkws.WithLogLevel(larkcore.LogLevelError),
		larkws.WithLogger(stderrLarkLogger{})) // WS 运行期错误日志也走 stderr
	slog.Info("飞书 WS 长连接启动")
	h.StreamUp("feishu")
	if err := cli.Start(ctx); err != nil { // Start 阻塞;断线重连由 SDK 内置
		h.StreamDown("feishu", err.Error())
		return err
	}
	h.StreamDown("feishu", "连接关闭")
	return nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
