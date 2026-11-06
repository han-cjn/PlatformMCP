// Package event 定义统一事件模型与分发器。
// 分发器(P1-2/P2-6):事件先落库(P0-2),worker 池异步消费,
// 单个 handler 出错/panic 不影响其他订阅者,失败重试并记录状态,超限进死信。
package event

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/example/mcp-hub/internal/store"
)

// Event 是平台无关的统一事件信封。
type Event struct {
	Platform string         `json:"platform"`  // feishu / dingtalk / wecom / tencent-docs
	Type     string         `json:"type"`      // im.message / im.message.unsupported / doc.updated / ...
	SourceID string         `json:"source_id"` // 平台侧业务唯一 ID(去重键):飞书 event_id、钉钉 msgId、企微 msgid
	Payload  map[string]any `json:"payload"`   // 归一化后的载荷:chat_id / sender / text / msg_type ...
	Received time.Time      `json:"received"`
	Attempts int            `json:"attempts"`
}

// Handler 处理事件;返回 error 会触发重试。
type Handler func(ctx context.Context, e Event) error

// Dedup 判定事件是否已见过(窗口由实现侧持有,默认 24h)。
type Dedup interface {
	Seen(platform, sourceID string) bool
}

// Dispatcher 是落库 + worker 池的持久化事件总线,实现 Bus 语义。
type Dispatcher struct {
	st          *store.SQLiteStore
	workers     int
	maxAttempts int

	mu       sync.RWMutex
	handlers []Handler

	notify  chan struct{}
	jobs    chan store.QueuedEvent
	stopCh  chan struct{}
	wg      sync.WaitGroup
	stopped atomic.Bool
}

// NewDispatcher workers 为并发消费数,maxAttempts 为重试上限(超出进 dead)。
func NewDispatcher(st *store.SQLiteStore, workers, maxAttempts int) *Dispatcher {
	if workers < 1 {
		workers = 4
	}
	if maxAttempts < 1 {
		maxAttempts = 3
	}
	return &Dispatcher{
		st: st, workers: workers, maxAttempts: maxAttempts,
		notify: make(chan struct{}, 1), jobs: make(chan store.QueuedEvent, workers*4),
		stopCh: make(chan struct{}),
	}
}

// Subscribe 注册处理器。
func (d *Dispatcher) Subscribe(h Handler) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.handlers = append(d.handlers, h)
}

// Publish 落库即返回(ACK 语义:调用方拿到 nil 代表事件已持久化,不会丢)。
func (d *Dispatcher) Publish(e Event) error {
	if d.stopped.Load() {
		return errors.New("dispatcher 已停止,拒绝接收新事件")
	}
	payload, _ := json.Marshal(e.Payload)
	if _, err := d.st.EnqueueEvent(e.Platform, e.Type, e.SourceID, string(payload)); err != nil {
		return err
	}
	d.noteChat(e)
	select {
	case d.notify <- struct{}{}:
	default:
	}
	return nil
}

// noteChat 沉淀事件来源会话(P0-3)。
func (d *Dispatcher) noteChat(e Event) {
	chatID, _ := e.Payload["chat_id"].(string)
	d.st.NoteChat(e.Platform, chatID, "")
}

// Start 启动调度循环与 worker 池。
func (d *Dispatcher) Start(ctx context.Context) {
	for i := 0; i < d.workers; i++ {
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			for ev := range d.jobs {
				d.dispatchOne(ev)
			}
		}()
	}
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-d.stopCh:
				close(d.jobs)
				return
			case <-ctx.Done():
				close(d.jobs)
				return
			case <-d.notify:
				d.claim()
			case <-ticker.C:
				d.claim()
			}
		}
	}()
}

// Stop 停止接收新事件,等待在途任务完成(带超时),实现优雅退出(P2-7)。
func (d *Dispatcher) Stop(timeout time.Duration) {
	d.stopped.Store(true)
	close(d.stopCh)
	done := make(chan struct{})
	go func() { d.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
		slog.Warn("dispatcher 停止超时,有任务未收尾", "timeout", timeout)
	}
}

// claim 领取到期事件投喂 worker。
func (d *Dispatcher) claim() {
	evs, err := d.st.ClaimPending(d.workers * 2)
	if err != nil {
		slog.Error("领取待处理事件失败", "err", err)
		return
	}
	for _, ev := range evs {
		select {
		case d.jobs <- ev:
		default: // jobs 满,下轮再取
			return
		}
	}
}

// dispatchOne 隔离执行所有订阅者(P2-6):任一 handler 出错/panic 不影响其他。
func (d *Dispatcher) dispatchOne(ev store.QueuedEvent) {
	var evt Event
	evt.Platform, evt.Type, evt.SourceID, evt.Attempts = ev.Platform, ev.Type, ev.SourceID, ev.Attempts
	evt.Received = time.Now()
	if err := json.Unmarshal([]byte(ev.Payload), &evt.Payload); err != nil {
		d.st.MarkRetry(ev.ID, "payload 反序列化失败: "+err.Error(), d.maxAttempts)
		return
	}
	var errs []string
	d.mu.RLock()
	handlers := append([]Handler(nil), d.handlers...)
	d.mu.RUnlock()
	for _, h := range handlers {
		func() {
			defer func() {
				if r := recover(); r != nil {
					errs = append(errs, "handler panic")
					slog.Error("handler panic 已隔离", "event_id", ev.ID, "panic", r)
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := h(ctx, evt); err != nil {
				errs = append(errs, err.Error())
				slog.Error("handler 处理失败(不影响其他订阅者)", "event_id", ev.ID, "err", err)
			}
		}()
	}
	if len(errs) > 0 {
		d.st.MarkRetry(ev.ID, strings.Join(errs, "; "), d.maxAttempts)
		return
	}
	d.st.MarkDone(ev.ID)
}

// Normalize 统一事件结构;未知平台原样透传,未知类型保持。
func Normalize(e *Event) {
	if e.Received.IsZero() {
		e.Received = time.Now()
	}
	if e.Payload == nil {
		e.Payload = map[string]any{}
	}
	// 会话 ID 归一:企微 chatid / 飞书 chat_id / 钉钉 openConversationId -> chat_id
	for _, k := range []string{"chatid", "openConversationId", "conversationId"} {
		if v, ok := e.Payload[k].(string); ok && v != "" {
			if _, exists := e.Payload["chat_id"]; !exists {
				e.Payload["chat_id"] = v
			}
		}
	}
	// 企微 text 是对象 {content};飞书 text 为字符串;钉钉 text.content
	if t, ok := e.Payload["text"].(map[string]any); ok {
		if c, ok := t["content"].(string); ok {
			e.Payload["text"] = c
		}
	}
	// 发送者归一:企微 from.userid / 飞书 sender_id / 钉钉 senderStaffId -> sender
	if _, ok := e.Payload["sender"]; !ok {
		for _, k := range []string{"sender_id", "senderStaffId", "from"} {
			switch v := e.Payload[k].(type) {
			case string:
				e.Payload["sender"] = v
			case map[string]any:
				if id, ok := v["userid"].(string); ok {
					e.Payload["sender"] = id
				}
			}
		}
	}
}
