// Package report 实现 M3 场景 B:每日定时汇总飞书 Bitable 新增记录,
// 生成腾讯文档日报(同日幂等更新),推送链接到配置的目标群。
//
// 可靠性语义:
//   - 水位线(上次成功时间)持久化在 SQLite kv 表,重启不丢;首次运行回填 backfill_hours
//   - 同日重复触发更新同一篇文档(date -> file_id 映射落 report_docs 表)
//   - 无新增记录时不生成空日报、不推送,审计留 skipped
//   - 错误分类:limit(429/配额,不重试)/ config / transient(有限重试)
//   - 每次运行(含失败/跳过)写审计,可查「哪天、多少条、文档 token、推送结果」
package report

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/example/mcp-hub/internal/adapter"
	"github.com/example/mcp-hub/internal/config"
	"github.com/example/mcp-hub/internal/semantic"
	"github.com/example/mcp-hub/internal/store"
)

// watermarkKey 是 kv 表中水位线的键。
const watermarkKey = "report.last_success_unix"

// Record 是一条归一化后的 Bitable 记录。
type Record struct {
	Content   string
	Platform  string // 来源平台
	Sender    string
	CreatedAt time.Time
}

// Downstream 抽象下游调用,便于单测(真实实现走 MCP,测试用 fake)。
type Downstream interface {
	// FetchRecords 拉取表内全部记录(含创建时间);增量过滤由 Pipeline 完成。
	FetchRecords(ctx context.Context) ([]Record, error)
	// CreateDoc 用 Markdown 新建文档,返回 file_id 与可打开链接。
	CreateDoc(ctx context.Context, title, markdown string) (fileID, url string, err error)
	// UpdateDoc 在同一篇文档顶部插入最新快照(远程 MCP 无整篇覆写 API,
	// 同日幂等 = 同一 file_id 不变、链接不变,顶部始终是最新数据)。
	UpdateDoc(ctx context.Context, fileID, markdown string) error
	// Push 向指定平台会话发文本。
	Push(ctx context.Context, platform, chatID, text string) error
}

// ErrKind 错误分类。
type ErrKind string

const (
	KindLimit     ErrKind = "limit"     // 429/配额:不盲目重试
	KindConfig    ErrKind = "config"    // 配置缺失:重试无意义
	KindTransient ErrKind = "transient" // 其他:有限重试
)

// Result 一次运行的完整结果(也会落入审计)。
type Result struct {
	Date      string            `json:"date"`
	Trigger   string            `json:"trigger"` // cron / manual
	Status    string            `json:"status"`  // success / skipped / failed
	Kind      string            `json:"kind,omitempty"`
	Records   int               `json:"records"`
	FileID    string            `json:"file_id,omitempty"`
	URL       string            `json:"url,omitempty"`
	Pushed    map[string]string `json:"pushed,omitempty"` // platform -> ok / err
	Err       string            `json:"err,omitempty"`
	Attempts  int               `json:"attempts"`
	Watermark string            `json:"watermark"` // 本次窗口起点
}

// Pipeline 日报联动主流程。
type Pipeline struct {
	St          *store.SQLiteStore
	Down        Downstream
	Cfg         config.ReportConfig
	PushTargets map[string]string
	loc         *time.Location
	maxRetries  int
}

// NewPipeline 构造;tz 为空用 Asia/Shanghai。
func NewPipeline(st *store.SQLiteStore, down Downstream, cfg config.ReportConfig, pushTargets map[string]string) *Pipeline {
	locName := cfg.Timezone
	if locName == "" {
		locName = "Asia/Shanghai"
	}
	loc, err := time.LoadLocation(locName)
	if err != nil {
		slog.Error("日报时区非法,回退本地时区", "tz", locName, "err", err)
		loc = time.Local
	}
	if cfg.BackfillHours <= 0 {
		cfg.BackfillHours = 24
	}
	if cfg.CreatedField == "" {
		cfg.CreatedField = "创建时间"
	}
	if len(cfg.PushPlatforms) == 0 {
		cfg.PushPlatforms = []string{"wecom"}
	}
	return &Pipeline{St: st, Down: down, Cfg: cfg, PushTargets: pushTargets, loc: loc, maxRetries: 2}
}

