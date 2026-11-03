package event

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
)

// ErrDuplicate 表示重复事件(窗口内已见过)。
var ErrDuplicate = errors.New("duplicate event")

// Ingress 事件接入:归一化 -> 去重 -> 落库(落库成功即 ACK,handler 异步消费)。
type Ingress struct {
	disp   *Dispatcher
	dedup  Dedup
	secret string // HUB_EVENT_SECRET;为空表示不校验(仅监听回环时默认可接受)
}

func NewIngress(disp *Dispatcher, dedup Dedup, secret string) *Ingress {
	return &Ingress{disp: disp, dedup: dedup, secret: secret}
}

// Ingest 同步部分只做归一化+去重+落库,快速返回,不阻塞长连接回调(P1-2)。
func (ig *Ingress) Ingest(e Event) error {
	Normalize(&e)
	if e.Platform == "" || e.SourceID == "" {
		return errors.New("platform/source_id required")
	}
	if ig.dedup.Seen(e.Platform, e.SourceID) {
		slog.Info("重复事件已丢弃", "platform", e.Platform, "source_id", e.SourceID)
		return ErrDuplicate
	}
	if err := ig.disp.Publish(e); err != nil {
		return err
	}
	slog.Info("事件已落库", "platform", e.Platform, "type", e.Type, "source_id", e.SourceID)
	return nil
}

// ServeHTTP 暴露 HTTP 端点供进程外适配器(企微)上报。
// P2-1:配置 HUB_EVENT_SECRET 时强制校验 X-Hub-Secret 头。
func (ig *Ingress) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if ig.secret != "" && r.Header.Get("X-Hub-Secret") != ig.secret {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var e Event
	if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	Normalize(&e)
	if e.Platform == "" || e.SourceID == "" {
		http.Error(w, "platform/source_id required", http.StatusBadRequest)
		return
	}
	if err := ig.Ingest(e); errors.Is(err, ErrDuplicate) {
		w.WriteHeader(http.StatusOK) // 重复也返回 200,避免上游重投风暴
		return
	} else if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// NewHTTPServer 构造事件接收端点。secret 为空时强制绑定回环并提示风险(P2-1)。
// 返回 *http.Server 以便调用方做优雅退出(P2-7)。
func NewHTTPServer(addr string, ig *Ingress) (*http.Server, error) {
	if ig.secret == "" && !strings.HasPrefix(addr, "127.0.0.1") && !strings.HasPrefix(addr, "localhost") {
		return nil, errors.New("未配置 HUB_EVENT_SECRET 时事件端点只允许绑定回环地址")
	}
	if ig.secret == "" {
		slog.Warn("HUB_EVENT_SECRET 未配置:/events 无鉴权,仅监听回环,本机任意进程可注入事件")
	}
	return &http.Server{Addr: addr, Handler: ig}, nil
}
