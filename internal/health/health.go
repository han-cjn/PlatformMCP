// Package health 维护各平台运行状态(凭据/挂载/长连接/限流),
// 供启动体检表与 hub.health 元工具输出(P1-3/P1-4/P1-5/P2-5)。
package health

import (
	"sync"
	"time"
)

// PlatformStatus 单平台状态快照。
type PlatformStatus struct {
	Credentials bool      `json:"credentials"`   // 凭据是否齐备
	Mounted     bool      `json:"mounted"`       // MCP 是否挂载成功
	Stream      string    `json:"stream"`        // 长连接:up / down / -(无)
	LastEventAt time.Time `json:"last_event_at"` // 最近一次收到事件时间
	LastError   string    `json:"last_error"`
	Calls       int64     `json:"calls"`        // 下游调用计数
	LimitErrors int64     `json:"limit_errors"` // 429/配额类错误计数(P2-5)
	Degraded    []string  `json:"degraded"`     // 降级原因清单(P1-3)
}

// Registry 线程安全的平台状态登记表。
type Registry struct {
	mu sync.RWMutex
	m  map[string]*PlatformStatus
}

func NewRegistry() *Registry {
	return &Registry{m: make(map[string]*PlatformStatus)}
}

func (r *Registry) forPlatform(p string) *PlatformStatus {
	st, ok := r.m[p]
	if !ok {
		st = &PlatformStatus{Stream: "-"}
		r.m[p] = st
	}
	return st
}

func (r *Registry) SetCredentials(p string, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.forPlatform(p).Credentials = ok
}

func (r *Registry) SetMounted(p string, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.forPlatform(p).Mounted = ok
}

func (r *Registry) StreamUp(p string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.forPlatform(p).Stream = "up"
}

func (r *Registry) StreamDown(p, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.forPlatform(p)
	st.Stream = "down"
	st.LastError = reason
}

func (r *Registry) TouchEvent(p string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.forPlatform(p).LastEventAt = time.Now()
}

func (r *Registry) AddCall(p string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.forPlatform(p).Calls++
}

// AddLimitError 记录限流/配额类错误(P2-5)。
func (r *Registry) AddLimitError(p string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.forPlatform(p)
	st.LimitErrors++
	st.LastError = err.Error()
}

func (r *Registry) SetDegraded(p string, reasons []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.forPlatform(p).Degraded = reasons
}

func (r *Registry) SetError(p, msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.forPlatform(p).LastError = msg
}

// Snapshot 返回全部平台状态副本。
func (r *Registry) Snapshot() map[string]PlatformStatus {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]PlatformStatus, len(r.m))
	for k, v := range r.m {
		out[k] = *v
	}
	return out
}