// Location 暴露调度用時区。
func (p *Pipeline) Location() *time.Location { return p.loc }

// Run 执行一次日报流程(now 决定归属日期;trigger 记录触发来源)。
// 幂等:同一天重复运行更新同一篇文档;整体失败做有限重试,limit 类错误不重试。
func (p *Pipeline) Run(ctx context.Context, now time.Time, trigger string) Result {
	date := now.In(p.loc).Format("2006-01-02")
	res := Result{Date: date, Trigger: trigger, Pushed: map[string]string{}}
	var lastErr error
	for attempt := 0; attempt <= p.maxRetries; attempt++ {
		res.Attempts = attempt + 1
		lastErr = p.runOnce(ctx, now, &res)
		if lastErr == nil {
			break
		}
		kind := classify(lastErr)
		res.Kind = string(kind)
		slog.Error("日报运行失败", "date", date, "attempt", attempt+1, "kind", kind, "err", lastErr)
		if kind != KindTransient {
			break // limit/config 不盲目重试
		}
		select {
		case <-ctx.Done():
			lastErr = ctx.Err()
			break
		case <-time.After(time.Duration(attempt+1) * 2 * time.Second):
		}
	}
	if lastErr != nil {
		res.Status = "failed"
		res.Err = lastErr.Error()
	}
	p.audit(&res)
	return res
}

// runOnce 单次尝试;成功时推进水位线。
func (p *Pipeline) runOnce(ctx context.Context, now time.Time, res *Result) error {
	// 1. 水位线:默认上次成功时间;首次回填 backfill_hours
	since := now.Add(-time.Duration(p.Cfg.BackfillHours) * time.Hour)
	if v, ok := p.St.GetKV(watermarkKey); ok {
		var ts int64
		if _, err := fmt.Sscanf(v, "%d", &ts); err == nil && ts > 0 {
			since = time.Unix(ts, 0)
		}
	}
	res.Watermark = since.In(p.loc).Format(time.DateTime)

	// 2. 增量拉取
	all, err := p.Down.FetchRecords(ctx)
	if err != nil {
		return fmt.Errorf("拉取 Bitable 记录: %w", err)
	}
	var fresh []Record
	for _, r := range all {
		if r.CreatedAt.After(since) {
			fresh = append(fresh, r)
		}
	}
	sort.Slice(fresh, func(i, j int) bool { return fresh[i].CreatedAt.Before(fresh[j].CreatedAt) })
	res.Records = len(fresh)

	// 3. 空数据:不生成空日报、不推送;水位线照样推进(避免下次再扫空窗口)
	if len(fresh) == 0 {
		res.Status = "skipped"
		p.advanceWatermark(now)
		slog.Info("日报跳过:窗口内无新增记录", "date", res.Date, "since", res.Watermark)
		return nil
	}

	// 4. 生成 Markdown 日报
	title := fmt.Sprintf("群聊记录日报 %s", res.Date)
	md := buildMarkdown(res.Date, now.In(p.loc), fresh, res.Watermark)

	// 5. 落文档:同日已建则更新同一篇(幂等),否则新建并落映射
	if doc, ok := p.St.GetReportDoc(res.Date); ok && doc.FileID != "" {
		if err := p.Down.UpdateDoc(ctx, doc.FileID, md); err != nil {
			return fmt.Errorf("更新腾讯文档: %w", err)
		}
		res.FileID, res.URL = doc.FileID, normalizeURL(doc.URL)
		slog.Info("日报已更新(同一篇)", "date", res.Date, "file_id", doc.FileID)
	} else {
		fileID, url, err := p.Down.CreateDoc(ctx, title, md)
		if err != nil {
			return fmt.Errorf("创建腾讯文档: %w", err)
		}
		if fileID == "" {
			return fmt.Errorf("创建腾讯文档: 响应未包含 file_id")
		}
		if err := p.St.PutReportDoc(res.Date, fileID, url); err != nil {
			return fmt.Errorf("落文档映射: %w", err)
		}
		res.FileID, res.URL = fileID, url
		slog.Info("日报已创建", "date", res.Date, "file_id", fileID, "url", url)
	}

	// 6. 推送目标群(默认企微;任一平台失败整体视为 transient 可重试)
	link := res.URL
	if link == "" {
		link = res.FileID
	}
	msg := fmt.Sprintf("📋 群聊记录日报(%s)\n新增记录:%d 条(窗口起点 %s)\n链接:%s", res.Date, res.Records, res.Watermark, link)
	var pushErrs []string
	for _, platform := range p.Cfg.PushPlatforms {
		chatID := p.PushTargets[platform]
		if chatID == "" {
			res.Pushed[platform] = "skipped: 未配置 push_targets." + platform
			continue
		}
		if err := p.Down.Push(ctx, platform, chatID, msg); err != nil {
			res.Pushed[platform] = "failed: " + err.Error()
			pushErrs = append(pushErrs, platform+": "+err.Error())
			slog.Error("日报推送失败", "platform", platform, "err", err)
		} else {
			res.Pushed[platform] = "ok"
			slog.Info("日报已推送", "platform", platform, "chat_id", chatID)
		}
	}
	if len(pushErrs) > 0 {
		return fmt.Errorf("推送失败: %s", strings.Join(pushErrs, "; "))
	}

	// 7. 全部成功:推进水位线
	p.advanceWatermark(now)
	res.Status = "success"
	return nil
}

