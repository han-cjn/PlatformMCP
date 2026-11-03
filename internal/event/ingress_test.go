package event

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/example/mcp-hub/internal/store"
)

func newTestStack(t *testing.T) (*store.SQLiteStore, *Dispatcher, *Ingress) {
	t.Helper()
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "t.db"), 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	disp := NewDispatcher(st, 2, 2)
	ig := NewIngress(disp, st, "test-secret")
	return st, disp, ig
}

// P1-2 关键验收:Ingest 返回前事件必须已落库(落库即 ACK)。
func TestIngestPersistsBeforeReturn(t *testing.T) {
	st, _, ig := newTestStack(t)
	err := ig.Ingest(Event{Platform: "wecom", Type: "im.message", SourceID: "m1", Payload: map[string]any{"text": "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	evs, _ := st.ClaimPending(10)
	if len(evs) != 1 || evs[0].SourceID != "m1" {
		t.Fatalf("Ingest 返回后事件应已在库: %+v", evs)
	}
}

func TestIngestDuplicate(t *testing.T) {
	_, _, ig := newTestStack(t)
	e := Event{Platform: "wecom", Type: "im.message", SourceID: "m1"}
	if err := ig.Ingest(e); err != nil {
		t.Fatal(err)
	}
	if err := ig.Ingest(e); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("重复事件应返回 ErrDuplicate, 实际: %v", err)
	}
}

func TestIngestValidation(t *testing.T) {
	_, _, ig := newTestStack(t)
	if err := ig.Ingest(Event{Type: "im.message", SourceID: "m1"}); err == nil {
		t.Fatal("空 platform 应被拒绝")
	}
	if err := ig.Ingest(Event{Platform: "wecom", Type: "im.message"}); err == nil {
		t.Fatal("空 source_id 应被拒绝")
	}
}

// P2-1:HTTP 端点鉴权。
func TestHTTPAuth(t *testing.T) {
	_, _, ig := newTestStack(t)
	srv := httptest.NewServer(ig)
	defer srv.Close()
	body := `{"platform":"wecom","type":"im.message","source_id":"h1","payload":{"text":"x"}}`

	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无密钥头应 401, 实际 %d", resp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(body))
	req.Header.Set("X-Hub-Secret", "test-secret")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusAccepted {
		t.Fatalf("带密钥头应 202, 实际 %d", resp2.StatusCode)
	}
}

// P2-6:单个 handler 出错/panic 不影响其他订阅者;失败被记录并重试。
func TestDispatchIsolationAndRetry(t *testing.T) {
	st, disp, ig := newTestStack(t)
	ctx := t.Context()
	disp.Start(ctx)
	defer disp.Stop(3 * time.Second)

	ran := make(chan struct{}, 1)
	disp.Subscribe(func(_ctx context.Context, _ Event) error {
		panic("boom") // panic 必须被隔离
	})
	disp.Subscribe(func(_ctx context.Context, _ Event) error {
		select {
		case ran <- struct{}{}:
		default:
		}
		return nil
	})

	if err := ig.Ingest(Event{Platform: "wecom", Type: "im.message", SourceID: "iso-1"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ran:
	case <-time.After(3 * time.Second):
		t.Fatal("panic 的 handler 不应阻断其他订阅者")
	}
	// 等待重试耗尽(2 次)→ dead
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		var status string
		row := st.DB().QueryRow(`SELECT status FROM events WHERE source_id='iso-1'`)
		if row.Scan(&status) == nil && status == "dead" {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatal("持续失败的事件最终应进入 dead 死信状态")
}
