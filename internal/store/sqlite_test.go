package store

import (
	"path/filepath"
	"testing"
	"time"
)

func openTemp(t *testing.T, ttl time.Duration) (*SQLiteStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := OpenSQLite(path, ttl)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return s, path
}

func TestDedupWindow(t *testing.T) {
	s, _ := openTemp(t, 24*time.Hour)
	defer s.Close()
	if s.Seen("wecom", "msg-1") {
		t.Fatal("首次不应判重")
	}
	if !s.Seen("wecom", "msg-1") {
		t.Fatal("窗口内重复应判重")
	}
	if s.Seen("wecom", "msg-2") {
		t.Fatal("不同 source_id 不应判重")
	}
	if s.Seen("dingtalk", "msg-1") {
		t.Fatal("不同平台不应判重")
	}
}

func TestDedupPersistsAcrossRestart(t *testing.T) {
	s, path := openTemp(t, 24*time.Hour)
	s.Seen("feishu", "evt-1")
	s.Close()

	s2, err := OpenSQLite(path, 24*time.Hour)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	if !s2.Seen("feishu", "evt-1") {
		t.Fatal("重启后去重窗口应保留(P1-1)")
	}
}

func TestDedupExpired(t *testing.T) {
	s, _ := openTemp(t, time.Millisecond)
	defer s.Close()
	s.Seen("wecom", "old")
	time.Sleep(5 * time.Millisecond)
	if s.Seen("wecom", "old") {
		t.Fatal("过期事件应允许重新处理")
	}
}

func TestEventQueueLifecycle(t *testing.T) {
	s, _ := openTemp(t, 24*time.Hour)
	defer s.Close()
	id, err := s.EnqueueEvent("wecom", "im.message", "m1", `{"text":"#记录 x"}`)
	if err != nil || id == 0 {
		t.Fatalf("enqueue: %v id=%d", err, id)
	}
	evs, err := s.ClaimPending(10)
	if err != nil || len(evs) != 1 {
		t.Fatalf("claim: %v n=%d", err, len(evs))
	}
	// 认领后处于 processing,处理期间不得被重复领取(防重复消费回归)
	if again, _ := s.ClaimPending(10); len(again) != 0 {
		t.Fatal("processing 中的事件不应被重复领取")
	}
	s.MarkRetry(evs[0].ID, "boom", 3)
	// 退避中(attempts=1 → 2s 内不可领取)
	if evs, _ := s.ClaimPending(10); len(evs) != 0 {
		t.Fatal("退避窗口内不应被领取")
	}
	s.MarkDone(id)
	if evs, _ := s.ClaimPending(10); len(evs) != 0 {
		t.Fatal("done 事件不应再被领取")
	}
}

func TestAuditAndChats(t *testing.T) {
	s, _ := openTemp(t, 24*time.Hour)
	defer s.Close()
	_ = s.AppendAudit(AuditRecord{Time: time.Now(), Tool: "im.send_text", Target: "wecom", Succeeded: true})
	if got := s.Audits(); len(got) != 1 || got[0].Tool != "im.send_text" {
		t.Fatalf("audit roundtrip: %+v", got)
	}
	s.NoteChat("wecom", "chat-1", "")
	s.NoteChat("wecom", "chat-1", "群名")
	chats := s.Chats()
	if len(chats) != 1 || chats[0].Name != "群名" {
		t.Fatalf("chats upsert: %+v", chats)
	}
}