// advanceWatermark 推进水位线(持久化,重启不丢)。
func (p *Pipeline) advanceWatermark(t time.Time) {
	if err := p.St.SetKV(watermarkKey, fmt.Sprintf("%d", t.Unix())); err != nil {
		slog.Error("水位线写入失败", "err", err)
	}
}

// audit 每次运行留审计(含 skipped/failed),可查「哪天/多少条/token/推送结果」。
func (p *Pipeline) audit(res *Result) {
	summary, _ := json.Marshal(res)
	if err := p.St.AppendAudit(store.AuditRecord{
		Time: time.Now(), Tool: "report.daily", Target: res.Date,
		Summary: string(summary), Succeeded: res.Status != "failed", Err: res.Err,
	}); err != nil {
		slog.Error("日报审计写入失败", "err", err)
	}
}

// classify 错误分类:limit 不重试,config 不重试,其余 transient 有限重试。
func classify(err error) ErrKind {
	msg := err.Error()
	if adapter.IsLimitError(msg) {
		return KindLimit
	}
	if strings.Contains(msg, "未配置") || strings.Contains(msg, "未包含") {
		return KindConfig
	}
	return KindTransient
}

// buildMarkdown 生成日报正文:条数概览 + 按来源平台/操作人分组 + 明细列表。
func buildMarkdown(date string, generatedAt time.Time, records []Record, since string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "> 自动生成于 %s,统计窗口:%s 之后,共 **%d** 条新增记录\n\n",
		generatedAt.Format(time.DateTime), since, len(records))

	byPlatform := map[string][]Record{}
	for _, r := range records {
		byPlatform[r.Platform] = append(byPlatform[r.Platform], r)
	}
	platforms := make([]string, 0, len(byPlatform))
	for k := range byPlatform {
		platforms = append(platforms, k)
	}
	sort.Strings(platforms)

	b.WriteString("## 概览(按来源平台)\n\n| 来源平台 | 条数 |\n|---|---|\n")
	for _, pl := range platforms {
		fmt.Fprintf(&b, "| %s | %d |\n", pl, len(byPlatform[pl]))
	}
	b.WriteString("\n")

	for _, pl := range platforms {
		fmt.Fprintf(&b, "## %s(%d 条)\n\n", pl, len(byPlatform[pl]))
		bySender := map[string][]Record{}
		for _, r := range byPlatform[pl] {
			bySender[r.Sender] = append(bySender[r.Sender], r)
		}
		senders := make([]string, 0, len(bySender))
		for k := range bySender {
			senders = append(senders, k)
		}
		sort.Strings(senders)
		for _, sender := range senders {
			fmt.Fprintf(&b, "### %s(%d 条)\n\n", sender, len(bySender[sender]))
			for _, r := range bySender[sender] {
				content := strings.ReplaceAll(strings.TrimSpace(r.Content), "\n", " ")
				fmt.Fprintf(&b, "- %s %s\n", r.CreatedAt.Format("15:04"), content)
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

// ---- MCP 实现 ----

// MCPDownstream 走已挂载 MCP 工具的真实下游。
type MCPDownstream struct {
	Mgr      *adapter.Manager
	Reg      *semantic.Registry
	AppToken string
	TableID  string
	// CreatedField 创建时间字段名(配合 automatic_fields)。
	CreatedField string
}

// FetchRecords 分页拉取 Bitable 全部记录(automatic_fields 带出创建时间)。
func (d *MCPDownstream) FetchRecords(ctx context.Context) ([]Record, error) {
	var out []Record
	pageToken := ""
	for {
		params := map[string]any{"page_size": 100}
		if pageToken != "" {
			params["page_token"] = pageToken
		}
		resp, err := d.Mgr.CallTool(ctx, "feishu", "bitable_v1_appTableRecord_search", map[string]any{
			"path":   map[string]any{"app_token": d.AppToken, "table_id": d.TableID},
			"params": params,
			"data":   map[string]any{"automatic_fields": true},
		})
		if err != nil {
			return nil, err
		}
		// 兼容两种返回:根级 {items,has_more,page_token} 或包一层 data;created_time 在记录顶层
		var parsed struct {
			HasMore   bool   `json:"has_more"`
			PageToken string `json:"page_token"`
			Items     []struct {
				Fields      map[string]any `json:"fields"`
				CreatedTime int64          `json:"created_time"`
			} `json:"items"`
			Data struct {
				HasMore   bool   `json:"has_more"`
				PageToken string `json:"page_token"`
				Items     []struct {
					Fields      map[string]any `json:"fields"`
					CreatedTime int64          `json:"created_time"`
				} `json:"items"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(resp), &parsed); err != nil {
			return nil, fmt.Errorf("解析 Bitable 响应: %w", err)
		}
		items := parsed.Items
		hasMore, nextToken := parsed.HasMore, parsed.PageToken
		if len(parsed.Data.Items) > 0 || parsed.Data.HasMore {
			items, hasMore, nextToken = parsed.Data.Items, parsed.Data.HasMore, parsed.Data.PageToken
		}
		for _, it := range items {
			created := time.UnixMilli(it.CreatedTime)
			if it.CreatedTime <= 0 {
				created = fieldTime(it.Fields, d.CreatedField)
			}
			out = append(out, Record{
				Content:   fieldString(it.Fields, "内容"),
				Platform:  fieldString(it.Fields, "来源平台"),
				Sender:    fieldString(it.Fields, "发送者"),
				CreatedAt: created,
			})
		}
		if !hasMore || nextToken == "" {
			break
		}
		pageToken = nextToken
	}
	return out, nil
}

// CreateDoc 经统一语义层 doc.create 创建 Markdown 文档,解析响应中的 file_id/url。
func (d *MCPDownstream) CreateDoc(ctx context.Context, title, markdown string) (string, string, error) {
	tool, err := d.Reg.Resolve("doc.create", "tencent-docs")
	if err != nil {
		return "", "", err
	}
	resp, err := d.Mgr.CallTool(ctx, "tencent-docs", tool, map[string]any{
		"title":           title,
		"base64_markdown": base64.StdEncoding.EncodeToString([]byte(markdown)),
	})
	if err != nil {
		return "", "", err
	}
	return parseDocRef(resp)
}

// UpdateDoc 在同篇文档顶部插入最新快照(file_id 不变,链接不变)。
func (d *MCPDownstream) UpdateDoc(ctx context.Context, fileID, markdown string) error {
	_, err := d.Mgr.CallTool(ctx, "tencent-docs", "doc.insert_markdown", map[string]any{
		"file_id":  fileID,
		"idx":      0,
		"markdown": fmt.Sprintf("## 🔄 更新于 %s\n\n%s", time.Now().Format(time.DateTime), markdown),
	})
	return err
}

// Push 复用统一语义层 im.send_text。
func (d *MCPDownstream) Push(ctx context.Context, platform, chatID, text string) error {
	_, err := semantic.SendText(ctx, d.Reg, d.Mgr, platform, chatID, text)
	return err
}

// parseDocRef 从创建响应中提取 file_id 与 url(兼容多种返回结构)。
func parseDocRef(resp string) (fileID, url string, err error) {
	var v any
	if err := json.Unmarshal([]byte(resp), &v); err != nil {
		// 非 JSON:尝试当纯文本找 URL
		if idx := strings.Index(resp, "https://docs.qq.com/"); idx >= 0 {
			end := strings.IndexAny(resp[idx:], "\"\n ]}")
			if end < 0 {
				end = len(resp) - idx
			}
			u := resp[idx : idx+end]
			return extractFileID(u), u, nil
		}
		return "", "", fmt.Errorf("创建响应无法解析: %.200s", resp)
	}
	url = findStringDeep(v, []string{"url", "file_url", "fileUrl", "link"})
	fileID = findStringDeep(v, []string{"file_id", "fileID", "fileId", "id", "token"})
	url = normalizeURL(url)
	if fileID == "" && url != "" {
		fileID = extractFileID(url)
	}
	if url == "" && fileID != "" {
		url = "https://docs.qq.com/doc/" + fileID
	}
	return fileID, url, nil
}

// normalizeURL 协议相对 URL 补 https 前缀。
func normalizeURL(u string) string {
	if strings.HasPrefix(u, "//") {
		return "https:" + u
	}
	return u
}

// extractFileID 从腾讯文档 URL 中取 file_id(最后一段路径)。
func extractFileID(u string) string {
	u = strings.TrimSuffix(u, "/")
	if idx := strings.LastIndex(u, "/"); idx >= 0 {
		return u[idx+1:]
	}
	return ""
}

// findStringDeep 在嵌套 JSON 中按候选键名找第一个字符串值。
func findStringDeep(v any, keys []string) string {
	switch t := v.(type) {
	case map[string]any:
		for _, k := range keys {
			if s, ok := t[k].(string); ok && s != "" {
				return s
			}
		}
		for _, sub := range t {
			if s := findStringDeep(sub, keys); s != "" {
				return s
			}
		}
	case []any:
		for _, sub := range t {
			if s := findStringDeep(sub, keys); s != "" {
				return s
			}
		}
	}
	return ""
}

// fieldString 提取 Bitable 字段文本(兼容纯文本/富文本段数组)。
func fieldString(fields map[string]any, name string) string {
	v, ok := fields[name]
	if !ok {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case []any: // 富文本:[{text: "..."}]
		var b strings.Builder
		for _, seg := range t {
			if m, ok := seg.(map[string]any); ok {
				if s, ok := m["text"].(string); ok {
					b.WriteString(s)
				}
			}
		}
		return b.String()
	}
	return fmt.Sprintf("%v", v)
}

// fieldTime 提取 Bitable 时间字段(毫秒时间戳数字/字符串);
// 依次尝试配置字段名与 automatic_fields 的默认名(created_time / 创建时间)。
func fieldTime(fields map[string]any, name string) time.Time {
	for _, key := range []string{name, "created_time", "创建时间"} {
		if ts := parseMs(fields[key]); !ts.IsZero() {
			return ts
		}
	}
	return time.Time{}
}

func parseMs(v any) time.Time {
	var ms int64
	switch t := v.(type) {
	case float64:
		ms = int64(t)
	case string:
		fmt.Sscanf(t, "%d", &ms)
	}
	if ms <= 0 {
		return time.Time{}
	}
	if ms < 1e12 { // 秒级时间戳兜底
		return time.Unix(ms, 0)
	}
	return time.UnixMilli(ms)
}
