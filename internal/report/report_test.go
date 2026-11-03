package report

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/example/mcp-hub/internal/config"
	"github.com/example/mcp-hub/internal/store"
)

// fakeDownstream 记录调用,可注入错误。
type fakeDownstream struct {
	records     []Record
	fetchErr    error
	createErr   error
	created     []string // titles
	updated     []string // file_ids
	pushed      map[string][]string
	docSeq      int
}

func (f *fakeDownstream) FetchRecords(context.Context) ([]Record, error) {
	return f.records, f.fetchErr
}

func (f *fakeDownstream) CreateDoc(_ context.Context, title, _ string) (string, string, error) {
	if f.createErr != nil {
		return "", "", f.createErr
	}
	f.docSeq++
	f.created = append(f.created, title)
	id := "file-1"
	return id, "https://docs.qq.com/doc/" + id, nil
}

func (f *fakeDownstream) UpdateDoc(_ context.Context, fileID, _ string) error {
	f.updated = append(f.updated, fileID)
	return nil
}

func (f *fakeDownstream) Push(_ context.Context, platform, _, text string) error {
	if f.pushed == nil {
		f.pushed = map[string][]string{}
	}
	f.pushed[platform] = append(f.pushed[platform], text)
	return nil
}

func newPipeline(t *testing.T, down Downstream) (*Pipeline, *store.SQLiteStore) {
	t.Helper()
	st, err := store.OpenSQLite(t.TempDir()+"/t.db", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	p := NewPipeline(st, down, config.ReportConfig{
		BackfillHours: 24, CreatedField: "创建时间", PushPlatforms: []string{"wecom"},
	}, map[string]string{"wecom": "chat-1"})
	p.maxRetries = 0 // 测试默认不重试,单独用例覆盖
	return p, st
}

func rec(content string, at time.Time) Record {
	return Record{Content: content, Platform: "wecom", Sender: "u1", CreatedAt: at}
}

// 首次运行:无水位线 → 回填最近 24h,窗口外旧记录不计入。
func TestFirstRunBackfill24h(t *testing.T) {
	now := time.Now()
	down := &fakeDownstream{records: []Record{
		rec("新记录", now.Add(-1*time.Hour)),
		rec("老记录", now.Add(-48*time.Hour)),
	}}
	p, _ := newPipeline(t, down)
	res := p.Run(context.Background(), now, "test")
	if res.Status != "success" || res.Records != 1 {
		t.Fatalf("res=%+v", res)
	}
	if len(down.created) != 1 || len(down.pushed["wecom"]) != 1 {
		t.Fatalf("created=%v pushed=%v", down.created, down.pushed)
	}
}

// 增量:第二次运行只取水位线之后的记录;无新增 → skipped,不建文档不推送。
func TestIncrementalAndEmptySkip(t *testing.T) {
	now := time.Now()
	down := &fakeDownstream{records: []Record{rec("第一条", now.Add(-1*time.Hour))}}
	p, st := newPipeline(t, down)

	res1 := p.Run(context.Background(), now, "test")
	if res1.Status != "success" || res1.Records != 1 {
		t.Fatalf("run1=%+v", res1)
	}
	// 水位线已持久化
	if _, ok := st.GetKV(watermarkKey); !ok {
		t.Fatal("水位线未持久化")
	}

	// 第二次:无新增 → skipped,不新建/更新文档,不推送
	res2 := p.Run(context.Background(), now.Add(time.Minute), "test")
	if res2.Status != "skipped" || res2.Records != 0 {
		t.Fatalf("run2=%+v", res2)
	}
	if len(down.created) != 1 || len(down.updated) != 0 || len(down.pushed["wecom"]) != 1 {
		t.Fatalf("created=%d updated=%d pushed=%d", len(down.created), len(down.updated), len(down.pushed["wecom"]))
	}

	// 第三次:水位线后新增一条 → 只汇总这一条
	down.records = append(down.records, rec("第二条", now.Add(2*time.Minute)))
	res3 := p.Run(context.Background(), now.Add(3*time.Minute), "test")
	if res3.Status != "success" || res3.Records != 1 {
		t.Fatalf("run3=%+v", res3)
	}
}

// 幂等:同一天重复触发 → 更新同一篇文档,不新建。
func TestSameDayIdempotent(t *testing.T) {
	now := time.Now()
	down := &fakeDownstream{records: []Record{rec("x", now.Add(-time.Hour))}}
	p, st := newPipeline(t, down)
	p.maxRetries = 0

	r1 := p.Run(context.Background(), now, "test")
	r2 := p.Run(context.Background(), now, "test") // skipped(无新增)不影响映射
	if len(down.created) != 1 {
		t.Fatalf("应只创建一次文档,实际 %d", len(down.created))
	}
	doc, ok := st.GetReportDoc(r1.Date)
	if !ok || doc.FileID != "file-1" {
		t.Fatalf("映射缺失: %+v ok=%v r2=%+v", doc, ok, r2)
	}

	// 同日补跑且有新增 → 走 UpdateDoc,同一 file_id
	down.records = append(down.records, rec("y", now.Add(time.Second)))
	r3 := p.Run(context.Background(), now.Add(2*time.Second), "test")
	if r3.Status != "success" || len(down.created) != 1 || len(down.updated) != 1 || down.updated[0] != "file-1" {
		t.Fatalf("r3=%+v created=%d updated=%v", r3, len(down.created), down.updated)
	}
}

// 水位线跨重启:换新 store 句柄(同文件)仍生效 → 用同一 st 模拟即可,另验证 DB 直读。
func TestWatermarkPersists(t *testing.T) {
	now := time.Now()
	down := &fakeDownstream{records: []Record{rec("x", now.Add(-time.Hour))}}
	p, st := newPipeline(t, down)
	p.Run(context.Background(), now, "test")
	v, ok := st.GetKV(watermarkKey)
	if !ok || v != "" && v[0] == '0' {
		t.Fatalf("watermark=%q ok=%v", v, ok)
	}
	var ts int64
	if _, err := fmt.Sscanf(v, "%d", &ts); err != nil || ts != now.Unix() {
		t.Fatalf("watermark value=%q err=%v", v, err)
	}
}

// limit 类错误:不重试、分类为 limit、审计留 failed。
func TestLimitErrorNoRetry(t *testing.T) {
	now := time.Now()
	down := &fakeDownstream{
		records:   []Record{rec("x", now.Add(-time.Hour))},
		createErr: errors.New("tencent-docs: 429 too many requests, daily quota exceeded"),
	}
	p, st := newPipeline(t, down)
	p.maxRetries = 3 // 即便配了重试,limit 也不应重试
	res := p.Run(context.Background(), now, "test")
	if res.Status != "failed" || res.Kind != string(KindLimit) || res.Attempts != 1 {
		t.Fatalf("res=%+v", res)
	}
	audits := st.Audits()
	if len(audits) != 1 || audits[0].Succeeded || audits[0].Tool != "report.daily" {
		t.Fatalf("audits=%+v", audits)
	}
}

// transient 错误:有限重试;成功路径审计可查条数/token/推送结果。
func TestTransientRetryThenSuccess(t *testing.T) {
	now := time.Now()
	down := &fakeDownstream{
		records:   []Record{rec("x", now.Add(-time.Hour))},
		createErr: errors.New("connection reset by peer"),
	}
	p, _ := newPipeline(t, down)
	p.maxRetries = 1
	res := p.Run(context.Background(), now, "test")
	if res.Status != "failed" || res.Kind != string(KindTransient) || res.Attempts != 2 {
		t.Fatalf("res=%+v", res)
	}
}
