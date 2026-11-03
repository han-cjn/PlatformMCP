// Package store 提供 ID 映射中心与审计日志的存储抽象。
// 骨架阶段为内存实现;M2/M4 切换 SQLite(GORM),接口不变。
package store

import (
	"sync"
	"time"
)

// Mapping 平台实体 ID <-> 统一内部 ID 映射。
type Mapping struct {
	InternalID string
	Platform   string // feishu / dingtalk / wecom / tencent-docs
	EntityType string // user / group / doc / table
	ExternalID string
}

// AuditRecord 跨平台写操作审计。
type AuditRecord struct {
	Time      time.Time
	Tool      string // 统一语义工具名
	Source    string // 源平台
	Target    string // 目标平台
	Summary   string // payload 摘要(不落敏感正文)
	Succeeded bool
	Err       string
}

// Store 聚合映射与审计两组接口。
type Store interface {
	PutMapping(m Mapping) error
	LookupInternalID(platform, entityType, externalID string) (string, bool)
	AppendAudit(r AuditRecord) error
	Audits() []AuditRecord
}

type memoryStore struct {
	mu       sync.Mutex
	mappings map[string]string // platform|type|externalID -> internalID
	audits   []AuditRecord
}

func NewMemoryStore() Store {
	return &memoryStore{mappings: make(map[string]string)}
}

func (s *memoryStore) PutMapping(m Mapping) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mappings[m.Platform+"|"+m.EntityType+"|"+m.ExternalID] = m.InternalID
	return nil
}

func (s *memoryStore) LookupInternalID(platform, entityType, externalID string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.mappings[platform+"|"+entityType+"|"+externalID]
	return id, ok
}

func (s *memoryStore) AppendAudit(r AuditRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audits = append(s.audits, r)
	return nil
}

func (s *memoryStore) Audits() []AuditRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]AuditRecord(nil), s.audits...)
}
