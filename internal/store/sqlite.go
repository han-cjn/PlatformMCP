// SQLite 持久化存储:Store 接口(映射+审计)、去重窗口、事件队列、已见会话缓存,
// 四者共用同一 DB 文件。驱动选用 modernc.org/sqlite(纯 Go,无 cgo,保持单静态二进制)。
package store

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// SQLiteStore 聚合所有持久化能力。
type SQLiteStore struct {
	db       *sql.DB
	dedupTTL time.Duration
}

// OpenSQLite 打开(必要时创建)DB 并执行建表迁移。
func OpenSQLite(path string, dedupTTL time.Duration) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	s := &SQLiteStore{db: db, dedupTTL: dedupTTL}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *SQLiteStore) Close() error { return s.db.Close() }

// DB 暴露底层 *sql.DB(测试与运维检查用)。
func (s *SQLiteStore) DB() *sql.DB { return s.db }

func (s *SQLiteStore) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS mappings(
			platform TEXT NOT NULL, entity_type TEXT NOT NULL, external_id TEXT NOT NULL,
			internal_id TEXT NOT NULL,
			PRIMARY KEY(platform, entity_type, external_id))`,
		`CREATE TABLE IF NOT EXISTS audits(
			id INTEGER PRIMARY KEY AUTOINCREMENT, time TEXT NOT NULL, tool TEXT, source TEXT,
			target TEXT, summary TEXT, succeeded INTEGER, err TEXT)`,
		// (platform, source_id) 主键兜底并发重复投递(P1-1)
		`CREATE TABLE IF NOT EXISTS dedup(
			platform TEXT NOT NULL, source_id TEXT NOT NULL, seen_at INTEGER NOT NULL,
			PRIMARY KEY(platform, source_id))`,
		`CREATE TABLE IF NOT EXISTS events(
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			platform TEXT NOT NULL, type TEXT NOT NULL, source_id TEXT NOT NULL,
			payload TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'pending',
			attempts INTEGER NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS idx_events_status ON events(status, updated_at, id)`,
		`CREATE TABLE IF NOT EXISTS chats(
			platform TEXT NOT NULL, chat_id TEXT NOT NULL, name TEXT NOT NULL DEFAULT '',
			last_seen INTEGER NOT NULL,
			PRIMARY KEY(platform, chat_id))`,
		// M3:通用 KV(水位线等)与日报文档映射(date -> file_id,幂等更新)
		`CREATE TABLE IF NOT EXISTS kv(
			key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_at INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS report_docs(
			date TEXT PRIMARY KEY, file_id TEXT NOT NULL, url TEXT NOT NULL DEFAULT '',
			updated_at INTEGER NOT NULL)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	return nil
}

// ---- Store 接口:ID 映射 + 审计(P1-6) ----

func (s *SQLiteStore) PutMapping(m Mapping) error {
	_, err := s.db.Exec(`INSERT INTO mappings(platform,entity_type,external_id,internal_id) VALUES(?,?,?,?)
		ON CONFLICT(platform,entity_type,external_id) DO UPDATE SET internal_id=excluded.internal_id`,
		m.Platform, m.EntityType, m.ExternalID, m.InternalID)
	return err
}

func (s *SQLiteStore) LookupInternalID(platform, entityType, externalID string) (string, bool) {
	var id string
	err := s.db.QueryRow(`SELECT internal_id FROM mappings WHERE platform=? AND entity_type=? AND external_id=?`,
		platform, entityType, externalID).Scan(&id)
	return id, err == nil
}

func (s *SQLiteStore) AppendAudit(r AuditRecord) error {
	succ := 0
	if r.Succeeded {
		succ = 1
	}
	_, err := s.db.Exec(`INSERT INTO audits(time,tool,source,target,summary,succeeded,err) VALUES(?,?,?,?,?,?,?)`,
		r.Time.UTC().Format(time.RFC3339), r.Tool, r.Source, r.Target, r.Summary, succ, r.Err)
	return err
}

func (s *SQLiteStore) Audits() []AuditRecord {
	rows, err := s.db.Query(`SELECT time,tool,source,target,summary,succeeded,err FROM audits ORDER BY id`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []AuditRecord
	for rows.Next() {
		var r AuditRecord
		var ts string
		var succ int
		if rows.Scan(&ts, &r.Tool, &r.Source, &r.Target, &r.Summary, &succ, &r.Err) == nil {
			r.Time, _ = time.Parse(time.RFC3339, ts)
			r.Succeeded = succ == 1
			out = append(out, r)
		}
	}
	return out
}

// ---- 去重窗口(P1-1):TTL 24h,重启后从 DB 恢复 ----

// Seen 实现 event.Dedup 语义:窗口内重复返回 true。
func (s *SQLiteStore) Seen(platform, sourceID string) bool {
	now := time.Now().Unix()
	var seenAt int64
	err := s.db.QueryRow(`SELECT seen_at FROM dedup WHERE platform=? AND source_id=?`, platform, sourceID).Scan(&seenAt)
	if err == nil && now-seenAt < int64(s.dedupTTL.Seconds()) {
		return true
	}
	_, err = s.db.Exec(`INSERT INTO dedup(platform,source_id,seen_at) VALUES(?,?,?)
		ON CONFLICT(platform,source_id) DO UPDATE SET seen_at=excluded.seen_at`,
		platform, sourceID, now)
	return err != nil // 写失败时按重复处理,宁可漏不可重
}

// PruneDedup 清理过期去重记录。
func (s *SQLiteStore) PruneDedup() {
	s.db.Exec(`DELETE FROM dedup WHERE seen_at<?`, time.Now().Add(-s.dedupTTL).Unix())
}

// ---- M3:KV(水位线)+ 日报文档映射(幂等) ----

// GetKV 读取键值;不存在返回 ok=false。
func (s *SQLiteStore) GetKV(key string) (string, bool) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM kv WHERE key=?`, key).Scan(&v)
	return v, err == nil
}

// SetKV 写入键值(upsert)。
func (s *SQLiteStore) SetKV(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO kv(key,value,updated_at) VALUES(?,?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		key, value, time.Now().Unix())
	return err
}

// ReportDoc 是「日期 -> 腾讯文档」的映射(同日重复触发更新同一篇,P4 幂等)。
type ReportDoc struct {
	Date   string
	FileID string
	URL    string
}

// GetReportDoc 查询某天的日报文档映射。
func (s *SQLiteStore) GetReportDoc(date string) (ReportDoc, bool) {
	var d ReportDoc
	d.Date = date
	err := s.db.QueryRow(`SELECT file_id,url FROM report_docs WHERE date=?`, date).Scan(&d.FileID, &d.URL)
	return d, err == nil
}

// PutReportDoc 记录/更新某天的日报文档映射。
func (s *SQLiteStore) PutReportDoc(date, fileID, url string) error {
	_, err := s.db.Exec(`INSERT INTO report_docs(date,file_id,url,updated_at) VALUES(?,?,?,?)
		ON CONFLICT(date) DO UPDATE SET file_id=excluded.file_id, url=excluded.url, updated_at=excluded.updated_at`,
		date, fileID, url, time.Now().Unix())
	return err
}

// ---- 事件队列(P0-2/P1-2):先落库再 ACK,worker 池消费 ----

// QueuedEvent 是落库后的事件记录。
type QueuedEvent struct {
	ID       int64
	Platform string
	Type     string
	SourceID string
	Payload  string
	Attempts int
}

// EnqueueEvent 事件落库,提交成功才返回(P0-2:适配器收到 200 即代表已持久化)。
func (s *SQLiteStore) EnqueueEvent(platform, typ, sourceID, payload string) (int64, error) {
	now := time.Now().Unix()
	res, err := s.db.Exec(`INSERT INTO events(platform,type,source_id,payload,status,created_at,updated_at) VALUES(?,?,?,?,'pending',?,?)`,
		platform, typ, sourceID, payload, now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ClaimPending 原子认领到期的待处理事件(attempts 越大退避越久)。
// 认领后状态置为 processing,处理期间不会被重复领取;崩溃残留由 RecoverProcessing 回收。
func (s *SQLiteStore) ClaimPending(limit int) ([]QueuedEvent, error) {
	now := time.Now().Unix()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(
		`SELECT id,platform,type,source_id,payload,attempts FROM events
		 WHERE status='pending' AND updated_at + attempts*2 <= ? ORDER BY id LIMIT ?`, now, limit)
	if err != nil {
		return nil, err
	}
	var out []QueuedEvent
	for rows.Next() {
		var e QueuedEvent
		if rows.Scan(&e.ID, &e.Platform, &e.Type, &e.SourceID, &e.Payload, &e.Attempts) == nil {
			out = append(out, e)
		}
	}
	rows.Close()
	for _, e := range out {
		if _, err := tx.Exec(`UPDATE events SET status='processing', updated_at=? WHERE id=?`, now, e.ID); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

// RecoverProcessing 启动时把残留的 processing(上次崩溃在途)重置回 pending 重新消费。
func (s *SQLiteStore) RecoverProcessing() {
	s.db.Exec(`UPDATE events SET status='pending' WHERE status='processing'`)
}

// MarkDone 标记处理成功。
func (s *SQLiteStore) MarkDone(id int64) {
	s.db.Exec(`UPDATE events SET status='done', updated_at=? WHERE id=?`, time.Now().Unix(), id)
}

// MarkRetry 记录失败并退避重试;超过 maxAttempts 进入 dead(死信,保留现场)。
func (s *SQLiteStore) MarkRetry(id int64, cause string, maxAttempts int) {
	now := time.Now().Unix()
	res, err := s.db.Exec(`UPDATE events SET status='pending', attempts=attempts+1, last_error=?, updated_at=? WHERE id=?`,
		cause, now, id)
	if err != nil {
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		s.db.Exec(`UPDATE events SET status='dead' WHERE id=? AND attempts>=?`, id, maxAttempts)
	}
}

// ---- 已见会话缓存(P0-3):企微/钉钉会话 ID 从消息帧自动沉淀 ----

// Chat 是一个平台会话。
type Chat struct {
	Platform string
	ChatID   string
	Name     string
	LastSeen time.Time
}

// NoteChat 记录/刷新一个已见会话。
func (s *SQLiteStore) NoteChat(platform, chatID, name string) {
	if chatID == "" {
		return
	}
	s.db.Exec(`INSERT INTO chats(platform,chat_id,name,last_seen) VALUES(?,?,?,?)
		ON CONFLICT(platform,chat_id) DO UPDATE SET
		name=CASE WHEN excluded.name='' THEN chats.name ELSE excluded.name END,
		last_seen=excluded.last_seen`,
		platform, chatID, name, time.Now().Unix())
}

// Chats 返回全部已见会话。
func (s *SQLiteStore) Chats() []Chat {
	rows, err := s.db.Query(`SELECT platform,chat_id,name,last_seen FROM chats ORDER BY last_seen DESC`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []Chat
	for rows.Next() {
		var c Chat
		var ts int64
		if rows.Scan(&c.Platform, &c.ChatID, &c.Name, &ts) == nil {
			c.LastSeen = time.Unix(ts, 0)
			out = append(out, c)
		}
	}
	return out
}
