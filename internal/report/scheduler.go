// 进程内 cron 调度(不依赖外部调度器);表达式/时区/开关均来自配置。
package report

import (
	"context"
	"log/slog"
	"time"

	"github.com/robfig/cron/v3"
)

// Scheduler 包装 cron,按配置定时触发日报。
type Scheduler struct {
	cron *cron.Cron
}

// StartScheduler 启用定时日报;返回 nil 表示未启用或配置非法(已降级为仅手动触发)。
func StartScheduler(ctx context.Context, p *Pipeline) *Scheduler {
	if p == nil {
		return nil // 日报联动未启用(BITABLE_APP_TOKEN 缺失)
	}
	if !p.Cfg.Enabled {
		slog.Info("日报定时调度未启用(report.enabled=false),仅手动触发")
		return nil
	}
	if p.Cfg.Cron == "" {
		slog.Error("report.enabled=true 但 cron 表达式为空,调度降级关闭")
		return nil
	}
	c := cron.New(cron.WithLocation(p.Location()))
	if _, err := c.AddFunc(p.Cfg.Cron, func() {
		res := p.Run(ctx, time.Now(), "cron")
		if res.Status == "failed" {
			slog.Error("定时日报失败", "date", res.Date, "kind", res.Kind, "err", res.Err)
		}
	}); err != nil {
		slog.Error("日报 cron 表达式非法,调度降级关闭", "cron", p.Cfg.Cron, "err", err)
		return nil
	}
	c.Start()
	slog.Info("日报定时调度已启动", "cron", p.Cfg.Cron, "tz", p.Location().String())
	return &Scheduler{cron: c}
}

// Stop 停止调度(优雅退出时调用)。
func (s *Scheduler) Stop() {
	if s == nil {
		return
	}
	ctx := s.cron.Stop()
	<-ctx.Done() // 等待在途任务完成
}
