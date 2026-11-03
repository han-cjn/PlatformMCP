package event

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/open-dingtalk/dingtalk-stream-sdk-go/chatbot"
	"github.com/open-dingtalk/dingtalk-stream-sdk-go/client"

	"github.com/example/mcp-hub/internal/health"
)

// StartDingTalkStream 钉钉事件订阅(Stream 长连接,免公网回调)。
// P2-3:goroutine panic recover + 指数退避重启,单次异常不再带走整个进程。
func StartDingTalkStream(ctx context.Context, clientID, clientSecret string, ig *Ingress, h *health.Registry) error {
	go func() {
		backoff := time.Second
		for {
			if ctx.Err() != nil {
				return
			}
			err := runDingTalkStream(ctx, clientID, clientSecret, ig, h)
			if ctx.Err() != nil {
				h.StreamDown("dingtalk", "进程退出")
				return
			}
			h.StreamDown("dingtalk", errString(err))
			slog.Error("钉钉 Stream 退出,退避重连", "err", err, "backoff", backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
		}
	}()
	return nil
}

// runDingTalkStream 单次连接生命周期;panic 被 recover 转为错误交给上层退避。
func runDingTalkStream(ctx context.Context, clientID, clientSecret string, ig *Ingress, h *health.Registry) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	cli := client.NewStreamClient(client.WithAppCredential(client.NewAppCredentialConfig(clientID, clientSecret)))
	cli.RegisterChatBotCallbackRouter(func(_ context.Context, c *chatbot.BotCallbackDataModel) ([]byte, error) {
		h.TouchEvent("dingtalk")
		payload := map[string]any{
			"chat_id": c.ConversationId,
			"sender":  c.SenderStaffId,
			"text":    c.Text.Content,
		}
		ig.Ingest(Event{
			Platform: "dingtalk", Type: "im.message",
			SourceID: c.MsgId, Payload: payload,
		})
		return []byte("ok"), nil
	})
	slog.Info("钉钉 Stream 长连接启动")
	h.StreamUp("dingtalk")
	// ctx 取消时主动关闭连接
	go func() {
		<-ctx.Done()
		cli.Close()
	}()
	// 注意:SDK 的 Start 在连接建立后即返回(nil),连接保活在 SDK 内部 goroutine;
	// 返回 nil 不代表断开,阻塞等待 ctx 取消,避免误判退出导致的重连风暴。
	if err := cli.Start(ctx); err != nil {
		return err
	}
	<-ctx.Done()
	return nil
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
